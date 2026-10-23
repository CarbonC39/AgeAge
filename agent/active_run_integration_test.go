package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ageage/config"
	"ageage/security"
	"ageage/skills"
	"ageage/tools"
)

type submitSteeringTool struct {
	handle *RunHandle
	text   string
	result string
	calls  int
}

func (t *submitSteeringTool) Name() string        { return "submit_steering" }
func (t *submitSteeringTool) Description() string { return "submit an update after the node completes" }
func (t *submitSteeringTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (t *submitSteeringTool) Execute(context.Context, json.RawMessage) (string, error) {
	t.calls++
	if !t.handle.Submit(UserInput{Text: t.text}) {
		return "", errors.New("steering was rejected")
	}
	return t.result, nil
}

type countTool struct {
	name  string
	calls int
}

func (t *countTool) Name() string        { return t.name }
func (t *countTool) Description() string { return "count executions" }
func (t *countTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (t *countTool) Execute(context.Context, json.RawMessage) (string, error) {
	t.calls++
	return "unexpected second node", nil
}

func TestPipelineSteeringPreservesCompletedAutoNodeAndSkipsNextNode(t *testing.T) {
	handle, claimed := NewSessionManager(t.TempDir()).ClaimRun(context.Background(), "pipeline")
	if !claimed {
		t.Fatal("could not claim pipeline run")
	}
	defer handle.Close()
	first := &submitSteeringTool{handle: handle, text: "change the next step", result: `{"answer":"first node result"}`}
	second := &countTool{name: "second_node"}
	registry := tools.NewRegistry()
	registry.Register(first)
	registry.Register(second)
	cfg := config.DefaultConfig()
	cfg.Workspace, cfg.WorkDir = t.TempDir(), t.TempDir()
	pipeline := &skills.PipelineSkill{
		Returns: "answer",
		Vars:    map[string]interface{}{},
		Pipeline: []skills.PipelineNode{
			{ID: "first", Type: "auto", Tool: first.Name(), Outputs: skills.OutputsMap{"answer": "answer"}},
			{ID: "second", Type: "auto", Tool: second.Name(), Outputs: skills.OutputsMap{"later": "result"}},
		},
	}
	executor := NewPipelineExecutor(pipeline, nil, &AgentFactory{Config: cfg}, "input", "", 0, nil, nil, nil, nil, nil, "", registry)
	_, err := executor.Run(WithRunHandle(context.Background(), handle))
	if !errors.Is(err, errRunSteering) {
		t.Fatalf("pipeline Run error = %v, want steering yield", err)
	}
	if first.calls != 1 || second.calls != 0 {
		t.Fatalf("node calls: first=%d second=%d", first.calls, second.calls)
	}
	if summary := executor.NodeSummary(); !strings.Contains(summary, "first node result") {
		t.Fatalf("completed node output was lost across steering yield: %s", summary)
	}
}

func TestPipelineSteeringRetainsCompletedForeachIterations(t *testing.T) {
	handle, _ := NewSessionManager(t.TempDir()).ClaimRun(context.Background(), "foreach")
	defer handle.Close()
	tool := &submitSteeringTool{handle: handle, text: "change the remaining items", result: `{"answer":"already completed"}`}
	registry := tools.NewRegistry()
	registry.Register(tool)
	cfg := config.DefaultConfig()
	cfg.Pipeline.ForeachConcurrency = 1
	pipeline := &skills.PipelineSkill{
		Returns:  "answers",
		Vars:     map[string]interface{}{"items": []interface{}{"first", "second"}},
		Pipeline: []skills.PipelineNode{{ID: "items", Type: "auto", Tool: tool.Name(), Foreach: "items", Outputs: skills.OutputsMap{"answers": "answer"}}},
	}
	executor := NewPipelineExecutor(pipeline, nil, &AgentFactory{Config: cfg}, "input", "", 0, nil, nil, nil, nil, nil, "", registry)
	_, err := executor.Run(WithRunHandle(context.Background(), handle))
	if !errors.Is(err, errRunSteering) || tool.calls != 1 {
		t.Fatalf("foreach yield: error=%v executions=%d", err, tool.calls)
	}
	summary := executor.NodeSummary()
	if !strings.Contains(summary, "already completed") || !strings.Contains(summary, "completed foreach iterations [0]") {
		t.Fatalf("completed iteration evidence missing from steering context: %s", summary)
	}
}

func TestStoppingRunCancelsPendingSupervisedConfirmationWithoutWriting(t *testing.T) {
	workDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Workspace, cfg.WorkDir = workDir, workDir
	cfg.Agent.Mode = config.AgentModeSupervised
	cfg.Agent.Tools = []string{"file_write"}
	cfg.Router.Enabled = false
	client := &fakeChatClient{steps: []fakeChatStep{{message: toolCallMessage("file_write", `{"path":"confirm-target.txt","content":"should not be written"}`, "write-confirmed")}}}
	factory := &AgentFactory{
		Config:          cfg,
		SecurityChecker: security.NewChecker(workDir, nil, nil, nil),
		UserInputMgr:    tools.NewUserInputManager(),
	}
	confirmMgr := tools.NewConfirmationManager()
	ag := factory.CreateAgent(confirmMgr, "room")
	ag.client = client
	scope := tools.InteractionScope{ChannelType: "matrix", ChannelID: "room", SessionID: "confirmation-session", SenderID: "alice"}
	ag.SetInteractionScope(scope)
	handle, claimed := NewSessionManager(t.TempDir()).ClaimRun(context.Background(), scope.SessionID)
	if !claimed {
		t.Fatal("could not claim confirmation run")
	}
	defer handle.Close()
	runDone := make(chan error, 1)
	go func() {
		_, err := ag.RunInput(WithRunHandle(handle.Context(), handle), UserInput{Text: "write the file"}, nil)
		runDone <- err
	}()
	deadline := time.After(2 * time.Second)
	for len(confirmMgr.GetPendingForScope(scope)) == 0 {
		select {
		case <-deadline:
			handle.Cancel()
			t.Fatal("supervised tool did not request confirmation")
		case <-time.After(time.Millisecond):
		}
	}
	pendingID := confirmMgr.GetPendingForScope(scope)[0].ID
	handle.Cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("cancelled RunInput returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run remained blocked on confirmation after cancellation")
	}
	if len(confirmMgr.GetPendingForScope(scope)) != 0 || confirmMgr.CancelRequest(pendingID) {
		t.Fatal("cancelled confirmation remained pending")
	}
	if _, err := os.Stat(filepath.Join(workDir, "confirm-target.txt")); !os.IsNotExist(err) {
		t.Fatalf("confirmation-gated file write had side effects, stat error = %v", err)
	}
}
