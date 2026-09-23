package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ageage/agent"
	"ageage/config"
	"ageage/llm"
)

const (
	defaultMaxBodyBytes  int64 = 4 * 1024 * 1024
	defaultMaxConcurrent       = 8
)

// writeJSONError writes a properly encoded JSON error response.
func writeJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    code,
		},
	})
	w.Write(body)
}

// corsMiddleware preserves the historical permissive CORS behavior for callers
// that use the handler directly. Servers created with NewServer use the
// configured CORS policy instead.
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return corsMiddlewareWithConfig(next, nil, false)
}

func corsMiddlewareWithConfig(next http.HandlerFunc, origins []string, authenticated bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := false
		// Do not combine a wildcard origin with a request that carries browser
		// credentials. This also covers clients sending an Authorization header
		// while the server is in its unauthenticated compatibility mode.
		carriesCredentials := authenticated || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != ""
		wildcard := len(origins) == 0 && !carriesCredentials
		if wildcard {
			allowed = true
		} else if origin != "" {
			for _, candidate := range origins {
				// A wildcard is intentionally ignored for authenticated APIs. A
				// credentialed browser request must name an explicit origin.
				if candidate != "*" && candidate == origin {
					allowed = true
					break
				}
			}
		}
		if origin != "" && !wildcard {
			w.Header().Add("Vary", "Origin")
			if !allowed {
				if r.Method == http.MethodOptions {
					writeJSONError(w, "origin is not allowed", http.StatusForbidden)
					return
				}
				// A non-browser client may omit Origin. For a disallowed browser
				// origin, omit CORS headers and let the browser enforce the policy.
				if origin != "" {
					next(w, r)
					return
				}
			}
		}
		if wildcard {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		} else if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			if carriesCredentials {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// Server is the OpenAI-compatible HTTP API server.
type Server struct {
	factory *agent.AgentFactory
	addr    string
	config  config.ServerConfig
	sema    chan struct{}
}

// NewServer creates a new API server.
func NewServer(factory *agent.AgentFactory, host string, port int) *Server {
	cfg := config.ServerConfig{}
	if factory != nil && factory.Config != nil {
		// Reading the complete server section here keeps the existing constructor
		// source-compatible while allowing configured auth and limits to take
		// effect. The host and port arguments remain authoritative for callers
		// that used the old constructor.
		cfg = factory.Config.Server
	}
	cfg.Host = host
	cfg.Port = port
	return NewServerWithConfig(factory, cfg)
}

// NewServerWithConfig creates a server with an explicit HTTP security policy.
// Values omitted from a hand-written config use safe finite defaults.
func NewServerWithConfig(factory *agent.AgentFactory, cfg config.ServerConfig) *Server {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = defaultMaxConcurrent
	}
	return &Server{
		factory: factory,
		addr:    fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		config:  cfg,
		sema:    make(chan struct{}, cfg.MaxConcurrent),
	}
}

// Handler returns the configured HTTP handler. It is useful for embedding the
// API in another server and for testing without binding a TCP listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	api := func(next http.HandlerFunc) http.HandlerFunc {
		h := next
		h = s.limitConcurrent(h)
		h = s.authenticate(h)
		return corsMiddlewareWithConfig(h, s.config.CORSOrigins, s.config.APIKey != "")
	}
	mux.HandleFunc("/v1/chat/completions", api(s.handleChatCompletions))
	mux.HandleFunc("/v1/models", api(s.handleModels))
	if s.config.HealthAuth {
		// Keep CORS outermost so authentication failures carry the same
		// configured CORS headers as successful health responses and /v1 errors.
		health := s.authenticate(s.handleHealth)
		mux.HandleFunc("/health", corsMiddlewareWithConfig(health, s.config.CORSOrigins, s.config.APIKey != ""))
	} else {
		mux.HandleFunc("/health", corsMiddlewareWithConfig(s.handleHealth, s.config.CORSOrigins, false))
	}
	return mux
}

// chatCompletionRequest mirrors the OpenAI request format.
// Unknown fields are accepted and silently ignored for broad client compatibility.
type chatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []llm.Message `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature"`
	MaxTokens   *int          `json:"max_tokens"`
	TopP        *float64      `json:"top_p"`
	N           *int          `json:"n"`
	Stop        any           `json:"stop"`
	User        string        `json:"user"`
}

// chatCompletionResponse mirrors the OpenAI response format.
type chatCompletionResponse struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []responseChoice `json:"choices"`
	Usage   *llm.Usage       `json:"usage,omitempty"`
}

type responseChoice struct {
	Index        int         `json:"index"`
	Message      llm.Message `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// Start starts the HTTP server.
func (s *Server) Start() error {
	srv := &http.Server{
		Addr:         s.addr,
		Handler:      s.Handler(),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10 * time.Minute, // Generous for long agent runs / streaming
		IdleTimeout:  2 * time.Minute,
	}

	fmt.Printf("AgeAge API server listening on %s\n", s.addr)
	return srv.ListenAndServe()
}

func (s *Server) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.config.APIKey == "" || authorizedBearer(r, s.config.APIKey) {
			next(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="ageage"`)
		writeJSONError(w, "authentication required", http.StatusUnauthorized)
	}
}

func authorizedBearer(r *http.Request, expected string) bool {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(value) <= len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return false
	}
	provided := strings.TrimSpace(value[len(prefix):])
	// Hashing first keeps ConstantTimeCompare's input lengths equal, avoiding
	// an early length-based return while comparing keys.
	wantSum := sha256.Sum256([]byte(expected))
	gotSum := sha256.Sum256([]byte(provided))
	return subtleConstantTimeCompare(wantSum[:], gotSum[:])
}

// Kept as a tiny wrapper so the security-sensitive operation is obvious at
// call sites and easy to audit.
func subtleConstantTimeCompare(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

func (s *Server) limitConcurrent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.sema == nil {
			// A zero-value Server is used by direct handler tests and has no
			// configured process-wide limit.
			next(w, r)
			return
		}
		select {
		case s.sema <- struct{}{}:
			defer func() { <-s.sema }()
			next(w, r)
		default:
			writeJSONError(w, "too many concurrent requests", http.StatusTooManyRequests)
		}
	}
}

func (s *Server) maxBodyBytes() int64 {
	if s.config.MaxBodyBytes > 0 {
		return s.config.MaxBodyBytes
	}
	return defaultMaxBodyBytes
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleModels returns a minimal /v1/models list. Many OpenAI-compatible
// clients (SillyTavern, OpenWebUI) call this endpoint before sending a request.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data": []map[string]any{
			{
				"id":       "ageage",
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "ageage",
			},
		},
	})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// MaxBytesReader returns a typed error once the limit is exceeded, allowing
	// callers to distinguish an oversized request from malformed JSON.
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes())
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		writeJSONError(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var req chatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// Find the last user message; everything before it is conversation history.
	lastUserIdx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}

	if lastUserIdx < 0 {
		writeJSONError(w, "no user message found", http.StatusBadRequest)
		return
	}

	lastMsg := req.Messages[lastUserIdx]
	if lastMsg.TextContent() == "" && len(lastMsg.Parts) == 0 {
		writeJSONError(w, "empty user message", http.StatusBadRequest)
		return
	}
	// Strip image parts if vision is disabled in config.
	if !s.factory.Config.Multimodal.Vision {
		lastMsg = lastMsg.StripImageParts()
	}
	userInput := lastMsg.TextContent()
	var userParts []llm.ContentPart
	if len(lastMsg.Parts) > 0 {
		userParts = lastMsg.Parts
	}

	if userInput == "" {
		writeJSONError(w, "empty user message", http.StatusBadRequest)
		return
	}

	ag := s.factory.CreateAgent(nil, "")

	// Seed agent with previous history, omitting the very last user message and
	// stripping any client-provided system message. AgeAge always rebuilds the
	// system prompt via buildSystemPrompt so that SOUL.md, AGENT.md, and context
	// are correctly injected regardless of what the client supplied.
	if lastUserIdx > 0 {
		history := req.Messages[:lastUserIdx]
		filtered := make([]llm.Message, 0, len(history))
		for _, m := range history {
			if m.Role != "system" {
				filtered = append(filtered, m)
			}
		}
		ag.SetMessages(filtered)
	}

	if req.Stream {
		s.handleStream(w, r, ag, userInput, userParts)
		return
	}

	// Non-streaming response.
	result, err := ag.RunWithParts(r.Context(), userInput, userParts, nil)
	if err != nil {
		writeJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := chatCompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "ageage",
		Choices: []responseChoice{
			{
				Index:        0,
				Message:      llm.Message{Role: "assistant", Content: result},
				FinishReason: "stop",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, ag *agent.Agent, userInput string, userParts []llm.ContentPart) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":{"message":"streaming not supported","type":"server_error"}}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	sendChunk := func(delta map[string]any, finishReason any) {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   "ageage",
			"choices": []map[string]any{
				{
					"index":         0,
					"delta":         delta,
					"finish_reason": finishReason,
				},
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// OpenAI streaming protocol: first chunk carries the role, subsequent
	// chunks carry content, final chunk carries finish_reason.
	sendChunk(map[string]any{"role": "assistant", "content": ""}, nil)

	var streamedContent strings.Builder
	callback := func(token string) {
		streamedContent.WriteString(token)
		sendChunk(map[string]any{"content": token}, nil)
	}

	result, err := ag.RunWithParts(r.Context(), userInput, userParts, callback)

	if err != nil {
		// Send error as a final data chunk before DONE.
		errChunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   "ageage",
			"choices": []map[string]any{},
			"error":   map[string]any{"message": err.Error(), "type": "server_error"},
		}
		data, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	} else {
		// finish_task returns the user-facing answer as the Run result, but tool
		// arguments are not content tokens and therefore never reach callback.
		// Pipeline runs and bare-text fallbacks do stream their result, so only
		// append it when it is not already the suffix of streamed content.
		if result != "" && !strings.HasSuffix(streamedContent.String(), result) {
			sendChunk(map[string]any{"content": result}, nil)
		}
		// Final chunk: empty delta + finish_reason.
		sendChunk(map[string]any{}, "stop")
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
