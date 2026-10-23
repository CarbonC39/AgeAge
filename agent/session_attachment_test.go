package agent

import (
	"strings"
	"testing"

	"ageage/llm"
)

func TestSessionReloadsScreenshotRecordLargerThanFourMiB(t *testing.T) {
	store := NewSessionManager(t.TempDir())
	url := "data:image/png;base64," + strings.Repeat("A", 4*1024*1024)
	messages := []llm.Message{{Role: "user", Parts: []llm.ContentPart{{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: url}}}}, {Role: "assistant", Content: "done"}}
	if err := store.SaveHistory("screenshot", messages); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadHistory("screenshot")
	if err != nil || len(loaded) != 2 || len(loaded[0].Parts) != 1 || loaded[0].Parts[0].ImageURL.URL != url {
		t.Fatalf("screenshot history did not roundtrip: records=%d err=%v", len(loaded), err)
	}
	infos, err := store.List()
	if err != nil || len(infos) != 1 || infos[0].TurnCount != 1 || infos[0].Preview != "[Image attachment]" {
		t.Fatalf("screenshot summary=%#v err=%v", infos, err)
	}
}

func TestSessionPreviewPrefersCaptionAndHidesStagingMetadata(t *testing.T) {
	store := NewSessionManager(t.TempDir())
	messages := []llm.Message{
		{Role: "user", Content: "Inspect the report", Parts: []llm.ContentPart{{Type: "text", Text: "Inspect the report"}, {Type: "text", Text: "[File report.pdf; staged at: /private/staging/path]\n\nconverted report"}}},
		{Role: "assistant", Content: "done"},
	}
	if err := store.SaveHistory("report", messages); err != nil {
		t.Fatal(err)
	}
	infos, err := store.List()
	if err != nil || len(infos) != 1 || infos[0].Preview != "Inspect the report" {
		t.Fatalf("caption preview=%#v err=%v", infos, err)
	}
}
