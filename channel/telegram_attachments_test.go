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

func TestTelegramParseIncomingChoosesLargestPhotoAndPreservesTopic(t *testing.T) {
	tg := NewTelegram("TOKEN", []string{"42"}, ChannelOptions{})
	tg.botUsername = "mybot"
	msg := &tgMessage{MessageID: 7, Caption: "@mybot inspect", Chat: tgChat{ID: -10, Type: "supergroup"}, From: tgUser{ID: 42, FirstName: "Alice"}, MessageThreadID: 99,
		Photo: []tgPhotoSize{{FileID: "large-byte-small", Width: 20, Height: 20, FileSize: 900}, {FileID: "largest-area", Width: 80, Height: 60, FileSize: 2}},
	}
	in, ok := tg.parseIncoming(msg)
	if !ok || in.Text != "inspect" || !in.BotMentioned || in.ThreadID != "99" || in.ChannelID != "-10" || len(in.Attachments) != 1 {
		t.Fatalf("message=%#v ok=%v", in, ok)
	}
	if in.Attachments[0].Filename != "photo.jpg" || in.Attachments[0].Size != 2 || in.Attachments[0].Open == nil {
		t.Fatalf("attachment=%#v", in.Attachments[0])
	}
}

func TestTelegramTopicRespondUsesTopic(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTEST/sendMessage" {
			t.Errorf("path=%q", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 8}})
	}))
	defer server.Close()
	tg := NewTelegram("TEST", nil, ChannelOptions{})
	tg.baseURL, tg.client = server.URL+"/botTEST", server.Client()
	in, ok := tg.parseIncoming(&tgMessage{MessageID: 7, Text: "hi", Chat: tgChat{ID: 9, Type: "private"}, From: tgUser{ID: 2}, MessageThreadID: 66})
	if !ok {
		t.Fatal("topic message was rejected")
	}
	if err := in.Respond("hello"); err != nil {
		t.Fatal(err)
	}
	replyParams, _ := got["reply_parameters"].(map[string]any)
	if got["message_thread_id"] != float64(66) || replyParams["message_id"] != float64(7) {
		t.Fatalf("send payload=%#v", got)
	}
}

func TestTelegramMalformedMediaMetadataDoesNotDiscardUpdate(t *testing.T) {
	var update tgUpdate
	raw := `{"update_id":1,"message":{"message_id":2,"caption":"keep caption","chat":{"id":3,"type":"private"},"from":{"id":4},"photo":[{"file_id":"broken","width":20,"height":20,"file_size":"oops"},{"file_id":"valid","width":50,"height":50,"file_size":10}],"document":{"file_id":"doc","file_name":"bad.bin","file_size":"oops"}}}`
	if err := json.Unmarshal([]byte(raw), &update); err != nil {
		t.Fatalf("malformed media metadata rejected whole update: %v", err)
	}
	in, ok := NewTelegram("TOKEN", nil, ChannelOptions{}).parseIncoming(update.Message)
	if !ok || in.Text != "keep caption" || len(in.Attachments) != 2 {
		t.Fatalf("message=%#v ok=%v", in, ok)
	}
	if in.Attachments[0].Open == nil || !strings.Contains(in.Attachments[1].UnsupportedReason, "file_size") {
		t.Fatalf("attachments=%#v", in.Attachments)
	}
}

func TestTelegramDocumentLazyDownloadUsesGetFileAndFileRoute(t *testing.T) {
	var gotAPI, gotFile string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/botTEST/getFile":
			gotAPI = r.URL.Query().Get("file_id")
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": "documents/report.pdf"}})
		case "/file/botTEST/documents/report.pdf":
			gotFile = r.URL.Path
			io.WriteString(w, "pdf-data")
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	tg := NewTelegram("TEST", nil, ChannelOptions{})
	tg.baseURL = server.URL + "/botTEST"
	tg.client = server.Client()
	msg := &tgMessage{From: tgUser{ID: 1}, Chat: tgChat{ID: 2, Type: "private"}, Document: &tgDocument{FileID: "file-1", FileName: "report.pdf", MIMEType: "application/pdf", FileSize: 8}}
	in, ok := tg.parseIncoming(msg)
	if !ok || in.Text != "" || len(in.Attachments) != 1 {
		t.Fatalf("message=%#v ok=%v", in, ok)
	}
	r, err := in.Attachments[0].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(body) != "pdf-data" || gotAPI != "file-1" || gotFile != "/file/botTEST/documents/report.pdf" {
		t.Fatalf("body=%q api=%q file=%q err=%v", body, gotAPI, gotFile, err)
	}
}

func TestTelegramAllowlistAndNoMediaFiltering(t *testing.T) {
	tg := NewTelegram("TOKEN", []string{"99"}, ChannelOptions{})
	msg := &tgMessage{From: tgUser{ID: 42}, Chat: tgChat{ID: -3, Type: "group"}, Photo: []tgPhotoSize{{FileID: "p"}}}
	if _, ok := tg.parseIncoming(msg); ok {
		t.Fatal("unauthorized group sender was accepted")
	}
	tg.AllowedUsers = nil
	if _, ok := tg.parseIncoming(msg); ok {
		t.Fatal("group message passed without allowlist")
	}
	msg.Chat.Type = "private"
	if got, ok := tg.parseIncoming(msg); !ok || len(got.Attachments) != 1 {
		t.Fatalf("private media message=%#v ok=%v", got, ok)
	}
}
