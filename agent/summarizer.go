package agent

import (
	"context"
	"fmt"
	"strings"

	"ageage/config"
	"ageage/llm"
)

// Summarizer compresses conversation history to save tokens.
type Summarizer struct {
	cfg    *config.Config
	client *llm.Client
	debug  bool
}

// NewSummarizer creates a new Summarizer.
func NewSummarizer(cfg *config.Config, client *llm.Client, debug bool) *Summarizer {
	return &Summarizer{
		cfg:    cfg,
		client: client,
		debug:  debug,
	}
}

// ShouldSummarize returns true if the messages exceed the threshold.
// Counts assistant messages: each LLM iteration produces exactly one, whether
// it comes from a new user turn (multi-turn chat) or a tool-call round (sub-agents).
// This ensures the threshold applies equally to main agents and pipeline/delegate
// sub-agents, which have only one user message but can accumulate many iterations.
func (s *Summarizer) ShouldSummarize(messages []llm.Message) bool {
	if !s.cfg.Summarize.Enabled {
		return false
	}

	rounds := 0
	for _, m := range messages {
		if m.Role == "assistant" {
			rounds++
		}
	}

	return rounds > s.cfg.Summarize.Threshold
}

// SetClient replaces the LLM client used to build the summarization client.
// Called by Agent.SetLLMClient so the summarizer tracks credential changes.
func (s *Summarizer) SetClient(client *llm.Client) {
	s.client = client
}

// Summarize compresses older messages into a summary, keeping recent messages intact.
// Returns the new message list with summary replacing old messages.
func (s *Summarizer) Summarize(ctx context.Context, messages []llm.Message) ([]llm.Message, error) {
	keepRecent := s.cfg.Summarize.KeepRecent
	if keepRecent < 2 {
		keepRecent = 2
	}

	// Find the split point: keep system message + last N messages.
	if len(messages) <= keepRecent+1 {
		return messages, nil // Not enough messages to summarize.
	}

	// Separate system message, old messages, and recent messages.
	var systemMsg *llm.Message
	startIdx := 0
	if len(messages) > 0 && messages[0].Role == "system" {
		systemMsg = &messages[0]
		startIdx = 1
	}

	cutPoint := len(messages) - keepRecent
	// Don't cut inside a tool-call exchange: walk back to a user-message boundary
	// so recentMessages never starts with orphaned role:"tool" messages.
	for cutPoint > startIdx && messages[cutPoint].Role != "user" {
		cutPoint--
	}
	oldMessages := messages[startIdx:cutPoint]
	recentMessages := messages[cutPoint:]

	if len(oldMessages) == 0 {
		return messages, nil
	}

	// Build the conversation text to summarize.
	var convText strings.Builder
	for _, m := range oldMessages {
		switch m.Role {
		case "user":
			convText.WriteString(fmt.Sprintf("User: %s\n", m.Content))
		case "assistant":
			if m.Content != "" {
				convText.WriteString(fmt.Sprintf("Assistant: %s\n", m.Content))
			}
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					convText.WriteString(fmt.Sprintf("Assistant called tool: %s\n", tc.Function.Name))
				}
			}
		case "tool":
			// Truncate tool results for summarization.
			content := m.Content
			if len(content) > 200 {
				content = content[:200] + "..."
			}
			convText.WriteString(fmt.Sprintf("Tool result: %s\n", content))
		}
	}

	// Call LLM to generate summary.
	summaryModel := s.cfg.Summarize.Model
	if summaryModel == "" {
		summaryModel = s.cfg.LLM.Model
	}

	summaryClient := llm.NewClient(
		s.client.APIKey(),
		s.client.BaseURL(),
		summaryModel,
		s.debug,
		0, // summarization calls don't need a token cap
	)

	// Strip existing summaries from the oldMessages slice so we don't summarize
	// a previous summary of a summary.
	var oldFiltered strings.Builder
	for _, m := range oldMessages {
		if m.Role == "system" && strings.HasPrefix(m.Content, "[Previous conversation summary]") {
			oldFiltered.WriteString(fmt.Sprintf("Earlier summary: %s\n", strings.TrimPrefix(m.Content, "[Previous conversation summary]\n")))
			continue
		}
		// ... (the existing logic for convText already covers other roles)
	}

	summaryMessages := []llm.Message{
		{
			Role: "system",
			Content: `You are a conversation summarizer. Summarize the following conversation concisely.
Focus on: key decisions, important information, task progress, and any unresolved items.
Keep the summary under 300 words. Output ONLY the summary, no preamble.`,
		},
		{
			Role:    "user",
			Content: fmt.Sprintf("Summarize this conversation:\n\n%s", convText.String()),
		},
	}

	if s.debug {
		fmt.Printf("  ⟳  %-10s compressing %d messages…\n", "Summarize", len(oldMessages))
	}

	resp, err := summaryClient.ChatCompletion(ctx, summaryMessages, nil, s.cfg.LLM.Temperature)
	if err != nil {
		return messages, fmt.Errorf("summarization failed: %w", err)
	}

	if len(resp.Choices) == 0 {
		return messages, fmt.Errorf("summarization returned empty response")
	}

	summary := resp.Choices[0].Message.Content

	if s.debug {
		preview := summary
		if len(preview) > 120 {
			preview = preview[:120] + "…"
		}
		fmt.Printf("  ⟳  %-10s done: %s\n", "Summarize", preview)
	}

	// Reconstruct message list.
	var newMessages []llm.Message

	if systemMsg != nil {
		newMessages = append(newMessages, *systemMsg)
	}

	// Insert summary as a system message.
	newMessages = append(newMessages, llm.Message{
		Role:    "system",
		Content: fmt.Sprintf("[Previous conversation summary]\n%s", strings.TrimSpace(summary)),
	})

	// Append recent messages, but filter out any old summaries that might be in recentMessages.
	for _, m := range recentMessages {
		if m.Role == "system" && strings.HasPrefix(m.Content, "[Previous conversation summary]") {
			continue
		}
		newMessages = append(newMessages, m)
	}

	return newMessages, nil
}
