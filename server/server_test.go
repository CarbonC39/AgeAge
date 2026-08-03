package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ageage/agent"
	"ageage/config"
	"ageage/llm"
	"ageage/tools"
)

func TestHealthModelsAndCORS(t *testing.T) {
	server := &Server{}

	health := httptest.NewRecorder()
	corsMiddleware(server.handleHealth)(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK || health.Header().Get("Access-Control-Allow-Origin") != "*" || !strings.Contains(health.Body.String(), `"status":"ok"`) {
		t.Fatalf("health response: code=%d headers=%v body=%s", health.Code, health.Header(), health.Body.String())
	}

	models := httptest.NewRecorder()
	server.handleModels(models, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), `"id":"ageage"`) {
		t.Fatalf("models response: code=%d body=%s", models.Code, models.Body.String())
	}

	options := httptest.NewRecorder()
	called := false
	corsMiddleware(func(http.ResponseWriter, *http.Request) { called = true })(
		options, httptest.NewRequest(http.MethodOptions, "/health", nil),
	)
	if options.Code != http.StatusNoContent || called {
		t.Fatalf("OPTIONS response code=%d called=%v", options.Code, called)
	}
}

func TestChatCompletionRejectsInvalidRequestsWithoutFactory(t *testing.T) {
	server := &Server{}
	tests := []struct {
		name   string
		method string
		body   string
		code   int
		want   string
	}{
		{"method", http.MethodGet, "", http.StatusMethodNotAllowed, "method not allowed"},
		{"json", http.MethodPost, "{", http.StatusBadRequest, "invalid JSON"},
		{"no user", http.MethodPost, `{"messages":[{"role":"assistant","content":"x"}]}`, http.StatusBadRequest, "no user message"},
		{"empty user", http.MethodPost, `{"messages":[{"role":"user","content":""}]}`, http.StatusBadRequest, "empty user message"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tt.method, "/v1/chat/completions", strings.NewReader(tt.body))
			server.handleChatCompletions(recorder, request)
			if recorder.Code != tt.code || !strings.Contains(recorder.Body.String(), tt.want) {
				t.Fatalf("response code=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("error response is not JSON: %v", err)
			}
		})
	}
}

func TestWriteJSONErrorShape(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeJSONError(recorder, "bad input", http.StatusUnprocessableEntity)
	if recorder.Code != http.StatusUnprocessableEntity || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("error response code=%d headers=%v", recorder.Code, recorder.Header())
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Message != "bad input" || body.Error.Code != http.StatusUnprocessableEntity {
		t.Fatalf("error body = %#v", body)
	}
}

func TestStreamIncludesFinishTaskResultExactlyOnce(t *testing.T) {
	tests := []struct {
		name            string
		streamedContent string
	}{
		{name: "tool arguments are not content tokens"},
		{name: "already streamed result is not duplicated", streamedContent: "final answer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				arguments, err := json.Marshal(map[string]string{
					"status":  "success",
					"summary": "final answer",
				})
				if err != nil {
					t.Error(err)
					return
				}
				chunk := map[string]any{
					"choices": []map[string]any{{
						"index": 0,
						"delta": map[string]any{
							"content": tt.streamedContent,
							"tool_calls": []map[string]any{{
								"index": 0,
								"id":    "call-finish",
								"type":  "function",
								"function": map[string]any{
									"name":      "finish_task",
									"arguments": string(arguments),
								},
							}},
						},
						"finish_reason": nil,
					}},
				}
				data, err := json.Marshal(chunk)
				if err != nil {
					t.Error(err)
					return
				}
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
			}))
			defer upstream.Close()

			cfg := config.DefaultConfig()
			cfg.Workspace = t.TempDir()
			cfg.WorkDir = cfg.Workspace
			client := llm.NewClient("", upstream.URL, "test-model", false, 0)
			registry := tools.NewRegistry()
			finishTool := &tools.FinishTool{}
			registry.Register(finishTool)
			ag := agent.NewAgent(cfg, client, registry, finishTool, nil, false)
			ag.Mode.InjectContext = false

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			(&Server{}).handleStream(recorder, request, ag, "hello", nil)

			body := recorder.Body.String()
			if recorder.Header().Get("Content-Type") != "text/event-stream" {
				t.Fatalf("content type = %q", recorder.Header().Get("Content-Type"))
			}
			if got := strings.Count(body, `"content":"final answer"`); got != 1 {
				t.Fatalf("final answer chunks = %d, body=%s", got, body)
			}
			if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
				t.Fatalf("incomplete stream: %s", body)
			}
		})
	}
}
