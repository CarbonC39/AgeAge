package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"ageage/agent"
	"ageage/llm"
)

// writeJSONError writes a properly encoded JSON error response.
func writeJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	body, _ := json.Marshal(map[string]string{"error": msg})
	w.Write(body)
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
type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []llm.Message `json:"messages"`
	Stream   bool          `json:"stream"`
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
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/health", s.handleHealth)

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

	// Extract the last user message as input (multimodal-aware).
	var userInput string
	var userParts []llm.ContentPart
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

	ag := s.factory.CreateAgent(nil, "")

	// Seed agent with previous history, omitting the very last user message and
	// stripping any system message the client sent. AgeAge always rebuilds the
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
	} else if lastUserIdx == 0 {
		// No history, just the user message.
		ag.SetMessages(nil)
	}

	lastMsg := req.Messages[lastUserIdx]
	// Strip image parts if vision is disabled in config.
	if !s.factory.Config.Multimodal.Vision {
		lastMsg = lastMsg.StripImageParts()
	}
	userInput = lastMsg.TextContent()
	if len(lastMsg.Parts) > 0 {
		userParts = lastMsg.Parts
	}

	if userInput == "" {
		writeJSONError(w, "no user message found", http.StatusBadRequest)
		return
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
				Index: 0,
				Message: llm.Message{
					Role:    "assistant",
					Content: result,
				},
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
		http.Error(w, `{"error":"streaming not supported"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	callback := func(token string) {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   "ageage",
			"choices": []map[string]any{
				{
					"index": 0,
					"delta": map[string]any{
						"content": token,
					},
					"finish_reason": nil,
				},
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	_, err := ag.RunWithParts(r.Context(), userInput, userParts, callback)

	if err != nil {
		errBody, _ := json.Marshal(map[string]string{"error": err.Error()})
		fmt.Fprintf(w, "data: %s\n\n", errBody)
		flusher.Flush()
	}

	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
