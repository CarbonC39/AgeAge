package channel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"ageage/conversation"
)

func TestMatrixSurfaceThreadCreationReplyEditAndLink(t *testing.T) {
	var bodies []map[string]any
	var routes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes = append(routes, r.Method+" "+r.URL.Path)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"event_id": "$event" + string(rune('0'+len(bodies)))})
	}))
	defer server.Close()
	mx := NewMatrix(server.URL, "@bot:example", "token", nil, nil, ChannelOptions{})
	room := conversation.Target{ChannelType: "matrix", ChannelID: "!room:example"}
	root, rootRef, err := NewSurface(mx, room).OpenThread("Topic", "Details")
	if err != nil {
		t.Fatal(err)
	}
	if root.Target().ThreadID != "$event1" || rootRef.MessageID != "$event1" || !strings.Contains(bodies[0]["body"].(string), "Topic") || !strings.Contains(bodies[0]["body"].(string), "Details") {
		t.Fatalf("root=%#v ref=%#v body=%#v", root.Target(), rootRef, bodies[0])
	}
	if _, err = root.Send("in thread"); err != nil {
		t.Fatal(err)
	}
	threadRel, _ := bodies[1]["m.relates_to"].(map[string]any)
	if threadRel["rel_type"] != "m.thread" || threadRel["event_id"] != "$event1" {
		t.Fatalf("thread relation=%#v", threadRel)
	}

	replySurface := NewSurface(mx, conversation.Target{ChannelType: "matrix", ChannelID: "!room:example", ThreadID: "$event1", ReplyTo: "$other"})
	if _, err = replySurface.Reply("reply"); err != nil {
		t.Fatal(err)
	}
	rel, _ := bodies[2]["m.relates_to"].(map[string]any)
	anchor, _ := rel["m.in_reply_to"].(map[string]any)
	if rel["rel_type"] != "m.thread" || rel["event_id"] != "$event1" || anchor["event_id"] != "$other" {
		t.Fatalf("reply relations=%#v", rel)
	}

	ref := conversation.Reference{Target: root.Target(), MessageID: "$event2"}
	if _, err = root.Update(ref, "updated"); err != nil {
		t.Fatal(err)
	}
	editRel, _ := bodies[3]["m.relates_to"].(map[string]any)
	newContent, _ := bodies[3]["m.new_content"].(map[string]any)
	newRel, _ := newContent["m.relates_to"].(map[string]any)
	if editRel["rel_type"] != "m.replace" || editRel["event_id"] != "$event2" || newContent["body"] != "updated" || newRel["rel_type"] != "m.thread" || newRel["event_id"] != "$event1" {
		t.Fatalf("edit payload=%#v", bodies[3])
	}
	if got := root.Link(rootRef); got != "https://matrix.to/#/%21room:example/$event1" {
		t.Fatalf("Matrix link=%q", got)
	}
	if _, err = root.Update(conversation.Reference{Target: room, MessageID: "$outside"}, "bad"); err != ErrWrongSurface {
		t.Fatalf("cross-surface update error=%v", err)
	}
	if err := root.Read(); err != nil {
		t.Fatal(err)
	}
	if bodies[4]["thread_id"] != "$event1" {
		t.Fatalf("thread read receipt body=%#v", bodies[4])
	}
	reactionRef, err := root.React(rootRef, "✅")
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Unreact(reactionRef); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(routes[len(routes)-3], "/receipt/m.read/$event1") || !strings.Contains(routes[len(routes)-2], "/send/m.reaction/") || !strings.Contains(routes[len(routes)-1], "/redact/") {
		t.Fatalf("surface routes=%#v", routes)
	}
}

func TestSurfaceReferenceCannotCrossOwnerBinding(t *testing.T) {
	target := conversation.Target{ChannelType: "discord", ChannelID: "room", ThreadID: "thread", OwnerID: "owner-a"}
	surface := NewSurface(&testChannel{name: "discord"}, target)
	ref := conversation.Reference{Target: conversation.Target{ChannelType: "discord", ChannelID: "room", ThreadID: "thread", OwnerID: "owner-b"}, MessageID: "message"}
	if _, err := surface.Update(ref, "replacement"); err != ErrWrongSurface {
		t.Fatalf("cross-owner Update error = %v, want %v", err, ErrWrongSurface)
	}
}

func TestDiscordDMSurfaceDoesNotAdvertiseOrAttemptThreadCreation(t *testing.T) {
	d := NewDiscord("token", nil, nil, ChannelOptions{})
	d.groupChannels["dm"] = false
	target := conversation.Target{ChannelType: "discord", ChannelID: "dm"}
	surface := NewSurface(d, target)
	if surface.Capabilities().CreateThread {
		t.Fatal("DM surface advertises thread creation")
	}
	if _, _, err := surface.OpenThread("title", "body"); err != ErrUnsupported {
		t.Fatalf("DM OpenThread error = %v, want %v", err, ErrUnsupported)
	}
}

func TestTelegramSurfaceAskRoutesInlineKeyboardToTopic(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/botTEST/sendMessage" {
			t.Errorf("path=%q", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&payload)
		io.WriteString(w, `{"ok":true,"result":{"message_id":2}}`)
	}))
	defer server.Close()
	tg := NewTelegram("TEST", nil, ChannelOptions{})
	tg.baseURL, tg.client = server.URL+"/botTEST", server.Client()
	target := conversation.Target{ChannelType: "telegram", ChannelID: "-100123", ThreadID: "44"}
	if err := NewSurface(tg, target).Ask("Choose", []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	if payload["message_thread_id"] != float64(44) || payload["chat_id"] != "-100123" {
		t.Fatalf("topic question payload=%#v", payload)
	}
	if got := payload["reply_markup"].(map[string]any)["inline_keyboard"].([]any); len(got) != 2 {
		t.Fatalf("buttons=%#v", got)
	}
	ref := conversation.Reference{Target: target, MessageID: "55"}
	if got := tg.ConversationLink(ref); got != "https://t.me/c/123/44/55" {
		t.Fatalf("Telegram link=%q", got)
	}
	if got := tg.ConversationLink(conversation.Reference{Target: conversation.Target{ChannelType: "telegram", ChannelID: "42"}, MessageID: "55"}); got != "" {
		t.Fatalf("non-determinate Telegram link=%q", got)
	}
	if _, _, err := NewSurface(tg, target).OpenThread("nested", "body"); err != ErrUnsupported {
		t.Fatalf("Telegram OpenThread error=%v", err)
	}
}

func TestDiscordSurfaceCreatesAndRoutesThread(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/channels/parent/messages":
			io.WriteString(w, `{"id":"root"}`)
		case "/channels/parent/messages/root/threads":
			io.WriteString(w, `{"id":"thread"}`)
		case "/channels/thread/messages":
			io.WriteString(w, `{"id":"reply"}`)
		case "/channels/thread/messages/old":
			if r.Method != http.MethodPatch {
				t.Errorf("edit method=%s", r.Method)
			}
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected Discord request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dc := NewDiscord("token", nil, nil, ChannelOptions{})
	dc.baseURL, dc.client = server.URL, server.Client()
	dc.guildIDs["parent"] = "guild"
	root := NewSurface(dc, conversation.Target{ChannelType: "discord", ChannelID: "parent"})
	thread, ref, err := root.OpenThread("Topic", "Details")
	if err != nil {
		t.Fatal(err)
	}
	if thread.Target().ThreadID != "thread" || thread.Target().ReplyTo != "root" || ref.MessageID != "root" {
		t.Fatalf("thread=%#v ref=%#v", thread.Target(), ref)
	}
	if _, err = thread.Send("reply"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"POST /channels/parent/messages", "POST /channels/parent/messages/root/threads", "POST /channels/thread/messages"}) {
		t.Fatalf("Discord routes=%#v", paths)
	}
	if got := thread.Link(conversation.Reference{Target: thread.Target(), MessageID: "reply"}); got != "https://discord.com/channels/guild/thread/reply" {
		t.Fatalf("Discord link=%q", got)
	}
	if got := thread.Link(ref); got != "https://discord.com/channels/guild/parent/root" {
		t.Fatalf("Discord root link=%q", got)
	}
	if _, err := thread.Update(conversation.Reference{Target: thread.Target(), MessageID: "old"}, "short update"); err != nil {
		t.Fatal(err)
	}
	if got := thread.Link(conversation.Reference{Target: thread.Target(), MessageID: "old"}); got != "https://discord.com/channels/guild/thread/old" {
		t.Fatalf("Discord thread link=%q", got)
	}
}

func TestIncomingMessageTargetFallsBackToLegacyFields(t *testing.T) {
	msg := IncomingMessage{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread", ReplyTo: "event", SenderID: "alice"}
	if got := msg.Target(); got.ChannelType != "matrix" || got.ChannelID != "room" || got.ThreadID != "thread" || got.ReplyTo != "event" || got.SenderID != "alice" {
		t.Fatalf("target=%#v", got)
	}
	msg.Conversation = conversation.Target{ChannelType: "matrix", ChannelID: "room", ThreadID: "normalized", SenderID: "bob"}
	if got := msg.Target(); got.ThreadID != "normalized" || got.ReplyTo != "event" || got.SenderID != "bob" {
		t.Fatalf("merged target=%#v", got)
	}
}

func TestSurfaceLongUpdatesFallbackToChunkedSameSurfaceSend(t *testing.T) {
	t.Run("matrix thread", func(t *testing.T) {
		var got []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			got = append(got, body["body"].(string))
			json.NewEncoder(w).Encode(map[string]string{"event_id": "$split" + string(rune('0'+len(got)))})
		}))
		defer server.Close()
		mx := NewMatrix(server.URL, "@bot:example", "token", nil, nil, ChannelOptions{})
		target := conversation.Target{ChannelType: "matrix", ChannelID: "!room:example", ThreadID: "$root", ReplyTo: "$latest"}
		s := NewSurface(mx, target)
		text := strings.Repeat("x", 4001)
		ref, err := s.Update(conversation.Reference{Target: target, MessageID: "$old"}, text)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0]+got[1] != text || ref.MessageID != "$split2" || ref.Target.ThreadID != "$root" {
			t.Fatalf("chunks=%d ref=%#v", len(got), ref)
		}
	})

	t.Run("discord thread", func(t *testing.T) {
		var got []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("unexpected method %s", r.Method)
			}
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			got = append(got, body["content"])
			json.NewEncoder(w).Encode(map[string]string{"id": "msg" + string(rune('0'+len(got)))})
		}))
		defer server.Close()
		dc := NewDiscord("token", nil, nil, ChannelOptions{})
		dc.baseURL, dc.client = server.URL, server.Client()
		target := conversation.Target{ChannelType: "discord", ChannelID: "parent", ThreadID: "thread"}
		s := NewSurface(dc, target)
		text := strings.Repeat("y", 1899) + "漢🙂" + strings.Repeat("z", 100)
		ref, err := s.Update(conversation.Reference{Target: target, MessageID: "old"}, text)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0]+got[1] != text || ref.Target.ThreadID != "thread" || !utf8.ValidString(got[0]) || !utf8.ValidString(got[1]) || strings.Contains(strings.Join(got, ""), "�") {
			t.Fatalf("chunks=%d ref=%#v", len(got), ref)
		}
	})
}

func TestMatrixThreadSurfaceUsesRoomIDForRoomScopedOperations(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if strings.Contains(r.URL.Path, "/send/m.reaction/") {
			io.WriteString(w, `{"event_id":"$reaction"}`)
			return
		}
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	mx := NewMatrix(server.URL, "@bot:example", "token", nil, nil, ChannelOptions{})
	target := conversation.Target{ChannelType: "matrix", ChannelID: "!room:example", ThreadID: "$root", ReplyTo: "$reply"}
	s := NewSurface(mx, target)
	if err := s.Typing(true); err != nil {
		t.Fatal(err)
	}
	if err := s.Typing(false); err != nil {
		t.Fatal(err)
	}
	reaction, err := s.React(conversation.Reference{Target: target, MessageID: "$message"}, "✅")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Unreact(reaction); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(conversation.Reference{Target: target, MessageID: "$message"}, "edited"); err != nil {
		t.Fatal(err)
	}
	for _, route := range paths {
		if strings.Contains(route, "/rooms//") || strings.Contains(route, "/channels/$root") {
			t.Fatalf("thread operation used wrong delivery ID: %#v", paths)
		}
	}
	joined := strings.Join(paths, "\n")
	for _, want := range []string{"/rooms/!room:example/typing/", "/rooms/!room:example/send/m.reaction/", "/rooms/!room:example/redact/", "/rooms/!room:example/send/m.room.message/"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing route %q in %s", want, joined)
		}
	}
}

func TestTelegramThreadSurfaceUsesChatIDForChatScopedOperations(t *testing.T) {
	payloads := make(chan map[string]any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		payloads <- payload
		io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer server.Close()
	tg := NewTelegram("TEST", nil, ChannelOptions{})
	tg.baseURL, tg.client = server.URL+"/botTEST", server.Client()
	target := conversation.Target{ChannelType: "telegram", ChannelID: "-100123", ThreadID: "44", ReplyTo: "55"}
	s := NewSurface(tg, target)
	if err := s.Typing(true); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-payloads:
		if payload["chat_id"] != "-100123" {
			t.Fatalf("typing payload=%#v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("typing action did not arrive")
	}
	if err := s.Typing(false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(conversation.Reference{Target: target, MessageID: "66"}, "edited"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.React(conversation.Reference{Target: target, MessageID: "66"}, "✅"); err != nil {
		t.Fatal(err)
	}
	if err := s.Unreact(conversation.Reference{Target: target, MessageID: "-100123|66|✅"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		select {
		case payload := <-payloads:
			if payload["chat_id"] != "-100123" {
				t.Fatalf("thread operation used wrong chat ID: %#v", payload)
			}
		case <-time.After(time.Second):
			t.Fatal("expected topic surface operation payload")
		}
	}
}
