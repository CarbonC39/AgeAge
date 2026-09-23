package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestSessionHistoryConcurrentSavesArePrivateAndValid(t *testing.T) {
	ageageDir := t.TempDir()
	smA := NewSessionManager(ageageDir)
	smB := NewSessionManager(ageageDir)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sm := smA
			if i%2 == 1 {
				sm = smB
			}
			if err := sm.SaveHistory("shared", []llm.Message{
				{Role: "user", Content: "writer"},
				{Role: "assistant", Content: strings.Repeat("x", i+1)},
			}); err != nil {
				t.Errorf("save %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	loaded, err := smA.LoadHistory("shared")
	if err != nil || len(loaded) != 2 || loaded[0].Role != "user" || loaded[1].Role != "assistant" {
		t.Fatalf("loaded concurrent history = %#v, err=%v", loaded, err)
	}
	info, err := os.Stat(smA.HistoryPath("shared"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("history mode = %o, want 600", got)
	}
}

func TestSessionTransactionCanLoadAndSaveUnderOneLock(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	if err := sm.SaveHistory("transaction", []llm.Message{{Role: "user", Content: "before"}}); err != nil {
		t.Fatal(err)
	}
	if err := sm.WithSessionTransaction("transaction", func(tx *SessionTransaction) error {
		messages, err := tx.LoadHistory()
		if err != nil {
			return err
		}
		messages = append(messages, llm.Message{Role: "assistant", Content: "after"})
		return tx.SaveHistory(messages)
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := sm.LoadHistory("transaction")
	if err != nil || len(loaded) != 2 || loaded[1].Content != "after" {
		t.Fatalf("transaction history = %#v, err=%v", loaded, err)
	}
}

func TestSessionRegistryPersistsBindingsAndIsMigrationSafe(t *testing.T) {
	ageageDir := t.TempDir()
	registry, err := OpenSessionRegistry(ageageDir)
	if err != nil {
		t.Fatal(err)
	}
	binding := SessionBinding{
		SessionID:   "matrix-room-thread",
		ChannelType: "matrix",
		ChannelID:   "!room:example.org",
		ThreadID:    "$event",
		OwnerID:     "@alice:example.org",
		Kind:        "thread",
	}
	if err := registry.Bind(binding); err != nil {
		t.Fatal(err)
	}
	key := KeyForBinding(binding)
	if got, ok := registry.Get(key); !ok || got.SessionID != binding.SessionID {
		t.Fatalf("binding = %#v, found=%v", got, ok)
	}
	reloaded, err := OpenSessionRegistry(ageageDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := reloaded.Get(key); !ok || got.ChannelID != binding.ChannelID {
		t.Fatalf("reloaded binding = %#v, found=%v", got, ok)
	}
	if err := reloaded.RemoveSession(binding.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Get(key); ok {
		t.Fatal("removed session binding still present")
	}
	info, err := os.Stat(reloaded.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("registry mode = %o, want 600", got)
	}
}

func TestSessionRegistryMissingFileStartsEmpty(t *testing.T) {
	registry, err := OpenSessionRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.List()) != 0 {
		t.Fatalf("new registry = %#v", registry.List())
	}
}

func TestSessionRegistryInstancesMergeBindingsUnderFileLock(t *testing.T) {
	ageageDir := t.TempDir()
	first, err := OpenSessionRegistry(ageageDir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := OpenSessionRegistry(ageageDir)
	if err != nil {
		t.Fatal(err)
	}
	a := SessionBinding{SessionID: "room-a", ChannelType: "matrix", ChannelID: "!a:example.org", Kind: "room"}
	b := SessionBinding{SessionID: "room-b", ChannelType: "matrix", ChannelID: "!b:example.org", Kind: "room"}
	if err := first.Bind(a); err != nil {
		t.Fatal(err)
	}
	if err := second.Bind(b); err != nil {
		t.Fatal(err)
	}
	third, err := OpenSessionRegistry(ageageDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.List()) != 2 {
		t.Fatalf("merged bindings = %#v", third.List())
	}
}

func TestCreateSessionRejectsExistingAndListHidesCronSessions(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	if err := sm.CreateSession("named"); err != nil {
		t.Fatal(err)
	}
	if err := sm.CreateSession("named"); err == nil {
		t.Fatal("creating an existing session succeeded")
	}
	if err := sm.EnsureSession("cron-internal"); err != nil {
		t.Fatal(err)
	}
	if err := sm.CreateSession("cron-user"); err == nil {
		t.Fatal("creating a reserved cron session succeeded")
	}
	if err := sm.Rename("named", "cron-renamed"); err == nil {
		t.Fatal("renaming into the reserved cron namespace succeeded")
	}
	infos, err := sm.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != "named" {
		t.Fatalf("user session list = %#v", infos)
	}
	if exact, matches, err := sm.FindByPrefix("cron"); err != nil || exact != nil || len(matches) != 0 {
		t.Fatalf("cron session leaked through prefix lookup: exact=%#v matches=%#v err=%v", exact, matches, err)
	}
}

func TestSessionTransactionsDoNotLoseConcurrentUpdates(t *testing.T) {
	smA := NewSessionManager(t.TempDir())
	smB := NewSessionManager(filepath.Dir(smA.sessionsDir))
	if err := smA.EnsureSession("shared"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sm := smA
			if i%2 == 1 {
				sm = smB
			}
			if err := sm.WithSessionTransaction("shared", func(tx *SessionTransaction) error {
				messages, err := tx.LoadHistory()
				if err != nil {
					return err
				}
				messages = append(messages, llm.Message{Role: "user", Content: fmt.Sprintf("%d", i)})
				return tx.SaveHistory(messages)
			}); err != nil {
				t.Errorf("transaction %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	messages, err := smA.LoadHistory("shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 20 {
		t.Fatalf("history has %d messages, want 20", len(messages))
	}
}

func TestRenameUsesDeterministicDualSessionLocks(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	if err := sm.CreateSession("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := sm.CreateSession("beta"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan struct{}, 2)
	for _, ids := range [][2]string{{"alpha", "gamma"}, {"beta", "alpha"}} {
		go func(oldID, newID string) {
			<-start
			_ = sm.Rename(oldID, newID)
			done <- struct{}{}
		}(ids[0], ids[1])
	}
	close(start)
	for range 2 {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent renames deadlocked")
		}
	}
}

func TestSessionRegistryRejectsDuplicateMutableAttachment(t *testing.T) {
	registry, err := OpenSessionRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := SessionBinding{SessionID: "shared", ChannelType: "matrix", ChannelID: "!one", Kind: "room"}
	second := SessionBinding{SessionID: "shared", ChannelType: "matrix", ChannelID: "!two", Kind: "room"}
	if err := registry.Bind(first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Bind(second); err == nil {
		t.Fatal("duplicate session attachment succeeded")
	}
	if err := registry.RenameSession("shared", "renamed"); err != nil {
		t.Fatal(err)
	}
	bindings := registry.GetForSession("renamed")
	if len(bindings) != 1 || bindings[0].ChannelID != "!one" {
		t.Fatalf("renamed bindings = %#v", bindings)
	}
}

func TestCreateSessionConcurrentCollisionHasOneWinner(t *testing.T) {
	smA := NewSessionManager(t.TempDir())
	smB := NewSessionManager(filepath.Dir(smA.sessionsDir))
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, sm := range []*SessionManager{smA, smB} {
		go func(manager *SessionManager) {
			<-start
			results <- manager.CreateSession("same-name")
		}(sm)
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("CreateSession successes = %d, want exactly one", successes)
	}
}

func TestRenameWaitsForActiveSessionTransaction(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	if err := sm.CreateSession("source"); err != nil {
		t.Fatal(err)
	}
	_, unlock := sm.BeginSessionTransaction("source")
	done := make(chan error, 1)
	go func() { done <- sm.Rename("source", "target") }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("Rename completed while transaction was active: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Rename did not continue after transaction released")
	}
}

func TestDeleteWaitsForActiveSessionTransaction(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	if err := sm.CreateSession("source"); err != nil {
		t.Fatal(err)
	}
	_, unlock := sm.BeginSessionTransaction("source")
	done := make(chan error, 1)
	go func() { done <- sm.Delete("source") }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("Delete completed while transaction was active: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Delete did not continue after transaction released")
	}
}

func TestBeginSessionTransactionContextCancelsWhileQueued(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	if err := sm.CreateSession("source"); err != nil {
		t.Fatal(err)
	}
	_, unlock := sm.BeginSessionTransaction("source")
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, queuedUnlock, err := sm.BeginSessionTransactionContext(ctx, "source"); err == nil {
		queuedUnlock()
		t.Fatal("queued transaction ignored cancellation")
	} else if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func TestBeginSessionTransactionContextRejectsAlreadyCanceledContext(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, unlock, err := sm.BeginSessionTransactionContext(ctx, "source"); err == nil {
		unlock()
		t.Fatal("transaction accepted an already canceled context")
	}
}
