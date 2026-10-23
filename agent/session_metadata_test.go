package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ageage/conversation"
	"ageage/llm"
)

func TestSessionDisplayNameAndCanonicalIDAreIndependent(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	id := "room-prefix-thread-opaque"
	if err := sm.CreateSession(id); err != nil {
		t.Fatal(err)
	}
	if err := sm.SetDisplayName(id, "Research notes"); err != nil {
		t.Fatal(err)
	}
	if err := sm.SaveHistory(id, []llm.Message{{Role: "user", Content: "latest request preview"}}); err != nil {
		t.Fatal(err)
	}

	name, err := sm.DisplayName(id)
	if err != nil || name != "Research notes" {
		t.Fatalf("DisplayName = %q, %v", name, err)
	}
	infos, err := sm.ListWithPrefix("room-prefix")
	if err != nil || len(infos) != 1 {
		t.Fatalf("ListWithPrefix = %#v, %v", infos, err)
	}
	got := infos[0]
	if got.FullID != id || got.ID != "thread-opaque" || got.DisplayName != "Research notes" {
		t.Fatalf("session identity/name = %#v", got)
	}
	if got.Preview != "latest request preview" || got.TurnCount != 1 {
		t.Fatalf("session preview/count = %#v", got)
	}

	legacyID := "room-prefix-legacy"
	if err := sm.CreateSession(legacyID); err != nil {
		t.Fatal(err)
	}
	legacy, err := sm.ListWithPrefix("room-prefix")
	if err != nil || len(legacy) != 2 {
		t.Fatalf("legacy ListWithPrefix = %#v, %v", legacy, err)
	}
	var foundLegacy bool
	for _, info := range legacy {
		if info.FullID == legacyID {
			foundLegacy = info.ID == "legacy" && info.DisplayName == "legacy"
		}
	}
	if !foundLegacy {
		t.Fatalf("legacy session did not fall back to prefix-relative ID: %#v", legacy)
	}
}

func TestSessionDisplayNameDoesNotCreateMissingSession(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	if _, err := sm.DisplayName("missing"); !os.IsNotExist(err) {
		t.Fatalf("DisplayName missing error = %v", err)
	}
	if err := sm.SetDisplayName("missing", "name"); !os.IsNotExist(err) {
		t.Fatalf("SetDisplayName missing error = %v", err)
	}
	if _, err := os.Stat(sm.SessionDir("missing")); !os.IsNotExist(err) {
		t.Fatalf("display name operation created missing session: %v", err)
	}
}

func TestSessionHistoryRoundTripsInputReplyAndListingSkipsIt(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	id := "typed-answer"
	messages := []llm.Message{
		{Role: "user", Content: "Take a look at this"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "ask", Function: llm.FunctionCall{Name: "ask_user", Arguments: `{}`}}}},
		{Role: "tool", ToolCallID: "ask", Content: "Screenshot caption"},
		{Role: "user", Content: "Screenshot caption", InputReply: true, Parts: []llm.ContentPart{
			{Type: "text", Text: "Screenshot caption"},
			{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,AA=="}},
		}},
		{Role: "assistant", Content: "Done"},
	}
	if err := sm.SaveHistory(id, messages); err != nil {
		t.Fatal(err)
	}
	loaded, err := sm.LoadHistory(id)
	if err != nil || len(loaded) != len(messages) {
		t.Fatalf("loaded history = %#v, %v", loaded, err)
	}
	if !loaded[3].InputReply || len(loaded[3].Parts) != 2 || loaded[3].Parts[1].ImageURL.URL != "data:image/png;base64,AA==" {
		t.Fatalf("typed internal reply did not round trip: %#v", loaded[3])
	}
	infos, err := sm.List()
	if err != nil || len(infos) != 1 {
		t.Fatalf("List = %#v, %v", infos, err)
	}
	if infos[0].TurnCount != 1 || infos[0].Preview != "Take a look at this" {
		t.Fatalf("internal reply counted as an external turn: %#v", infos[0])
	}

	imageOnlyID := "image-only"
	if err := sm.SaveHistory(imageOnlyID, []llm.Message{{Role: "user", Parts: []llm.ContentPart{{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,AA=="}}}}}); err != nil {
		t.Fatal(err)
	}
	imageInfos, err := sm.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range imageInfos {
		if info.ID == imageOnlyID && info.Preview != "[Image attachment]" {
			t.Fatalf("image-only preview = %q", info.Preview)
		}
	}
}

func TestSessionListReadsCommittedSnapshotWithoutSessionTransactionLock(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	id := "blocked-agent"
	if err := sm.CreateSession(id); err != nil {
		t.Fatal(err)
	}
	if err := sm.SetDisplayName(id, "A waiting conversation"); err != nil {
		t.Fatal(err)
	}
	committed := []llm.Message{
		{Role: "user", Content: "Committed preview"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "ask", Function: llm.FunctionCall{Name: "ask_user", Arguments: `{}`}}}},
		{Role: "tool", ToolCallID: "ask", Content: "waiting for screenshot"},
		{Role: "user", Content: "Screenshot caption", InputReply: true, Parts: []llm.ContentPart{
			{Type: "text", Text: "Screenshot caption"},
			{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,AA=="}},
		}},
	}
	if err := sm.SaveHistory(id, committed); err != nil {
		t.Fatal(err)
	}

	_, release := sm.BeginSessionTransaction(id)
	done := make(chan struct {
		infos []SessionInfo
		err   error
	}, 1)
	go func() {
		infos, err := sm.List()
		done <- struct {
			infos []SessionInfo
			err   error
		}{infos: infos, err: err}
	}()

	var result struct {
		infos []SessionInfo
		err   error
	}
	select {
	case result = <-done:
		release()
	case <-time.After(300 * time.Millisecond):
		release()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("List waited for a session transaction held across ask_user")
	}
	if result.err != nil || len(result.infos) != 1 {
		t.Fatalf("List = %#v, %v", result.infos, result.err)
	}
	info := result.infos[0]
	if info.DisplayName != "A waiting conversation" || info.TurnCount != 1 || info.Preview != "Committed preview" {
		t.Fatalf("List did not report the committed snapshot or counted InputReply: %#v", info)
	}
}

func TestSessionRegistryThreadBindingIsStableButRoomCanSwitch(t *testing.T) {
	registry, err := OpenSessionRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	threadTarget := conversation.Target{ChannelType: "matrix", ChannelID: "!room:example.org", ThreadID: "$root"}
	if err := registry.BindTarget(threadTarget, "opaque-thread-id", "thread"); err != nil {
		t.Fatal(err)
	}
	got, ok := registry.ResolveTarget(threadTarget)
	if !ok || got.SessionID != "opaque-thread-id" || got.Target() != threadTarget {
		t.Fatalf("resolved target binding = %#v, %v", got, ok)
	}
	if err := registry.BindTarget(threadTarget, "other-thread-id", "thread"); err == nil || !strings.Contains(err.Error(), "thread") {
		t.Fatalf("thread rebinding error = %v", err)
	}
	roomTarget := threadTarget.Room()
	if err := registry.BindTarget(roomTarget, "room-session-one", "room"); err != nil {
		t.Fatal(err)
	}
	if err := registry.BindTarget(roomTarget, "room-session-two", "room"); err != nil {
		t.Fatalf("room session switch was rejected: %v", err)
	}
	if got, ok := registry.ResolveTarget(roomTarget); !ok || got.SessionID != "room-session-two" {
		t.Fatalf("room binding after switch = %#v, %v", got, ok)
	}
}

func TestSessionBindingKeyKeepsLegacyHashFormat(t *testing.T) {
	values := []string{"matrix", "!room:example.org", "$thread:event", "@owner:example.org"}
	got := BindingKey(values[0], values[1], values[2], values[3])
	want := conversation.BindingKey(values[0], values[1], values[2], values[3])
	if got != want {
		t.Fatalf("BindingKey = %q, want legacy compatible %q", got, want)
	}
	if got := filepath.Base(NewSessionManager(t.TempDir()).MetadataPath("session")); got != "metadata.json" {
		t.Fatalf("metadata basename = %q", got)
	}
}
