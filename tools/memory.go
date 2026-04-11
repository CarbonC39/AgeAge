package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// MemoryEntry represents a single memory record in MEMORY.jsonl.
type MemoryEntry struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Tags      string `json:"tags,omitempty"`
}

// MemoryStoreTool writes information to long-term memory.
type MemoryStoreTool struct {
	MemoryPath string
	Supervised bool
	// ConfirmFunc is called in supervised mode. Returns true to allow execution.
	ConfirmFunc func(operation string) bool
}

func (t *MemoryStoreTool) Name() string { return "memory_store" }

func (t *MemoryStoreTool) Description() string {
	return "Store important information in long-term memory for future recall. Use this for facts, user preferences, or key decisions."
}

func (t *MemoryStoreTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The information to remember.",
			},
			"tags": map[string]any{
				"type":        "string",
				"description": "Optional comma-separated tags for categorization.",
			},
		},
		"required": []string{"content"},
	}
}

func (t *MemoryStoreTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Content string `json:"content"`
		Tags    string `json:"tags"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	operation := fmt.Sprintf("Store memory: %s", params.Content)

	if t.Supervised && t.ConfirmFunc != nil {
		if !t.ConfirmFunc(operation) {
			return "Memory storage denied by user.", nil
		}
	}

	entry := MemoryEntry{
		ID:        fmt.Sprintf("mem_%d", time.Now().UnixNano()),
		Content:   params.Content,
		Timestamp: time.Now().Format(time.RFC3339),
		Tags:      params.Tags,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("failed to marshal memory entry: %w", err)
	}

	f, err := os.OpenFile(t.MemoryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("failed to open memory file: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(string(data) + "\n"); err != nil {
		return "", fmt.Errorf("failed to write memory: %w", err)
	}

	return fmt.Sprintf("Memory stored: %s (id: %s)", params.Content, entry.ID), nil
}

// MemoryRecallTool searches long-term memory.
type MemoryRecallTool struct {
	MemoryPath string
}

func (t *MemoryRecallTool) Name() string { return "memory_recall" }

func (t *MemoryRecallTool) Description() string {
	return "Search long-term memory for previously stored information. Performs keyword matching on content and tags."
}

func (t *MemoryRecallTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Keywords to search for in memory.",
			},
		},
		"required": []string{"query"},
	}
}

func (t *MemoryRecallTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	data, err := os.ReadFile(t.MemoryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "No memories found.", nil
		}
		return "", fmt.Errorf("failed to read memory file: %w", err)
	}

	queryLower := strings.ToLower(params.Query)
	keywords := strings.Fields(queryLower)

	var matches []MemoryEntry
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry MemoryEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		// Simple keyword matching.
		entryText := strings.ToLower(entry.Content + " " + entry.Tags)
		matched := false
		for _, kw := range keywords {
			if strings.Contains(entryText, kw) {
				matched = true
				break
			}
		}
		if matched {
			matches = append(matches, entry)
		}
	}

	if len(matches) == 0 {
		return fmt.Sprintf("No memories found matching: %s", params.Query), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d memory(ies):\n\n", len(matches)))
	for _, m := range matches {
		sb.WriteString(fmt.Sprintf("- [%s] %s", m.Timestamp, m.Content))
		if m.Tags != "" {
			sb.WriteString(fmt.Sprintf(" (tags: %s)", m.Tags))
		}
		sb.WriteString("\n")
	}

	return sb.String(), nil
}

// MemoryForgetTool removes a memory entry by ID.
type MemoryForgetTool struct {
	MemoryPath string
	Supervised bool
	// ConfirmFunc is called in supervised mode. Returns true to allow execution.
	ConfirmFunc func(operation string) bool
}

func (t *MemoryForgetTool) Name() string { return "memory_forget" }

func (t *MemoryForgetTool) Description() string {
	return "Remove a specific memory entry by its ID."
}

func (t *MemoryForgetTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"id": map[string]interface{}{
				"type":        "string",
				"description": "The ID of the memory entry to remove.",
			},
		},
		"required": []string{"id"},
	}
}

func (t *MemoryForgetTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	operation := fmt.Sprintf("Forget memory: %s", params.ID)

	if t.Supervised && t.ConfirmFunc != nil {
		if !t.ConfirmFunc(operation) {
			return "Memory forget denied by user.", nil
		}
	}

	data, err := os.ReadFile(t.MemoryPath)
	if err != nil {
		return "", fmt.Errorf("failed to read memory file: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var kept []string
	found := false

	for _, line := range lines {
		if line == "" {
			continue
		}
		var entry MemoryEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			kept = append(kept, line)
			continue
		}
		if entry.ID == params.ID {
			found = true
			continue // Skip this entry.
		}
		kept = append(kept, line)
	}

	if !found {
		return fmt.Sprintf("Memory with ID %s not found.", params.ID), nil
	}

	newContent := strings.Join(kept, "\n")
	if newContent != "" {
		newContent += "\n"
	}
	if err := os.WriteFile(t.MemoryPath, []byte(newContent), 0o644); err != nil {
		return "", fmt.Errorf("failed to write memory file: %w", err)
	}

	return fmt.Sprintf("Memory %s removed.", params.ID), nil
}
