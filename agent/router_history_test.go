package agent

import (
	"strings"
	"testing"

	"ageage/config"
	"ageage/llm"
)

func TestFilterHistoryForRouterProjectsImageTurnsToSafeText(t *testing.T) {
	r := &Router{cfg: &config.Config{Router: config.RouterConfig{MaxHistory: 8}}}
	secretURL := "data:image/png;base64,SECRET_BASE64"
	history := []llm.Message{
		{Role: "user", Parts: []llm.ContentPart{{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: secretURL}}}},
		{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "What is this?"}, {Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "https://private.example/image"}}}},
		{Role: "user", Content: "ordinary question"},
		{Role: "tool", Content: "tool result"},
		{Role: "system", Content: "system prompt"},
	}
	got := r.filterHistoryForRouter(history)
	if len(got) != 3 {
		t.Fatalf("projected history has %d messages: %#v", len(got), got)
	}
	if got[0].Role != "user" || got[0].Content != "[User attached 1 image(s)]" {
		t.Fatalf("pure image projection = %#v", got[0])
	}
	if got[1].Content != "What is this?\n[User attached 1 image(s)]" {
		t.Fatalf("caption projection = %#v", got[1])
	}
	if got[2].Content != "ordinary question" {
		t.Fatalf("plain text projection = %#v", got[2])
	}
	for _, msg := range got {
		if len(msg.Parts) != 0 {
			t.Fatalf("router projection retained structured parts: %#v", msg)
		}
		if strings.Contains(msg.Content, "data:image") || strings.Contains(msg.Content, "SECRET_BASE64") || strings.Contains(msg.Content, "private.example") {
			t.Fatalf("image payload leaked into router: %q", msg.Content)
		}
	}
	if len(history[0].Parts) != 1 || history[0].Parts[0].ImageURL.URL != secretURL || len(history[1].Parts) != 2 {
		t.Fatal("original history was mutated")
	}
}

func TestFilterHistoryForRouterKeepsMostRecentProjectedMessagesInOrder(t *testing.T) {
	r := &Router{cfg: &config.Config{Router: config.RouterConfig{MaxHistory: 2}}}
	history := []llm.Message{
		{Role: "user", Content: "old"},
		{Role: "assistant", Content: "middle"},
		{Role: "user", Parts: []llm.ContentPart{{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/jpeg;base64,secret"}}}},
		{Role: "tool", Content: "tool"},
		{Role: "system", Content: "system"},
	}
	got := r.filterHistoryForRouter(history)
	if len(got) != 2 || got[0].Content != "middle" || got[1].Content != "[User attached 1 image(s)]" {
		t.Fatalf("recent projection = %#v", got)
	}
}
