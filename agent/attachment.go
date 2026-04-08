package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"ageage/config"
	"ageage/llm"
)

// imageExts maps lowercase file extensions to MIME types for images
// that can be sent directly to vision-capable LLMs.
var imageExts = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// processAttachment reads the file at path and returns the appropriate
// ContentPart(s) based on the file type and multimodal config.
//
// For images with vision=true:  base64 data URI image_url part.
// For images with vision=false: text placeholder or converter output.
// For documents with a converter: runs the converter, returns text part with output.
// For other files: reads as text, returns a text part.
func processAttachment(path string, cfg *config.Config, tmpMgr *TmpManager) ([]llm.ContentPart, error) {
	mm := cfg.Multimodal
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot access %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%q is a directory", path)
	}

	ext := strings.ToLower(filepath.Ext(path))

	// ── Image handling ────────────────────────────────────────────────────────
	if mime, isImage := imageExts[ext]; isImage {
		if mm.MaxImageBytes > 0 && info.Size() > mm.MaxImageBytes {
			return nil, fmt.Errorf("image %q is too large (%d bytes, limit %d)", filepath.Base(path), info.Size(), mm.MaxImageBytes)
		}
		if mm.Vision {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read image %q: %w", path, err)
			}
			encoded := base64.StdEncoding.EncodeToString(data)
			return []llm.ContentPart{{
				Type: "image_url",
				ImageURL: &llm.ImageURLPart{
					URL:    fmt.Sprintf("data:%s;base64,%s", mime, encoded),
					Detail: "auto",
				},
			}}, nil
		}
		// vision=false: try converter, else placeholder
		extNoDot := strings.TrimPrefix(ext, ".")
		if conv := cfg.FindConverter(extNoDot); conv != nil {
			text, tmpPath, err := runConverter(path, conv, tmpMgr)
			if err == nil {
				return []llm.ContentPart{{Type: "text", Text: fmt.Sprintf("[Image %q converted to text — %d lines]\n\n%s\n[tmp: %s]", filepath.Base(path), countLines(text), text, tmpPath)}}, nil
			}
		}
		return []llm.ContentPart{{Type: "text", Text: fmt.Sprintf("[Image attachment: %q — vision not enabled and no converter configured]", filepath.Base(path))}}, nil
	}

	// ── Converter-handled document formats ───────────────────────────────────
	extNoDot := strings.TrimPrefix(ext, ".")
	if conv := cfg.FindConverter(extNoDot); conv != nil {
		text, tmpPath, err := runConverter(path, conv, tmpMgr)
		if err != nil {
			return nil, fmt.Errorf("convert %q: %w", filepath.Base(path), err)
		}
		total := countLines(text)
		return []llm.ContentPart{{Type: "text", Text: fmt.Sprintf("[File %q converted to text — %d lines]\n\n%s\n[tmp: %s]", filepath.Base(path), total, text, tmpPath)}}, nil
	}

	// ── Plain text / source code fallback ────────────────────────────────────
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	total := countLines(string(data))
	return []llm.ContentPart{{Type: "text", Text: fmt.Sprintf("[File %q — %d lines]\n\n%s", filepath.Base(path), total, string(data))}}, nil
}

// runConverter executes a configured converter on inputPath, writing output to
// a managed tmp .md file. Returns (output text, tmp path, error).
//
// The command template supports two substitution tokens:
//
//	{input}  — absolute path to the input file
//	{output} — absolute path to the output .md tmp file
func runConverter(inputPath string, conv *config.ConverterConfig, tmpMgr *TmpManager) (string, string, error) {
	outPath, err := tmpMgr.NewFile(".md")
	if err != nil {
		return "", "", err
	}

	absInput, err := filepath.Abs(inputPath)
	if err != nil {
		return "", "", err
	}

	// Split the command template into tokens BEFORE substituting paths, so that
	// file paths containing spaces are never split across multiple arguments.
	// e.g. template: `pdftotext {input} {output}` → ["pdftotext", "{input}", "{output}"]
	// then each token gets the path substituted in-place as a single argument.
	templateTokens := strings.Fields(conv.Command)
	if len(templateTokens) == 0 {
		return "", outPath, fmt.Errorf("converter command is empty")
	}
	cmdParts := make([]string, len(templateTokens))
	for i, tok := range templateTokens {
		tok = strings.ReplaceAll(tok, "{input}", absInput)
		tok = strings.ReplaceAll(tok, "{output}", outPath)
		cmdParts[i] = tok
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, cmdParts[0], cmdParts[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", outPath, fmt.Errorf("converter exited with error: %w\n%s", err, string(out))
	}

	// Some converters write to stdout instead of the output file.
	// If the output file is empty, use stdout as the content.
	content, readErr := os.ReadFile(outPath)
	if readErr != nil || len(strings.TrimSpace(string(content))) == 0 {
		if len(out) > 0 {
			if writeErr := os.WriteFile(outPath, out, 0o644); writeErr == nil {
				content = out
			}
		}
	}

	if len(strings.TrimSpace(string(content))) == 0 {
		return "", outPath, fmt.Errorf("converter produced no output")
	}

	// Cap output to prevent a single attachment exhausting the LLM context window.
	const maxConverterBytes = 512 * 1024 // 512 KB
	if len(content) > maxConverterBytes {
		content = content[:maxConverterBytes]
		// Trim to the last newline to avoid cutting mid-line.
		if idx := strings.LastIndexByte(string(content), '\n'); idx > 0 {
			content = content[:idx]
		}
		content = append(content, []byte("\n[... output truncated at 512 KB ...]")...)
	}

	return string(content), outPath, nil
}

// ParseCLIInput scans userInput for @path tokens, processes each as a file
// attachment, and returns:
//   - cleaned text (with @path tokens removed)
//   - accumulated content parts (text part first, then attachments)
//   - non-fatal warning strings for missing files or oversized images
func ParseCLIInput(userInput string, cfg *config.Config, tmpMgr *TmpManager) (text string, parts []llm.ContentPart, warnings []string) {
	tokens := strings.Fields(userInput)
	var textTokens []string
	var attParts []llm.ContentPart

	for _, tok := range tokens {
		if !strings.HasPrefix(tok, "@") {
			textTokens = append(textTokens, tok)
			continue
		}

		rawPath := strings.TrimPrefix(tok, "@")
		absPath, err := filepath.Abs(rawPath)
		if err != nil {
			textTokens = append(textTokens, tok) // keep as-is
			warnings = append(warnings, fmt.Sprintf("cannot resolve path %q: %s", rawPath, err))
			continue
		}

		if _, err := os.Stat(absPath); err != nil {
			textTokens = append(textTokens, tok) // not a file — might be an email or mention
			continue
		}

		fileParts, err := processAttachment(absPath, cfg, tmpMgr)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("attachment %q: %s", rawPath, err))
			continue
		}
		attParts = append(attParts, fileParts...)
	}

	cleanText := strings.Join(textTokens, " ")

	if len(attParts) == 0 {
		// No attachments — return plain text, no parts.
		return cleanText, nil, warnings
	}

	// Build multimodal parts: text first, then attachments.
	parts = append([]llm.ContentPart{{Type: "text", Text: cleanText}}, attParts...)
	return cleanText, parts, warnings
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
