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
	answer := input.UserInput{Text: "Here", Warnings: []string{"converter warning; keep out of tool result"}, Parts: []llm.ContentPart{
		{Type: "text", Text: "Here"},
		{Type: "text", Text: "private extracted file text"},
		{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,very-secret"}},
		{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "https://private.example/image"}},
	}}
	if !manager.RespondInputForScope(scope, answer) {
		t.Fatal("typed answer was rejected")
	}
	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	const expectedToolResult = "[User provided additional input; see the following user message.]"
	if got.text != expectedToolResult || received.Text != answer.Text || len(received.Parts) != 4 || received.Warnings[0] != answer.Warnings[0] || received.Parts[0].Text != answer.Parts[0].Text || received.Parts[1].Text != answer.Parts[1].Text || received.Parts[2].ImageURL.URL != answer.Parts[2].ImageURL.URL || received.Parts[3].ImageURL.URL != answer.Parts[3].ImageURL.URL {
		t.Fatalf("result=%q received=%#v", got.text, received)
	}
	if strings.Contains(got.text, "private extracted file text") || strings.Contains(got.text, answer.Warnings[0]) || strings.Contains(got.text, "very-secret") {
		t.Fatalf("tool result leaked additional input data: %q", got.text)
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

func TestAskUserPartsOnlyAnswerUsesFixedToolResult(t *testing.T) {
	manager := NewUserInputManager()
	scope := InteractionScope{ChannelType: "matrix", ChannelID: "room", SenderID: "alice"}
	tool := &AskUserTool{Scope: scope, Manager: manager, NotifyFuncPtr: new(func(string, []string))}
	answer := input.UserInput{Warnings: []string{"download warning"}, Parts: []llm.ContentPart{
		{Type: "text", Text: "first extracted file text"},
		{Type: "text", Text: "second extracted file text"},
		{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,one"}},
		{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "https://private.example/two"}},
	}}
	var received input.UserInput
	ctx := WithUserInputHandler(context.Background(), func(value input.UserInput) { received = value })
	type result struct {
		text string
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		text, err := tool.Execute(ctx, json.RawMessage(`{"question":"send files"}`))
		resultCh <- result{text, err}
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
	if !manager.RespondInputForScope(scope, answer) {
		t.Fatal("parts-only answer was rejected")
	}
	got := <-resultCh
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.text != "[User provided additional input; see the following user message.]" {
		t.Fatalf("tool result = %q", got.text)
	}
	if len(received.Parts) != len(answer.Parts) || received.Parts[0].Text != answer.Parts[0].Text || received.Parts[1].Text != answer.Parts[1].Text || received.Parts[2].ImageURL.URL != answer.Parts[2].ImageURL.URL || received.Parts[3].ImageURL.URL != answer.Parts[3].ImageURL.URL || received.Warnings[0] != answer.Warnings[0] {
		t.Fatalf("typed callback lost parts or warnings: %#v", received)
	}
	if strings.Contains(got.text, "extracted file text") || strings.Contains(got.text, "download warning") || strings.Contains(got.text, "private.example") || strings.Contains(got.text, "base64") {
		t.Fatalf("tool result leaked answer data: %q", got.text)
	}
}
