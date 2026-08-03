package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ageage/agent"
	"ageage/llm"
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

// corsMiddleware adds permissive CORS headers required by browser-based clients
// (SillyTavern, OpenWebUI, etc.).
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
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
}

// NewServer creates a new API server.
func NewServer(factory *agent.AgentFactory, host string, port int) *Server {
	return &Server{
		factory: factory,
		addr:    fmt.Sprintf("%s:%d", host, port),
	}
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
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", corsMiddleware(s.handleChatCompletions))
	mux.HandleFunc("/v1/models", corsMiddleware(s.handleModels))
	mux.HandleFunc("/health", corsMiddleware(s.handleHealth))

	srv := &http.Server{
		Addr:         s.addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 10 * time.Minute, // Generous for long agent runs / streaming
		IdleTimeout:  2 * time.Minute,
	}

	fmt.Printf("AgeAge API server listening on %s\n", s.addr)
	return srv.ListenAndServe()
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
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
