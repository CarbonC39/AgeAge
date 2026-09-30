package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	maxMemoryRecallEntries = 50
	maxMemoryRecallChars   = 12000
)

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

func (t *MemoryStoreTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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

	entry, err := NewMemoryRepository(t.MemoryPath).Add(ctx, params.Content, params.Tags)
	if err != nil {
		return "", fmt.Errorf("failed to write memory: %w", err)
	}

	return fmt.Sprintf("Memory stored: %s (id: %s)", params.Content, entry.ID), nil
}

// MemoryRecallTool searches long-term memory.
type MemoryRecallTool struct {
	MemoryPath string
}

// HasMemories reports whether the backing store currently contains data. It is
// checked per turn so memory_recall becomes available immediately after the
// first memory_store call without advertising an empty tool beforehand.
func (t *MemoryRecallTool) HasMemories() bool {
	hasData, _ := NewMemoryRepository(t.MemoryPath).HasData(context.Background())
	return hasData
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

func (t *MemoryRecallTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	matches, err := NewMemoryRepository(t.MemoryPath).Search(ctx, params.Query)
	if err != nil {
		return "", fmt.Errorf("failed to read memory file: %w", err)
	}
	if len(matches) > maxMemoryRecallEntries {
		matches = matches[:maxMemoryRecallEntries]
	}

	if len(matches) == 0 {
		return fmt.Sprintf("No memories found matching: %s", params.Query), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d memory(ies):\n\n", len(matches)))
	for _, record := range matches {
		m := record.Entry
		line := fmt.Sprintf("- [%s] %s", m.Timestamp, m.Content)
		if m.Tags != "" {
			line += fmt.Sprintf(" (tags: %s)", m.Tags)
		}
		line += "\n"
		if sb.Len()+len(line) > maxMemoryRecallChars {
			const notice = "... (memory recall truncated)\n"
			remaining := maxMemoryRecallChars - sb.Len()
			if remaining > 0 {
				if remaining < len(notice) {
					sb.WriteString(notice[:remaining])
				} else {
					sb.WriteString(notice)
				}
			}
			break
		}
		sb.WriteString(line)
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

func (t *MemoryForgetTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
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

	repo := NewMemoryRepository(t.MemoryPath)
	snapshot, err := repo.List(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to read memory file: %w", err)
	}
	var selected *MemoryRecord
	for i := range snapshot.Records {
		if snapshot.Records[i].Entry.ID == params.ID {
			if selected != nil {
				return "", fmt.Errorf("%w: %s", ErrMemoryAmbiguous, params.ID)
			}
			selected = &snapshot.Records[i]
		}
	}
	if selected == nil {
		return fmt.Sprintf("Memory with ID %s not found.", params.ID), nil
	}
	if err := repo.Remove(ctx, map[string]string{params.ID: selected.Revision}); err != nil {
		return "", fmt.Errorf("failed to remove memory: %w", err)
	}

	return fmt.Sprintf("Memory %s removed.", params.ID), nil
}
