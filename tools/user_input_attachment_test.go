package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"ageage/input"
	"ageage/llm"
)

func TestTypedUserInputScopeAndLegacyStringCompatibility(t *testing.T) {
	manager := NewUserInputManager()
	owner := InteractionScope{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread", SessionID: "session", SenderID: "alice"}
	other := owner
	other.SenderID = "bob"
	_, typed := manager.RequestUserInputScoped(owner)
	answer := input.UserInput{Text: "caption", Parts: []llm.ContentPart{{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,secret"}}}}
	if manager.RespondInputForScope(other, answer) {
		t.Fatal("cross-sender typed response accepted")
	}
	if !manager.RespondInputForScope(owner, answer) {
		t.Fatal("owner typed response rejected")
	}
	got := <-typed
	if got.Text != answer.Text || len(got.Parts) != 1 || got.Parts[0].ImageURL.URL != answer.Parts[0].ImageURL.URL {
		t.Fatalf("typed answer = %#v", got)
	}

	legacy := manager.RequestInput("legacy-room")
	if manager.RespondInputForScope(LegacyInteractionScope("legacy-room"), answer) {
		t.Fatal("legacy request accepted typed attachments")
	}
	if !manager.Respond("legacy-room", "plain answer") || <-legacy != "plain answer" {
		t.Fatal("legacy string API failed")
	}
}

func TestTypedUserInputCancelAndReplaceCloseOnlyActiveChannels(t *testing.T) {
	manager := NewUserInputManager()
	scope := InteractionScope{ChannelType: "telegram", ChannelID: "room", SessionID: "one", SenderID: "alice"}
	_, first := manager.RequestUserInputScoped(scope)
	_, second := manager.RequestUserInputScoped(scope)
	if _, ok := <-first; ok {
		t.Fatal("replaced typed channel remained open")
	}
	if !manager.CancelForScope(scope) {
		t.Fatal("typed request cancel failed")
	}
	if _, ok := <-second; ok {
		t.Fatal("cancelled typed channel remained open")
	}
	if manager.RespondInputForScope(scope, input.UserInput{Text: "late"}) {
		t.Fatal("cancelled request accepted a response")
	}
}

func TestTypedUserInputByIDRejectsExpiredAndWrongOwner(t *testing.T) {
	manager := NewUserInputManager()
	scope := InteractionScope{ChannelType: "telegram", ChannelID: "room", ThreadID: "topic", SessionID: "session", SenderID: "alice"}
	oldID, oldCh := manager.RequestUserInputScoped(scope)
	currentID, currentCh := manager.RequestUserInputScoped(scope)
	if _, ok := <-oldCh; ok {
		t.Fatal("replaced request remained open")
	}
	answer := input.UserInput{Text: "caption", Parts: []llm.ContentPart{{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,secret"}}}}
	if manager.RespondInputByID(oldID, scope, answer) {
		t.Fatal("expired request ID accepted an answer")
	}
	wrongOwner := scope
	wrongOwner.SenderID = "bob"
	if manager.RespondInputByID(currentID, wrongOwner, answer) {
		t.Fatal("request accepted answer from wrong owner")
	}
	if !manager.RespondInputByID(currentID, scope, answer) {
		t.Fatal("current request rejected owner answer")
	}
	got := <-currentCh
	if got.Text != answer.Text || len(got.Parts) != 1 || got.Parts[0].ImageURL.URL != answer.Parts[0].ImageURL.URL {
		t.Fatalf("answer = %#v", got)
	}
}

func TestTypedByIDRejectsPartsForLegacyRequest(t *testing.T) {
	manager := NewUserInputManager()
	scope := LegacyInteractionScope("legacy")
	id, result := manager.RequestInputScoped(scope)
	if manager.RespondInputByID(id, scope, input.UserInput{Text: "caption", Parts: []llm.ContentPart{{Type: "image_url"}}}) {
		t.Fatal("legacy request accepted typed parts")
	}
	if !manager.RespondInputByID(id, scope, input.UserInput{Text: "plain"}) || <-result != "plain" {
		t.Fatal("legacy request rejected plain typed text")
	}
}

func TestAskUserTypedAttachmentsReachContextHandler(t *testing.T) {
	manager := NewUserInputManager()
	scope := InteractionScope{ChannelType: "discord", ChannelID: "channel", ThreadID: "thread", SessionID: "session", SenderID: "alice"}
	var notified bool
	notifiedCh := make(chan struct{}, 1)
	notify := func(string, []string) { notified = true; notifiedCh <- struct{}{} }
	tool := &AskUserTool{Scope: scope, Manager: manager, NotifyFuncPtr: &notify}
	var received input.UserInput
	ctx := WithUserInputHandler(context.Background(), func(value input.UserInput) { received = value })
	result := make(chan struct {
		text string
		err  error
	}, 1)
	go func() {
		text, err := tool.Execute(ctx, json.RawMessage(`{"question":"send a picture"}`))
		result <- struct {
			text string
			err  error
		}{text, err}
	}()

	var pending *PendingUserInput
	deadline := time.After(time.Second)
	for pending == nil {
		select {
		case <-deadline:
			t.Fatal("ask_user did not register a typed request")
		default:
			requests := manager.GetPendingForScope(scope)
			if len(requests) > 0 {
				pending = requests[0]
			}
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case <-notifiedCh:
	case <-time.After(time.Second):
		t.Fatal("question was not notified")
	}
	if !notified {
		t.Fatal("question was not notified")
	}
	answer := input.UserInput{Text: "Here", Parts: []llm.ContentPart{{Type: "text", Text: "Here"}, {Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,very-secret"}}}}
	if !manager.RespondInputForScope(scope, answer) {
		t.Fatal("typed answer was rejected")
	}
	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.text != "Here" || received.Text != answer.Text || len(received.Parts) != 2 || received.Parts[1].ImageURL.URL != answer.Parts[1].ImageURL.URL {
		t.Fatalf("result=%q received=%#v", got.text, received)
	}
	if strings.Contains(got.text, "very-secret") {
		t.Fatal("tool summary exposed image payload")
	}
}

func TestAskUserTypedAttachmentsRequireHandler(t *testing.T) {
	manager := NewUserInputManager()
	scope := InteractionScope{ChannelType: "matrix", ChannelID: "room", SenderID: "alice"}
	tool := &AskUserTool{Scope: scope, Manager: manager}
	result := make(chan error, 1)
	go func() {
		_, err := tool.Execute(context.Background(), json.RawMessage(`{"question":"attach"}`))
		result <- err
	}()
	deadline := time.After(time.Second)
	for !manager.HasPendingScope(scope) {
		select {
		case <-deadline:
			t.Fatal("request was not registered")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if !manager.RespondInputForScope(scope, input.UserInput{Parts: []llm.ContentPart{{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,x"}}}}) {
		t.Fatal("typed answer was rejected")
	}
	if err := <-result; err == nil || !strings.Contains(err.Error(), "without an input handler") {
		t.Fatalf("error = %v", err)
	}
}

func TestAskUserLegacyStringAnswerDoesNotInvokeTypedHandler(t *testing.T) {
	manager := NewUserInputManager()
	scope := InteractionScope{ChannelType: "matrix", ChannelID: "room", SenderID: "alice"}
	tool := &AskUserTool{Scope: scope, Manager: manager}
	called := false
	ctx := WithUserInputHandler(context.Background(), func(input.UserInput) { called = true })
	result := make(chan struct {
		text string
		err  error
	}, 1)
	go func() {
		text, err := tool.Execute(ctx, json.RawMessage(`{"question":"continue?"}`))
		result <- struct {
			text string
			err  error
		}{text, err}
	}()
	deadline := time.After(time.Second)
	for !manager.HasPendingScope(scope) {
		select {
		case <-deadline:
			t.Fatal("request was not registered")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if !manager.RespondForScope(scope, "yes") {
		t.Fatal("legacy string response was rejected")
	}
	got := <-result
	if got.err != nil || got.text != "yes" {
		t.Fatalf("result=(%q, %v)", got.text, got.err)
	}
	if called {
		t.Fatal("legacy text answer invoked typed handler")
	}
}
