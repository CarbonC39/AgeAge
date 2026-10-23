package agent

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
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
