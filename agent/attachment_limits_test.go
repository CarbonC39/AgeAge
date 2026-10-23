package agent

import (
	"context"
	"strings"
	"testing"

	"ageage/attachment"
	"ageage/config"
)

func TestDefaultAttachmentLimitsAcceptTwoSixMiBFiles(t *testing.T) {
	cfg := config.DefaultConfig()
	payload := []byte(strings.Repeat("log line\n", 6*1024*1024/9))
	input := NormalizeUserInput(context.Background(), "", []attachment.Attachment{
		attachmentFromBytes("one.log", "text/plain", payload),
		attachmentFromBytes("two.log", "text/plain", payload),
	}, cfg, newTmpManager(t.TempDir()))
	files := 0
	for _, part := range input.Parts {
		if strings.HasPrefix(part.Text, "[File ") {
			files++
		}
	}
	if files != 2 || strings.Contains(strings.Join(input.Warnings, " "), "too large") {
		t.Fatalf("two files within separate file/turn limits should succeed: files=%d warnings=%v", files, input.Warnings)
	}
}

func TestAttachmentLimitWarningsDistinguishFileAndTurn(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Multimodal.MaxAttachmentBytes = 4
	cfg.Multimodal.MaxTurnAttachmentBytes = 6
	input := NormalizeUserInput(context.Background(), "", []attachment.Attachment{
		attachmentFromBytes("oversized.txt", "text/plain", []byte("12345")),
		attachmentFromBytes("first.txt", "text/plain", []byte("1234")),
		attachmentFromBytes("second.txt", "text/plain", []byte("1234")),
	}, cfg, newTmpManager(t.TempDir()))
	warnings := strings.Join(input.Warnings, "\n")
	if !strings.Contains(warnings, "single-file attachment limit: 4 bytes") || !strings.Contains(warnings, "remaining turn attachment budget: 2 bytes") {
		t.Fatalf("file and turn failures must explain their separate limits: %s", warnings)
	}
}
