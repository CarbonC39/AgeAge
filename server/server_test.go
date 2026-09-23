package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func TestConfiguredAuthenticationAndHealthPolicy(t *testing.T) {
	cfg := config.DefaultConfig().Server
	cfg.APIKey = "test-api-key"
	cfg.HealthAuth = true
	cfg.CORSOrigins = []string{"https://client.example"}
	s := NewServerWithConfig(nil, cfg)

	for _, tc := range []struct {
		name   string
		path   string
		auth   string
		status int
	}{
		{name: "missing api key", path: "/v1/models", status: http.StatusUnauthorized},
		{name: "wrong api key", path: "/v1/models", auth: "Bearer wrong", status: http.StatusUnauthorized},
		{name: "valid api key", path: "/v1/models", auth: "Bearer test-api-key", status: http.StatusOK},
		{name: "health also protected", path: "/health", status: http.StatusUnauthorized},
		{name: "health valid api key", path: "/health", auth: "Bearer test-api-key", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
			}
			if tc.status == http.StatusUnauthorized && !strings.Contains(rec.Body.String(), `"error"`) {
				t.Fatalf("unauthorized response is not OpenAI JSON: %s", rec.Body.String())
			}
		})
	}
	unauthorizedHealth := httptest.NewRecorder()
	healthReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	healthReq.Header.Set("Origin", "https://client.example")
	s.Handler().ServeHTTP(unauthorizedHealth, healthReq)
	if got := unauthorizedHealth.Header().Get("Access-Control-Allow-Origin"); got != "https://client.example" {
		t.Fatalf("authenticated health error origin = %q", got)
	}
}

func TestCORSConfigurationDoesNotEmitWildcardForAuthenticatedAPI(t *testing.T) {
	cfg := config.DefaultConfig().Server
	cfg.APIKey = "secret"
	cfg.CORSOrigins = []string{"https://allowed.example"}
	s := NewServerWithConfig(nil, cfg)

	allowed := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Origin", "https://allowed.example")
	s.Handler().ServeHTTP(allowed, req)
	if got := allowed.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.example" {
		t.Fatalf("allowed origin = %q", got)
	}
	if got := allowed.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Fatal("authenticated CORS response must not use wildcard")
	}
	if got := allowed.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("authenticated CORS credentials header = %q", got)
	}

	disallowed := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodOptions, "/v1/models", nil)
	req.Header.Set("Origin", "https://evil.example")
	s.Handler().ServeHTTP(disallowed, req)
	if disallowed.Code != http.StatusForbidden {
		t.Fatalf("disallowed preflight status = %d", disallowed.Code)
	}

	// Even in compatibility mode, a caller-supplied credential must not be
	// paired with the wildcard response.
	compat := NewServerWithConfig(nil, config.DefaultConfig().Server)
	withCredential := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Authorization", "Bearer caller-supplied")
	compat.Handler().ServeHTTP(withCredential, req)
	if got := withCredential.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Fatal("wildcard CORS must not be emitted with credentials")
	}
	if got := withCredential.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Fatalf("disallowed origin received credentials header %q", got)
	}
}

func TestChatCompletionRequestBodyLimit(t *testing.T) {
	cfg := config.DefaultConfig().Server
	cfg.MaxBodyBytes = 16
	s := NewServerWithConfig(nil, cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("x", 32)))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "request body too large") {
		t.Fatalf("oversized body error = %s", rec.Body.String())
	}
}

func TestConcurrentRequestLimitReleasesAfterCompletion(t *testing.T) {
	cfg := config.DefaultConfig().Server
	cfg.MaxConcurrent = 1
	s := NewServerWithConfig(nil, cfg)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	handler := s.limitConcurrent(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		w.WriteHeader(http.StatusNoContent)
	})

	firstDone := make(chan struct{})
	go func() {
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		close(firstDone)
	}()
	<-entered

	second := httptest.NewRecorder()
	handler(second, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d", second.Code)
	}

	close(release)
	<-firstDone
	third := httptest.NewRecorder()
	handler(third, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	// The test handler blocks every accepted request; release it so the test
	// can verify the token was acquired again and then complete.
	select {
	case <-firstDone:
	default:
	}
	if third.Code != http.StatusNoContent {
		t.Fatalf("third request status = %d", third.Code)
	}
}

func TestConcurrentRequestLimitReleasesAfterPanic(t *testing.T) {
	cfg := config.DefaultConfig().Server
	cfg.MaxConcurrent = 1
	s := NewServerWithConfig(nil, cfg)
	panicking := s.limitConcurrent(func(http.ResponseWriter, *http.Request) {
		panic("test handler failure")
	})
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected handler panic")
			}
		}()
		panicking(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	}()

	released := httptest.NewRecorder()
	s.limitConcurrent(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})(released, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if released.Code != http.StatusNoContent {
		t.Fatalf("request after panic status = %d", released.Code)
	}
}

func TestConcurrentRequestLimitReleasesWhenContextCancels(t *testing.T) {
	cfg := config.DefaultConfig().Server
	cfg.MaxConcurrent = 1
	s := NewServerWithConfig(nil, cfg)
	started := make(chan struct{})
	done := make(chan struct{})
	handler := s.limitConcurrent(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		w.WriteHeader(http.StatusRequestTimeout)
		close(done)
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)
	go handler(httptest.NewRecorder(), req)
	<-started
	cancel()
	<-done

	released := httptest.NewRecorder()
	s.limitConcurrent(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})(released, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if released.Code != http.StatusNoContent {
		t.Fatalf("request after context cancellation status = %d", released.Code)
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
