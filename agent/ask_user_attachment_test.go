package agent

import (
	"context"
	"strings"
	"testing"

	"ageage/input"
	"ageage/llm"
	"ageage/tools"
)

func TestAskUserTypedAttachmentAppendsAfterCompleteToolBatch(t *testing.T) {
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "ask-1", Type: "function", Function: llm.FunctionCall{Name: "ask_user", Arguments: `{"question":"Please send a screenshot"}`}},
			{ID: "capture-1", Type: "function", Function: llm.FunctionCall{Name: "capture", Arguments: `{}`}},
		}}},
		{message: finishMessage("screenshot received")},
	}}
	capture := &loopCaptureTool{result: "capture complete"}
	ag := newLoopTestAgent(t, client, capture)
	ag.cfg.Agent.MaxParallelTools = 2

	manager := tools.NewUserInputManager()
	scope := tools.InteractionScope{ChannelType: "test", ChannelID: "room", SessionID: "session", SenderID: "user"}
	var notify func(string, []string)
	notify = func(question string, _ []string) {
		if question != "Please send a screenshot" {
			t.Errorf("unexpected ask_user question %q", question)
		}
		answer := input.UserInput{Text: "Screenshot caption", Warnings: []string{"converter warning; do not add to the tool result"}, Parts: []llm.ContentPart{
			{Type: "text", Text: "Screenshot caption"},
			{Type: "text", Text: "private extracted file text"},
			{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,ZmFrZQ==", Detail: "high"}},
			{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "https://private.example/second"}},
		}}
		if !manager.RespondInputForScope(scope, answer) {
			t.Error("typed ask_user response was not accepted")
		}
	}
	ag.registry.Register(&tools.AskUserTool{Manager: manager, Scope: scope, NotifyFuncPtr: &notify})

	result, err := ag.Run(context.Background(), "Inspect this screenshot", nil)
	if err != nil || result != "screenshot received" {
		t.Fatalf("Run = (%q, %v)", result, err)
	}
	if len(client.calls) != 2 {
		t.Fatalf("LLM calls = %d, want one tool round and one follow-up in the same Run", len(client.calls))
	}
	if capture.calls != 1 {
		t.Fatalf("same-batch capture calls = %d, want 1", capture.calls)
	}

	messages := client.calls[1].messages
	toolIndices := map[string]int{}
	for i, message := range messages {
		if message.Role == "tool" {
			toolIndices[message.ToolCallID] = i
		}
	}
	askIndex, askOK := toolIndices["ask-1"]
	captureIndex, captureOK := toolIndices["capture-1"]
	if !askOK || !captureOK {
		t.Fatalf("follow-up request lacks both tool responses: %#v", messages)
	}
	if captureIndex != askIndex+1 {
		t.Fatalf("tool responses were not kept adjacent: ask=%d capture=%d messages=%#v", askIndex, captureIndex, messages)
	}
	if askIndex == 0 || messages[askIndex-1].Role != "assistant" || len(messages[askIndex-1].ToolCalls) != 2 {
		t.Fatalf("tool responses do not immediately follow their assistant batch: %#v", messages)
	}
	if captureIndex+1 >= len(messages) || messages[captureIndex+1].Role != "user" {
		t.Fatalf("typed answer was not appended after the complete tool batch: %#v", messages)
	}
	answer := messages[captureIndex+1]
	if !strings.Contains(answer.TextContent(), "Screenshot caption") {
		t.Fatalf("typed answer text/parts were not preserved: %#v", answer)
	}
	foundImage := false
	imageCount := 0
	for _, part := range answer.Parts {
		if part.Type == "image_url" && part.ImageURL != nil {
			imageCount++
			if part.ImageURL.URL == "data:image/png;base64,ZmFrZQ==" && part.ImageURL.Detail == "high" {
				foundImage = true
			}
		}
	}
	if !foundImage || imageCount != 2 {
		t.Fatalf("image part was not fully preserved: %#v", answer.Parts)
	}
	const expectedToolResult = "[User provided additional input; see the following user message.]"
	if messages[askIndex].Content != expectedToolResult {
		t.Fatalf("ask_user tool response = %q", messages[askIndex].Content)
	}
	if strings.Contains(messages[askIndex].Content, "Screenshot caption") || strings.Contains(messages[askIndex].Content, "private extracted file text") || strings.Contains(messages[askIndex].Content, "converter warning") || strings.Contains(messages[askIndex].Content, "data:image/png") {
		t.Fatal("tool response duplicated typed answer data")
	}
}
