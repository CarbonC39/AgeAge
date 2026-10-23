package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"ageage/attachment"
	"ageage/config"
	"ageage/input"
	"ageage/llm"
)

const (
	defaultAttachmentBytes int64 = 10 * 1024 * 1024
	maxAttachmentTextBytes       = 512 * 1024
	maxIncomingAttachments       = 16
)

// UserInput is a normalized turn, independent of transport and run scheduling.
// It contains no open readers or download callbacks and can later be queued in
// a RunHandle inbox. Warnings are also included as text parts for model context.
// Referenced files belong to the session's TmpManager until history releases them.
type UserInput = input.UserInput

// RunInput submits a previously normalized turn. Normalization can be performed
// before choosing whether to start a run or enqueue input into an active run.
func (a *Agent) RunInput(ctx context.Context, input UserInput, cb llm.StreamCallback) (string, error) {
	return a.RunWithParts(ctx, input.Text, input.Parts, cb)
}

// NormalizeUserInput owns the common incoming attachment pipeline for CLI and IM.
// Each attachment fails independently. Single-file limits and the total
// successfully staged bytes in a turn have separate configuration settings.
func NormalizeUserInput(ctx context.Context, text string, attachments []attachment.Attachment, cfg *config.Config, tmpMgr *TmpManager) UserInput {
	input := UserInput{Text: text}
	if len(attachments) == 0 {
		return input
	}
	if cfg == nil {
		cfg = config.DefaultConfig()
	}
	limit, turnLimit := cfg.Multimodal.AttachmentLimits()
	// Keep limit+1 representable even for a manually constructed configuration.
	if limit == int64(^uint64(0)>>1) {
		limit--
	}
	if turnLimit == int64(^uint64(0)>>1) {
		turnLimit--
	}
	retainPaths := tmpMgr != nil
	if tmpMgr == nil {
		dir, err := os.MkdirTemp("", "ageage-attachments-*")
		if err != nil {
			input.Warnings = append(input.Warnings, "attachments: cannot create temporary storage")
			input.Parts = warningParts(text, input.Warnings)
			return input
		}
		defer os.RemoveAll(dir)
		tmpMgr = newTmpManager(dir)
		defer tmpMgr.ClearAll()
	}
	if text != "" {
		input.Parts = append(input.Parts, llm.ContentPart{Type: "text", Text: text})
	}
	remaining := turnLimit
	for i, att := range attachments {
		if i >= maxIncomingAttachments {
			warning := fmt.Sprintf("attachments: only the first %d files per message can be processed", maxIncomingAttachments)
			input.Warnings = append(input.Warnings, warning)
			input.Parts = append(input.Parts, llm.ContentPart{Type: "text", Text: "[Attachment warning: " + warning + "]"})
			break
		}
		att.Filename = sanitizeAttachmentName(att.Filename)
		effectiveLimit, limitName := limit, "single-file attachment limit"
		if remaining < limit {
			effectiveLimit, limitName = remaining, "remaining turn attachment budget"
		}
		parts, warnings, size := normalizeAttachment(ctx, att, cfg, tmpMgr, effectiveLimit, retainPaths)
		for index := range warnings {
			if strings.HasPrefix(warnings[index], "too large:") {
				warnings[index] += fmt.Sprintf(" (%s: %d bytes)", limitName, effectiveLimit)
			}
		}
		remaining -= size
		input.Parts = append(input.Parts, parts...)
		for _, warning := range warnings {
			warning = fmt.Sprintf("attachment %q: %s", att.Filename, warning)
			input.Warnings = append(input.Warnings, warning)
			input.Parts = append(input.Parts, llm.ContentPart{Type: "text", Text: "[Attachment warning: " + warning + "]"})
		}
	}
	return input
}

func warningParts(text string, warnings []string) []llm.ContentPart {
	var parts []llm.ContentPart
	if text != "" {
		parts = append(parts, llm.ContentPart{Type: "text", Text: text})
	}
	for _, w := range warnings {
		parts = append(parts, llm.ContentPart{Type: "text", Text: "[Attachment warning: " + w + "]"})
	}
	return parts
}

// sanitizeAttachmentName treats both Windows and POSIX paths as untrusted names.
// Files are always created using a random prefix beneath the staging directory.
func sanitizeAttachmentName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ':' {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if name == "" {
		name = "attachment"
	}
	if len(name) > 180 {
		ext := filepath.Ext(name)
		if len(ext) > 30 {
			ext = ""
		}
		name = name[:150]
		for !utf8.ValidString(name) {
			name = name[:len(name)-1]
		}
		name += ext
	}
	return name
}

func normalizeAttachment(ctx context.Context, att attachment.Attachment, cfg *config.Config, mgr *TmpManager, limit int64, retainPaths bool) ([]llm.ContentPart, []string, int64) {
	metadata := fmt.Sprintf("[Attachment %q; MIME: %s; size: %d bytes]", att.Filename, cleanMIME(att.MIMEType), att.Size)
	if att.UnsupportedReason != "" {
		return []llm.ContentPart{{Type: "text", Text: metadata}}, []string{att.UnsupportedReason}, 0
	}
	if att.Size < 0 {
		return nil, []string{"invalid negative file size"}, 0
	}
	if att.Size > limit || limit <= 0 {
		return nil, []string{fmt.Sprintf("too large: reported size %d bytes exceeds the allowed size", att.Size)}, 0
	}
	if att.Open == nil {
		return nil, []string{"download unavailable"}, 0
	}
	if err := ctx.Err(); err != nil {
		return nil, []string{err.Error()}, 0
	}
	body, err := att.Open(ctx)
	if err != nil {
		return nil, []string{fmt.Sprintf("download failed: %s", err)}, 0
	}
	if body == nil {
		return nil, []string{"download returned no stream"}, 0
	}
	defer body.Close()
	path, err := mgr.NewStagedFile(att.Filename)
	if err != nil {
		return nil, []string{"cannot create staged file"}, 0
	}
	keep := false
	defer func() {
		if !keep {
			mgr.Remove(path)
		}
	}()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return nil, []string{"cannot write staged file"}, 0
	}
	// Never trust the platform's size. Read at most limit+1 to detect overflow,
	// streaming directly to disk instead of buffering the incoming body in RAM.
	n, copyErr := io.Copy(f, &io.LimitedReader{R: &attachmentContextReader{ctx: ctx, r: body}, N: limit + 1})
	closeErr := f.Close()
	if n > limit {
		return nil, []string{"too large: download exceeds the allowed size"}, 0
	}
	if copyErr != nil || closeErr != nil {
		return nil, []string{"failed to read or stage attachment"}, 0
	}
	data, err := os.ReadFile(path) // file was exclusively created and bounded above
	if err != nil {
		return nil, []string{"cannot read staged file"}, 0
	}
	sniffed := cleanMIME(http.DetectContentType(data))
	ext := strings.ToLower(filepath.Ext(att.Filename))
	declared := cleanMIME(att.MIMEType)
	inferred := sniffed
	if inferred == "application/octet-stream" && declared != "" {
		inferred = declared
	}
	if inferred == "application/octet-stream" {
		if m := mime.TypeByExtension(ext); m != "" {
			inferred = cleanMIME(m)
		}
	}
	isImage := isVisionMIME(sniffed)
	if isImage && cfg.Multimodal.Vision {
		return []llm.ContentPart{{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:" + sniffed + ";base64," + base64.StdEncoding.EncodeToString(data), Detail: "auto"}}}, nil, n
	}
	// Prefer the content-derived image extension when a filename is missing or
	// misleading, so vision-disabled images can still use a configured converter.
	convExt := strings.TrimPrefix(ext, ".")
	if isImage {
		if e := imageMIMEExtension(sniffed); e != "" {
			convExt = e
		}
	}
	var warnings []string
	conv := cfg.FindConverter(strings.TrimPrefix(ext, "."))
	if conv == nil {
		conv = cfg.FindConverter(convExt)
	}
	if conv == nil {
		// Media often arrives without an extension. Prefer the sniffed MIME,
		// then the platform hint, to locate an existing configured converter.
		for _, mediaType := range []string{sniffed, declared} {
			if mediaType == "application/octet-stream" {
				continue
			}
			extensions, _ := mime.ExtensionsByType(mediaType)
			for _, extension := range extensions {
				if conv = cfg.FindConverter(strings.TrimPrefix(extension, ".")); conv != nil {
					break
				}
			}
			if conv != nil {
				break
			}
		}
	}
	if conv != nil {
		converted, outputPath, truncated, convErr := runConverterContext(ctx, path, conv, mgr)
		if convErr == nil {
			keep = true
			if truncated {
				warnings = append(warnings, "converter output truncated at 512 KiB")
			}
			note, outputNote := "", ""
			if retainPaths {
				note = "; staged at: " + path
				outputNote = "\n[tmp: " + outputPath + "]"
			}
			return []llm.ContentPart{{Type: "text", Text: fmt.Sprintf("[File %q converted to text — %d lines%s]\n\n%s%s", att.Filename, countLines(converted), note, converted, outputNote)}}, warnings, n
		}
		warnings = append(warnings, fmt.Sprintf("conversion failed: %s", convErr))
	}
	if isImage || strings.HasPrefix(sniffed, "image/") {
		warnings = append(warnings, "image cannot be read: vision not enabled or image format unsupported; configure a text converter")
		return []llm.ContentPart{{Type: "text", Text: metadata}}, warnings, n
	}
	if isTextAttachment(data, sniffed) {
		content := string(data)
		if len(content) > maxAttachmentTextBytes {
			content = truncateAttachmentText(content, maxAttachmentTextBytes)
			warning := "text output truncated at 512 KiB"
			if retainPaths {
				warning += "; full file remains staged"
			}
			warnings = append(warnings, warning)
		}
		keep = true
		note := ""
		if retainPaths {
			note = "; staged at: " + path
		}
		return []llm.ContentPart{{Type: "text", Text: fmt.Sprintf("[File %q — %d lines; MIME: %s%s]\n\n%s", att.Filename, countLines(string(data)), inferred, note, content)}}, warnings, n
	}
	warnings = append(warnings, fmt.Sprintf("unsupported binary format (%s); configure a converter", inferred))
	return []llm.ContentPart{{Type: "text", Text: metadata}}, warnings, n
}

type attachmentContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *attachmentContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func cleanMIME(s string) string {
	m, _, err := mime.ParseMediaType(s)
	if err != nil {
		return "application/octet-stream"
	}
	return strings.ToLower(m)
}
func isVisionMIME(m string) bool {
	return m == "image/png" || m == "image/jpeg" || m == "image/gif" || m == "image/webp"
}
func imageMIMEExtension(m string) string {
	switch m {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	}
	return ""
}
func isTextAttachment(data []byte, detected string) bool {
	if !utf8.Valid(data) || !strings.HasPrefix(detected, "text/") {
		return false
	}
	for _, r := range string(data) {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}
func truncateAttachmentText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text + "\n[... output truncated at 512 KiB ...]"
}

func localAttachment(path string, info os.FileInfo) attachment.Attachment {
	return attachment.Attachment{Filename: filepath.Base(path), Size: info.Size(), Open: func(ctx context.Context) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return os.Open(path)
	}}
}

// processAttachment is retained as the local-file adapter; all interpretation
// and limits live in NormalizeUserInput, exactly as for incoming IM files.
func processAttachment(path string, cfg *config.Config, tmpMgr *TmpManager) ([]llm.ContentPart, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	input := NormalizeUserInput(context.Background(), "", []attachment.Attachment{localAttachment(path, info)}, cfg, tmpMgr)
	if len(input.Warnings) > 0 {
		return input.Parts, fmt.Errorf("%s", strings.Join(input.Warnings, "; "))
	}
	return input.Parts, nil
}

// ParseCLIInput retains the CLI's @path grammar, with the same runtime pipeline
// as IM. Missing paths remain literal text (mentions and email addresses).
func ParseCLIInput(userInput string, cfg *config.Config, tmpMgr *TmpManager) (text string, parts []llm.ContentPart, warnings []string) {
	var textTokens []string
	var attachments []attachment.Attachment
	for _, tok := range strings.Fields(userInput) {
		if !strings.HasPrefix(tok, "@") {
			textTokens = append(textTokens, tok)
			continue
		}
		path, err := filepath.Abs(strings.TrimPrefix(tok, "@"))
		if err != nil {
			textTokens = append(textTokens, tok)
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			textTokens = append(textTokens, tok)
			continue
		}
		if !info.Mode().IsRegular() {
			warnings = append(warnings, fmt.Sprintf("attachment %q: not a regular file", sanitizeAttachmentName(path)))
			continue
		}
		attachments = append(attachments, localAttachment(path, info))
	}
	text = strings.Join(textTokens, " ")
	input := NormalizeUserInput(context.Background(), text, attachments, cfg, tmpMgr)
	warnings = append(warnings, input.Warnings...)
	if len(attachments) == 0 && len(warnings) > 0 {
		input.Parts = warningParts(text, warnings)
	}
	return text, input.Parts, warnings
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
