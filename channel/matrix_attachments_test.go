package channel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMatrixParseIncomingAttachmentAndMention(t *testing.T) {
	m := NewMatrix("https://hs.example", "@bot:example", "secret", nil, []string{"@alice:example"}, ChannelOptions{})
	m.groupRooms["!room:example"] = true
	event := matrixEvent{Type: "m.room.message", Sender: "@alice:example", EventID: "$event", Content: map[string]interface{}{
		"msgtype": "m.image", "body": "photo.png", "url": "mxc://media.example/id123", "info": map[string]interface{}{"mimetype": "image/png", "size": float64(12)},
		"m.mentions": map[string]interface{}{"user_ids": []interface{}{"@bot:example"}},
	}}
	msg, ok := m.parseIncoming("!room:example", event)
	if !ok || !msg.BotMentioned || msg.Text != "photo.png" || len(msg.Attachments) != 1 {
		t.Fatalf("message = %#v, ok=%v", msg, ok)
	}
	att := msg.Attachments[0]
	if att.Filename != "photo.png" || att.MIMEType != "image/png" || att.Size != 12 || att.URL != "mxc://media.example/id123" || att.Open == nil {
		t.Fatalf("attachment = %#v", att)
	}
}

func TestMatrixParseIncomingEncryptedAndAudioMetadata(t *testing.T) {
	m := NewMatrix("https://hs.example", "@bot:example", "token", nil, []string{"@alice:example"}, ChannelOptions{})
	m.groupRooms["!dm:example"] = false
	event := matrixEvent{Type: "m.room.message", Sender: "@alice:example", Content: map[string]interface{}{"msgtype": "m.file", "body": "secret.bin", "file": map[string]interface{}{"url": "mxc://media/id", "key": map[string]interface{}{}}}}
	msg, ok := m.parseIncoming("!dm:example", event)
	if !ok || len(msg.Attachments) != 1 || msg.Attachments[0].Open != nil || !strings.Contains(msg.Attachments[0].UnsupportedReason, "encrypted") {
		t.Fatalf("encrypted attachment = %#v, ok=%v", msg, ok)
	}
	event.Content = map[string]interface{}{"msgtype": "m.audio", "body": "voice.ogg", "url": "mxc://media/id"}
	msg, ok = m.parseIncoming("!dm:example", event)
	if !ok || len(msg.Attachments) != 1 || msg.Attachments[0].Open != nil || !strings.Contains(msg.Attachments[0].UnsupportedReason, "audio/video") {
		t.Fatalf("audio attachment = %#v, ok=%v", msg, ok)
	}
}

func TestMatrixAttachmentCaptionThreadAndFiltering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		rel, _ := body["m.relates_to"].(map[string]any)
		if rel["rel_type"] != "m.thread" || rel["event_id"] != "$root" {
			t.Errorf("thread relation = %#v", rel)
		}
		json.NewEncoder(w).Encode(map[string]string{"event_id": "$reply"})
	}))
	defer server.Close()
	m := NewMatrix(server.URL, "@bot:example", "token", nil, []string{"@alice:example"}, ChannelOptions{})
	m.groupRooms["!room:example"] = true
	event := matrixEvent{Type: "m.room.message", Sender: "@alice:example", EventID: "$event", Content: map[string]interface{}{"msgtype": "m.file", "body": "caption here", "filename": "report.txt", "url": "mxc://media/file", "m.relates_to": map[string]interface{}{"rel_type": "m.thread", "event_id": "$root"}}}
	msg, ok := m.parseIncoming("!room:example", event)
	if !ok || msg.Text != "caption here" || msg.ThreadID != "$root" || msg.Attachments[0].Filename != "report.txt" {
		t.Fatalf("message=%#v ok=%v", msg, ok)
	}
	if err := msg.Respond("reply"); err != nil {
		t.Fatal(err)
	}
	if _, ok = m.parseIncoming("!room:example", matrixEvent{Type: "m.room.message", Sender: "@alice:example", Content: map[string]interface{}{"msgtype": "m.text", "body": "", "url": "mxc://media/id"}}); ok {
		t.Fatal("empty text event accepted")
	}
	event.Content["m.relates_to"] = map[string]interface{}{"rel_type": "m.replace"}
	if _, ok = m.parseIncoming("!room:example", event); ok {
		t.Fatal("edit event accepted")
	}
	event.Content["m.relates_to"] = map[string]interface{}{"rel_type": "m.thread", "event_id": "$root"}
	event.Sender = "@mallory:example"
	if _, ok = m.parseIncoming("!room:example", event); ok {
		t.Fatal("sender outside allowlist accepted")
	}
}

func TestMatrixMediaDownloadAuthAndCrossOriginRedirect(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/_matrix/client/v1/media/download/media.example/id" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, "bytes")
	}))
	defer server.Close()
	m := NewMatrix(server.URL, "@bot:example", "matrix-secret", nil, nil, ChannelOptions{})
	r, err := m.openMatrixMedia(context.Background(), "media.example", "id")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r)
	r.Close()
	if string(data) != "bytes" || gotAuth != "Bearer matrix-secret" {
		t.Fatalf("body=%q auth=%q", data, gotAuth)
	}

	otherHit := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherHit = true }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, http.StatusFound) }))
	defer redirect.Close()
	m = NewMatrix(redirect.URL, "@bot:example", "matrix-secret", nil, nil, ChannelOptions{})
	if _, err = m.openMatrixMedia(context.Background(), "media.example", "id"); err == nil {
		t.Fatal("cross-origin redirect should fail")
	}
	if otherHit {
		t.Fatal("request followed cross-origin redirect")
	}
}
