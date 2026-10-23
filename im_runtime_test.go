package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ageage/agent"
	"ageage/attachment"
	"ageage/channel"
	"ageage/config"
	"ageage/conversation"
	"ageage/llm"
	"ageage/tools"
)

// runtimeTestChannel records semantic channel operations without a real IM.
type runtimeTestChannel struct {
	mu           sync.Mutex
	threads      int
	questions    []string
	replies      []string
	replyTargets []conversation.Target
	sends        []string
	edits        []string
}

func (*runtimeTestChannel) Name() string                       { return config.ChannelMatrix }
func (*runtimeTestChannel) Start(channel.MessageHandler) error { return nil }
func (*runtimeTestChannel) Stop() error                        { return nil }
func (c *runtimeTestChannel) Send(_, text string) error {
	c.mu.Lock()
	c.sends = append(c.sends, text)
	c.mu.Unlock()
	return nil
}
func (c *runtimeTestChannel) Reply(_, _, text string) error {
	c.mu.Lock()
	c.replies = append(c.replies, text)
	c.mu.Unlock()
	return nil
}
func (c *runtimeTestChannel) SendMessage(_, text string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, text)
	return fmt.Sprintf("msg-%d", len(c.sends)), nil
}
func (c *runtimeTestChannel) EditMessage(_, _, text string) error {
	c.mu.Lock()
	c.edits = append(c.edits, text)
	c.mu.Unlock()
	return nil
}
func (c *runtimeTestChannel) SendMessageInThread(_, _, _, text string) (string, error) {
	return c.SendMessage("", text)
}
func (c *runtimeTestChannel) ReplyMessage(target conversation.Target, text string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replies = append(c.replies, text)
	c.replyTargets = append(c.replyTargets, target)
	return "reply", nil
}
func (c *runtimeTestChannel) AskInConversation(_ conversation.Target, question string, _ []string) error {
	c.mu.Lock()
	c.questions = append(c.questions, question)
	c.mu.Unlock()
	return nil
}
func (c *runtimeTestChannel) CreateConversationThread(target conversation.Target, title, body string) (conversation.Target, conversation.Reference, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.threads++
	t := target.Room()
	t.ThreadID = fmt.Sprintf("sibling-%d", c.threads)
	return t, conversation.Reference{Target: t, MessageID: "thread-root"}, nil
}
func (*runtimeTestChannel) ConversationLink(ref conversation.Reference) string {
	return "https://chat.invalid/" + ref.Target.ThreadID
}

func TestIMRuntimeAttachmentAnswersPendingAskAndSessionCommandsStayResponsive(t *testing.T) {
	secondRequest := make(chan map[string]json.RawMessage, 1)
	releaseSecond := make(chan struct{})
	var callsMu sync.Mutex
	calls := 0
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read model request: %v", err)
			return
		}
		var req struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err = json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode model request: %v", err)
			return
		}
		callsMu.Lock()
		calls++
		n := calls
		callsMu.Unlock()
		if n == 2 {
			var captured map[string]json.RawMessage
			_ = json.Unmarshal(body, &captured)
			secondRequest <- captured
			select {
			case <-releaseSecond:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"ask-1","type":"function","function":{"name":"ask_user","arguments":"{\"question\":\"Please send a screenshot\"}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"finish-1","type":"function","function":{"name":"finish_task","arguments":"{\"status\":\"success\",\"summary\":\"Screenshot received\"}"}}]},"finish_reason":"tool_calls"}]}`)
		}
	}))
	defer llmServer.Close()

	work := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Workspace = work
	cfg.WorkDir = work
	cfg.Agent.Mode = config.AgentModeFull
	cfg.Agent.Tools = []string{"ask_user"}
	cfg.Router.Enabled = false
	cfg.MCP.Enabled = false
	cfg.Notifications.Preset = config.ProgressPresetQuiet
	factory := &agent.AgentFactory{Config: cfg, LLMClient: llm.NewClient("test-key", llmServer.URL, "test-model", false, 0), UserInputMgr: tools.NewUserInputManager()}
	ch := &runtimeTestChannel{}
	runtime := newIMRuntime(factory, map[string]channel.Channel{config.ChannelMatrix: ch})
	target := conversation.Target{ChannelType: config.ChannelMatrix, ChannelID: "room-1", ThreadID: "original-thread", ReplyTo: "root-event", SenderID: "user-1"}
	initialDone := make(chan string, 1)
	go func() {
		initialDone <- runtime.Handler(channel.IncomingMessage{Conversation: target, Text: "Inspect this screenshot"})
	}()
	waitFor(t, 5*time.Second, func() bool { ch.mu.Lock(); defer ch.mu.Unlock(); return len(ch.questions) == 1 })
	oldID := runtime.Sessions.Current(target)
	if oldID == "" {
		t.Fatal("original thread has no session binding")
	}

	// List and create a sibling session while the original agent owns its session transaction.
	listDone := make(chan string, 1)
	go func() {
		listDone <- runtime.Handler(channel.IncomingMessage{Conversation: target, Text: "!session list"})
	}()
	select {
	case <-listDone:
	case <-time.After(2 * time.Second):
		t.Fatal("session list blocked behind the pending ask_user transaction")
	}
	if !factory.UserInputMgr.HasPendingScope(interactionScope(target, oldID)) {
		t.Fatal("ask_user request is not registered in the owning session scope after session list")
	}
	newDone := make(chan string, 1)
	go func() {
		newDone <- runtime.Handler(channel.IncomingMessage{Conversation: target, Text: "!session new 截图 检查"})
	}()
	select {
	case <-newDone:
	case <-time.After(2 * time.Second):
		t.Fatal("session new blocked behind pending run")
	}
	if got := runtime.Sessions.Current(target); got != oldID {
		t.Fatalf("original thread binding changed: got %q want %q", got, oldID)
	}
	child := conversation.Target{ChannelType: target.ChannelType, ChannelID: target.ChannelID, ThreadID: "sibling-1"}
	if childID := runtime.Sessions.Current(child); childID == "" || childID == oldID {
		t.Fatalf("sibling thread session = %q, original = %q", childID, oldID)
	}

	image := []byte("\x89PNG\r\n\x1a\nimage-data")
	attachmentValue := attachment.Attachment{Filename: "screen.png", MIMEType: "image/png", Size: int64(len(image)), Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(image)), nil }}
	answerDone := make(chan string, 1)
	go func() {
		answerDone <- runtime.Handler(channel.IncomingMessage{Conversation: target, Text: "Here is the error screen", Attachments: []attachment.Attachment{attachmentValue}})
	}()
	select {
	case <-answerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("attachment answer waited on the running chat lock")
	}
	var captured map[string]json.RawMessage
	select {
	case captured = <-secondRequest:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not receive follow-up request after attachment answer")
	}
	close(releaseSecond)
	select {
	case <-initialDone:
	case <-time.After(5 * time.Second):
		t.Fatal("agent run did not complete")
	}
	if !bytes.Contains(captured["messages"], []byte(base64.StdEncoding.EncodeToString(image))) {
		t.Fatal("follow-up LLM request did not contain the screenshot")
	}
	if !bytes.Contains(captured["messages"], []byte("Here is the error screen")) {
		t.Fatal("follow-up LLM request lost the attachment caption")
	}
	if got := bytes.Count(captured["messages"], []byte("data:image/png;base64,")); got != 1 {
		t.Fatalf("follow-up request has %d screenshot image parts, want one", got)
	}

	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.sends) != 1 || !strings.Contains(ch.sends[0], "Screenshot received") {
		t.Fatalf("final result sends = %#v, want one completion", ch.sends)
	}
	if len(ch.replies) < 2 {
		t.Fatalf("expected list/new/attachment acknowledgements, replies=%#v", ch.replies)
	}
}

func TestIMRuntimeSteeringInputJoinsActiveRunAndPersistsAfterToolBatch(t *testing.T) {
	firstRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondRequest := make(chan []json.RawMessage, 1)
	var callsMu sync.Mutex
	calls := 0
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read model request: %v", err)
			return
		}
		callsMu.Lock()
		calls++
		n := calls
		callsMu.Unlock()
		if n == 1 {
			close(firstRequest)
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
				return
			}
		} else if n == 2 {
			var req struct {
				Messages []json.RawMessage `json:"messages"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("decode follow-up request: %v", err)
			}
			secondRequest <- req.Messages
		}
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"read-1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"details.txt\"}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"finish-1","type":"function","function":{"name":"finish_task","arguments":"{\"status\":\"success\",\"summary\":\"Updated answer\"}"}}]},"finish_reason":"tool_calls"}]}`)
		}
	}))
	defer llmServer.Close()

	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "details.txt"), []byte("existing detail"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Workspace, cfg.WorkDir = work, work
	cfg.Agent.Mode = config.AgentModeFull
	cfg.Agent.Tools = []string{"file_read"}
	cfg.Router.Enabled = false
	cfg.MCP.Enabled = false
	cfg.Notifications.Preset = config.ProgressPresetQuiet
	factory := &agent.AgentFactory{Config: cfg, LLMClient: llm.NewClient("test-key", llmServer.URL, "test-model", false, 0), UserInputMgr: tools.NewUserInputManager()}
	ch := &runtimeTestChannel{}
	runtime := newIMRuntime(factory, map[string]channel.Channel{config.ChannelMatrix: ch})
	target := conversation.Target{ChannelType: config.ChannelMatrix, ChannelID: "steering-room", SenderID: "user-1"}
	initialDone := make(chan string, 1)
	go func() {
		initialDone <- runtime.Handler(channel.IncomingMessage{Conversation: target, Text: "Read the existing details"})
	}()
	select {
	case <-firstRequest:
	case <-time.After(5 * time.Second):
		t.Fatal("initial model request did not start")
	}

	image := []byte("\x89PNG\r\n\x1a\nsteering-image")
	attachmentValue := attachment.Attachment{Filename: "reference.png", MIMEType: "image/png", Size: int64(len(image)), Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(image)), nil }}
	steeringDone := make(chan string, 1)
	go func() {
		steeringDone <- runtime.Handler(channel.IncomingMessage{Conversation: target, Text: "Also use this reference", Attachments: []attachment.Attachment{attachmentValue}})
	}()
	select {
	case <-steeringDone:
	case <-time.After(2 * time.Second):
		t.Fatal("steering input waited behind the active run")
	}
	close(releaseFirst)

	var messages []json.RawMessage
	select {
	case messages = <-secondRequest:
	case <-time.After(5 * time.Second):
		t.Fatal("active run did not make the follow-up model request")
	}
	select {
	case <-initialDone:
	case <-time.After(5 * time.Second):
		t.Fatal("active run did not complete")
	}

	assistantBatch, toolResult, steering := -1, -1, -1
	for i, raw := range messages {
		var msg struct {
			Role       string            `json:"role"`
			Content    json.RawMessage   `json:"content"`
			ToolCallID string            `json:"tool_call_id"`
			ToolCalls  []json.RawMessage `json:"tool_calls"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatalf("decode message %d: %v", i, err)
		}
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			assistantBatch = i
		}
		if msg.Role == "tool" && msg.ToolCallID == "read-1" {
			toolResult = i
		}
		if msg.Role == "user" && bytes.Contains(raw, []byte("Also use this reference")) {
			steering = i
		}
	}
	if assistantBatch < 0 || toolResult != assistantBatch+1 || steering <= toolResult {
		t.Fatalf("tool batch and steering order is invalid: assistant=%d result=%d steering=%d messages=%s", assistantBatch, toolResult, steering, marshalMessages(messages))
	}
	if !bytes.Contains(messages[steering], []byte(base64.StdEncoding.EncodeToString(image))) {
		t.Fatal("steering image did not reach the next model request")
	}
	if count := bytes.Count(messages[steering], []byte("data:image/png;base64,")); count != 1 {
		t.Fatalf("steering image parts = %d, want exactly one", count)
	}
	ch.mu.Lock()
	var gotWarning bool
	for _, reply := range ch.replies {
		if strings.Contains(reply, "⚠️") {
			gotWarning = true
		}
	}
	ch.mu.Unlock()
	if gotWarning {
		t.Fatal("successful steering sent an unnecessary warning reply")
	}
	sessionID := runtime.Sessions.Current(target)
	history, err := agent.NewSessionManager(cfg.AgeAgeDirPath()).LoadHistory(sessionID)
	if err != nil {
		t.Fatalf("load persisted history: %v", err)
	}
	var persistedSteering bool
	for _, message := range history {
		if message.Role == "user" && strings.Contains(message.TextContent(), "Also use this reference") {
			persistedSteering = true
		}
	}
	if !persistedSteering {
		t.Fatal("steering message was not persisted in session history")
	}
}

func TestIMRuntimeActiveSteeringAttachmentWarningsAreRepliedAndRetained(t *testing.T) {
	openCount := &atomic.Int32{}
	tests := []struct {
		name       string
		attachment attachment.Attachment
		configure  func(*config.Config)
		warning    string
	}{
		{
			name: "oversize",
			attachment: attachment.Attachment{Filename: "large.pdf", MIMEType: "application/pdf", Size: 128, Open: func(context.Context) (io.ReadCloser, error) {
				openCount.Add(1)
				return io.NopCloser(strings.NewReader("unused")), nil
			}},
			configure: func(cfg *config.Config) { cfg.Multimodal.MaxAttachmentBytes = 1 },
			warning:   "too large",
		},
		{
			name: "converter failure",
			attachment: attachment.Attachment{Filename: "broken.pdf", MIMEType: "application/pdf", Size: 9, Open: func(context.Context) (io.ReadCloser, error) {
				openCount.Add(1)
				return io.NopCloser(strings.NewReader("%PDF-1.4\n")), nil
			}},
			configure: func(cfg *config.Config) {
				cfg.Multimodal.Converters = []config.ConverterConfig{{Extensions: []string{"pdf"}, Command: "false {input} {output}"}}
			},
			warning: "conversion failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			openCount.Store(0)
			firstRequest, releaseFirst := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
			secondRequest := make(chan []json.RawMessage, 1)
			var calls atomic.Int32
			llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				n := calls.Add(1)
				if n == 1 {
					close(firstRequest)
					select {
					case <-releaseFirst:
					case <-r.Context().Done():
						return
					}
				} else if n == 2 {
					var req struct {
						Messages []json.RawMessage `json:"messages"`
					}
					_ = json.Unmarshal(body, &req)
					secondRequest <- req.Messages
				}
				w.Header().Set("Content-Type", "application/json")
				if n == 1 {
					_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"read-1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"details.txt\"}"}}]},"finish_reason":"tool_calls"}]}`)
				} else {
					_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"finish-1","type":"function","function":{"name":"finish_task","arguments":"{\"status\":\"success\",\"summary\":\"Done\"}"}}]},"finish_reason":"tool_calls"}]}`)
				}
			}))
			defer func() {
				release()
				llmServer.Close()
			}()
			work := t.TempDir()
			_ = os.WriteFile(filepath.Join(work, "details.txt"), []byte("detail"), 0o600)
			cfg := config.DefaultConfig()
			cfg.Workspace, cfg.WorkDir = work, work
			cfg.Agent.Mode, cfg.Agent.Tools = config.AgentModeFull, []string{"file_read"}
			cfg.Router.Enabled, cfg.MCP.Enabled = false, false
			cfg.Notifications.Preset = config.ProgressPresetQuiet
			tt.configure(cfg)
			factory := &agent.AgentFactory{Config: cfg, LLMClient: llm.NewClient("test-key", llmServer.URL, "test-model", false, 0), UserInputMgr: tools.NewUserInputManager()}
			ch := &runtimeTestChannel{}
			runtime := newIMRuntime(factory, map[string]channel.Channel{config.ChannelMatrix: ch})
			target := conversation.Target{ChannelType: config.ChannelMatrix, ChannelID: "warning-room", SenderID: "user-1", ThreadID: "thread-1"}
			initialDone := make(chan string, 1)
			go func() {
				initialDone <- runtime.Handler(channel.IncomingMessage{Conversation: target, Text: "Read details"})
			}()
			select {
			case <-firstRequest:
			case <-time.After(5 * time.Second):
				t.Fatal("initial model request did not start")
			}
			steeringDone := make(chan string, 1)
			go func() {
				steeringDone <- runtime.Handler(channel.IncomingMessage{Conversation: target, Text: "Use this file", Attachments: []attachment.Attachment{tt.attachment}})
			}()
			select {
			case fallback := <-steeringDone:
				if fallback != "" {
					t.Fatalf("warning reply fallback = %q", fallback)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("active steering waited for the run")
			}
			release()
			var messages []json.RawMessage
			select {
			case messages = <-secondRequest:
			case <-time.After(5 * time.Second):
				t.Fatal("active run did not make follow-up model request")
			}
			select {
			case <-initialDone:
			case <-time.After(5 * time.Second):
				t.Fatal("active run did not complete")
			}
			payload := marshalMessages(messages)
			if !strings.Contains(payload, tt.warning) || !strings.Contains(payload, "Use this file") {
				t.Fatalf("follow-up model request missing steering warning/text: %s", payload)
			}
			ch.mu.Lock()
			gotWarning := false
			for _, reply := range ch.replies {
				gotWarning = gotWarning || strings.Contains(reply, tt.warning)
			}
			ch.mu.Unlock()
			if !gotWarning {
				t.Fatalf("Surface.Reply did not deliver warning %q", tt.warning)
			}
			if tt.name == "oversize" && openCount.Load() != 0 {
				t.Fatalf("oversized attachment opened %d times, want no download", openCount.Load())
			}
			if tt.name == "converter failure" && openCount.Load() != 1 {
				t.Fatalf("converter failure opened %d times, want exactly one download", openCount.Load())
			}
			ch.mu.Lock()
			defer ch.mu.Unlock()
			if len(ch.replyTargets) == 0 || ch.replyTargets[0].ChannelID != target.ChannelID || ch.replyTargets[0].ThreadID != target.ThreadID {
				t.Fatalf("warning reply target = %#v, want original room/thread %#v", ch.replyTargets, target)
			}
		})
	}
}

func TestIMRuntimeStopCancelsActiveRunWithoutStartingAnotherTurn(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(requestStarted)
		select {
		case <-r.Context().Done():
		case <-releaseHandler:
		}
	}))
	t.Cleanup(func() {
		close(releaseHandler)
		llmServer.Close()
	})
	work := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Workspace, cfg.WorkDir = work, work
	cfg.Agent.Mode = config.AgentModeFull
	cfg.Router.Enabled = false
	cfg.MCP.Enabled = false
	cfg.Notifications.Preset = config.ProgressPresetQuiet
	factory := &agent.AgentFactory{Config: cfg, LLMClient: llm.NewClient("test-key", llmServer.URL, "test-model", false, 0), UserInputMgr: tools.NewUserInputManager()}
	runtime := newIMRuntime(factory, map[string]channel.Channel{config.ChannelMatrix: &runtimeTestChannel{}})
	target := conversation.Target{ChannelType: config.ChannelMatrix, ChannelID: "stop-room", SenderID: "user-1"}
	initialDone := make(chan string, 1)
	go func() {
		initialDone <- runtime.Handler(channel.IncomingMessage{Conversation: target, Text: "Start a slow task"})
	}()
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("initial model request did not start")
	}
	stopDone := make(chan string, 1)
	go func() { stopDone <- runtime.Handler(channel.IncomingMessage{Conversation: target, Text: "!stop"}) }()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stop command blocked behind active run")
	}
	select {
	case <-initialDone:
	case <-time.After(5 * time.Second):
		t.Fatal("active run did not stop")
	}
	if active := agent.NewSessionManager(cfg.AgeAgeDirPath()).ActiveRun(runtime.Sessions.Current(target)); active != nil {
		t.Fatal("stopped run remained registered after persistence")
	}
}

func TestIMRuntimePreservesStartupInputsWhenHistoryCannotLoad(t *testing.T) {
	work := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Workspace, cfg.WorkDir = work, work
	cfg.Agent.Mode = config.AgentModeFull
	cfg.Router.Enabled = false
	cfg.MCP.Enabled = false
	cfg.Notifications.Preset = config.ProgressPresetQuiet
	factory := &agent.AgentFactory{Config: cfg, LLMClient: llm.NewClient("test-key", "http://127.0.0.1:1", "test-model", false, 0), UserInputMgr: tools.NewUserInputManager()}
	ch := &runtimeTestChannel{}
	runtime := newIMRuntime(factory, map[string]channel.Channel{config.ChannelMatrix: ch})
	target := conversation.Target{ChannelType: config.ChannelMatrix, ChannelID: "startup-error-room", SenderID: "user-1"}
	sessionID, err := runtime.Sessions.Resolve(target)
	if err != nil {
		t.Fatal(err)
	}
	store := agent.NewSessionManager(cfg.AgeAgeDirPath())
	if err := os.Remove(store.HistoryPath(sessionID)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.HistoryPath(sessionID), 0o700); err != nil {
		t.Fatal(err)
	}

	openStarted := make(chan struct{})
	releaseOpen := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseOpen) }) }
	t.Cleanup(release)
	image := []byte("\x89PNG\r\n\x1a\nfailed-inbox-image")
	attachmentValue := attachment.Attachment{Filename: "initial.png", MIMEType: "image/png", Size: int64(len(image)), Open: func(context.Context) (io.ReadCloser, error) {
		close(openStarted)
		<-releaseOpen
		return io.NopCloser(bytes.NewReader(image)), nil
	}}
	initialDone := make(chan string, 1)
	go func() {
		initialDone <- runtime.Handler(channel.IncomingMessage{Conversation: target, Text: "Initial request", Attachments: []attachment.Attachment{attachmentValue}})
	}()
	select {
	case <-openStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("initial attachment normalization did not start")
	}
	steeringDone := make(chan string, 1)
	go func() {
		steeringDone <- runtime.Handler(channel.IncomingMessage{Conversation: target, Text: "Queued while starting"})
	}()
	select {
	case <-steeringDone:
	case <-time.After(2 * time.Second):
		t.Fatal("startup steering waited behind initial normalization")
	}
	release()
	select {
	case <-initialDone:
	case <-time.After(5 * time.Second):
		t.Fatal("startup failure did not finish")
	}

	data, err := os.ReadFile(filepath.Join(store.SessionDir(sessionID), failedInboxFilename))
	if err != nil {
		t.Fatalf("failed inbox was not saved: %v", err)
	}
	var preserved []agent.UserInput
	if err := json.Unmarshal(data, &preserved); err != nil {
		t.Fatal(err)
	}
	if len(preserved) != 2 || preserved[0].Text != "Initial request" || preserved[1].Text != "Queued while starting" {
		t.Fatalf("preserved startup inputs = %#v", preserved)
	}
	foundImage := false
	for _, part := range preserved[0].Parts {
		foundImage = foundImage || part.ImageURL != nil
	}
	if !foundImage {
		t.Fatalf("initial image was not preserved: %#v", preserved[0].Parts)
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if len(ch.replies) == 0 || !strings.Contains(ch.replies[len(ch.replies)-1], "1 queued update(s) were preserved") {
		t.Fatalf("startup failure did not explain preserved updates: %#v", ch.replies)
	}
}

func marshalMessages(messages []json.RawMessage) string {
	data, _ := json.Marshal(messages)
	return string(data)
}

func waitFor(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true before timeout")
}
