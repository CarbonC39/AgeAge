package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"ageage/jsonutil"
)

// ContentPart is a single element of a multimodal content array.
type ContentPart struct {
	Type     string        `json:"type"`                // "text" or "image_url"
	Text     string        `json:"text,omitempty"`      // populated when Type == "text"
	ImageURL *ImageURLPart `json:"image_url,omitempty"` // populated when Type == "image_url"
}

// ImageURLPart carries the URL (or base64 data URI) and optional detail level.
type ImageURLPart struct {
	URL    string `json:"url"`              // "data:image/jpeg;base64,..." or https URL
	Detail string `json:"detail,omitempty"` // "auto" | "low" | "high"
}

// Message represents a chat message in the OpenAI format.
// Content holds plain text for text-only messages.
// Parts holds structured content parts for multimodal messages; when set,
// it takes precedence over Content during JSON serialization.
type Message struct {
	Role       string        // message role: "system" | "user" | "assistant" | "tool"
	Content    string        // plain-text content (always valid as a fallback)
	Parts      []ContentPart // multimodal content parts; non-nil overrides Content in JSON
	ToolCalls  []ToolCall
	ToolCallID string
	// ReasoningContent captures thinking-model output (e.g. Gemini's reasoning_content).
	// Always stripped before any outbound request.
	ReasoningContent string
}

// TextContent returns the text portion of the message regardless of form.
// For Parts messages it concatenates all text-type parts; otherwise returns Content.
func (m *Message) TextContent() string {
	if len(m.Parts) == 0 {
		return m.Content
	}
	var sb strings.Builder
	for _, p := range m.Parts {
		if p.Type == "text" && p.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// StripImageParts returns a copy of the message with image_url parts removed.
// A placeholder text part is added if images were present.
func (m Message) StripImageParts() Message {
	if len(m.Parts) == 0 {
		return m
	}
	var kept []ContentPart
	stripped := 0
	for _, p := range m.Parts {
		if p.Type == "image_url" {
			stripped++
		} else {
			kept = append(kept, p)
		}
	}
	if stripped > 0 {
		kept = append(kept, ContentPart{Type: "text", Text: fmt.Sprintf("[%d image attachment(s) removed — vision not enabled]", stripped)})
	}
	m.Parts = kept
	m.Content = m.TextContent()
	return m
}

// messageWire is used for custom JSON marshaling to avoid infinite recursion.
type messageWire struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

// MarshalJSON emits content as a JSON array when Parts is set, or as a string otherwise.
func (m Message) MarshalJSON() ([]byte, error) {
	w := messageWire{
		Role:             m.Role,
		ToolCalls:        m.ToolCalls,
		ToolCallID:       m.ToolCallID,
		ReasoningContent: m.ReasoningContent,
	}
	if len(m.Parts) > 0 {
		b, err := json.Marshal(m.Parts)
		if err != nil {
			return nil, err
		}
		w.Content = b
	} else if m.Content != "" || m.ToolCallID != "" {
		b, err := json.Marshal(m.Content)
		if err != nil {
			return nil, err
		}
		w.Content = b
	}
	return json.Marshal(w)
}

// UnmarshalJSON accepts content as either a JSON string or an array of ContentParts.
func (m *Message) UnmarshalJSON(data []byte) error {
	var w messageWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	m.Role = w.Role
	m.ToolCalls = w.ToolCalls
	m.ToolCallID = w.ToolCallID
	m.ReasoningContent = w.ReasoningContent

	if len(w.Content) == 0 || string(w.Content) == "null" {
		return nil
	}
	// Try string first (the common case).
	var s string
	if err := json.Unmarshal(w.Content, &s); err == nil {
		m.Content = s
		return nil
	}
	// Try content parts array.
	var parts []ContentPart
	if err := json.Unmarshal(w.Content, &parts); err == nil {
		m.Parts = parts
		m.Content = m.TextContent() // keep Content populated for backward-compat readers
		return nil
	}
	return fmt.Errorf("message content is neither a string nor a content-part array")
}

// ToolCallGoogleExtra holds Gemini-specific metadata on a tool call.
type ToolCallGoogleExtra struct {
	ThoughtSignature string `json:"thought_signature,omitempty"`
}

// ToolCallExtraContent is the extra_content envelope used by Gemini's
// OpenAI-compat layer to carry thought signatures.
type ToolCallExtraContent struct {
	Google *ToolCallGoogleExtra `json:"google,omitempty"`
}

// ToolCall represents a function call in the model response.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
	// ExtraContent carries Gemini thought signatures (extra_content.google.thought_signature).
	// It is preserved verbatim when the message is echoed back in history.
	ExtraContent *ToolCallExtraContent `json:"extra_content,omitempty"`
}

// FunctionCall represents the function name and arguments.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef defines a tool/function for the API request.
type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef is the function definition within a tool.
type FunctionDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// ResponseFormat requests a specific output format from the model.
type ResponseFormat struct {
	Type string `json:"type"` // "json_object" | "text"
}

// ChatRequest is the request body for /v1/chat/completions.
type ChatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Tools          []ToolDef       `json:"tools,omitempty"`
	Temperature    float64         `json:"temperature,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// ChatResponse is the response from /v1/chat/completions (non-streaming).
type ChatResponse struct {
	ID      string   `json:"id"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice represents a single choice in the response.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage represents token usage info.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamToolCall is a tool call delta in a streaming chunk (has Index).
type StreamToolCall struct {
	Index        int                   `json:"index"`
	ID           string                `json:"id,omitempty"`
	Type         string                `json:"type,omitempty"`
	Function     FunctionCall          `json:"function"`
	ExtraContent *ToolCallExtraContent `json:"extra_content,omitempty"`
}

// StreamDelta is the delta object in a streaming chunk.
type StreamDelta struct {
	Role      string           `json:"role,omitempty"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []StreamToolCall `json:"tool_calls,omitempty"`
}

// StreamChoice is a choice in a streaming chunk.
type StreamChoice struct {
	Index        int         `json:"index"`
	Delta        StreamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

// StreamChunk represents one SSE chunk.
type StreamChunk struct {
	ID      string         `json:"id"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"` // populated in the final chunk by some providers
}

// Client wraps the OpenAI-compatible API.
type Client struct {
	apiKey    string
	baseURL   string
	model     string
	http      *http.Client
	debug     bool
	maxTokens int // -1 = no limit; 0 treated as no limit
}

// NewClient creates a new LLM client.
func NewClient(apiKey, baseURL, model string, debug bool, maxTokens int) *Client {
	return &Client{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http: &http.Client{
			Timeout: 5 * time.Minute,
		},
		debug:     debug,
		maxTokens: maxTokens,
	}
}

// maxTokensPtr returns a pointer to maxTokens for the request, or nil if unlimited.
func (c *Client) maxTokensPtr() *int {
	if c.maxTokens <= 0 {
		return nil
	}
	n := c.maxTokens
	return &n
}

// APIKey returns the configured API key.
func (c *Client) APIKey() string { return c.apiKey }

// BaseURL returns the configured base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// isGemini reports whether the client is talking to Google's Gemini API.
func (c *Client) isGemini() bool {
	return strings.Contains(c.baseURL, "generativelanguage.googleapis.com")
}

// prepareMessages returns a message slice safe to send for this client.
// It always strips ReasoningContent (never valid in outbound requests), and
// applies additional Gemini-specific sanitization when talking to that API.
func (c *Client) prepareMessages(messages []Message) []Message {
	// Strip ReasoningContent from every message — it is captured from responses
	// for observability but must never be echoed back in requests.
	// Also re-encode tool-call arguments so any raw control characters inside
	// string values are properly escaped — some providers re-parse `arguments`
	// server-side and reject requests with unescaped control chars.
	out := make([]Message, len(messages))
	for i, m := range messages {
		m.ReasoningContent = ""
		if len(m.ToolCalls) > 0 {
			fixed := make([]ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				tc.Function.Arguments = jsonutil.SanitizeArgs(tc.Function.Arguments)
				fixed[j] = tc
			}
			m.ToolCalls = fixed
		}
		out[i] = m
	}
	if c.isGemini() {
		return sanitizeForGemini(out)
	}
	return out
}

// sanitizeForGemini normalizes a message list for the Gemini OpenAI-compat endpoint.
// Gemini rejects: (1) system messages after index 0, (2) consecutive same-role messages.
func sanitizeForGemini(msgs []Message) []Message {
	if len(msgs) == 0 {
		return msgs
	}

	// Collect all system messages into one; remove them from the main list.
	var systemParts []string
	var rest []Message
	for _, m := range msgs {
		if m.Role == "system" {
			if t := strings.TrimSpace(m.TextContent()); t != "" {
				systemParts = append(systemParts, t)
			}
		} else {
			rest = append(rest, m)
		}
	}

	var out []Message
	if len(systemParts) > 0 {
		out = append(out, Message{Role: "system", Content: strings.Join(systemParts, "\n\n")})
	}

	// Merge consecutive same-role plain-text messages (user/assistant).
	// Tool-related messages are never merged.
	for _, m := range rest {
		prev := len(out) - 1
		if len(out) > 0 &&
			out[prev].Role == m.Role &&
			len(out[prev].ToolCalls) == 0 && out[prev].ToolCallID == "" &&
			len(m.ToolCalls) == 0 && m.ToolCallID == "" {

			if len(out[prev].Parts) == 0 && len(m.Parts) == 0 {
				// Both plain text: simple concatenation.
				out[prev].Content += "\n\n" + m.TextContent()
			} else if len(m.Parts) == 0 {
				// Merge plain text m into multimodal out[prev].
				out[prev].Parts = append(out[prev].Parts, ContentPart{
					Type: "text",
					Text: "\n\n" + m.Content,
				})
				// Also update Content for backward-compatibility.
				out[prev].Content = out[prev].TextContent()
			} else {
				// m has parts or both have parts: don't attempt merge to preserve structure.
				out = append(out, m)
			}
		} else {
			out = append(out, m)
		}
	}

	return out
}

// ChatCompletionJSON is like ChatCompletion but instructs the model to return
// valid JSON. Use this for structured outputs like router responses.
func (c *Client) ChatCompletionJSON(ctx context.Context, messages []Message, temperature float64) (*ChatResponse, error) {
	return c.chatCompletion(ctx, messages, nil, temperature, &ResponseFormat{Type: "json_object"})
}

// ChatCompletion sends a non-streaming chat completion request.
func (c *Client) ChatCompletion(ctx context.Context, messages []Message, tools []ToolDef, temperature float64) (*ChatResponse, error) {
	return c.chatCompletion(ctx, messages, tools, temperature, nil)
}

// prettyJSON attempts to indent JSON for debug readability.
// Falls back to the raw string if the input is not valid JSON.
func prettyJSON(data []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return string(data)
	}
	return buf.String()
}

// isRetryableError reports whether the HTTP status + body combination is a
// transient error worth retrying (provider degraded, rate-limited, or 5xx).
func isRetryableError(statusCode int, body []byte) bool {
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if statusCode >= 500 {
		return true
	}
	// "DEGRADED function cannot be invoked" — transient provider endpoint failure.
	if statusCode == http.StatusBadRequest && strings.Contains(string(body), "DEGRADED") {
		return true
	}
	return false
}

func (c *Client) chatCompletion(ctx context.Context, messages []Message, tools []ToolDef, temperature float64, respFmt *ResponseFormat) (*ChatResponse, error) {
	req := ChatRequest{
		Model:          c.model,
		Messages:       c.prepareMessages(messages),
		Tools:          tools,
		Temperature:    temperature,
		MaxTokens:      c.maxTokensPtr(),
		Stream:         false,
		ResponseFormat: respFmt,
	}

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	if c.debug {
		fmt.Printf("[DEBUG] LLM Request:\n%s\n", prettyJSON(bodyBytes))
	}

	const maxAttempts = 3
	var data []byte
	var statusCode int

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second // 1 s, 2 s
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			if c.debug {
				fmt.Printf("[DEBUG] LLM retry %d/%d after HTTP %d\n", attempt+1, maxAttempts, statusCode)
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		}

		resp, err := c.http.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("send request: %w", err)
		}
		data, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		statusCode = resp.StatusCode

		if c.debug {
			fmt.Printf("[DEBUG] LLM Response (%d):\n%s\n", statusCode, prettyJSON(data))
		}

		if statusCode == http.StatusOK {
			break
		}
		if isRetryableError(statusCode, data) && attempt < maxAttempts-1 {
			continue
		}
		break
	}

	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("LLM API error (HTTP %d): %s", statusCode, string(data))
	}

	// Token optimization: some models output <thought> tags even in json_object mode.
	// Strip them before unmarshaling.
	cleanedData := stripThinkBlocksBytes(data)

	var chatResp ChatResponse
	if err := json.Unmarshal(cleanedData, &chatResp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	if c.debug && chatResp.Usage != nil {
		u := chatResp.Usage
		fmt.Printf("\n  ◎  Tokens     ↑ %d  ↓ %d  ∑ %d\n\n", u.PromptTokens, u.CompletionTokens, u.TotalTokens)
	}

	return &chatResp, nil
}

// thinkBlockBytesRe matches thinking-block tags in raw response data.
// Covers <think>…</think> (DeepSeek-R1, QwQ) and <thought>…</thought> (Gemma 4).
var thinkBlockBytesRe = regexp.MustCompile(`(?s)<think>.*?</think>|<thought>.*?</thought>`)

// stripThinkBlocksBytes removes thinking-block tags from raw JSON response data.
func stripThinkBlocksBytes(data []byte) []byte {
	cleaned := thinkBlockBytesRe.ReplaceAll(data, nil)
	return []byte(strings.TrimSpace(string(cleaned)))
}

// StreamCallback is called for each content token during streaming.
type StreamCallback func(token string)

// ToolCallStreamCb is called during streaming as soon as a tool call's arguments
// form a syntactically complete JSON object. Index is the zero-based position of
// the call in the response. The callback fires at most once per tool call index.
// Implementations must be non-blocking (use a buffered channel or goroutine).
type ToolCallStreamCb func(index int, call ToolCall)

// isJSONComplete reports whether s is a syntactically complete JSON object or array.
// Uses a bracket-depth counter rather than full parsing for speed.
func isJSONComplete(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return false
	}
	switch s[0] {
	case '{', '[':
	default:
		return false
	}
	depth := 0
	inStr := false
	esc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if esc {
			esc = false
			continue
		}
		if c == '\\' && inStr {
			esc = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return true
			}
		}
	}
	return false
}

// ChatCompletionStream sends a streaming chat completion request.
// The callback is called for each content token. toolCallStreamCb (optional) is
// called as soon as each tool call's JSON arguments are complete; pass nil to disable.
// Returns the assembled message and token usage (if reported by the provider).
func (c *Client) ChatCompletionStream(ctx context.Context, messages []Message, tools []ToolDef, temperature float64, callback StreamCallback, toolCallStreamCb ToolCallStreamCb) (*Message, *Usage, error) {
	req := ChatRequest{
		Model:       c.model,
		Messages:    c.prepareMessages(messages),
		Tools:       tools,
		Temperature: temperature,
		MaxTokens:   c.maxTokensPtr(),
		Stream:      true,
	}

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}

	if c.debug {
		fmt.Printf("[DEBUG] LLM Stream Request:\n%s\n", prettyJSON(bodyBytes))
	}

	const maxAttempts = 3
	var resp *http.Response
	var statusCode int

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(delay):
			}
			if c.debug {
				fmt.Printf("[DEBUG] LLM stream retry %d/%d after HTTP %d\n", attempt+1, maxAttempts, statusCode)
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, nil, fmt.Errorf("create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		}

		resp, err = c.http.Do(httpReq)
		if err != nil {
			return nil, nil, fmt.Errorf("send request: %w", err)
		}
		statusCode = resp.StatusCode

		if statusCode == http.StatusOK {
			break
		}
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if isRetryableError(statusCode, errBody) && attempt < maxAttempts-1 {
			continue
		}
		return nil, nil, fmt.Errorf("LLM API error (HTTP %d): %s", statusCode, string(errBody))
	}
	defer resp.Body.Close()

	// Parse SSE stream.
	result := &Message{Role: "assistant"}
	var contentBuf strings.Builder
	toolCallMap := make(map[int]*ToolCall)
	firedToolCalls := make(map[int]bool) // tracks which tool call indices have fired toolCallStreamCb
	var lastUsage *Usage

	scanner := bufio.NewScanner(resp.Body)
	// LLM responses can include large JSON payloads in a single SSE line.
	// Increase the buffer beyond the default 64KB to avoid scan errors.
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk StreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			if c.debug {
				fmt.Printf("[DEBUG] Failed to parse chunk: %s\n", data)
			}
			continue
		}

		if chunk.Usage != nil {
			lastUsage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			delta := choice.Delta

			if delta.Content != "" {
				contentBuf.WriteString(delta.Content)
				if callback != nil {
					callback(delta.Content)
				}
			}

			for _, tc := range delta.ToolCalls {
				existing, ok := toolCallMap[tc.Index]
				if !ok {
					newTC := ToolCall{
						ID:           tc.ID,
						Type:         tc.Type,
						ExtraContent: tc.ExtraContent,
						Function: FunctionCall{
							Name:      tc.Function.Name,
							Arguments: tc.Function.Arguments,
						},
					}
					toolCallMap[tc.Index] = &newTC
				} else {
					if tc.ID != "" {
						existing.ID = tc.ID
					}
					if tc.Type != "" {
						existing.Type = tc.Type
					}
					if tc.ExtraContent != nil {
						existing.ExtraContent = tc.ExtraContent
					}
					if tc.Function.Name != "" {
						existing.Function.Name += tc.Function.Name
					}
					existing.Function.Arguments += tc.Function.Arguments
				}

				// Fire toolCallStreamCb as soon as this tool call's JSON args are complete.
				if toolCallStreamCb != nil && !firedToolCalls[tc.Index] {
					if entry := toolCallMap[tc.Index]; entry != nil && entry.Function.Name != "" && isJSONComplete(entry.Function.Arguments) {
						firedToolCalls[tc.Index] = true
						toolCallStreamCb(tc.Index, *entry)
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, lastUsage, fmt.Errorf("read streaming response: %w", err)
	}

	result.Content = contentBuf.String()

	if c.debug && lastUsage != nil {
		fmt.Printf("\n  ◎  Tokens     ↑ %d  ↓ %d  ∑ %d\n\n", lastUsage.PromptTokens, lastUsage.CompletionTokens, lastUsage.TotalTokens)
	}

	// Convert tool call map to slice.
	if len(toolCallMap) > 0 {
		result.ToolCalls = make([]ToolCall, 0, len(toolCallMap))
		for i := 0; i < len(toolCallMap); i++ {
			if tc, ok := toolCallMap[i]; ok {
				result.ToolCalls = append(result.ToolCalls, *tc)
			}
		}
	}

	return result, lastUsage, nil
}
