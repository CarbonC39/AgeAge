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

func TestScopedInteractionsCannotCrossUsersOrThreads(t *testing.T) {
	manager := NewConfirmationManager()
	owner := InteractionScope{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread-a", SessionID: "session-a", SenderID: "alice"}
	otherUser := owner
	otherUser.SenderID = "bob"
	otherThread := owner
	otherThread.ThreadID = "thread-b"
	_, result := manager.RequestConfirmationScoped("write", owner, time.Second)
	if manager.RespondForScope(otherUser, true) || manager.RespondForScope(otherThread, true) {
		t.Fatal("cross-user or cross-thread confirmation was accepted")
	}
	if !manager.RespondForScope(owner, true) {
		t.Fatal("owner confirmation was rejected")
	}
	if allowed, ok := <-result; !ok || !allowed {
		t.Fatalf("owner result = %v, %v", allowed, ok)
	}

	input := NewUserInputManager()
	_, answer := input.RequestInputScoped(owner)
	if input.RespondForScope(otherUser, "bad") || input.RespondForScope(otherThread, "bad") {
		t.Fatal("cross-scope user input was accepted")
	}
	if !input.RespondForScope(owner, "good") {
		t.Fatal("owner user input was rejected")
	}
	if got := <-answer; got != "good" {
		t.Fatalf("answer = %q", got)
	}
}

func TestScopedInteractionRejectsAmbiguousResponses(t *testing.T) {
	manager := NewConfirmationManager()
	scope := InteractionScope{ChannelType: "telegram", ChannelID: "room", ThreadID: "thread", SessionID: "session", SenderID: "alice"}
	_, first := manager.RequestConfirmationScoped("one", scope, time.Second)
	_, second := manager.RequestConfirmationScoped("two", scope, time.Second)
	if manager.RespondForScope(scope, true) {
		t.Fatal("ambiguous confirmation was accepted")
	}
	if len(manager.GetPendingForScope(scope)) != 2 {
		t.Fatal("ambiguous requests were not retained")
	}
	pending := manager.GetPendingForScope(scope)
	if manager.RespondToConfirmationScoped("missing", scope, false) {
		t.Fatal("missing confirmation ID unexpectedly accepted")
	}
	for _, request := range pending {
		if !manager.RespondToConfirmationScoped(request.ID, scope, false) {
			t.Fatal("explicit scoped confirmation was rejected")
		}
	}
	for _, ch := range []chan bool{first, second} {
		if allowed, ok := <-ch; !ok || allowed {
			t.Fatalf("explicit response = %v, %v", allowed, ok)
		}
	}
}

func TestConfirmationChannelAllowlistCanApprove(t *testing.T) {
	manager := NewConfirmationManager()
	owner := InteractionScope{ChannelType: "discord", ChannelID: "room", ThreadID: "thread", SessionID: "session", SenderID: "alice"}
	moderator := owner
	moderator.SenderID = "moderator"
	manager.SetAllowedRespondersForScope(owner, moderator.SenderID)
	_, result := manager.RequestConfirmationScoped("write", owner, time.Second)
	if !manager.RespondForScope(moderator, true) {
		t.Fatal("configured channel approver was rejected")
	}
	if allowed, ok := <-result; !ok || !allowed {
		t.Fatalf("allowlisted result = %v, %v", allowed, ok)
	}
}

func TestConfirmationAllowlistsAreIsolatedByChannelType(t *testing.T) {
	manager := NewConfirmationManager()
	telegram := InteractionScope{ChannelType: "telegram", ChannelID: "same-id", SessionID: "tg-session", SenderID: "alice"}
	matrix := InteractionScope{ChannelType: "matrix", ChannelID: "same-id", SessionID: "mx-session", SenderID: "bob"}
	moderator := matrix
	moderator.SenderID = "moderator"
	manager.SetAllowedRespondersForScope(telegram, moderator.SenderID)
	_, result := manager.RequestConfirmationScoped("write", matrix, time.Second)
	if manager.RespondForScope(moderator, true) {
		t.Fatal("approver from another channel type was accepted")
	}
	if !manager.RespondForScope(matrix, false) {
		t.Fatal("owner response was rejected")
	}
	if allowed, ok := <-result; !ok || allowed {
		t.Fatalf("owner result = %v, %v", allowed, ok)
	}
}
