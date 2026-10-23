package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
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
	if events[1].Kind != event.ModelStarted || events[2].Kind != event.ToolStarted || events[2].Name != "capture" || events[2].Arguments != `{"value":"x"}` || events[3].Kind != event.ToolCompleted || events[4].Kind != event.ModelStarted {
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
	if len(events) != 5 || events[0].Kind != event.SubagentStarted || events[0].Name != "research" ||
		events[1].Kind != event.ModelStarted || events[2].Kind != event.ToolStarted || events[2].Name != "node_complete" ||
		events[3].Kind != event.ToolCompleted || events[3].Name != "node_complete" ||
		events[4].Kind != event.SubagentCompleted || events[4].Name != "research" {
		t.Fatalf("node events = %#v", events)
	}
}

func TestPipelineSupervisedChildConfirmationIsVisibleAndCancelable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		approve bool
	}{
		{name: "approved", approve: true},
		{name: "cancelled", approve: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			cfg := config.DefaultConfig()
			cfg.Workspace, cfg.WorkDir = workDir, workDir
			cfg.Agent.Mode = config.AgentModeSupervised
			cfg.Agent.Tools = []string{"file_write"}
			cfg.Router.Enabled = false
			factory := &AgentFactory{Config: cfg, SecurityChecker: security.NewChecker(workDir, nil, nil, nil)}
			confirmMgr := tools.NewConfirmationManager()
			scope := tools.InteractionScope{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread", SessionID: "pipeline-session", SenderID: "alice"}
			pipeline := &skills.PipelineSkill{Vars: map[string]interface{}{}, Pipeline: []skills.PipelineNode{}}
			executor := NewPipelineExecutor(pipeline, nil, factory, "", "", 0, nil, nil, nil, nil, confirmMgr, scope.ChannelID, nil)
			executor.scope = scope
			path := filepath.Join(workDir, "pipeline-write.txt")
			writeArgs, _ := json.Marshal(map[string]string{"path": path, "content": "approved"})
			executor.agentBuilder = func(cm *tools.ConfirmationManager, channel string, allowed []string) *Agent {
				ag := factory.CreateAgentFiltered(cm, channel, allowed)
				ag.SetLLMClient(&fakeChatClient{steps: []fakeChatStep{
					{message: toolCallMessage("file_write", string(writeArgs), "write-file")},
					{message: toolCallMessage("node_complete", `{"status":"success","vars":{"result":"written"}}`, "node-done")},
				}})
				return ag
			}
			var events []event.Event
			registeredBeforeConfirmation := false
			confirmationCount := 0
			executor.eventSink = func(ev event.Event) {
				events = append(events, ev)
				if ev.Kind != event.ConfirmationRequested {
					return
				}
				confirmationCount++
				registeredBeforeConfirmation = len(confirmMgr.GetPendingForScope(scope)) == 1
				if tc.approve && registeredBeforeConfirmation {
					confirmMgr.RespondForScope(scope, true)
				}
			}
			ctx := context.Background()
			if !tc.approve {
				cancelCtx, cancel := context.WithCancel(ctx)
				ctx = cancelCtx
				defer cancel()
				priorSink := executor.eventSink
				executor.eventSink = func(ev event.Event) {
					priorSink(ev)
					if ev.Kind == event.ConfirmationRequested {
						cancel()
					}
				}
			}
			node := skills.PipelineNode{ID: "write", Prompt: "write the requested file", Outputs: skills.OutputsMap{"result": "result"}}
			got, err := executor.runAgentNodeAttempt(ctx, node, nil, []string{"file_write"}, "write the file", "")
			if confirmationCount != 1 || !registeredBeforeConfirmation {
				t.Fatalf("confirmation count=%d pending-before-event=%v events=%#v", confirmationCount, registeredBeforeConfirmation, events)
			}
			if tc.approve {
				if err != nil || got["result"] != "written" {
					t.Fatalf("node result=%#v err=%v", got, err)
				}
				if data, readErr := os.ReadFile(path); readErr != nil || string(data) != "approved" {
					t.Fatalf("file content=%q err=%v", data, readErr)
				}
			} else if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("cancelled confirmation wrote file; stat error=%v node err=%v", statErr, err)
			}
			for _, ev := range events {
				if ev.Kind == event.RunStarted || ev.Kind == event.RunCompleted || ev.Kind == event.RunFailed || ev.Kind == event.RunCancelled {
					t.Fatalf("child lifecycle event escaped into parent stream: %#v", events)
				}
			}
		})
	}
}

func TestPipelineChildAskUserQuestionAndWaitingAreDeliveredOnce(t *testing.T) {
	workDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Workspace, cfg.WorkDir = workDir, workDir
	cfg.Agent.Tools = []string{"ask_user"}
	cfg.Router.Enabled = false
	inputMgr := tools.NewUserInputManager()
	factory := &AgentFactory{Config: cfg, UserInputMgr: inputMgr}
	scope := tools.InteractionScope{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread", SessionID: "ask-session", SenderID: "alice"}
	pipeline := &skills.PipelineSkill{Vars: map[string]interface{}{}, Pipeline: []skills.PipelineNode{}}
	taskCalls := 0
	waitingCount := 0
	confirmMgr := tools.NewConfirmationManager()
	parent := newLoopTestAgent(t, &fakeChatClient{}, nil)
	parent.Callbacks.Event = func(ev event.Event) {
		if ev.Kind == event.Waiting {
			waitingCount++
		}
	}
	parent.Callbacks.AskUser = func(question string, _ []string) {
		taskCalls++
		if question != "Which option?" || !inputMgr.RespondForScope(scope, "first") {
			t.Errorf("ask_user notification question=%q did not have a pending input", question)
		}
	}
	// Reproduce the parent RunWithParts bridge setup and preserve the raw
	// transport callback that runPipelineSkill must pass to its child.
	parent.runAskUser = parent.Callbacks.AskUser
	parent.installEventCallbackBridge()
	executor := NewPipelineExecutor(pipeline, nil, factory, "", "", 0, nil, nil, nil, parent.runAskUser, confirmMgr, scope.ChannelID, nil)
	executor.scope = scope
	executor.agentBuilder = func(cm *tools.ConfirmationManager, channel string, allowed []string) *Agent {
		ag := factory.CreateAgentFiltered(cm, channel, allowed)
		ag.SetLLMClient(&fakeChatClient{steps: []fakeChatStep{
			{message: toolCallMessage("ask_user", `{"question":"Which option?","options":["first","second"]}`, "ask")},
			{message: toolCallMessage("node_complete", `{"status":"success","vars":{"result":"answered"}}`, "done")},
		}})
		return ag
	}
	executor.eventSink = parent.Callbacks.Event
	_, err := executor.runAgentNodeAttempt(context.Background(), skills.PipelineNode{ID: "ask", Prompt: "ask a question", Outputs: skills.OutputsMap{"result": "result"}}, nil, []string{"ask_user"}, "ask", "")
	if err != nil {
		t.Fatal(err)
	}
	if taskCalls != 1 || waitingCount != 1 {
		t.Fatalf("ask_user notifications=%d waiting events=%d", taskCalls, waitingCount)
	}
}

func TestParentRunPipelinePreservesRawAskUserTransportAcrossEventBridge(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"ask-1","type":"function","function":{"name":"ask_user","arguments":"{\"question\":\"Which option?\",\"options\":[\"first\",\"second\"]}"}}]},"finish_reason":"tool_calls"}]}`)
		case 2:
			fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"done-1","type":"function","function":{"name":"node_complete","arguments":"{\"status\":\"success\",\"vars\":{\"result\":\"pipeline answered\"}}"}}]},"finish_reason":"tool_calls"}]}`)
		default:
			t.Errorf("unexpected model request %d", call)
			fmt.Fprint(w, `{"choices":[]}`)
		}
	}))
	defer upstream.Close()

	workDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Workspace, cfg.WorkDir = workDir, workDir
	cfg.Agent.Mode = config.AgentModeFull
	cfg.Agent.MaxParallelTools = 1
	cfg.Router.Enabled = false
	cfg.LLM.BaseURL = upstream.URL
	cfg.LLM.Model = "test-model"
	pipeline := &skills.PipelineSkill{
		Returns: "answer",
		Vars:    map[string]interface{}{"answer": ""},
		Pipeline: []skills.PipelineNode{{
			ID:      "ask",
			Type:    "agent",
			Tools:   []string{"ask_user"},
			Prompt:  "Ask the user which option they prefer.",
			Outputs: skills.OutputsMap{"answer": "result"},
		}},
	}
	skill := skills.Skill{Name: "pipeline", Description: "pipeline fixture", Pipeline: pipeline}
	inputMgr := tools.NewUserInputManager()
	factory := &AgentFactory{
		Config:       cfg,
		LLMClient:    llm.NewClient("", upstream.URL, "test-model", false, 0),
		UserInputMgr: inputMgr,
		Skills:       []skills.Skill{skill},
	}
	confirmMgr := tools.NewConfirmationManager()
	parent := factory.CreateAgent(confirmMgr, "room")
	scope := tools.InteractionScope{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread", SessionID: "parent-pipeline", SenderID: "alice"}
	parent.SetInteractionScope(scope)
	waitingCount := atomic.Int32{}
	askCount := atomic.Int32{}
	parent.Callbacks.Event = func(ev event.Event) {
		if ev.Kind == event.Waiting && ev.Name == "ask_user" {
			waitingCount.Add(1)
		}
	}
	rawAskUser := func(question string, _ []string) {
		askCount.Add(1)
		if question != "Which option?" || !inputMgr.RespondForScope(scope, "first") {
			t.Errorf("ask_user transport question=%q did not find the scoped pending input", question)
		}
	}
	parent.Callbacks.AskUser = rawAskUser

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := parent.Run(ctx, "/pipeline choose", nil)
	if err != nil || result != "pipeline answered" {
		t.Fatalf("parent Run = (%q, %v)", result, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("model calls=%d, want ask_user and node_complete", got)
	}
	if got := waitingCount.Load(); got != 1 {
		t.Fatalf("parent Waiting events=%d, want 1", got)
	}
	if got := askCount.Load(); got != 1 {
		t.Fatalf("raw AskUser calls=%d, want 1", got)
	}
	if parent.runAskUser != nil {
		t.Fatal("raw run AskUser callback leaked after parent Run")
	}
	if parent.Callbacks.AskUser == nil || reflect.ValueOf(parent.Callbacks.AskUser).Pointer() != reflect.ValueOf(rawAskUser).Pointer() {
		t.Fatal("parent Run did not restore the original AskUser callback")
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
