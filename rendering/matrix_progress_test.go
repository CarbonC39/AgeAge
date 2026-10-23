package rendering

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ageage/channel"
	"ageage/config"
	"ageage/conversation"
	"ageage/event"
)

func TestBalancedMatrixProgressEditsOriginalCardInItsThread(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:example/send/m.room.message/") {
			t.Errorf("unexpected Matrix progress route: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests = append(requests, body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"event_id": fmt.Sprintf("$event%d", len(requests))})
	}))
	defer server.Close()
	matrix := channel.NewMatrix(server.URL, "@bot:example", "test-token", nil, nil, channel.ChannelOptions{})
	target := conversation.Target{ChannelType: "matrix", ChannelID: "!room:example", ThreadID: "$thread", ReplyTo: "$user"}
	run := NewRun(channel.NewSurface(matrix, target), config.DefaultNotificationConfig())
	defer run.Close()
	if err := run.Handle(event.Event{Kind: event.RunStarted}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []event.Event{
		{Kind: event.ModelStarted},
		{Kind: event.ToolStarted, Name: "file_read"},
		{Kind: event.ToolCompleted, Name: "file_read"},
	} {
		if err := run.Handle(ev); err != nil {
			t.Fatal(err)
		}
		if err := run.reporter.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Complete("answer", nil); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 5 || run.MessageID() != "$event1" {
		t.Fatalf("status card was not reused for all stages: requests=%#v message=%q", requests, run.MessageID())
	}
	for i, want := range []string{"Thinking", "Running file_read", "Finished file_read", "Completed"} {
		body := requests[i+1]
		rel, _ := body["m.relates_to"].(map[string]any)
		content, _ := body["m.new_content"].(map[string]any)
		thread, _ := content["m.relates_to"].(map[string]any)
		text, _ := content["body"].(string)
		if rel["rel_type"] != "m.replace" || rel["event_id"] != "$event1" || thread["rel_type"] != "m.thread" || thread["event_id"] != "$thread" || !strings.Contains(text, want) {
			t.Fatalf("stage %s did not edit the original thread card: %#v", want, body)
		}
	}
}
