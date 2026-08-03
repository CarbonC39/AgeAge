package agent

import (
	"context"

	"ageage/llm"
)

// ChatClient is the subset of an OpenAI-compatible client used by Agent.
// Keeping the execution loop behind this interface makes its orchestration
// behavior testable without a live model or HTTP server.
type ChatClient interface {
	ChatCompletion(
		ctx context.Context,
		messages []llm.Message,
		tools []llm.ToolDef,
		temperature float64,
	) (*llm.ChatResponse, error)
	ChatCompletionStream(
		ctx context.Context,
		messages []llm.Message,
		tools []llm.ToolDef,
		temperature float64,
		callback llm.StreamCallback,
		toolCallStreamCb llm.ToolCallStreamCb,
	) (*llm.Message, *llm.Usage, error)
	APIKey() string
	BaseURL() string
}

var _ ChatClient = (*llm.Client)(nil)
