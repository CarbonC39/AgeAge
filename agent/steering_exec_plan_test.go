package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ageage/config"
	"ageage/llm"
	"ageage/security"
	"ageage/skills"
	"ageage/tools"
)

// The initial tool definitions model a restrictive router decision. Steering
// arrives while that decision's tool is running, after which the model must be
// shown a plan rebuilt from the registered tool set.
func TestSteeringRefreshesExecutionPlanAfterToolBatch(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "nonstreaming"
		if stream {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) { testSteeringRefreshesExecutionPlan(t, stream) })
	}
}

func testSteeringRefreshesExecutionPlan(t *testing.T, stream bool) {
	blocker := &inboxBlockingTool{entered: make(chan struct{}), release: make(chan struct{})}
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: toolCallMessage("inbox_block", `{}`, "call-block")},
		{message: toolCallMessage("file_write", `{"path":"steered.txt","content":"written from steering"}`, "call-write")},
		{message: finishMessage("written")},
	}}
	ag := newLoopTestAgent(t, client, nil)
	ag.registry.Register(blocker)
	ag.registry.Register(&tools.FileWriteTool{Security: security.NewChecker(ag.cfg.Workspace, nil, nil, nil)})
	manager := NewSessionManager(t.TempDir())
	handle, ok := manager.ClaimRun(context.Background(), "plan-refresh")
	if !ok {
		t.Fatal("could not claim run")
	}
	defer handle.Close()
	ag.inbox = handle
	ag.conv.Reset([]llm.Message{{Role: "user", Content: "start with the web"}})
	ctx := WithRunHandle(handle.Context(), handle)

	// Only inbox_block and finish_task are exposed by the initial router plan.
	initialDefs := ag.registry.ToOpenAIToolsFiltered([]string{"inbox_block", "finish_task"})
	done := make(chan error, 1)
	var streamCallback llm.StreamCallback
	if stream {
		streamCallback = func(string) {}
	}
	go func() {
		_, err := ag.runLoop(ctx, streamCallback, initialDefs, client, false)
		done <- err
	}()
	select {
	case <-blocker.entered:
	case <-time.After(2 * time.Second):
		handle.Cancel()
		t.Fatal("restricted-plan tool did not start")
	}
	if !handle.Submit(UserInput{Text: "also write the result to a file"}) {
		t.Fatal("steering input was rejected")
	}
	close(blocker.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runLoop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		handle.Cancel()
		t.Fatal("runLoop did not finish")
	}

	if len(client.calls) != 3 {
		t.Fatalf("model calls = %d, want 3", len(client.calls))
	}
	if hasToolDef(client.calls[0].tools, "file_write") {
		t.Fatal("initial restrictive router plan unexpectedly exposed file_write")
	}
	if !hasToolDef(client.calls[1].tools, "file_write") {
		t.Fatalf("next model call did not refresh tools: %#v", toolDefNames(client.calls[1].tools))
	}
	if hasToolDef(client.calls[1].tools, "not_registered_tool") {
		t.Fatal("refreshed plan exposed a tool absent from the registry")
	}
	if !messagesContain(client.calls[1].messages, "user", "also write the result to a file") {
		t.Fatalf("next model call omitted steering: %#v", client.calls[1].messages)
	}
	if !hasToolDef(client.calls[2].tools, "file_write") {
		t.Fatalf("file_write disappeared from later decision: %#v", toolDefNames(client.calls[2].tools))
	}
	if got, err := os.ReadFile(filepath.Join(ag.cfg.Workspace, "steered.txt")); err != nil || string(got) != "written from steering" {
		t.Fatalf("steered file content = %q, err=%v", got, err)
	}
	// The assistant call batch must be followed immediately by its tool result;
	// steering is appended only after all results have been recorded.
	callAt, resultAt, steeringAt := -1, -1, -1
	for i, msg := range client.calls[1].messages {
		if msg.Role == "assistant" && len(msg.ToolCalls) == 1 && msg.ToolCalls[0].ID == "call-block" {
			callAt = i
		}
		if msg.Role == "tool" && msg.ToolCallID == "call-block" {
			resultAt = i
		}
		if msg.Role == "user" && strings.Contains(msg.Content, "also write the result") {
			steeringAt = i
		}
	}
	if callAt < 0 || resultAt != callAt+1 || steeringAt != resultAt+1 {
		t.Fatalf("tool batch/steering order invalid (call=%d result=%d steering=%d): %#v", callAt, resultAt, steeringAt, client.calls[1].messages)
	}
}

func TestSteeringAfterPostBatchConsumptionStillRefreshesNextDecision(t *testing.T) {
	blocker := &inboxBlockingTool{entered: make(chan struct{}), release: make(chan struct{})}
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: toolCallMessage("inbox_block", `{}`, "call-post-batch")},
		{message: finishMessage("done")},
	}}
	ag := newLoopTestAgent(t, client, nil)
	ag.registry.Register(blocker)
	target := &loopCaptureTool{result: "target ran"}
	ag.registry.Register(target)
	handle, ok := NewSessionManager(t.TempDir()).ClaimRun(context.Background(), "post-batch-refresh")
	if !ok {
		t.Fatal("could not claim run")
	}
	defer handle.Close()
	ag.inbox = handle
	ag.conv.Reset([]llm.Message{{Role: "user", Content: "start"}})
	initialDefs := ag.registry.ToOpenAIToolsFiltered([]string{"inbox_block", "finish_task"})
	done := make(chan error, 1)
	go func() {
		_, err := ag.runLoop(WithRunHandle(handle.Context(), handle), nil, initialDefs, client, false)
		done <- err
	}()
	select {
	case <-blocker.entered:
	case <-time.After(2 * time.Second):
		handle.Cancel()
		t.Fatal("blocking tool did not start")
	}
	if !handle.Submit(UserInput{Text: "include the target operation"}) {
		t.Fatal("steering input was rejected")
	}
	close(blocker.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runLoop returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		handle.Cancel()
		t.Fatal("runLoop did not finish")
	}
	if len(client.calls) != 2 || !hasToolDef(client.calls[1].tools, "capture") {
		t.Fatalf("post-batch consumed input did not refresh plan: calls=%d tools=%v", len(client.calls), toolDefNames(client.calls[1].tools))
	}
	if !messagesContain(client.calls[1].messages, "user", "include the target operation") {
		t.Fatalf("post-batch input missing from next call: %#v", client.calls[1].messages)
	}
}

func TestSteeringDuringRouterDiscardsStalePipelineSelection(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"checks\":{\"needs_tools\":true,\"needs_multiple_calls\":false,\"needs_synthesis\":false,\"is_recurring_workflow\":false},\"skill\":\"Stale pipeline\",\"required_tools\":[\"inbox_block\"],\"reasoning\":\"old task\"}"}}]}`))
	}))
	defer server.Close()

	marker := &countTool{name: "pipeline_marker"}
	client := &fakeChatClient{steps: []fakeChatStep{{message: finishMessage("handled latest request")}}}
	ag := newLoopTestAgent(t, client, nil)
	ag.cfg.LLM.Model = "test-model"
	ag.cfg.LLM.BaseURL = server.URL
	pipelineSkill := skills.Skill{
		Name: "Stale pipeline",
		Pipeline: &skills.PipelineSkill{Pipeline: []skills.PipelineNode{{
			ID: "obsolete-node", Type: "auto", Tool: "pipeline_marker", Inputs: map[string]interface{}{},
		}}},
	}
	ag.skills = []skills.Skill{pipelineSkill}
	ag.router = NewRouter(ag.cfg, llm.NewClient("", server.URL, "test-router", false, 0), ag.skills, false)
	ag.deps = &AgentFactory{Config: ag.cfg, LLMClient: llm.NewClient("", server.URL, "test-model", false, 0), Skills: ag.skills}
	ag.registry.Register(&tools.FileWriteTool{Security: security.NewChecker(ag.cfg.Workspace, nil, nil, nil)})
	ag.registry.Register(marker)
	handle, ok := NewSessionManager(t.TempDir()).ClaimRun(context.Background(), "router-steering")
	if !ok {
		t.Fatal("could not claim run")
	}
	defer handle.Close()
	done := make(chan error, 1)
	go func() {
		_, err := ag.RunInput(WithRunHandle(handle.Context(), handle), UserInput{Text: "original request"}, nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		handle.Cancel()
		t.Fatal("router request did not start")
	}
	if !handle.Submit(UserInput{Text: "ignore the old pipeline and write a file"}) {
		t.Fatal("steering input was rejected during router request")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunInput returned error: %v", err)
		}
	case <-time.After(4 * time.Second):
		handle.Cancel()
		t.Fatal("run did not finish after router returned")
	}
	if marker.calls != 0 {
		t.Fatalf("stale router-selected pipeline executed %d obsolete nodes", marker.calls)
	}
	if len(client.calls) != 1 || !hasToolDef(client.calls[0].tools, "file_write") {
		t.Fatalf("main model did not receive refreshed plan: calls=%d tools=%v", len(client.calls), func() []string {
			if len(client.calls) == 0 {
				return nil
			}
			return toolDefNames(client.calls[0].tools)
		}())
	}
	if !messagesContain(client.calls[0].messages, "user", "ignore the old pipeline") {
		t.Fatalf("latest input missing from main model call: %#v", client.calls[0].messages)
	}
}

func TestSteeringBeforeExplicitPipelineRunSkipsStalePipeline(t *testing.T) {
	marker := &countTool{name: "pipeline_marker"}
	client := &fakeChatClient{steps: []fakeChatStep{{message: finishMessage("handled update")}}}
	ag := newLoopTestAgent(t, client, nil)
	pipelineSkill := skills.Skill{
		Name:          "Explicit pipeline",
		RequiredTools: []string{"pipeline_marker"},
		Pipeline: &skills.PipelineSkill{Pipeline: []skills.PipelineNode{{
			ID: "obsolete-node", Type: "auto", Tool: "pipeline_marker", Inputs: map[string]interface{}{},
		}}},
	}
	ag.skills = []skills.Skill{pipelineSkill}
	ag.deps = &AgentFactory{Config: ag.cfg, LLMClient: llm.NewClient("", "", "test-model", false, 0), Skills: ag.skills}
	ag.registry.Register(marker)
	ag.registry.Register(&tools.FileWriteTool{Security: security.NewChecker(ag.cfg.Workspace, nil, nil, nil)})
	handle, ok := NewSessionManager(t.TempDir()).ClaimRun(context.Background(), "explicit-pipeline-steering")
	if !ok {
		t.Fatal("could not claim run")
	}
	defer handle.Close()
	if !handle.Submit(UserInput{Text: "do the updated one-off task"}) {
		t.Fatal("pre-run steering input was rejected")
	}
	_, err := ag.RunInput(WithRunHandle(handle.Context(), handle), UserInput{Text: "/explicit-pipeline run it"}, nil)
	if err != nil {
		t.Fatalf("RunInput returned error: %v", err)
	}
	if marker.calls != 0 {
		t.Fatalf("explicit pipeline ran its stale auto node %d times after pre-run steering", marker.calls)
	}
	if len(client.calls) != 1 || !hasToolDef(client.calls[0].tools, "file_write") {
		t.Fatalf("main model did not receive current registry plan: calls=%d tools=%v", len(client.calls), func() []string {
			if len(client.calls) == 0 {
				return nil
			}
			return toolDefNames(client.calls[0].tools)
		}())
	}
	if !messagesContain(client.calls[0].messages, "user", "do the updated one-off task") {
		t.Fatalf("pre-run steering missing from model context: %#v", client.calls[0].messages)
	}
}

func TestSteeringDuringRouterSkipsStaleRecurringPlanner(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")
	if err := os.WriteFile(configPath, []byte("workspace = \""+root+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load isolated config: %v", err)
	}
	cfg.Planner.Enabled = true
	started, release := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"checks\":{\"needs_tools\":true,\"needs_multiple_calls\":true,\"needs_synthesis\":true,\"is_recurring_workflow\":true},\"skill\":\"\",\"required_tools\":[\"web_search\"],\"reasoning\":\"stale recurring task\"}"}}]}`))
	}))
	defer server.Close()
	client := &fakeChatClient{steps: []fakeChatStep{{message: finishMessage("handled latest request")}}}
	ag := newLoopTestAgent(t, client, nil)
	ag.cfg = cfg
	factoryLLM := llm.NewClient("", server.URL, "test-router", false, 0)
	ag.deps = &AgentFactory{Config: cfg, LLMClient: factoryLLM}
	ag.router = NewRouter(cfg, factoryLLM, nil, false)
	handle, ok := NewSessionManager(t.TempDir()).ClaimRun(context.Background(), "router-recurring-steering")
	if !ok {
		t.Fatal("could not claim run")
	}
	defer handle.Close()
	done := make(chan error, 1)
	go func() {
		_, runErr := ag.RunInput(WithRunHandle(handle.Context(), handle), UserInput{Text: "old recurring task"}, nil)
		done <- runErr
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		handle.Cancel()
		t.Fatal("router request did not start")
	}
	if !handle.Submit(UserInput{Text: "new one-off task"}) {
		t.Fatal("steering input was rejected during router request")
	}
	close(release)
	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("RunInput returned error: %v", runErr)
		}
	case <-time.After(4 * time.Second):
		handle.Cancel()
		t.Fatal("run did not finish after router returned")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("planner made stale follow-up requests after steering: %d total HTTP requests", got)
	}
	if _, err := os.Stat(cfg.SkillsDir()); !os.IsNotExist(err) {
		t.Fatalf("stale planner created skills directory %q (stat err=%v)", cfg.SkillsDir(), err)
	}
	if len(client.calls) != 1 || !messagesContain(client.calls[0].messages, "user", "new one-off task") {
		t.Fatalf("main model did not proceed with latest input: %#v", client.calls)
	}
}

func hasToolDef(defs []llm.ToolDef, name string) bool {
	for _, def := range defs {
		if def.Function.Name == name {
			return true
		}
	}
	return false
}

func toolDefNames(defs []llm.ToolDef) []string {
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Function.Name)
	}
	return names
}
