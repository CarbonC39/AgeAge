package channel

import (
	"errors"
	"strings"
	"sync"
	"testing"
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
