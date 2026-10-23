package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestAskUserUsesLiveScopeAndPreservesSessionOnlyCLIScope(t *testing.T) {
	manager := NewUserInputManager()
	scope := InteractionScope{SessionID: "cli-session"}
	notify := func(string, []string) {
		if !manager.RespondForScope(scope, "CLI answer") {
			t.Error("session-only CLI scope could not answer ask_user")
		}
	}
	tool := &AskUserTool{
		Manager:       manager,
		ScopeFunc:     func() InteractionScope { return scope },
		NotifyFuncPtr: &notify,
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"question":"Clarify this"}`))
	if err != nil || result != "CLI answer" {
		t.Fatalf("Execute() = (%q, %v), want CLI answer", result, err)
	}
}

func TestAskUserScopeGetterReadsCurrentAgentScopeAtExecution(t *testing.T) {
	manager := NewUserInputManager()
	scope := InteractionScope{SessionID: "before-run"}
	tool := &AskUserTool{Manager: manager, ScopeFunc: func() InteractionScope { return scope }}
	scope = InteractionScope{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread", SessionID: "active-session", SenderID: "user"}
	finished := make(chan struct{})
	var result string
	var executeErr error
	go func() {
		result, executeErr = tool.Execute(context.Background(), json.RawMessage(`{"question":"Attach an image"}`))
		close(finished)
	}()
	deadline := time.Now().Add(time.Second)
	for !manager.HasPendingScope(scope) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !manager.RespondForScope(scope, "answer from active session") {
		t.Fatal("response for live scope was not accepted")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("ask_user did not finish after matching the live scope")
	}
	if executeErr != nil || result != "answer from active session" {
		t.Fatalf("Execute() = (%q, %v), want answer from active session", result, executeErr)
	}
}
