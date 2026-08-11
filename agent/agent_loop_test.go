package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"ageage/config"
	"ageage/llm"
	"ageage/skills"
	"ageage/tools"
)

type fakeChatStep struct {
	message llm.Message
	usage   *llm.Usage
	err     error
	chunks  []string
}

type fakeChatCall struct {
	messages []llm.Message
	tools    []llm.ToolDef
	stream   bool
}

type fakeChatClient struct {
	steps   []fakeChatStep
	calls   []fakeChatCall
	apiKey  string
	baseURL string
}

func (f *fakeChatClient) next(messages []llm.Message, defs []llm.ToolDef, stream bool) (fakeChatStep, error) {
	f.calls = append(f.calls, fakeChatCall{
		messages: append([]llm.Message(nil), messages...),
		tools:    append([]llm.ToolDef(nil), defs...),
		stream:   stream,
	})
	if len(f.steps) == 0 {
		return fakeChatStep{}, errors.New("fake chat client has no remaining steps")
	}
	step := f.steps[0]
	f.steps = f.steps[1:]
	return step, step.err
}

func (f *fakeChatClient) ChatCompletion(
	_ context.Context,
	messages []llm.Message,
	defs []llm.ToolDef,
	_ float64,
) (*llm.ChatResponse, error) {
	step, err := f.next(messages, defs, false)
	if err != nil {
		return nil, err
	}
	return &llm.ChatResponse{
		Choices: []llm.Choice{{Index: 0, Message: step.message}},
		Usage:   step.usage,
	}, nil
}

func (f *fakeChatClient) ChatCompletionStream(
	_ context.Context,
	messages []llm.Message,
	defs []llm.ToolDef,
	_ float64,
	callback llm.StreamCallback,
	toolCallCallback llm.ToolCallStreamCb,
) (*llm.Message, *llm.Usage, error) {
	step, err := f.next(messages, defs, true)
	if err != nil {
		return nil, step.usage, err
	}
	chunks := step.chunks
	if len(chunks) == 0 && step.message.Content != "" {
		chunks = []string{step.message.Content}
	}
	for _, chunk := range chunks {
		if callback != nil {
			callback(chunk)
		}
	}
	if toolCallCallback != nil {
		for i, call := range step.message.ToolCalls {
			toolCallCallback(i, call)
		}
	}
	message := step.message
	return &message, step.usage, nil
}

func (f *fakeChatClient) APIKey() string  { return f.apiKey }
func (f *fakeChatClient) BaseURL() string { return f.baseURL }

type loopCaptureTool struct {
	result string
	err    error
	calls  int
	args   []json.RawMessage
}

func (t *loopCaptureTool) Name() string        { return "capture" }
func (t *loopCaptureTool) Description() string { return "capture test input" }
func (t *loopCaptureTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (t *loopCaptureTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	t.calls++
	t.args = append(t.args, append(json.RawMessage(nil), args...))
	return t.result, t.err
}

func finishMessage(summary string) llm.Message {
	args, _ := json.Marshal(map[string]string{"status": "success", "summary": summary})
	return toolCallMessage("finish_task", string(args), "call-finish")
}

func toolCallMessage(name, args, id string) llm.Message {
	return llm.Message{
		Role: "assistant",
		ToolCalls: []llm.ToolCall{{
			ID:   id,
			Type: "function",
			Function: llm.FunctionCall{
				Name:      name,
				Arguments: args,
			},
		}},
	}
}

func newLoopTestAgent(t *testing.T, client ChatClient, capture *loopCaptureTool) *Agent {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Workspace = t.TempDir()
	cfg.WorkDir = cfg.Workspace
	cfg.Agent.Mode = "full"
	cfg.Agent.MaxParallelTools = 1

	registry := tools.NewRegistry()
	finishTool := &tools.FinishTool{}
	registry.Register(finishTool)
	if capture != nil {
		registry.Register(capture)
	}
	ag := NewAgent(cfg, client, registry, finishTool, nil, false)
	ag.Mode.InjectContext = false
	return ag
}

func messagesContain(messages []llm.Message, role, text string) bool {
	for _, message := range messages {
		if message.Role == role && strings.Contains(message.TextContent(), text) {
			return true
		}
	}
	return false
}

func TestAgentRunCompletesThroughFinishTool(t *testing.T) {
	client := &fakeChatClient{steps: []fakeChatStep{{message: finishMessage("complete answer")}}}
	ag := newLoopTestAgent(t, client, nil)

	result, err := ag.Run(context.Background(), "question", nil)
	if err != nil || result != "complete answer" {
		t.Fatalf("Run = (%q, %v)", result, err)
	}
	if len(client.calls) != 1 || len(client.calls[0].tools) != 1 || client.calls[0].tools[0].Function.Name != "finish_task" {
		t.Fatalf("model calls = %#v", client.calls)
	}
	if messages := ag.Messages(); messages[len(messages)-1].Content != "complete answer" {
		t.Fatalf("conversation = %#v", messages)
	}
}

func TestAgentRunFeedsToolResultIntoNextModelCall(t *testing.T) {
	capture := &loopCaptureTool{result: "captured output"}
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: toolCallMessage("capture", `{"value":"input"}`, "call-capture")},
		{message: finishMessage("used the output")},
	}}
	ag := newLoopTestAgent(t, client, capture)

	result, err := ag.Run(context.Background(), "use a tool", nil)
	if err != nil || result != "used the output" || capture.calls != 1 {
		t.Fatalf("Run = (%q, %v), tool calls=%d", result, err, capture.calls)
	}
	if len(client.calls) != 2 || !messagesContain(client.calls[1].messages, "tool", "captured output") {
		t.Fatalf("second model input = %#v", client.calls)
	}
}

func TestAgentRunAddsRecoveryHintAfterToolError(t *testing.T) {
	capture := &loopCaptureTool{err: errors.New("capture failed")}
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: toolCallMessage("capture", `{}`, "call-capture")},
		{message: finishMessage("recovered")},
	}}
	ag := newLoopTestAgent(t, client, capture)

	result, err := ag.Run(context.Background(), "recover", nil)
	if err != nil || result != "recovered" {
		t.Fatalf("Run = (%q, %v)", result, err)
	}
	secondInput := client.calls[1].messages
	if !messagesContain(secondInput, "tool", "Error: capture failed") ||
		!messagesContain(secondInput, "user", ".ageage/docs/troubleshooting.md") {
		t.Fatalf("recovery input = %#v", secondInput)
	}
}

func TestAgentRunAcceptsSecondBareTextResponse(t *testing.T) {
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: llm.Message{Role: "assistant", Content: "draft"}},
		{message: llm.Message{Role: "assistant", Content: "final text"}},
	}}
	ag := newLoopTestAgent(t, client, nil)

	result, err := ag.Run(context.Background(), "question", nil)
	if err != nil || result != "final text" {
		t.Fatalf("Run = (%q, %v)", result, err)
	}
	if !messagesContain(client.calls[1].messages, "user", "did not call finish_task") {
		t.Fatalf("second model input lacks completion hint: %#v", client.calls[1].messages)
	}
}

func TestAgentRunRecoversFromEmptyAssistantResponse(t *testing.T) {
	// First response is degenerate (no content, no tool calls) — e.g. a
	// thinking model that streamed only reasoning. The loop must not persist
	// it, must nudge, and must succeed on the retry instead of sending an
	// invalid empty assistant message back to the provider.
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: llm.Message{Role: "assistant"}},
		{message: finishMessage("recovered answer")},
	}}
	ag := newLoopTestAgent(t, client, nil)

	result, err := ag.Run(context.Background(), "question", nil)
	if err != nil || result != "recovered answer" {
		t.Fatalf("Run = (%q, %v)", result, err)
	}
	if len(client.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(client.calls))
	}
	for i, call := range client.calls {
		for _, m := range call.messages {
			if m.Role == "assistant" && m.Content == "" && len(m.ToolCalls) == 0 {
				t.Fatalf("call %d sent empty assistant message: %#v", i+1, call.messages)
			}
		}
	}
	if !messagesContain(client.calls[1].messages, "user", "did not call finish_task") {
		t.Fatalf("retry input lacks completion hint: %#v", client.calls[1].messages)
	}
	for _, m := range ag.Messages() {
		if m.Role == "assistant" && m.Content == "" && len(m.ToolCalls) == 0 {
			t.Fatalf("empty assistant message persisted in history: %#v", ag.Messages())
		}
	}
}

func TestAgentRunFallsBackAfterUpgradedClientError(t *testing.T) {
	base := &fakeChatClient{steps: []fakeChatStep{{message: finishMessage("fallback answer")}}}
	upgraded := &fakeChatClient{steps: []fakeChatStep{{err: errors.New("upgrade unavailable")}}}
	ag := newLoopTestAgent(t, base, nil)

	result, err := ag.runLoop(context.Background(), nil, ag.registry.ToOpenAITools(), upgraded, true)
	if err != nil || result != "fallback answer" {
		t.Fatalf("runLoop = (%q, %v)", result, err)
	}
	if len(upgraded.calls) != 1 || len(base.calls) != 1 {
		t.Fatalf("upgraded calls=%d base calls=%d", len(upgraded.calls), len(base.calls))
	}
}

func TestAgentRunStopsAtIterationLimit(t *testing.T) {
	capture := &loopCaptureTool{result: "again"}
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: toolCallMessage("capture", `{}`, "call-1")},
		{message: toolCallMessage("capture", `{}`, "call-2")},
	}}
	ag := newLoopTestAgent(t, client, capture)
	ag.MaxIterations = 2

	result, err := ag.Run(context.Background(), "loop", nil)
	if err == nil || result != "" || !strings.Contains(err.Error(), "maximum iterations (2)") {
		t.Fatalf("Run = (%q, %v)", result, err)
	}
	if capture.calls != 2 || len(client.calls) != 2 {
		t.Fatalf("tool calls=%d model calls=%d", capture.calls, len(client.calls))
	}
}

func TestAgentRunCompressesOlderToolTurns(t *testing.T) {
	capture := &loopCaptureTool{result: "tool output"}
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: toolCallMessage("capture", `{"value":"first"}`, "call-1")},
		{message: toolCallMessage("capture", `{"value":"second"}`, "call-2")},
		{message: finishMessage("done")},
	}}
	ag := newLoopTestAgent(t, client, capture)
	ag.cfg.History.CompressToolTurns = true
	ag.cfg.History.KeepRecentTurns = 1

	if result, err := ag.Run(context.Background(), "compress", nil); err != nil || result != "done" {
		t.Fatalf("Run = (%q, %v)", result, err)
	}

	messages := ag.Messages()
	compressedFirst := false
	rawCaptureCalls := 0
	for _, message := range messages {
		if message.Role == "assistant" && len(message.ToolCalls) == 0 && strings.Contains(message.Content, "capture: first") {
			compressedFirst = true
		}
		for _, call := range message.ToolCalls {
			if call.Function.Name == "capture" {
				rawCaptureCalls++
				if strings.Contains(call.Function.Arguments, "first") {
					t.Fatalf("old raw tool call was retained: %#v", messages)
				}
			}
		}
	}
	if !compressedFirst || rawCaptureCalls != 1 {
		t.Fatalf("history was not compressed: %#v", messages)
	}
}

func TestAgentRunPropagatesBaseClientError(t *testing.T) {
	client := &fakeChatClient{steps: []fakeChatStep{{err: errors.New("provider down")}}}
	ag := newLoopTestAgent(t, client, nil)

	result, err := ag.Run(context.Background(), "question", nil)
	if err == nil || result != "" || !strings.Contains(err.Error(), "LLM call failed at iteration 1: provider down") {
		t.Fatalf("Run = (%q, %v)", result, err)
	}
	if got := fmt.Sprint(ag.Messages()); !strings.Contains(got, "question") {
		t.Fatalf("conversation lost user input: %s", got)
	}
}

func TestAgentRunSegmentedSkillAdvancesSegments(t *testing.T) {
	segSkill := &skills.Skill{
		Name:      "segtest",
		Segmented: true,
		Segments:  []string{"Segment ONE instructions here.", "Segment TWO final instructions."},
	}
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: toolCallMessage("next_step", `{}`, "call-next")},
		{message: finishMessage("final result")},
	}}
	ag := newLoopTestAgent(t, client, nil)
	ag.skills = []skills.Skill{*segSkill}
	// No factory deps in the test harness, so inject next_step manually.
	ag.registry.Register(&NextStepTool{agent: ag})

	result, err := ag.Run(context.Background(), "/segtest", nil)
	if err != nil || result != "final result" {
		t.Fatalf("Run = (%q, %v)", result, err)
	}
	if len(client.calls) != 2 {
		t.Fatalf("model calls = %d, want 2", len(client.calls))
	}
	firstSystem := client.calls[0].messages[0].Content
	secondSystem := client.calls[1].messages[0].Content
	if !strings.Contains(firstSystem, "Segment ONE") || strings.Contains(firstSystem, "Segment TWO") {
		t.Fatalf("first call system prompt should contain only segment 1:\n%s", firstSystem)
	}
	if !strings.Contains(firstSystem, "next_step") {
		t.Fatalf("first call should instruct using next_step:\n%s", firstSystem)
	}
	if !strings.Contains(secondSystem, "Segment TWO") || strings.Contains(secondSystem, "Segment ONE") {
		t.Fatalf("second call system prompt should contain only segment 2:\n%s", secondSystem)
	}
	if !strings.Contains(secondSystem, "finish_task") {
		t.Fatalf("second call should instruct using finish_task:\n%s", secondSystem)
	}
}

func TestFinishToolBlocksSuccessBeforeFinalSegment(t *testing.T) {
	segSkill := &skills.Skill{
		Name:      "segtest",
		Segmented: true,
		Segments:  []string{"Segment one.", "Segment two."},
	}
	client := &fakeChatClient{steps: []fakeChatStep{
		// Agent tries to finish on segment 0 → guarded, loop continues with hint.
		{message: finishMessage("done early")},
		{message: toolCallMessage("next_step", `{}`, "call-next")},
		{message: finishMessage("done properly")},
	}}
	ag := newLoopTestAgent(t, client, nil)
	ag.skills = []skills.Skill{*segSkill}
	ag.registry.Register(&NextStepTool{agent: ag})

	result, err := ag.Run(context.Background(), "/segtest", nil)
	if err != nil || result != "done properly" {
		t.Fatalf("Run = (%q, %v)", result, err)
	}
	// The guarded finish_task must have returned a tool-result hint telling the
	// agent to advance instead.
	secondCall := client.calls[1].messages
	if !messagesContain(secondCall, "tool", "next_step") {
		t.Fatalf("blocked finish did not steer the agent to next_step: %#v", secondCall)
	}
}

func TestBuildSystemPromptPipelineAgentUsesNodeComplete(t *testing.T) {
	ag := newLoopTestAgent(t, &fakeChatClient{}, nil)
	ag.registry.Unregister("finish_task")
	ag.registry.Register(&NodeCompleteTool{resultCh: make(chan NodeResult, 1), finishTool: ag.finishTool})

	prompt := ag.buildSystemPrompt(nil)
	if !strings.Contains(prompt, "MUST call the node_complete tool") {
		t.Fatalf("pipeline prompt missing node_complete rule:\n%s", prompt)
	}
	if strings.Contains(prompt, "MUST call the finish_task tool") {
		t.Fatalf("pipeline prompt still names finish_task as the completion tool:\n%s", prompt)
	}
	if strings.Contains(prompt, "`summary` field IS your message") {
		t.Fatalf("pipeline prompt leaks summary-field guidance that node_complete does not have:\n%s", prompt)
	}
	if strings.Contains(prompt, "Set status=\"success\" only when ALL todos are done") {
		t.Fatalf("pipeline prompt includes main-agent todo rule:\n%s", prompt)
	}
}

func TestBuildSystemPromptMainAgentKeepsSummaryGuidance(t *testing.T) {
	ag := newLoopTestAgent(t, &fakeChatClient{}, nil)
	prompt := ag.buildSystemPrompt(nil)
	if !strings.Contains(prompt, "MUST call the finish_task tool") {
		t.Fatalf("main prompt missing finish_task rule:\n%s", prompt)
	}
	if !strings.Contains(prompt, "`summary` field IS your message") {
		t.Fatalf("main prompt missing summary-field guidance:\n%s", prompt)
	}
}

func TestAgentRunSegmentedSkillNudgesNextStepOnBareText(t *testing.T) {
	segSkill := &skills.Skill{
		Name:      "segtest",
		Segmented: true,
		Segments:  []string{"Segment one.", "Segment two."},
	}
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: llm.Message{Role: "assistant", Content: "just some text"}},
		{message: toolCallMessage("next_step", `{}`, "call-next")},
		{message: finishMessage("done")},
	}}
	ag := newLoopTestAgent(t, client, nil)
	ag.skills = []skills.Skill{*segSkill}
	ag.registry.Register(&NextStepTool{agent: ag})

	result, err := ag.Run(context.Background(), "/segtest", nil)
	if err != nil || result != "done" {
		t.Fatalf("Run = (%q, %v)", result, err)
	}
	// The bare-text nudge must steer the agent to next_step, not finish_task.
	if !messagesContain(client.calls[1].messages, "user", "next_step") {
		t.Fatalf("bare-text nudge did not mention next_step: %#v", client.calls[1].messages)
	}
}
