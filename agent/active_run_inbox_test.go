package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ageage/llm"
	"ageage/tools"
)

func TestSessionManagersClaimOnlyOneActiveRunPerSession(t *testing.T) {
	root := t.TempDir()
	first := NewSessionManager(root)
	second := NewSessionManager(filepath.Join(root, "."))

	owner, claimed := first.ClaimRun(context.Background(), "shared")
	if !claimed || owner == nil {
		t.Fatal("first manager did not claim the run")
	}
	other, claimed := second.ClaimRun(context.Background(), "shared")
	if claimed || other != owner {
		t.Fatalf("second manager claim = (%p, %v), want active owner (%p, false)", other, claimed, owner)
	}
	owner.Close()
	next, claimed := second.ClaimRun(context.Background(), "shared")
	if !claimed {
		t.Fatal("session was not claimable after the active run closed")
	}
	next.Close()
}

func TestRunHandlePreparationOrderingSealAndRelease(t *testing.T) {
	sm := NewSessionManager(t.TempDir())
	owner, claimed := sm.ClaimRun(context.Background(), "prepare")
	if !claimed {
		t.Fatal("could not claim run")
	}
	firstStarted, releaseFirst := make(chan struct{}), make(chan struct{})
	firstDone := make(chan bool, 1)
	go func() {
		firstDone <- owner.Prepare(func(context.Context) UserInput {
			close(firstStarted)
			<-releaseFirst
			return UserInput{Text: "first"}
		})
	}()
	<-firstStarted
	secondDone := make(chan bool, 1)
	go func() {
		secondDone <- owner.Prepare(func(context.Context) UserInput { return UserInput{Text: "second"} })
	}()
	select {
	case ok := <-secondDone:
		if !ok {
			t.Fatal("second preparation was rejected")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second preparation did not finish")
	}

	if owner.TryFinish() {
		t.Fatal("TryFinish sealed a run with pending preparations")
	}
	if owner.Submit(UserInput{Text: "third"}) != true {
		t.Fatal("an unsealed run rejected an input")
	}
	close(releaseFirst)
	if !<-firstDone {
		t.Fatal("first preparation was rejected")
	}
	values := owner.Drain(context.Background())
	if len(values) != 3 || values[0].Text != "first" || values[1].Text != "second" || values[2].Text != "third" {
		t.Fatalf("prepared inputs lost reservation order: %#v", values)
	}
	if !owner.TryFinish() {
		t.Fatal("empty run could not seal")
	}
	if owner.Submit(UserInput{Text: "too late"}) {
		t.Fatal("sealed run accepted a new input")
	}
	owner.Close()
	select {
	case <-owner.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not signal Done")
	}
	if next, ok := sm.ClaimRun(context.Background(), "prepare"); !ok {
		t.Fatal("session was not released after Close")
	} else {
		next.Close()
	}
}

func TestAgentRejectsConcurrentRunOnSameInstance(t *testing.T) {
	blocker := &inboxBlockingTool{entered: make(chan struct{}), release: make(chan struct{})}
	client := &fakeChatClient{steps: []fakeChatStep{{message: toolCallMessage("inbox_block", `{}`, "call-concurrent")}, {message: finishMessage("done")}}}
	ag := newLoopTestAgent(t, client, nil)
	ag.registry.Register(blocker)
	firstDone := make(chan error, 1)
	go func() {
		_, err := ag.Run(context.Background(), "first", nil)
		firstDone <- err
	}()
	select {
	case <-blocker.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not reach its blocking tool")
	}
	if _, err := ag.Run(context.Background(), "second", nil); err == nil || !strings.Contains(err.Error(), "already has an active run") {
		t.Fatalf("concurrent Run error = %v", err)
	}
	close(blocker.release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first Run returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first run did not finish")
	}
}

type progressiveErrorClient struct {
	started chan struct{}
	call    llm.ToolCall
}

func (c *progressiveErrorClient) ChatCompletion(context.Context, []llm.Message, []llm.ToolDef, float64) (*llm.ChatResponse, error) {
	return nil, errors.New("unexpected non-streaming completion")
}
func (c *progressiveErrorClient) ChatCompletionStream(_ context.Context, _ []llm.Message, _ []llm.ToolDef, _ float64, _ llm.StreamCallback, dispatch llm.ToolCallStreamCb) (*llm.Message, *llm.Usage, error) {
	dispatch(0, c.call)
	select {
	case <-c.started:
	case <-time.After(2 * time.Second):
		return nil, nil, errors.New("progressive tool did not start")
	}
	return nil, nil, errors.New("stream failed after dispatch")
}
func (*progressiveErrorClient) APIKey() string  { return "" }
func (*progressiveErrorClient) BaseURL() string { return "" }

type progressiveReadTool struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (t *progressiveReadTool) Name() string        { return "progressive_read" }
func (t *progressiveReadTool) Description() string { return "read-only test tool" }
func (t *progressiveReadTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (t *progressiveReadTool) Metadata() tools.ToolMetadata {
	return tools.ToolMetadata{ReadOnly: true, Idempotent: true}
}
func (t *progressiveReadTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	close(t.started)
	select {
	case <-t.release:
		close(t.finished)
		return "read complete", nil
	case <-ctx.Done():
		close(t.finished)
		return "", ctx.Err()
	}
}

func TestStreamingFailureJoinsProgressiveToolsBeforeReturning(t *testing.T) {
	tool := &progressiveReadTool{started: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	client := &progressiveErrorClient{
		started: tool.started,
		call:    llm.ToolCall{ID: "progressive-call", Type: "function", Function: llm.FunctionCall{Name: "progressive_read", Arguments: `{}`}},
	}
	ag := newLoopTestAgent(t, client, nil)
	ag.cfg.Agent.MaxParallelTools = 2
	ag.registry.Register(tool)
	var resultCallbacks atomic.Int32
	ag.Callbacks.ToolResult = func(_, _ string) { resultCallbacks.Add(1) }
	done := make(chan error, 1)
	go func() {
		_, err := ag.RunWithParts(context.Background(), "stream", nil, func(string) {})
		done <- err
	}()
	select {
	case <-tool.started:
	case <-time.After(2 * time.Second):
		t.Fatal("progressive tool did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("run returned before the progressive tool finished: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(tool.release)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stream failed") {
			t.Fatalf("RunWithParts error = %v, want the stream failure", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("run did not return after progressive tool completion")
	}
	select {
	case <-tool.finished:
	default:
		t.Fatal("run returned before the tool completion signal")
	}
	if got := resultCallbacks.Load(); got != 1 {
		t.Fatalf("tool result callbacks = %d, want exactly one", got)
	}
	messageCount := len(ag.Messages())
	time.Sleep(20 * time.Millisecond)
	if len(ag.Messages()) != messageCount || resultCallbacks.Load() != 1 {
		t.Fatal("run-owned history or callbacks changed after RunWithParts returned")
	}
}

type inboxBlockingTool struct {
	entered chan struct{}
	release chan struct{}
}

func (t *inboxBlockingTool) Name() string        { return "inbox_block" }
func (t *inboxBlockingTool) Description() string { return "blocks until released by the test" }
func (t *inboxBlockingTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (t *inboxBlockingTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	close(t.entered)
	select {
	case <-t.release:
		return "tool finished", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestActiveRunConsumesOrderedMultimodalSteeringAfterToolBatch(t *testing.T) {
	blocker := &inboxBlockingTool{entered: make(chan struct{}), release: make(chan struct{})}
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: toolCallMessage("inbox_block", `{}`, "call-block")},
		{message: finishMessage("updated answer")},
	}}
	ag := newLoopTestAgent(t, client, nil)
	ag.registry.Register(blocker)
	manager := NewSessionManager(t.TempDir())
	handle, claimed := manager.ClaimRun(context.Background(), "inbox")
	if !claimed {
		t.Fatal("could not claim test run")
	}

	runDone := make(chan error, 1)
	go func() {
		_, err := ag.RunInput(WithRunHandle(handle.Context(), handle), UserInput{Text: "initial request"}, nil)
		runDone <- err
	}()
	select {
	case <-blocker.entered:
	case <-time.After(2 * time.Second):
		handle.Cancel()
		t.Fatal("tool did not start")
	}

	image := llm.ContentPart{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,steering-image"}}
	if !handle.Submit(UserInput{Text: "first update", Parts: []llm.ContentPart{image}}) {
		t.Fatal("first steering input was rejected")
	}
	if !handle.Submit(UserInput{Text: "second update"}) {
		t.Fatal("second steering input was rejected")
	}
	close(blocker.release)

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("RunInput returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		handle.Cancel()
		t.Fatal("active run did not finish")
	}
	handle.Close()

	if len(client.calls) != 2 {
		t.Fatalf("model call count = %d, want 2", len(client.calls))
	}
	messages := client.calls[1].messages
	toolCallAt, toolResultAt, firstSteeringAt, secondSteeringAt := -1, -1, -1, -1
	for i, message := range messages {
		if message.Role == "assistant" && len(message.ToolCalls) == 1 && message.ToolCalls[0].ID == "call-block" {
			toolCallAt = i
		}
		if message.Role == "tool" && message.ToolCallID == "call-block" && strings.Contains(message.Content, "tool finished") {
			toolResultAt = i
		}
		if message.Role == "user" && message.Content == "first update" {
			firstSteeringAt = i
			if len(message.Parts) != 1 || message.Parts[0].ImageURL == nil || message.Parts[0].ImageURL.URL != image.ImageURL.URL {
				t.Fatalf("multimodal steering parts were lost: %#v", message.Parts)
			}
		}
		if message.Role == "user" && strings.Contains(message.Content, "second update") {
			secondSteeringAt = i
		}
	}
	if !(toolCallAt >= 0 && toolResultAt == toolCallAt+1 && firstSteeringAt == toolResultAt+1 && secondSteeringAt == firstSteeringAt+1) {
		t.Fatalf("tool batch / steering order is invalid (call=%d result=%d first=%d second=%d): %#v", toolCallAt, toolResultAt, firstSteeringAt, secondSteeringAt, messages)
	}
}

func TestCancelledActiveRunKeepsAcceptedSteeringAndCompleteToolBatch(t *testing.T) {
	blocker := &inboxBlockingTool{entered: make(chan struct{}), release: make(chan struct{})}
	client := &fakeChatClient{steps: []fakeChatStep{{message: toolCallMessage("inbox_block", `{}`, "call-cancel")}}}
	ag := newLoopTestAgent(t, client, nil)
	ag.registry.Register(blocker)
	manager := NewSessionManager(t.TempDir())
	handle, claimed := manager.ClaimRun(context.Background(), "cancel")
	if !claimed {
		t.Fatal("could not claim test run")
	}

	runDone := make(chan error, 1)
	go func() {
		_, err := ag.RunInput(WithRunHandle(handle.Context(), handle), UserInput{Text: "initial"}, nil)
		runDone <- err
	}()
	select {
	case <-blocker.entered:
	case <-time.After(2 * time.Second):
		handle.Cancel()
		t.Fatal("tool did not start")
	}
	if !handle.Submit(UserInput{Text: "accepted before stop"}) {
		t.Fatal("steering input was rejected")
	}
	handle.Cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("cancelled RunInput returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled run did not stop")
	}
	handle.Close()

	messages := ag.Messages()
	assistantAt, toolAt, steeringAt := -1, -1, -1
	for i, message := range messages {
		if message.Role == "assistant" && len(message.ToolCalls) == 1 && message.ToolCalls[0].ID == "call-cancel" {
			assistantAt = i
		}
		if message.Role == "tool" && message.ToolCallID == "call-cancel" {
			toolAt = i
		}
		if message.Role == "user" && message.Content == "accepted before stop" {
			steeringAt = i
		}
	}
	if assistantAt < 0 || toolAt != assistantAt+1 || steeringAt != toolAt+1 {
		t.Fatalf("cancelled history broke tool batch or lost accepted input (assistant=%d tool=%d steering=%d): %#v", assistantAt, toolAt, steeringAt, messages)
	}
	if err := manager.SaveHistory("cancel", messages); err != nil {
		t.Fatalf("save cancelled history: %v", err)
	}
	loaded, err := manager.LoadHistory("cancel")
	if err != nil || !messagesContain(loaded, "user", "accepted before stop") {
		t.Fatalf("persisted cancelled history = %#v, err=%v", loaded, err)
	}
}

func TestSteeringDefersToolsThatHaveNotStartedInTheCurrentBatch(t *testing.T) {
	blocker := &inboxBlockingTool{entered: make(chan struct{}), release: make(chan struct{})}
	capture := &loopCaptureTool{result: "should not run"}
	batch := llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
		{ID: "call-slow", Type: "function", Function: llm.FunctionCall{Name: "inbox_block", Arguments: `{}`}},
		{ID: "call-later", Type: "function", Function: llm.FunctionCall{Name: "capture", Arguments: `{}`}},
	}}
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: batch},
		{message: finishMessage("replanned")},
	}}
	ag := newLoopTestAgent(t, client, capture)
	ag.registry.Register(blocker)
	manager := NewSessionManager(t.TempDir())
	handle, claimed := manager.ClaimRun(context.Background(), "deferred")
	if !claimed {
		t.Fatal("could not claim test run")
	}
	done := make(chan error, 1)
	go func() {
		_, err := ag.RunInput(WithRunHandle(handle.Context(), handle), UserInput{Text: "start"}, nil)
		done <- err
	}()
	select {
	case <-blocker.entered:
	case <-time.After(2 * time.Second):
		handle.Cancel()
		t.Fatal("first tool did not start")
	}
	if !handle.Submit(UserInput{Text: "change the plan"}) {
		t.Fatal("steering input was rejected")
	}
	close(blocker.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunInput returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		handle.Cancel()
		t.Fatal("active run did not finish")
	}
	handle.Close()

	if capture.calls != 0 {
		t.Fatalf("tool after steering was executed %d times", capture.calls)
	}
	if len(client.calls) != 2 {
		t.Fatalf("model call count = %d, want 2", len(client.calls))
	}
	messages := client.calls[1].messages
	var sawDeferred, sawCompleted, sawSteering bool
	for _, message := range messages {
		if message.Role == "tool" && message.ToolCallID == "call-later" && strings.Contains(message.Content, "Tool deferred") {
			sawDeferred = true
		}
		if message.Role == "tool" && message.ToolCallID == "call-slow" && strings.Contains(message.Content, "tool finished") {
			sawCompleted = true
		}
		if message.Role == "user" && strings.Contains(message.Content, "change the plan") {
			sawSteering = true
		}
	}
	if !sawDeferred || !sawCompleted || !sawSteering {
		t.Fatalf("next model call missing completed/deferred batch results or steering: %#v", messages)
	}
}

func TestInputDuringFinishResultReopensDecisionBeforeFinalAnswer(t *testing.T) {
	client := &fakeChatClient{steps: []fakeChatStep{
		{message: finishMessage("outdated answer")},
		{message: finishMessage("updated answer")},
	}}
	ag := newLoopTestAgent(t, client, nil)
	handle, _ := NewSessionManager(t.TempDir()).ClaimRun(context.Background(), "finish-update")
	defer handle.Close()
	accepted := false
	ag.Callbacks.ToolResult = func(name, _ string) {
		if name == "finish_task" && !accepted {
			accepted = handle.Submit(UserInput{Text: "include my final correction"})
		}
	}
	result, err := ag.RunInput(WithRunHandle(handle.Context(), handle), UserInput{Text: "start"}, nil)
	if err != nil || result != "updated answer" || !accepted || len(client.calls) != 2 {
		t.Fatalf("finish checkpoint: result=%q error=%v accepted=%v calls=%d", result, err, accepted, len(client.calls))
	}
	for _, message := range ag.Messages() {
		if message.Role == "assistant" && message.Content == "outdated answer" {
			t.Fatal("an obsolete final answer was committed before consuming the update")
		}
	}
	if handle.Submit(UserInput{Text: "after seal"}) {
		t.Fatal("finalized run still accepted steering")
	}
}
