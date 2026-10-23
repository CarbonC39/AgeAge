package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDiscordPollPreservesTextAndAttachments(t *testing.T) {
	var server *httptest.Server
	mediaToken := make(chan string, 1)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/cdn/"):
			mediaToken <- r.Header.Get("Authorization")
			_, _ = io.WriteString(w, "media")
		case strings.HasPrefix(r.URL.Path, "/channels/c/messages"):
			if r.Header.Get("Authorization") != "Bot test-token" {
				t.Errorf("Discord API request missing bot auth")
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "1", "content": "hello", "author": map[string]any{"id": "u", "username": "user"}},
				{"id": "2", "content": "", "author": map[string]any{"id": "u", "username": "user"}, "attachments": []map[string]any{{"filename": "a.png", "content_type": "image/png", "size": 3, "url": server.URL + "/cdn/a"}}},
				{"id": "3", "content": "look", "author": map[string]any{"id": "u", "username": "user"}, "attachments": []map[string]any{
					{"filename": "a.png", "content_type": "image/png", "size": 3, "url": server.URL + "/cdn/a"},
					{"filename": "b.pdf", "content_type": "application/pdf", "size": 5, "url": server.URL + "/cdn/b"},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	d := NewDiscord("test-token", []string{"c"}, nil, ChannelOptions{})
	d.baseURL = server.URL
	d.client = server.Client()
	d.lastMsgIDs["c"] = "0"
	got := make(chan IncomingMessage, 3)
	d.pollChannel("c", func(m IncomingMessage) string { got <- m; return "" })

	messages := make(map[string]IncomingMessage)
	for range 3 {
		select {
		case m := <-got:
			messages[m.ReplyTo] = m
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for polled messages")
		}
	}
	if messages["1"].Text != "hello" || len(messages["1"].Attachments) != 0 {
		t.Fatalf("text message = %#v", messages["1"])
	}
	if messages["2"].Text != "" || len(messages["2"].Attachments) != 1 {
		t.Fatalf("attachment-only message = %#v", messages["2"])
	}
	if messages["3"].Text != "look" || len(messages["3"].Attachments) != 2 {
		t.Fatalf("mixed message = %#v", messages["3"])
	}
	a := messages["3"].Attachments[0]
	if a.Filename != "a.png" || a.MIMEType != "image/png" || a.Size != 3 || a.Open == nil {
		t.Fatalf("attachment metadata = %#v", a)
	}
	stream, err := a.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil || string(data) != "media" {
		t.Fatalf("attachment stream = %q, %v", data, err)
	}
	if auth := <-mediaToken; auth != "" {
		t.Fatalf("CDN request forwarded authorization header %q", auth)
	}
}

func TestDiscordPollThreadMentionAndAllowlistWithAttachment(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/threads"):
			_, _ = io.WriteString(w, `{"id":"thread"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/channels/c/messages"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": "1", "content": "<@bot> hi", "author": map[string]any{"id": "allowed", "username": "ok"},
				"attachments": []map[string]any{{"filename": "x.txt", "url": "https://cdn.example/x"}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	d := NewDiscord("token", []string{"c"}, []string{"allowed"}, ChannelOptions{})
	d.baseURL, d.client, d.botUserID = server.URL, server.Client(), "bot"
	d.groupChannels["c"] = true
	d.lastMsgIDs["c"] = "0"
	got := make(chan IncomingMessage, 1)
	d.pollChannel("c", func(m IncomingMessage) string { got <- m; return "" })
	select {
	case m := <-got:
		if m.Text != "hi" || !m.BotMentioned || m.ThreadID != "thread" || m.ChannelID != "c" || len(m.Attachments) != 1 {
			t.Fatalf("thread/mention message = %#v", m)
		}
	case <-time.After(time.Second):
		t.Fatal("allowed mention was not delivered")
	}

	d.AllowedUsers = []string{"someone-else"}
	d.lastMsgIDs["c"] = "0"
	d.pollChannel("c", func(m IncomingMessage) string { t.Errorf("disallowed user delivered: %#v", m); return "" })
}

func TestDiscordAttachmentRejectsUnsafeURLWithoutLeakingURL(t *testing.T) {
	d := NewDiscord("token", nil, nil, ChannelOptions{})
	for _, unsafe := range []string{"file:///secret?sig=hidden", "https://user:password@cdn.example/file?sig=hidden"} {
		a := d.incomingAttachments([]discordAttachment{{Filename: "x", URL: unsafe}})[0]
		if a.Open != nil || a.UnsupportedReason == "" || strings.Contains(a.UnsupportedReason, "hidden") || strings.Contains(a.UnsupportedReason, "password") {
			t.Fatalf("unsafe URL attachment = %#v", a)
		}
	}
}

func TestDiscordMalformedAttachmentMetadataDoesNotDropMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channels/c/messages" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `[{"id":"1","content":"caption","author":{"id":"u","username":"user"},"attachments":[{"filename":"broken.txt","size":"bad","url":"https://cdn.example/broken"},{"filename":"good.txt","content_type":"text/plain","size":4,"url":"https://cdn.example/good"}]}]`)
	}))
	defer server.Close()

	d := NewDiscord("token", []string{"c"}, nil, ChannelOptions{})
	d.baseURL, d.client = server.URL, server.Client()
	d.lastMsgIDs["c"] = "0"
	got := make(chan IncomingMessage, 1)
	d.pollChannel("c", func(m IncomingMessage) string { got <- m; return "" })
	select {
	case m := <-got:
		if m.Text != "caption" || len(m.Attachments) != 2 {
			t.Fatalf("message lost caption or attachments: %#v", m)
		}
		if m.Attachments[0].UnsupportedReason == "" || m.Attachments[0].Open != nil {
			t.Fatalf("malformed attachment not isolated: %#v", m.Attachments[0])
		}
		if m.Attachments[1].Filename != "good.txt" || m.Attachments[1].UnsupportedReason != "" || m.Attachments[1].Open == nil {
			t.Fatalf("valid sibling attachment lost: %#v", m.Attachments[1])
		}
	case <-time.After(time.Second):
		t.Fatal("message with malformed attachment was not delivered")
	}
}
