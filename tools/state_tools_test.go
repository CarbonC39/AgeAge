package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTodoStoreUpdateCompletionAndEditFallback(t *testing.T) {
	var sent []string
	store := &TodoStore{
		SendFunc: func(text string) string {
			sent = append(sent, text)
			return "message-id"
		},
		EditFunc: func(string, string) error { return errors.New("edit failed") },
	}
	store.Update([]TodoItem{{Task: "one", Status: "pending"}})
	if store.IsComplete() || !strings.Contains(store.PendingList(), "one") || len(sent) != 1 {
		t.Fatalf("initial todo state sent=%#v pending=%q", sent, store.PendingList())
	}
	store.Update([]TodoItem{{Task: "one", Status: "done"}, {Task: "two", Status: "skipped"}})
	if !store.IsComplete() || len(sent) != 2 {
		t.Fatalf("completed todo state sent=%d complete=%v", len(sent), store.IsComplete())
	}
	formatted := store.Format()
	if !strings.Contains(formatted, "[x] one") || !strings.Contains(formatted, "[-] two") {
		t.Fatalf("formatted todos:\n%s", formatted)
	}
	store.Clear()
	if !store.IsEmpty() || !store.IsComplete() || store.Format() != "" {
		t.Fatal("Clear did not reset todo store")
	}
}

func TestUpdateTodosToolAndFinishGuard(t *testing.T) {
	store := &TodoStore{}
	update := &UpdateTodosTool{Store: store}
	finish := &FinishTool{CheckTodos: func() (bool, string) {
		return store.IsComplete(), store.PendingList()
	}}

	if _, err := update.Execute(context.Background(), json.RawMessage(`{"todos":[{"task":"work","status":"pending"}]}`)); err != nil {
		t.Fatal(err)
	}
	got, err := finish.Execute(context.Background(), json.RawMessage(`{"status":"success","summary":"done"}`))
	if err != nil || finish.Finished || !strings.Contains(got, "pending todos") {
		t.Fatalf("guarded finish = (%q, %v), state=%#v", got, err, finish)
	}
	if _, err := update.Execute(context.Background(), json.RawMessage(`{"todos":[{"task":"work","status":"done"}]}`)); err != nil {
		t.Fatal(err)
	}
	got, err = finish.Execute(context.Background(), json.RawMessage(`{"status":"success","summary":"done"}`))
	if err != nil || !finish.Finished || got != "done" {
		t.Fatalf("completed finish = (%q, %v), state=%#v", got, err, finish)
	}
	finish.Reset()
	if finish.Finished || finish.Summary != "" || finish.Status != "" {
		t.Fatalf("Reset failed: %#v", finish)
	}
}

func TestUserInputManagerRespondReplaceAndCancel(t *testing.T) {
	manager := NewUserInputManager()
	first := manager.RequestInput("room")
	second := manager.RequestInput("room")
	if _, ok := <-first; ok {
		t.Fatal("replaced request was not closed")
	}
	if !manager.HasPending("room") || !manager.Respond("room", "answer") {
		t.Fatal("pending request was not answered")
	}
	if got := <-second; got != "answer" || manager.HasPending("room") {
		t.Fatalf("answer=%q pending=%v", got, manager.HasPending("room"))
	}
	third := manager.RequestInput("room")
	if !manager.Cancel("room") {
		t.Fatal("cancel returned false")
	}
	if _, ok := <-third; ok {
		t.Fatal("cancelled request was not closed")
	}
	if manager.Respond("missing", "x") || manager.Cancel("missing") {
		t.Fatal("missing request unexpectedly handled")
	}
}

func TestConfirmationManagerRespondAndTimeout(t *testing.T) {
	manager := NewConfirmationManager()
	id, result := manager.RequestConfirmation("write file", "room", time.Second)
	if len(manager.GetAllPending("room")) != 1 {
		t.Fatal("confirmation not listed")
	}
	if !manager.RespondToConfirmation(id, true) || manager.RespondToConfirmation(id, false) {
		t.Fatal("confirmation response state is wrong")
	}
	if allowed, ok := <-result; !ok || !allowed {
		t.Fatalf("confirmation result = %v, %v", allowed, ok)
	}
	if _, ok := <-result; ok {
		t.Fatal("confirmation channel was not closed")
	}

	_, timed := manager.RequestConfirmation("timeout", "room", 5*time.Millisecond)
	select {
	case _, ok := <-timed:
		if ok {
			t.Fatal("timed-out channel delivered a value")
		}
	case <-time.After(time.Second):
		t.Fatal("confirmation did not time out")
	}
}
