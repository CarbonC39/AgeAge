package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
