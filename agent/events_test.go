package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ageage/config"
	"ageage/event"
	"ageage/llm"
	"ageage/security"
	"ageage/skills"
	"ageage/tools"
)

func TestAgentEmitsRunAndToolEventsAndKeepsLegacyCallbacks(t *testing.T) {
	capture := &loopCaptureTool{result: "captured"}
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: toolCallMessage("capture", `{"value":"x"}`, "call-1")},
		{message: finishMessage("finished")},
	}}
	ag := newLoopTestAgent(t, client, capture)
	var events []event.Event
	legacyStarts, legacyEnds := 0, 0
	ag.Callbacks.Event = func(ev event.Event) { events = append(events, ev) }
	ag.Callbacks.ToolStart = func(string, string) { legacyStarts++ }
	ag.Callbacks.ToolEnd = func(string) { legacyEnds++ }
	if _, err := ag.Run(context.Background(), "task", nil); err != nil {
		t.Fatal(err)
	}
	if len(events) < 4 || events[0].Kind != event.RunStarted || events[len(events)-1].Kind != event.RunCompleted || events[len(events)-1].Text != "finished" {
		t.Fatalf("run events = %#v", events)
	}
	if events[1].Kind != event.ToolStarted || events[1].Name != "capture" || events[1].Arguments != `{"value":"x"}` || events[2].Kind != event.ToolCompleted {
		t.Fatalf("tool events = %#v", events)
	}
	if events[0].At.IsZero() || events[len(events)-1].At.IsZero() {
		t.Fatal("event timestamps were not assigned")
	}
	if legacyStarts != 2 || legacyEnds != 2 {
		t.Fatalf("legacy callbacks start=%d end=%d", legacyStarts, legacyEnds)
	}
	if ag.Callbacks.ToolStart == nil || ag.Callbacks.ToolEnd == nil {
		t.Fatal("legacy callbacks were not restored after the run")
	}
}

type namedEventTool struct{ name string }

func (t *namedEventTool) Name() string        { return t.name }
func (t *namedEventTool) Description() string { return "event test tool" }
func (t *namedEventTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (t *namedEventTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "delegated", nil
}

func TestAgentMapsDelegateHooksToSubagentEvents(t *testing.T) {
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: toolCallMessage("delegate", `{}`, "call-delegate")},
		{message: finishMessage("finished")},
	}}
	ag := newLoopTestAgent(t, client, nil)
	ag.registry.Register(&namedEventTool{name: "delegate"})
	var events []event.Event
	ag.Callbacks.Event = func(ev event.Event) { events = append(events, ev) }
	if _, err := ag.Run(context.Background(), "task", nil); err != nil {
		t.Fatal(err)
	}
	var started, completed bool
	for _, ev := range events {
		started = started || ev.Kind == event.SubagentStarted
		completed = completed || ev.Kind == event.SubagentCompleted
	}
	if !started || !completed {
		t.Fatalf("delegate event kinds missing: %#v", events)
	}
}

func TestSubagentRunDoesNotEmitParentRunLifecycleEvents(t *testing.T) {
	client := &fakeChatClient{steps: []fakeChatStep{{message: finishMessage("done")}}}
	ag := newLoopTestAgent(t, client, nil)
	ag.Mode.IsSubAgent = true
	var events []event.Event
	ag.Callbacks.Event = func(ev event.Event) { events = append(events, ev) }
	if _, err := ag.Run(context.Background(), "task", nil); err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Kind == event.RunStarted || ev.Kind == event.RunCompleted || ev.Kind == event.RunFailed || ev.Kind == event.RunCancelled {
			t.Fatalf("sub-agent emitted parent lifecycle event: %#v", events)
		}
	}
}

func TestAgentEmitsFailureAndCancellationEvents(t *testing.T) {
	modelErr := errors.New("model failed")
	client := &fakeChatClient{steps: []fakeChatStep{{err: modelErr}}}
	ag := newLoopTestAgent(t, client, nil)
	var events []event.Event
	ag.Callbacks.Event = func(ev event.Event) { events = append(events, ev) }
	if _, err := ag.Run(context.Background(), "task", nil); !errors.Is(err, modelErr) {
		t.Fatalf("Run error = %v", err)
	} else if len(events) == 0 || events[len(events)-1].Text != err.Error() {
		t.Fatalf("failure event text = %#v; run error = %v", events, err)
	}
	if len(events) == 0 || events[len(events)-1].Kind != event.RunFailed {
		t.Fatalf("failure events = %#v", events)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelClient := &fakeChatClient{steps: []fakeChatStep{{err: context.Canceled}}}
	cancelAgent := newLoopTestAgent(t, cancelClient, nil)
	events = nil
	cancelAgent.Callbacks.Event = func(ev event.Event) { events = append(events, ev) }
	_, _ = cancelAgent.Run(ctx, "task", nil)
	if len(events) == 0 || events[0].Kind != event.RunStarted || events[len(events)-1].Kind != event.RunCancelled || events[len(events)-1].Text != context.Canceled.Error() {
		t.Fatalf("cancel events = %#v", events)
	}
}

func TestPipelineAgentNodeEmitsParentSubagentEvents(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.SubAgent.MaxIterations = 3
	factory := &AgentFactory{Config: cfg}
	pipeline := &skills.PipelineSkill{Vars: map[string]interface{}{}, Pipeline: []skills.PipelineNode{}}
	executor := NewPipelineExecutor(pipeline, nil, factory, "", "", 0, nil, nil, nil, nil, nil, "", nil)
	executor.agentBuilder = func(*tools.ConfirmationManager, string, []string) *Agent {
		client := &fakeChatClient{steps: []fakeChatStep{{message: toolCallMessage("node_complete", `{"status":"success","vars":{"result":"node answer"}}`, "node-complete")}}}
		return newLoopTestAgent(t, client, nil)
	}
	var events []event.Event
	executor.eventSink = func(ev event.Event) { events = append(events, ev) }
	node := skills.PipelineNode{ID: "research", Prompt: "inspect source", Outputs: skills.OutputsMap{"answer": "result"}}
	got, err := executor.runAgentNodeAttempt(context.Background(), node, nil, nil, "inspect", "")
	if err != nil || got["answer"] != "node answer" {
		t.Fatalf("node result = %#v, err=%v", got, err)
	}
	if len(events) != 2 || events[0].Kind != event.SubagentStarted || events[0].Name != "research" || events[1].Kind != event.SubagentCompleted || events[1].Name != "research" {
		t.Fatalf("node events = %#v", events)
	}
}

func TestTypedAskUserReplyIsMarkedAsInputReply(t *testing.T) {
	manager := tools.NewUserInputManager()
	scope := tools.InteractionScope{ChannelType: "matrix", ChannelID: "room", SenderID: "alice"}
	notify := func(string, []string) {}
	ag := newLoopTestAgent(t, &fakeChatClient{steps: []fakeChatStep{
		{message: toolCallMessage("ask_user", `{"question":"attach"}`, "ask-1")},
		{message: finishMessage("done")},
	}}, nil)
	ag.registry.Register(&tools.AskUserTool{Manager: manager, Scope: scope, NotifyFuncPtr: &notify})
	done := make(chan error, 1)
	go func() {
		_, err := ag.Run(context.Background(), "please ask", nil)
		done <- err
	}()
	deadline := time.After(time.Second)
	for !manager.HasPendingScope(scope) {
		select {
		case <-deadline:
			t.Fatal("ask_user request did not become pending")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	answer := UserInput{Text: "answer", Parts: []llm.ContentPart{{Type: "text", Text: "answer"}}}
	if !manager.RespondInputForScope(scope, answer) {
		t.Fatal("typed response was rejected")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, message := range ag.Messages() {
		if message.InputReply {
			if message.TextContent() != "answer" || len(message.Parts) != 1 || message.Parts[0].Text != "answer" {
				t.Fatalf("input reply message = %#v", message)
			}
			return
		}
	}
	t.Fatalf("conversation did not mark ask_user answer as InputReply: %#v", ag.Messages())
}

func TestConfirmationRegistersBeforeStructuredNotification(t *testing.T) {
	workspace := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Workspace = workspace
	cfg.WorkDir = workspace
	cfg.Agent.Mode = config.AgentModeSupervised
	factory := &AgentFactory{Config: cfg, SecurityChecker: security.NewChecker(workspace, nil, []string{workspace}, nil)}
	confirmMgr := tools.NewConfirmationManager()
	ag := factory.CreateAgent(confirmMgr, "room")
	scope := tools.InteractionScope{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread", SessionID: "session", SenderID: "alice"}
	ag.SetInteractionScope(scope)
	legacyNotifyCalls := 0
	ag.Callbacks.Notify = func(string) { legacyNotifyCalls++ }
	events := 0
	registeredBeforeEvent := false
	ag.Callbacks.Event = func(ev event.Event) {
		if ev.Kind != event.ConfirmationRequested {
			return
		}
		events++
		pending := confirmMgr.GetPendingForScope(scope)
		registeredBeforeEvent = len(pending) == 1
		if registeredBeforeEvent {
			confirmMgr.RespondForScope(scope, true)
		}
	}
	tool, ok := ag.registry.Get("file_write")
	if !ok {
		t.Fatal("file_write tool was not registered")
	}
	path := filepath.Join(workspace, "approved.txt")
	args, _ := json.Marshal(map[string]string{"path": path, "content": "approved"})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	if events != 1 || !registeredBeforeEvent {
		t.Fatalf("confirmation event count=%d pending-before-event=%v", events, registeredBeforeEvent)
	}
	if legacyNotifyCalls != 0 {
		t.Fatalf("structured confirmation also called legacy notify %d times", legacyNotifyCalls)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "approved" {
		t.Fatalf("approved file content=%q err=%v", data, err)
	}
}

func TestEvaluatorNotificationCapturesBackgroundHandler(t *testing.T) {
	var got event.Event
	closedRun := true
	background := func(ev event.Event) {
		if !closedRun {
			t.Error("test expected background handler to outlive the run")
		}
		got = ev
	}
	legacyCalled := false
	notify := evaluatorNotify(background, func(string) { legacyCalled = true })
	// The per-run callback can now be restored/closed; the evaluator's captured
	// background callback remains an independent delivery path.
	background = func(ev event.Event) { got = ev }
	notify("evaluation complete")
	if got.Kind != event.Notification || got.Text != "evaluation complete" || got.At.IsZero() {
		t.Fatalf("background event = %#v", got)
	}
	if legacyCalled {
		t.Fatal("legacy run-scoped callback used despite a background handler")
	}
}
