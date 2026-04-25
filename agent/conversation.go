package agent

import (
	"ageage/llm"
)

// ToolRecord captures one tool call and its result from conversation history.
type ToolRecord struct {
	Name   string `json:"name"`
	Args   string `json:"args"`
	Result string `json:"result"`
}

// Conversation wraps the agent's message history with controlled access.
// All mutations go through its methods, making invariants enforceable and
// keeping the storage backend swappable.
type Conversation struct {
	msgs []llm.Message
}

// Len returns the number of messages currently stored.
func (c *Conversation) Len() int { return len(c.msgs) }

// All returns the underlying slice. Treat the result as read-only; it is NOT a copy.
func (c *Conversation) All() []llm.Message { return c.msgs }

// Snapshot returns a shallow copy safe for mutation (e.g. ephemeral LLM calls).
func (c *Conversation) Snapshot() []llm.Message {
	out := make([]llm.Message, len(c.msgs))
	copy(out, c.msgs)
	return out
}

// Reset replaces the entire message history.
func (c *Conversation) Reset(msgs []llm.Message) { c.msgs = msgs }

// TruncateTo discards all messages from index n onward, breaking the backing
// array to avoid retaining memory.
func (c *Conversation) TruncateTo(n int) { c.msgs = c.msgs[:n:n] }

// Append adds one or more messages to the end of the history.
func (c *Conversation) Append(msgs ...llm.Message) {
	c.msgs = append(c.msgs, msgs...)
}

// HasSystem reports whether the first message is a system message.
func (c *Conversation) HasSystem() bool {
	return len(c.msgs) > 0 && c.msgs[0].Role == "system"
}

// SetSystemContent updates the content of the first system message in-place.
// No-op if the history is empty or the first message is not a system message.
func (c *Conversation) SetSystemContent(content string) {
	if len(c.msgs) > 0 && c.msgs[0].Role == "system" {
		c.msgs[0].Content = content
	}
}

// PrependSystem inserts a system message at position 0.
func (c *Conversation) PrependSystem(msg llm.Message) {
	c.msgs = append([]llm.Message{msg}, c.msgs...)
}

// ToolHistory returns all tool call/result pairs recorded after the last user
// message. It is used by the Evaluator to pass execution context.
func (c *Conversation) ToolHistory() []ToolRecord {
	msgs := c.msgs

	lastUserIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return nil
	}

	var records []ToolRecord
	// pending maps tool-call ID to the FunctionCall so we can pair it with the
	// corresponding tool-result message.
	pending := make(map[string]llm.FunctionCall)

	for i := lastUserIdx + 1; i < len(msgs); i++ {
		msg := msgs[i]
		switch msg.Role {
		case "assistant":
			pending = make(map[string]llm.FunctionCall)
			for _, tc := range msg.ToolCalls {
				pending[tc.ID] = tc.Function
			}
		case "tool":
			if fn, ok := pending[msg.ToolCallID]; ok {
				records = append(records, ToolRecord{
					Name:   fn.Name,
					Args:   fn.Arguments,
					Result: msg.Content,
				})
			}
		}
	}
	return records
}

// Splice replaces messages [start, end) with a single replacement message.
// Returns the net decrease in slice length (end-start-1), which callers use
// to shift index-based records (e.g. compressOldestTurn's pendingTurns offsets).
func (c *Conversation) Splice(start, end int, replacement llm.Message) int {
	removed := end - start
	newMsgs := make([]llm.Message, 0, len(c.msgs)-removed+1)
	newMsgs = append(newMsgs, c.msgs[:start]...)
	newMsgs = append(newMsgs, replacement)
	newMsgs = append(newMsgs, c.msgs[end:]...)
	c.msgs = newMsgs
	return removed - 1
}
