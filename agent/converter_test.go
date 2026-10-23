package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"ageage/attachment"
	"ageage/config"
)

func TestConverterStdoutExactlyOneByteOverLimitWarns(t *testing.T) {
	in := normalizeConverterStdout(t, "TestConverterStdoutExactlyOneByteOverLimitHelper")
	if !strings.Contains(strings.Join(in.Warnings, " "), "truncated at 512 KiB") {
		t.Fatalf("expected truncation warning at exactly limit+1: %v", in.Warnings)
	}
	if len(in.Parts) == 0 || !strings.Contains(in.Parts[0].Text, "[... output truncated at 512 KiB ...]") {
		t.Fatalf("output was not truncated: %#v", in.Parts)
	}
	if !utf8.ValidString(in.Parts[0].Text) {
		t.Fatal("truncated output is not valid UTF-8")
	}
}

func TestConverterStdoutUTF8BoundaryTruncatesValidlyAndCompletes(t *testing.T) {
	in := normalizeConverterStdout(t, "TestConverterStdoutUTF8BoundaryHelper")
	if !strings.Contains(strings.Join(in.Warnings, " "), "truncated at 512 KiB") {
		t.Fatalf("expected UTF-8 boundary truncation warning: %v", in.Warnings)
	}
	if len(in.Parts) == 0 || !strings.Contains(in.Parts[0].Text, "[... output truncated at 512 KiB ...]") {
		t.Fatalf("output was not truncated successfully: %#v", in.Parts)
	}
	if !utf8.ValidString(in.Parts[0].Text) {
		t.Fatal("UTF-8 boundary produced invalid output")
	}
	if strings.Contains(strings.Join(in.Warnings, " "), "binary output") {
		t.Fatalf("UTF-8 truncation was misclassified as binary: %v", in.Warnings)
	}
}

func TestConverterStdoutExactlyOneByteOverLimitHelper(t *testing.T) {
	if testingHelperMode("TestConverterStdoutExactlyOneByteOverLimitHelper") {
		_, _ = io.WriteString(os.Stdout, strings.Repeat("a", maxAttachmentTextBytes+1))
	}
}

func TestConverterStdoutUTF8BoundaryHelper(t *testing.T) {
	if testingHelperMode("TestConverterStdoutUTF8BoundaryHelper") {
		_, _ = io.WriteString(os.Stdout, strings.Repeat("a", maxAttachmentTextBytes-1))
		_, _ = io.WriteString(os.Stdout, "界")
		// Reaching this write proves overflow did not cancel the converter.
		_, _ = io.WriteString(os.Stdout, "completed")
	}
}

func normalizeConverterStdout(t *testing.T, helper string) UserInput {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Multimodal.Converters = []config.ConverterConfig{{
		Extensions: []string{"docx"},
		Command:    os.Args[0] + " -test.run=" + helper,
	}}
	a := attachment.Attachment{Filename: "boundary.docx", Size: 6, Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("source")), nil
	}}
	return NormalizeUserInput(context.Background(), "", []attachment.Attachment{a}, cfg, newTmpManager(t.TempDir()))
}

func testingHelperMode(name string) bool {
	for _, arg := range os.Args {
		if arg == "-test.run="+name {
			return true
		}
	}
	return false
}

func TestConverterFileOutputHardLimitStopsActiveWriter(t *testing.T) {
	mgr := newTmpManager(t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	marker := filepath.Join(mgr.dir, "converter-finished.marker")
	in := normalizeConverterFile(t, mgr, "TestConverterFileOutputHardLimitActiveHelper", ctx, marker)
	if elapsed := time.Since(started); elapsed >= 3*time.Second {
		t.Fatalf("hard limit did not stop active converter before parent deadline: %s", elapsed)
	}
	if !strings.Contains(strings.Join(in.Warnings, " "), "hard limit") {
		t.Fatalf("expected hard-limit warning, got %v", in.Warnings)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("converter reached its post-limit marker: stat err=%v", err)
	}
	assertNoConverterOutputFiles(t, mgr)
}

func TestConverterFileOutputHardLimitCatchesQuickExitAndCleansOutput(t *testing.T) {
	mgr := newTmpManager(t.TempDir())
	in := normalizeConverterFile(t, mgr, "TestConverterFileOutputHardLimitQuickHelper", context.Background())
	if !strings.Contains(strings.Join(in.Warnings, " "), "hard limit") {
		t.Fatalf("quick converter overrun escaped hard limit: warnings=%v parts=%#v", in.Warnings, in.Parts)
	}
	if strings.Contains(strings.Join(texts(in.Parts), " "), "converted to text") {
		t.Fatal("quick over-limit converter output was accepted")
	}
	assertNoConverterOutputFiles(t, mgr)
}

func TestConverterFileOutputExactlyHardLimitUsesSoftLimit(t *testing.T) {
	mgr := newTmpManager(t.TempDir())
	in := normalizeConverterFile(t, mgr, "TestConverterFileOutputExactlyHardLimitHelper", context.Background())
	warnings := strings.Join(in.Warnings, " ")
	if strings.Contains(warnings, "hard limit") || !strings.Contains(warnings, "truncated at 512 KiB") {
		t.Fatalf("exact hard limit should pass and use model soft cap: warnings=%v", in.Warnings)
	}
	if !strings.Contains(strings.Join(texts(in.Parts), " "), "converted to text") {
		t.Fatalf("exact hard-limit output did not complete conversion: %#v", in.Parts)
	}
	if partTextLen(in.Parts) > maxAttachmentTextBytes+4*1024 {
		t.Fatalf("soft-capped converter output too large: %d", partTextLen(in.Parts))
	}
}

func TestConverterFileOutputHardLimitActiveHelper(t *testing.T) {
	if !testingHelperMode("TestConverterFileOutputHardLimitActiveHelper") {
		return
	}
	path := converterHelperOutputPath()
	args := converterHelperArgs()
	marker := ""
	if len(args) > 1 {
		marker = args[1]
	}
	if err := os.WriteFile(path, bytesOf('x', maxConverterOutputBytes+1), 0600); err != nil {
		os.Exit(2)
	}
	// The monitor should kill the process long before this post-limit marker.
	time.Sleep(time.Second)
	if marker != "" {
		_ = os.WriteFile(marker, []byte("done"), 0600)
	}
}

func TestConverterFileOutputHardLimitQuickHelper(t *testing.T) {
	if testingHelperMode("TestConverterFileOutputHardLimitQuickHelper") {
		if err := os.WriteFile(converterHelperOutputPath(), bytesOf('x', maxConverterOutputBytes+1), 0600); err != nil {
			os.Exit(2)
		}
	}
}

func TestConverterFileOutputExactlyHardLimitHelper(t *testing.T) {
	if testingHelperMode("TestConverterFileOutputExactlyHardLimitHelper") {
		if err := os.WriteFile(converterHelperOutputPath(), bytesOf('x', maxConverterOutputBytes), 0600); err != nil {
			os.Exit(2)
		}
	}
}

func normalizeConverterFile(t *testing.T, mgr *TmpManager, helper string, ctx context.Context, extraArgs ...string) UserInput {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Multimodal.Converters = []config.ConverterConfig{{
		Extensions: []string{"docx"},
		Command:    os.Args[0] + " -test.run=" + helper + " -- {output} " + strings.Join(extraArgs, " "),
	}}
	a := attachment.Attachment{Filename: "large.docx", Size: 6, Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("source")), nil
	}}
	return NormalizeUserInput(ctx, "", []attachment.Attachment{a}, cfg, mgr)
}

func converterHelperOutputPath() string {
	args := converterHelperArgs()
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func converterHelperArgs() []string {
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			return os.Args[i+1:]
		}
	}
	return nil
}

func assertNoConverterOutputFiles(t *testing.T, mgr *TmpManager) {
	t.Helper()
	mgr.mu.Lock()
	paths := append([]string(nil), mgr.files...)
	mgr.mu.Unlock()
	for _, path := range paths {
		if filepath.Ext(path) == ".md" {
			t.Fatalf("failed converter output was not cleaned up: %q", path)
		}
		if strings.HasSuffix(path, ".done") {
			t.Fatalf("converter ran past hard-limit stop: %q", path)
		}
	}
	entries, err := os.ReadDir(mgr.dir)
	if err != nil {
		t.Fatalf("read converter temp directory: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".done") || strings.HasSuffix(entry.Name(), ".md") {
			t.Fatalf("converter output/marker was not cleaned: %q", entry.Name())
		}
	}
}

func bytesOf(b byte, n int) []byte {
	return []byte(strings.Repeat(string([]byte{b}), n))
}
