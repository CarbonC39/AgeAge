package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"ageage/llm"
)

// RunHandle owns admission to one session run. Only its runner touches history;
// transports may reserve and prepare inputs without acquiring the history lock.
type RunHandle struct {
	mu          sync.Mutex
	notifyMu    sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	queue       []*inboxEntry
	sealed      bool
	closed      bool
	done        chan struct{}
	observer    func(int)
	notified    bool
	lastPending int
	release     func()
}

type inboxEntry struct {
	ready chan struct{}
	input UserInput
}

var activeSessions = struct {
	sync.Mutex
	runs map[string]*RunHandle
}{runs: make(map[string]*RunHandle)}

// ClaimRun atomically chooses the owner before any normalization or history I/O.
// An existing handle stays registered through persistence and transaction release.
func (sm *SessionManager) ClaimRun(ctx context.Context, id string) (*RunHandle, bool) {
	key, _ := filepath.Abs(sm.SessionDir(id))
	activeSessions.Lock()
	defer activeSessions.Unlock()
	if h := activeSessions.runs[key]; h != nil {
		return h, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	h := &RunHandle{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	h.release = func() {
		activeSessions.Lock()
		defer activeSessions.Unlock()
		if activeSessions.runs[key] == h {
			delete(activeSessions.runs, key)
		}
	}
	activeSessions.runs[key] = h
	return h, true
}

func (sm *SessionManager) ActiveRun(id string) *RunHandle {
	key, _ := filepath.Abs(sm.SessionDir(id))
	activeSessions.Lock()
	defer activeSessions.Unlock()
	return activeSessions.runs[key]
}

func (h *RunHandle) Context() context.Context { return h.ctx }
func (h *RunHandle) Done() <-chan struct{}    { return h.done }
func (h *RunHandle) Cancel()                  { h.cancel() }

// Prepare reserves an ordered slot before performing attachment conversion.
// A checkpoint waits for these reservations, so neither finalization nor tmp GC
// can race with a download whose message has already been accepted.
func (h *RunHandle) Prepare(prepare func(context.Context) UserInput) bool {
	h.mu.Lock()
	if h.sealed || h.closed {
		h.mu.Unlock()
		return false
	}
	entry := &inboxEntry{ready: make(chan struct{})}
	h.queue = append(h.queue, entry)
	h.mu.Unlock()
	value := prepare(h.ctx)
	entry.input = cloneRunInput(value)
	close(entry.ready)
	h.notifyPending()
	return true
}

func cloneRunInput(value UserInput) UserInput {
	value.Parts = append([]llm.ContentPart(nil), value.Parts...)
	for i := range value.Parts {
		if value.Parts[i].ImageURL != nil {
			part := *value.Parts[i].ImageURL
			value.Parts[i].ImageURL = &part
		}
	}
	value.Warnings = append([]string(nil), value.Warnings...)
	return value
}

func (h *RunHandle) Submit(value UserInput) bool {
	return h.Prepare(func(context.Context) UserInput { return value })
}

func (h *RunHandle) Pending() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.queue)
}

func (h *RunHandle) SetObserver(observer func(int)) {
	h.mu.Lock()
	h.observer = observer
	h.mu.Unlock()
	h.notifyPending()
}

// Read the latest count while serializing notifications. Concurrent producers
// cannot overwrite a newer renderer snapshot with an older pending count.
func (h *RunHandle) notifyPending() {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()
	h.mu.Lock()
	n, observer := len(h.queue), h.observer
	if observer == nil || !h.notified && n == 0 || h.notified && h.lastPending == n {
		h.mu.Unlock()
		return
	}
	h.notified, h.lastPending = true, n
	h.mu.Unlock()
	observer(n)
}

// Drain is runner-only. Even on cancellation, accepted preparations complete
// before history is saved. Their contexts have already been cancelled.
func (h *RunHandle) Drain(_ context.Context) []UserInput {
	h.mu.Lock()
	entries := append([]*inboxEntry(nil), h.queue...)
	h.mu.Unlock()
	if len(entries) == 0 {
		return nil
	}
	values := make([]UserInput, 0, len(entries))
	for _, entry := range entries {
		<-entry.ready
		values = append(values, entry.input)
	}
	h.mu.Lock()
	h.queue = h.queue[len(entries):]
	h.mu.Unlock()
	h.notifyPending()
	return values
}

// TryFinish makes the final empty-inbox check atomic with stopping admission.
// Rejected submissions wait for Done and may then claim a new run.
func (h *RunHandle) TryFinish() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.queue) != 0 {
		return false
	}
	h.sealed = true
	return true
}

// Close must be called by the owner after saving history and releasing its
// session transaction. It also covers failures before the Agent starts.
func (h *RunHandle) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed, h.sealed = true, true
	h.observer = nil
	h.mu.Unlock()
	h.cancel()
	h.release()
	close(h.done)
}

type runHandleContextKey struct{}

var errRunSteering = errors.New("execution paused for new user input")

// Nested agents and pipeline nodes yield to their owning run; they must never
// drain its inbox into an isolated conversation or execute an obsolete plan.
func steeringPending(ctx context.Context) bool {
	h := runHandleFromContext(ctx)
	return h != nil && h.Pending() > 0
}

func (a *Agent) yieldForSteering() error {
	progress, _ := json.Marshal(a.conv.ToolHistory())
	return fmt.Errorf("%w; completed tool calls (do not repeat): %s", errRunSteering, progress)
}

func WithRunHandle(ctx context.Context, h *RunHandle) context.Context {
	return context.WithValue(ctx, runHandleContextKey{}, h)
}

func runHandleFromContext(ctx context.Context) *RunHandle {
	h, _ := ctx.Value(runHandleContextKey{}).(*RunHandle)
	return h
}

func (a *Agent) executionContext() context.Context {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	if a.activeContext != nil {
		return a.activeContext
	}
	return context.Background()
}

func (a *Agent) consumeInbox(ctx context.Context) int {
	if a.inbox == nil {
		return 0
	}
	values := a.inbox.Drain(ctx)
	for _, value := range values {
		a.conv.Append(llm.Message{Role: "user", Content: value.Text, Parts: value.Parts})
	}
	if len(values) > 0 {
		a.steeringVersion++
		a.finishTool.Reset()
		a.hintOnNextCall = "The user supplied updates during this run. Reconsider the remaining plan using the latest user messages before continuing. Tools with deferred results have not executed."
	}
	return len(values)
}
