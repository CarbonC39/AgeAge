package rendering

import (
	"strings"
	"testing"
)

func TestRenderSessionsEscapesLabelsAndShowsMetadata(t *testing.T) {
	got := RenderSessions([]SessionEntry{{
		ID:      "opaque-session-id",
		Name:    "Plan *[Draft]_one`\nnext",
		Preview: "Preview *[text]_one`\nnext",
		Link:    "https://example.test/thread",
		Turns:   3,
		Current: true,
	}})
	for _, want := range []string{
		"[Plan \\*\\[Draft\\]\\_one\\` next](https://example.test/thread)",
		"3 turns · current",
		"ID: `opaque-session-id`",
		"Preview \\*\\[text\\]\\_one\\` next",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered session list missing %q: %s", want, got)
		}
	}
}

func TestRenderSessionRootEscapesDisplayName(t *testing.T) {
	got := RenderSessionRoot("Plan *[Draft]_one`\nnext", "opaque-id")
	if !strings.Contains(got, "**Session: Plan \\*\\[Draft\\]\\_one\\` next**") || !strings.Contains(got, "Session `opaque-id`") {
		t.Fatalf("rendered session root = %q", got)
	}
}
