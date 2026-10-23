package agent

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ageage/attachment"
	"ageage/config"
	"ageage/llm"
)

func attachmentFromBytes(name, mime string, data []byte) attachment.Attachment {
	return attachment.Attachment{Filename: name, MIMEType: mime, Size: int64(len(data)), Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(string(data))), nil
	}}
}

func TestNormalizeUserInputTextAndMultipleAttachments(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Multimodal.Vision = false
	mgr := newTmpManager(t.TempDir())
	in := NormalizeUserInput(context.Background(), "hello", []attachment.Attachment{
		attachmentFromBytes("one.txt", "text/plain", []byte("first")),
		attachmentFromBytes("two.txt", "text/plain", []byte("second")),
	}, cfg, mgr)
	if in.Text != "hello" {
		t.Fatalf("Text = %q", in.Text)
	}
	if len(in.Parts) != 3 || in.Parts[0].Type != "text" || in.Parts[0].Text != "hello" {
		t.Fatalf("unexpected parts: %#v", in.Parts)
	}
	joined := in.Parts[1].Text + in.Parts[2].Text
	if !strings.Contains(joined, "first") || !strings.Contains(joined, "second") {
		t.Fatalf("missing attachment content: %q", joined)
	}
	if len(in.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", in.Warnings)
	}
	plain := NormalizeUserInput(context.Background(), "just text", nil, cfg, mgr)
	if plain.Text != "just text" || len(plain.Parts) != 0 {
		t.Fatalf("plain input = %#v", plain)
	}
}

func TestNormalizeUserInputWithoutTmpManagerHasNoDanglingPath(t *testing.T) {
	cfg := config.DefaultConfig()
	in := NormalizeUserInput(context.Background(), "caption", []attachment.Attachment{attachmentFromBytes("note.txt", "text/plain", []byte("content survives"))}, cfg, nil)
	if len(in.Warnings) != 0 || !strings.Contains(strings.Join(texts(in.Parts), " "), "content survives") {
		t.Fatalf("nil manager input failed: parts=%#v warnings=%v", in.Parts, in.Warnings)
	}
	joined := strings.Join(texts(in.Parts), " ")
	if strings.Contains(joined, "staged at:") || strings.Contains(joined, "[tmp:") {
		t.Fatalf("input references temporary path that was cleaned up: %q", joined)
	}
}

func TestNormalizeUserInputVisionSniffsPNGAndDisabledVisionWarns(t *testing.T) {
	// A real PNG signature with an intentionally misleading extension and MIME.
	png := []byte("\x89PNG\r\n\x1a\n" + "minimal-png-payload")
	a := attachmentFromBytes("photo.bin", "application/octet-stream", png)
	cfg := config.DefaultConfig()
	in := NormalizeUserInput(context.Background(), "", []attachment.Attachment{a}, cfg, newTmpManager(t.TempDir()))
	if len(in.Parts) != 1 || in.Parts[0].Type != "image_url" || in.Parts[0].ImageURL == nil {
		t.Fatalf("PNG not recognized as image: %#v; warnings %v", in.Parts, in.Warnings)
	}
	if !strings.HasPrefix(in.Parts[0].ImageURL.URL, "data:image/png;base64,") {
		t.Fatalf("wrong image URI: %q", in.Parts[0].ImageURL.URL)
	}
	got, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(in.Parts[0].ImageURL.URL, "data:image/png;base64,"))
	if err != nil || string(got) != string(png) {
		t.Fatalf("PNG bytes changed: %v", err)
	}

	cfg.Multimodal.Vision = false
	in = NormalizeUserInput(context.Background(), "", []attachment.Attachment{a}, cfg, newTmpManager(t.TempDir()))
	if len(in.Warnings) == 0 {
		t.Fatal("vision-disabled image should produce a warning")
	}
	if len(in.Parts) == 0 || in.Parts[0].Type == "image_url" {
		t.Fatalf("image part emitted with vision disabled: %#v", in.Parts)
	}
}

func TestNormalizeUserInputConverterAndOutputLimit(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Multimodal.Vision = false
	cfg.Multimodal.Converters = []config.ConverterConfig{{Extensions: []string{"docx"}, Command: os.Args[0] + " -test.run=TestAttachmentConverterHelper -- {input} {output}"}}
	a := attachmentFromBytes("report.docx", "application/octet-stream", []byte("source"))
	in := NormalizeUserInput(context.Background(), "", []attachment.Attachment{a}, cfg, newTmpManager(t.TempDir()))
	if len(in.Parts) == 0 || !strings.Contains(in.Parts[0].Text, "converted-text") {
		t.Fatalf("converter output absent: %#v, warnings=%v", in.Parts, in.Warnings)
	}

	cfg.Multimodal.Converters[0].Command = os.Args[0] + " -test.run=TestAttachmentConverterLargeHelper -- {input} {output}"
	in = NormalizeUserInput(context.Background(), "", []attachment.Attachment{a}, cfg, newTmpManager(t.TempDir()))
	if len(in.Warnings) == 0 {
		t.Fatal("truncated converter output should be accompanied by a warning")
	}
	if len(in.Parts) == 0 || len(in.Parts[0].Text) > 512*1024+2048 {
		t.Fatalf("converter text exceeds bound: parts=%d textLen=%d", len(in.Parts), partTextLen(in.Parts))
	}
}

func TestNormalizeUserInputConverterByMIMEWithoutExtension(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Multimodal.Converters = []config.ConverterConfig{
		{Extensions: []string{"pdf"}, Command: os.Args[0] + " -test.run=TestAttachmentConverterHelper -- {input} {output}"},
		{Extensions: []string{"png"}, Command: os.Args[0] + " -test.run=TestAttachmentConverterHelper -- {input} {output}"},
	}
	pdf := []byte("%PDF-1.4\nmock pdf payload")
	for _, name := range []string{"report", "report.bin"} {
		in := NormalizeUserInput(context.Background(), "", []attachment.Attachment{attachmentFromBytes(name, "application/octet-stream", pdf)}, cfg, newTmpManager(t.TempDir()))
		joined := strings.Join(texts(in.Parts), " ")
		if !strings.Contains(joined, "converted-text") || strings.Contains(strings.Join(in.Warnings, " "), "unsupported binary format") {
			t.Fatalf("PDF named %q was not converted based on sniffed MIME: parts=%#v warnings=%v", name, in.Parts, in.Warnings)
		}
	}

	// With Vision disabled, a real PNG signature still selects the configured
	// image converter even when neither filename extension nor MIME is useful.
	cfg.Multimodal.Vision = false
	png := []byte("\x89PNG\r\n\x1a\nmock png payload")
	in := NormalizeUserInput(context.Background(), "", []attachment.Attachment{attachmentFromBytes("image", "application/octet-stream", png)}, cfg, newTmpManager(t.TempDir()))
	if !strings.Contains(strings.Join(texts(in.Parts), " "), "converted-text") {
		t.Fatalf("extensionless PNG did not use png converter: parts=%#v warnings=%v", in.Parts, in.Warnings)
	}
}

func TestAttachmentConverterHelper(t *testing.T) {
	converterHelper(t, "converted-text")
}

func TestAttachmentConverterLargeHelper(t *testing.T) {
	converterHelper(t, strings.Repeat("x", 600*1024))
}

func TestAttachmentConverterHugeFileHelper(t *testing.T) {
	converterHelper(t, strings.Repeat("y", 3*1024*1024))
}

func TestAttachmentConverterStdoutHelper(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-test.run=TestAttachmentConverterStdoutHelper") {
		return
	}
	_, _ = os.Stdout.WriteString("stdout-converted")
}

func TestAttachmentConverterHugeStdoutHelper(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-test.run=TestAttachmentConverterHugeStdoutHelper") {
		return
	}
	_, _ = io.WriteString(os.Stdout, strings.Repeat("z", 600*1024))
}

func TestAttachmentConverterFailureHelper(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-test.run=TestAttachmentConverterFailureHelper") {
		return
	}
	_, _ = os.Stderr.WriteString("converter deliberately failed")
	os.Exit(7)
}

func converterHelper(t *testing.T, content string) {
	if !strings.Contains(strings.Join(os.Args, " "), "-test.run=TestAttachmentConverter") {
		return
	}
	args := os.Args
	for _, arg := range args {
		if arg == "--" && len(args) > 3 {
			if err := os.WriteFile(args[len(args)-1], []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
}

func partTextLen(parts []llm.ContentPart) int {
	n := 0
	for _, p := range parts {
		n += len(p.Text)
	}
	return n
}

func TestNormalizeUserInputUnsupportedMetadataAndReadLimit(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Multimodal.MaxAttachmentBytes = 4
	opened := false
	unsupported := attachment.Attachment{Filename: "voice.ogg", MIMEType: "audio/ogg", UnsupportedReason: "encrypted", Open: func(context.Context) (io.ReadCloser, error) {
		opened = true
		return io.NopCloser(strings.NewReader("x")), nil
	}}
	badMeta := attachment.Attachment{Filename: "huge.txt", Size: 5, Open: func(context.Context) (io.ReadCloser, error) {
		opened = true
		return io.NopCloser(strings.NewReader("12345")), nil
	}}
	in := NormalizeUserInput(context.Background(), "", []attachment.Attachment{unsupported, badMeta}, cfg, newTmpManager(t.TempDir()))
	if opened {
		t.Fatal("metadata-rejected attachment was opened")
	}
	if len(in.Warnings) < 2 {
		t.Fatalf("expected warnings for unsupported and oversized metadata: %v", in.Warnings)
	}

	var read, closed int
	stream := attachment.Attachment{Filename: "stream.txt", Open: func(context.Context) (io.ReadCloser, error) {
		return &trackingReader{Reader: strings.NewReader("0123456789"), read: &read, closed: &closed}, nil
	}}
	in = NormalizeUserInput(context.Background(), "", []attachment.Attachment{stream}, cfg, newTmpManager(t.TempDir()))
	if read > 5 {
		t.Fatalf("read %d bytes, want at most limit+1", read)
	}
	if closed != 1 {
		t.Fatalf("stream close count = %d", closed)
	}
	if len(in.Warnings) == 0 {
		t.Fatal("expected over-limit stream warning")
	}
}

func TestNormalizeUserInputTotalBudgetAndOverrunDoesNotBlockLaterFile(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Multimodal.MaxAttachmentBytes = 5
	cfg.Multimodal.MaxTurnAttachmentBytes = 5
	cfg.Multimodal.Vision = false
	var opens []string
	makeAtt := func(name, content string) attachment.Attachment {
		return attachment.Attachment{Filename: name, Size: int64(len(content)), Open: func(context.Context) (io.ReadCloser, error) {
			opens = append(opens, name)
			return io.NopCloser(strings.NewReader(content)), nil
		}}
	}
	in := NormalizeUserInput(context.Background(), "", []attachment.Attachment{makeAtt("first.txt", "123"), makeAtt("second.txt", "456")}, cfg, newTmpManager(t.TempDir()))
	if len(opens) != 1 {
		t.Fatalf("total budget should reject second attachment before opening; opens=%v warnings=%v", opens, in.Warnings)
	}
	if len(in.Warnings) == 0 {
		t.Fatal("expected total-budget warning")
	}

	// Unknown metadata size cannot prevent the streaming overrun check, and the
	// failure of that stream must not stop a following smaller attachment.
	var read, closed int
	over := attachment.Attachment{Filename: "over.txt", Open: func(context.Context) (io.ReadCloser, error) {
		return &trackingReader{Reader: strings.NewReader("123456789"), read: &read, closed: &closed}, nil
	}}
	good := makeAtt("good.txt", "ok")
	in = NormalizeUserInput(context.Background(), "", []attachment.Attachment{over, good}, cfg, newTmpManager(t.TempDir()))
	if closed != 1 || read > 6 {
		t.Fatalf("over-limit stream was not bounded and closed: read=%d closed=%d", read, closed)
	}
	if !strings.Contains(strings.Join(texts(in.Parts), " "), "ok") {
		t.Fatalf("valid later attachment lost after overflow: %#v warnings=%v", in.Parts, in.Warnings)
	}
}

func TestNormalizeUserInputSameNamesAndMisleadingImageClaims(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := newTmpManager(t.TempDir())
	attachments := []attachment.Attachment{attachmentFromBytes("same.txt", "text/plain", []byte("one")), attachmentFromBytes("same.txt", "text/plain", []byte("two"))}
	in := NormalizeUserInput(context.Background(), "", attachments, cfg, mgr)
	entries, err := os.ReadDir(mgr.StagingDir())
	if err != nil || len(entries) != 2 {
		t.Fatalf("same-name attachments overwrote staging: entries=%d err=%v", len(entries), err)
	}
	if !strings.Contains(strings.Join(texts(in.Parts), " "), "one") || !strings.Contains(strings.Join(texts(in.Parts), " "), "two") {
		t.Fatalf("same-name content missing: %#v", in.Parts)
	}

	// Neither an image suffix nor an image MIME declaration makes arbitrary
	// binary bytes safe to send to Vision.
	fake := attachmentFromBytes("fake.png", "image/png", []byte{0, 1, 2, 3, 0xff, 0x10})
	in = NormalizeUserInput(context.Background(), "", []attachment.Attachment{fake}, cfg, newTmpManager(t.TempDir()))
	for _, p := range in.Parts {
		if p.Type == "image_url" {
			t.Fatalf("unverified image was sent to vision: %#v", p)
		}
	}
	if len(in.Warnings) == 0 {
		t.Fatal("fake image should be rejected with warning")
	}
}

func TestNormalizeUserInputConverterStdoutAndFailureFallback(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Multimodal.Converters = []config.ConverterConfig{{Extensions: []string{"docx"}, Command: os.Args[0] + " -test.run=TestAttachmentConverterStdoutHelper"}}
	a := attachmentFromBytes("x.docx", "", []byte("source"))
	in := NormalizeUserInput(context.Background(), "", []attachment.Attachment{a}, cfg, newTmpManager(t.TempDir()))
	if !strings.Contains(strings.Join(texts(in.Parts), " "), "stdout-converted") {
		t.Fatalf("stdout fallback missing: %#v warnings=%v", in.Parts, in.Warnings)
	}

	cfg.Multimodal.Converters[0].Command = os.Args[0] + " -test.run=TestAttachmentConverterFailureHelper"
	in = NormalizeUserInput(context.Background(), "", []attachment.Attachment{a}, cfg, newTmpManager(t.TempDir()))
	if len(in.Warnings) == 0 || !strings.Contains(strings.Join(in.Warnings, " "), "conversion failed") {
		t.Fatalf("failed converter must warn: %v", in.Warnings)
	}
	if !strings.Contains(strings.Join(texts(in.Parts), " "), "source") {
		t.Fatalf("text fallback was lost on converter error: %#v", in.Parts)
	}
}

func TestNormalizeUserInputCancelledContextClosesOpenedStream(t *testing.T) {
	cfg := config.DefaultConfig()
	ctx, cancel := context.WithCancel(context.Background())
	var closed int
	a := attachment.Attachment{Filename: "slow.txt", Open: func(context.Context) (io.ReadCloser, error) {
		cancel()
		return &trackingReader{Reader: strings.NewReader("payload"), read: new(int), closed: &closed}, nil
	}}
	in := NormalizeUserInput(ctx, "", []attachment.Attachment{a}, cfg, newTmpManager(t.TempDir()))
	if closed != 1 {
		t.Fatalf("opened stream close count = %d", closed)
	}
	if len(in.Warnings) == 0 {
		t.Fatal("cancelled read should be reported")
	}
}

type trackingReader struct {
	io.Reader
	read, closed *int
}

func (r *trackingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	*r.read += n
	return n, err
}
func (r *trackingReader) Close() error { *r.closed++; return nil }

func TestNormalizeUserInputSanitizesSuccessfulTraversalFilename(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Multimodal.Vision = false
	workspace := t.TempDir()
	mgr := newTmpManager(workspace)
	in := NormalizeUserInput(context.Background(), "", []attachment.Attachment{attachmentFromBytes("../../escape\\secret.txt", "text/plain", []byte("survived"))}, cfg, mgr)
	if len(in.Warnings) != 0 || !strings.Contains(strings.Join(texts(in.Parts), " "), "survived") {
		t.Fatalf("successful traversal-named attachment failed: parts=%#v warnings=%v", in.Parts, in.Warnings)
	}
	for _, p := range in.Parts {
		if strings.Contains(p.Text, "../") || strings.Contains(p.Text, `\`) {
			t.Fatalf("unsafe name leaked: %q", p.Text)
		}
	}
	root, err := filepath.Abs(mgr.StagingDir())
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("staged files = %d, err=%v", len(entries), err)
	}
	stagedPath := filepath.Join(root, entries[0].Name())
	rel, err := filepath.Rel(root, stagedPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Dir(rel) != "." {
		t.Fatalf("staged file escaped root: root=%q path=%q rel=%q err=%v", root, stagedPath, rel, err)
	}
	if strings.Contains(entries[0].Name(), "..") || strings.ContainsAny(entries[0].Name(), `/\\`) || !strings.HasSuffix(entries[0].Name(), "secret.txt") {
		t.Fatalf("unsafe staged basename: %q", entries[0].Name())
	}
	if escaped := filepath.Join(workspace, "escape", "secret.txt"); func() bool { _, e := os.Stat(escaped); return !os.IsNotExist(e) }() {
		t.Fatalf("unexpected file created outside staging root")
	}

	// Empty names and concurrent duplicate names must still be stored under
	// unique paths inside the staging root.
	if got := NormalizeUserInput(context.Background(), "", []attachment.Attachment{attachmentFromBytes("", "text/plain", []byte("empty-name"))}, cfg, mgr); len(got.Warnings) != 0 {
		t.Fatalf("empty filename was rejected: %v", got.Warnings)
	}
	const concurrent = 8
	var wg sync.WaitGroup
	paths := make(chan string, concurrent)
	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := NormalizeUserInput(context.Background(), "", []attachment.Attachment{attachmentFromBytes("same.txt", "text/plain", []byte("same"))}, cfg, mgr)
			for _, part := range got.Parts {
				if strings.Contains(part.Text, "staged at: ") {
					path := strings.SplitN(strings.SplitN(part.Text, "staged at: ", 2)[1], "]", 2)[0]
					paths <- path
				}
			}
		}()
	}
	wg.Wait()
	close(paths)
	seen := map[string]bool{}
	for path := range paths {
		if seen[path] {
			t.Fatalf("concurrent duplicate names share staged path %q", path)
		}
		seen[path] = true
	}
	if len(seen) != concurrent {
		t.Fatalf("got %d concurrent staged paths, want %d", len(seen), concurrent)
	}
}

func TestNormalizeUserInputCapsConverterOutputAndContinues(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Multimodal.Vision = false
	cfg.Multimodal.Converters = []config.ConverterConfig{{Extensions: []string{"docx"}, Command: os.Args[0] + " -test.run=TestAttachmentConverterHugeStdoutHelper"}}
	attachments := []attachment.Attachment{attachmentFromBytes("noisy.docx", "", []byte("source")), attachmentFromBytes("later.txt", "text/plain", []byte("later-file-survived"))}
	in := NormalizeUserInput(context.Background(), "", attachments, cfg, newTmpManager(t.TempDir()))
	if !strings.Contains(strings.Join(in.Warnings, " "), "truncated at 512 KiB") {
		t.Fatalf("expected converter truncation warning: %v", in.Warnings)
	}
	if !strings.Contains(strings.Join(texts(in.Parts), " "), "later-file-survived") {
		t.Fatalf("later file was not processed after converter stdout cap: %#v", in.Parts)
	}
	if partTextLen(in.Parts) > 512*1024+4*1024 {
		t.Fatalf("converter output was not capped: %d bytes", partTextLen(in.Parts))
	}

	cfg.Multimodal.Converters[0].Command = os.Args[0] + " -test.run=TestAttachmentConverterHugeFileHelper -- {output}"
	in = NormalizeUserInput(context.Background(), "", attachments, cfg, newTmpManager(t.TempDir()))
	warnings := strings.Join(in.Warnings, " ")
	if !strings.Contains(warnings, "hard limit") {
		t.Fatalf("large converter output should exceed hard limit: %v", in.Warnings)
	}
	if strings.Contains(strings.Join(texts(in.Parts), " "), "converted to text") {
		t.Fatal("hard-limit converter output must not be accepted as a conversion")
	}
	if !strings.Contains(strings.Join(texts(in.Parts), " "), "later-file-survived") {
		t.Fatalf("later file was not processed after oversized converter file: %#v", in.Parts)
	}
}

func texts(parts []llm.ContentPart) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Text)
	}
	return out
}

func TestTmpManagerGCAndClearAllReclaimAttachmentFiles(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Multimodal.Vision = false
	mgr := newTmpManager(t.TempDir())
	in := NormalizeUserInput(context.Background(), "", []attachment.Attachment{attachmentFromBytes("keep.txt", "text/plain", []byte("payload"))}, cfg, mgr)
	entries, err := os.ReadDir(mgr.StagingDir())
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected staged attachment: entries=%d err=%v", len(entries), err)
	}
	mgr.GC(nil)
	entries, err = os.ReadDir(mgr.StagingDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("GC did not reclaim staging files: entries=%d err=%v", len(entries), err)
	}
	_ = in

	// A staged path present in current history must survive GC, then be removed
	// once the message referencing it is released.
	in = NormalizeUserInput(context.Background(), "", []attachment.Attachment{attachmentFromBytes("retain.txt", "text/plain", []byte("retained"))}, cfg, mgr)
	var stagedPath string
	for _, p := range in.Parts {
		if strings.Contains(p.Text, "staged at: ") {
			stagedPath = strings.SplitN(strings.SplitN(p.Text, "staged at: ", 2)[1], "]", 2)[0]
		}
	}
	if stagedPath == "" {
		t.Fatalf("staged path not referenced in parts: %#v", in.Parts)
	}
	mgr.GC([]llm.Message{{Role: "user", Parts: in.Parts}})
	if _, err := os.Stat(stagedPath); err != nil {
		t.Fatalf("GC removed referenced staging: %v", err)
	}
	mgr.GC(nil)
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("GC retained released staging: %v", err)
	}

	// Converter output is a managed file retained while referenced in history.
	cfg.Multimodal.Converters = []config.ConverterConfig{{Extensions: []string{"docx"}, Command: os.Args[0] + " -test.run=TestAttachmentConverterHelper -- {input} {output}"}}
	in = NormalizeUserInput(context.Background(), "", []attachment.Attachment{attachmentFromBytes("keep.docx", "", []byte("x"))}, cfg, mgr)
	managed := filepath.Join(mgr.dir, "missing")
	files, _ := filepath.Glob(filepath.Join(mgr.dir, "att-*.md"))
	if len(files) == 0 {
		t.Fatalf("converter produced no managed file: %#v warnings=%v", in.Parts, in.Warnings)
	}
	managed = files[0]
	mgr.GC(nil)
	if _, err := os.Stat(managed); !os.IsNotExist(err) {
		t.Fatalf("GC retained unreferenced converter result: %v", err)
	}
	in = NormalizeUserInput(context.Background(), "", []attachment.Attachment{attachmentFromBytes("again.docx", "", []byte("x"))}, cfg, mgr)
	files, _ = filepath.Glob(filepath.Join(mgr.dir, "att-*.md"))
	if len(files) == 0 {
		t.Fatal("converter produced no file before ClearAll")
	}
	mgr.ClearAll()
	for _, f := range files {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("ClearAll retained %s: %v", f, err)
		}
	}
	_ = in
}

func TestParseCLIInputStillProcessesAtFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(p, []byte("cli-file-content"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	text, parts, warnings := ParseCLIInput("please @"+p, cfg, newTmpManager(t.TempDir()))
	if text != "please" || len(warnings) != 0 || len(parts) < 2 {
		t.Fatalf("CLI parse regression: text=%q parts=%#v warnings=%v", text, parts, warnings)
	}
	if !strings.Contains(strings.Join(texts(parts), " "), "cli-file-content") {
		t.Fatalf("file content missing: %#v", parts)
	}
}
