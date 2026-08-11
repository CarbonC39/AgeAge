package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMessageJSONRoundTripWithContentParts(t *testing.T) {
	original := Message{
		Role: "user",
		Parts: []ContentPart{
			{Type: "text", Text: "describe"},
			{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64,abc", Detail: "high"}},
		},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.TextContent() != "describe" || len(got.Parts) != 2 {
		t.Fatalf("round trip = %#v", got)
	}
	stripped := got.StripImageParts()
	if strings.Contains(stringMustJSON(t, stripped), "image_url") || !strings.Contains(stripped.Content, "removed") {
		t.Fatalf("stripped message = %#v", stripped)
	}
}

func stringMustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPrepareMessagesStripsReasoningContent(t *testing.T) {
	client := NewClient("", "http://example.invalid", "model", false, 0)
	got := client.prepareMessages([]Message{{
		Role:             "assistant",
		Content:          "answer",
		ReasoningContent: "private reasoning",
	}})
	if len(got) != 1 || got[0].ReasoningContent != "" || got[0].Content != "answer" {
		t.Fatalf("prepared messages = %#v", got)
	}
}

func TestPrepareMessagesDropsDegenerateAssistantMessages(t *testing.T) {
	client := NewClient("", "http://example.invalid", "model", false, 0)
	messages := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "question"},
		{Role: "assistant", Content: "valid answer"},
		{Role: "assistant"}, // degenerate: neither content nor tool_calls
		{Role: "user", Content: "next"},
	}
	got := client.prepareMessages(messages)
	if len(got) != 4 {
		t.Fatalf("prepared messages = %#v, want 4", got)
	}
	for _, m := range got {
		if m.Role == "assistant" && m.Content == "" && len(m.ToolCalls) == 0 {
			t.Fatalf("degenerate assistant message survived: %#v", got)
		}
	}
	if got[2].Content != "valid answer" {
		t.Fatalf("valid assistant message was altered: %#v", got)
	}
}

func TestPrepareMessagesKeepsEmptyToolCallMessage(t *testing.T) {
	client := NewClient("", "http://example.invalid", "model", false, 0)
	msg := Message{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			ID:       "call-1",
			Type:     "function",
			Function: FunctionCall{Name: "bash", Arguments: `{"command":"ls"}`},
		}},
	}
	got := client.prepareMessages([]Message{msg})
	if len(got) != 1 || len(got[0].ToolCalls) != 1 {
		t.Fatalf("tool-call assistant message was dropped: %#v", got)
	}
}

func TestChatCompletionStreamAssemblesContentAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var request ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if !request.Stream || request.Model != "test-model" {
			t.Errorf("request = %#v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"index":0,"delta":{"content":"hel"},"finish_reason":null}]}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "data: [DONE]")
		fmt.Fprintln(w)
	}))
	defer server.Close()

	client := NewClient("", server.URL, "test-model", false, 0)
	client.http.Timeout = 2 * time.Second
	var streamed strings.Builder
	message, usage, err := client.ChatCompletionStream(
		context.Background(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		0,
		func(token string) { streamed.WriteString(token) },
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "hello" || streamed.String() != "hello" {
		t.Fatalf("message=%q callback=%q", message.Content, streamed.String())
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestChatCompletionStreamAssemblesToolCallAndFiresOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"capture","arguments":"{\"x\":"}}]},"finish_reason":null}]}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":null}]}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "data: [DONE]")
	}))
	defer server.Close()

	client := NewClient("", server.URL, "model", false, 0)
	callbackCalls := 0
	var callbackCall ToolCall
	message, _, err := client.ChatCompletionStream(
		context.Background(), []Message{{Role: "user", Content: "call"}}, nil, 0, nil,
		func(_ int, call ToolCall) {
			callbackCalls++
			callbackCall = call
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if callbackCalls != 1 || callbackCall.Function.Arguments != `{"x":1}` {
		t.Fatalf("callback calls=%d call=%#v", callbackCalls, callbackCall)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "capture" || message.ToolCalls[0].Function.Arguments != `{"x":1}` {
		t.Fatalf("assembled tool calls = %#v", message.ToolCalls)
	}
}

func TestChatCompletionStreamReturnsScannerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ")
		fmt.Fprintln(w, strings.Repeat("x", 4*1024*1024+1))
	}))
	defer server.Close()

	client := NewClient("", server.URL, "model", false, 0)
	_, _, err := client.ChatCompletionStream(
		context.Background(), []Message{{Role: "user", Content: "large"}}, nil, 0, nil, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "read streaming response") {
		t.Fatalf("scanner error = %v", err)
	}
}

func TestIsJSONComplete(t *testing.T) {
	for _, complete := range []string{`{}`, `{"a":[1,2]}`, `{"brace":"}"}`} {
		if !isJSONComplete(complete) {
			t.Errorf("expected complete: %s", complete)
		}
	}
	for _, incomplete := range []string{"", `{`, `{"a":`} {
		if isJSONComplete(incomplete) {
			t.Errorf("expected incomplete: %s", incomplete)
		}
	}
}
