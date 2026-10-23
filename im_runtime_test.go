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
	"strings"
	"sync"
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
	mu        sync.Mutex
	threads   int
	questions []string
	replies   []string
	sends     []string
	edits     []string
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
func (c *runtimeTestChannel) ReplyMessage(_ conversation.Target, text string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.replies = append(c.replies, text)
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
