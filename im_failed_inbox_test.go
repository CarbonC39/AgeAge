package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ageage/agent"
	"ageage/llm"
)

func TestPersistFailedInboxAppendsPrivatelyAndDrainsInOrder(t *testing.T) {
	dir := t.TempDir()
	want := []agent.UserInput{
		{Text: "initial"},
		{Text: "first update", Parts: []llm.ContentPart{{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,a"}}}},
		{Text: "second update"},
	}
	if err := persistFailedInbox(dir, want[:2]); err != nil {
		t.Fatal(err)
	}
	if err := persistFailedInbox(dir, want[2:]); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, failedInboxFilename)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("failed inbox mode = %o, want 600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []agent.UserInput
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("persisted %d inputs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Text != want[i].Text {
			t.Fatalf("input %d = %q, want %q", i, got[i].Text, want[i].Text)
		}
	}
	if got[1].Parts[0].ImageURL.URL != want[1].Parts[0].ImageURL.URL {
		t.Fatalf("attachment did not survive persistence: %#v", got[1].Parts)
	}
}
