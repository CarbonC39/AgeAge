package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMarkdownToHTMLEscapesCodeAndFormatsCommonMarkdown(t *testing.T) {
	input := "# Title\n\n- **bold**\n- `code`\n\n```\n<script>alert(1)</script>\n```\n[link](https://example.com)"
	got := markdownToHTML(input)
	for _, want := range []string{
		"<h1>Title</h1>",
		"<strong>bold</strong>",
		"<code>code</code>",
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		`<a href="https://example.com">link</a>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !matchOrderedList("12. item") || matchOrderedList("item") {
		t.Fatal("ordered-list detection is incorrect")
	}
}

type testChannel struct {
	name      string
	startErr  error
	mu        sync.Mutex
	stopCalls int
}

func (c *testChannel) Name() string                       { return c.name }
func (c *testChannel) Start(MessageHandler) error         { return c.startErr }
func (c *testChannel) Send(string, string) error          { return nil }
func (c *testChannel) Reply(string, string, string) error { return nil }
func (c *testChannel) Stop() error {
	c.mu.Lock()
	c.stopCalls++
	c.mu.Unlock()
	return nil
}

func TestChannelManagerLifecycle(t *testing.T) {
	manager := NewManager(func(IncomingMessage) string { return "ok" })
	if err := manager.StartAll(); err == nil || !strings.Contains(err.Error(), "no channels") {
		t.Fatalf("empty StartAll error = %v", err)
	}

	channelErr := errors.New("connection failed")
	ch := &testChannel{name: "test", startErr: channelErr}
	manager.Register(ch)
	if manager.ChannelCount() != 1 {
		t.Fatalf("channel count = %d", manager.ChannelCount())
	}
	err := manager.StartAll()
	if err == nil || !strings.Contains(err.Error(), "connection failed") {
		t.Fatalf("StartAll error = %v", err)
	}
	ch.mu.Lock()
	stops := ch.stopCalls
	ch.mu.Unlock()
	if stops != 1 {
		t.Fatalf("Stop calls = %d", stops)
	}
}

func TestChannelManagerHandlesNormalConnectorExit(t *testing.T) {
	manager := NewManager(nil)
	ch := &testChannel{name: "normal"}
	manager.Register(ch)
	if err := manager.StartAll(); err != nil {
		t.Fatalf("normal exit returned error: %v", err)
	}
}

func TestMatrixTypingRefreshesAndStops(t *testing.T) {
	type typingEvent struct{ Typing bool }
	events := make(chan typingEvent, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event typingEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode typing request: %v", err)
			return
		}
		events <- event
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	matrix := NewMatrix(server.URL, "@bot:example.org", "token", nil, nil, ChannelOptions{})
	matrix.client = server.Client()
	matrix.typingRefreshInterval = 10 * time.Millisecond
	if err := matrix.SendTyping("!room:example.org", true); err != nil {
		t.Fatal(err)
	}

	select {
	case event := <-events:
		if !event.Typing {
			t.Fatal("initial typing request was not enabled")
		}
	case <-time.After(time.Second):
		t.Fatal("initial typing request not observed")
	}
	select {
	case event := <-events:
		if !event.Typing {
			t.Fatal("refresh request was not enabled")
		}
	case <-time.After(time.Second):
		t.Fatal("typing refresh was not observed")
	}

	if err := matrix.SendTyping("!room:example.org", false); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Typing {
			t.Fatal("typing stop request was not disabled")
		}
	case <-time.After(time.Second):
		t.Fatal("typing stop request not observed")
	}

	// Stop is idempotent and must cancel any remaining refresh loop.
	if err := matrix.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := matrix.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestMatrixTypingOverlappingRunsReferenceCount(t *testing.T) {
	type typingEvent struct{ Typing bool }
	events := make(chan typingEvent, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event typingEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode typing request: %v", err)
		}
		events <- event
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	matrix := NewMatrix(server.URL, "@bot:example.org", "token", nil, nil, ChannelOptions{})
	matrix.typingRefreshInterval = time.Hour
	if err := matrix.SendTyping("!room:example.org", true); err != nil {
		t.Fatal(err)
	}
	if event := <-events; !event.Typing {
		t.Fatal("initial typing event was false")
	}
	if err := matrix.SendTyping("!room:example.org", true); err != nil {
		t.Fatal(err)
	}
	if err := matrix.SendTyping("!room:example.org", false); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		t.Fatalf("first overlapping close emitted typing=%v", event.Typing)
	case <-time.After(20 * time.Millisecond):
	}
	if err := matrix.SendTyping("!room:example.org", false); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Typing {
			t.Fatal("final overlapping close did not clear typing")
		}
	case <-time.After(time.Second):
		t.Fatal("final typing=false event not observed")
	}
}

func TestMatrixStopClearsActiveTyping(t *testing.T) {
	type typingEvent struct{ Typing bool }
	events := make(chan typingEvent, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event typingEvent
		if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
			t.Errorf("decode typing request: %v", err)
		}
		events <- event
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	matrix := NewMatrix(server.URL, "@bot:example.org", "token", nil, nil, ChannelOptions{})
	matrix.typingRefreshInterval = time.Hour
	if err := matrix.SendTyping("!room:example.org", true); err != nil {
		t.Fatal(err)
	}
	if event := <-events; !event.Typing {
		t.Fatal("initial typing event was false")
	}
	if err := matrix.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-events:
		if event.Typing {
			t.Fatal("Stop did not clear remote typing")
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not send typing=false")
	}
}

func TestMatrixTypingRequestHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	matrix := NewMatrix(server.URL, "@bot:example.org", "token", nil, nil, ChannelOptions{})
	matrix.client = server.Client()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	if _, err := matrix.doRequestContext(ctx, http.MethodPut, "/typing", map[string]bool{"typing": true}); err == nil {
		t.Fatal("expected canceled Matrix request to fail")
	}
}
