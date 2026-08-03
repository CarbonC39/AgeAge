package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ageage/llm"
)

func TestConversationMutationAndSnapshotIsolation(t *testing.T) {
	var conv Conversation
	conv.Append(
		llm.Message{Role: "system", Content: "system"},
		llm.Message{Role: "user", Content: "hello"},
	)
	if !conv.HasSystem() || conv.Len() != 2 {
		t.Fatalf("conversation state = %#v", conv.All())
	}
	conv.SetSystemContent("updated")
	snapshot := conv.Snapshot()
	snapshot[0].Content = "mutated copy"
	if conv.All()[0].Content != "updated" {
		t.Fatal("Snapshot shared message storage")
	}

	conv.PrependSystem(llm.Message{Role: "system", Content: "first"})
	removed := conv.Splice(1, 3, llm.Message{Role: "assistant", Content: "compressed"})
	if removed != 1 || conv.Len() != 2 || conv.All()[1].Content != "compressed" {
		t.Fatalf("splice result removed=%d messages=%#v", removed, conv.All())
	}
	conv.TruncateTo(1)
	if conv.Len() != 1 {
		t.Fatalf("truncate len = %d", conv.Len())
	}
}

func TestConversationToolHistoryUsesLastUserTurn(t *testing.T) {
	conv := Conversation{}
	conv.Append(
		llm.Message{Role: "user", Content: "old"},
		llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "old-call", Function: llm.FunctionCall{Name: "old", Arguments: `{}`}}}},
		llm.Message{Role: "tool", ToolCallID: "old-call", Content: "old result"},
		llm.Message{Role: "user", Content: "new"},
		llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "new-call", Function: llm.FunctionCall{Name: "new", Arguments: `{"x":1}`}}}},
		llm.Message{Role: "tool", ToolCallID: "new-call", Content: "new result"},
	)
	records := conv.ToolHistory()
	if len(records) != 1 || records[0].Name != "new" || records[0].Result != "new result" {
		t.Fatalf("tool history = %#v", records)
	}
}

func TestSessionHistoryRoundTripAndListing(t *testing.T) {
	ageageDir := t.TempDir()
	sm := NewSessionManager(ageageDir)
	messages := []llm.Message{
		{Role: "system", Content: "do not persist"},
		{Role: "user", Content: strings.Repeat("界", 55)},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call", Function: llm.FunctionCall{Name: "read", Arguments: `{}`}}}},
		{Role: "tool", ToolCallID: "call", Content: "result"},
		{Role: "assistant", Content: "done"},
	}
	if err := sm.SaveHistory("research", messages); err != nil {
		t.Fatal(err)
	}
	loaded, err := sm.LoadHistory("research")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != len(messages)-1 || loaded[0].Role != "user" || loaded[1].ToolCalls[0].ID != "call" {
		t.Fatalf("loaded history = %#v", loaded)
	}
	infos, err := sm.List()
	if err != nil || len(infos) != 1 || infos[0].TurnCount != 1 || !strings.HasSuffix(infos[0].Preview, "…") {
		t.Fatalf("session list = %#v, %v", infos, err)
	}
	if len([]rune(strings.TrimSuffix(infos[0].Preview, "…"))) != 50 {
		t.Fatalf("preview length = %d", len([]rune(infos[0].Preview)))
	}
}

func TestSessionPrefixRenameAndDelete(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	for _, id := range []string{"chat-default", "chat-work", "other"} {
		if err := sm.EnsureSession(id); err != nil {
			t.Fatal(err)
		}
	}
	exact, matches, err := sm.FindByPrefix("chat-work")
	if err != nil || exact == nil || exact.ID != "chat-work" || len(matches) != 0 {
		t.Fatalf("prefix lookup exact=%#v matches=%#v err=%v", exact, matches, err)
	}
	filtered, err := sm.ListWithPrefix("chat")
	if err != nil || len(filtered) != 2 {
		t.Fatalf("filtered sessions = %#v, %v", filtered, err)
	}
	if err := sm.Rename("chat-work", "chat-renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sm.SessionDir("chat-renamed")); err != nil {
		t.Fatal(err)
	}
	if err := sm.Delete("chat-renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sm.SessionDir("chat-renamed")); !os.IsNotExist(err) {
		t.Fatalf("deleted session still exists: %v", err)
	}
}

func TestSanitizeSessionIDAndDerivedPaths(t *testing.T) {
	if got := SanitizeSessionID("  room/@alice:example.org  "); got != "room-alice-example-org" {
		t.Fatalf("sanitized ID = %q", got)
	}
	if got := SanitizeSessionID("!!!"); got != "default" {
		t.Fatalf("empty sanitized ID = %q", got)
	}
	sm := NewSessionManager("/tmp/ageage-test")
	if filepath.Base(sm.ContextPath("one")) != "CONTEXT.md" || filepath.Base(sm.HistoryPath("one")) != "history.jsonl" {
		t.Fatal("derived session paths are incorrect")
	}
}
