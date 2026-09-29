package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxMemoryRecallEntries = 50
	maxMemoryRecallChars   = 12000
)

var memoryLocks sync.Map // canonical path -> *sync.RWMutex

func memoryLock(path string) *sync.RWMutex {
	canonical, err := filepath.Abs(path)
	if err != nil {
		canonical = path
	}
	lock := &sync.RWMutex{}
	actual, _ := memoryLocks.LoadOrStore(canonical, lock)
	return actual.(*sync.RWMutex)
}

// ensureMemoryPermissions repairs files created by older releases. It must be
// called while holding the path's write lock because chmod mutates filesystem
// metadata and must not race with atomic replacement.
func ensureMemoryPermissions(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.Chmod(path, 0o600)
}

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

	lock := memoryLock(t.MemoryPath)
	lock.Lock()
	defer lock.Unlock()
	f, err := os.OpenFile(t.MemoryPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("failed to open memory file: %w", err)
	}
	defer f.Close()
	// OpenFile does not change an existing file's mode; tighten it on every
	// write so files created by older versions are repaired automatically.
	if err := f.Chmod(0o600); err != nil {
		return "", fmt.Errorf("failed to protect memory file: %w", err)
	}

	if _, err := f.WriteString(string(data) + "\n"); err != nil {
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
	lock := memoryLock(t.MemoryPath)
	lock.Lock()
	defer lock.Unlock()
	if err := ensureMemoryPermissions(t.MemoryPath); err != nil {
		return false
	}
	info, err := os.Stat(t.MemoryPath)
	return err == nil && info.Size() > 0
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

	lock := memoryLock(t.MemoryPath)
	lock.Lock()
	defer lock.Unlock()
	if err := ensureMemoryPermissions(t.MemoryPath); err != nil {
		return "", fmt.Errorf("failed to protect memory file: %w", err)
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
			if len(matches) >= maxMemoryRecallEntries {
				break
			}
		}
	}

	if len(matches) == 0 {
		return fmt.Sprintf("No memories found matching: %s", params.Query), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d memory(ies):\n\n", len(matches)))
	for _, m := range matches {
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

	lock := memoryLock(t.MemoryPath)
	lock.Lock()
	defer lock.Unlock()
	if err := ensureMemoryPermissions(t.MemoryPath); err != nil {
		return "", fmt.Errorf("failed to protect memory file: %w", err)
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
	// Rewrite through a same-directory temporary file and atomic rename. This
	// prevents concurrent readers or crashes from observing a truncated JSONL.
	dir := filepath.Dir(t.MemoryPath)
	tmp, err := os.CreateTemp(dir, ".memory-*.tmp")
	if err != nil {
		return "", fmt.Errorf("failed to create memory temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", fmt.Errorf("failed to protect memory temp file: %w", err)
	}
	if _, err := tmp.WriteString(newContent); err != nil {
		tmp.Close()
		return "", fmt.Errorf("failed to write memory temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("failed to sync memory temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("failed to close memory temp file: %w", err)
	}
	if err := os.Rename(tmpName, t.MemoryPath); err != nil {
		return "", fmt.Errorf("failed to replace memory file: %w", err)
	}

	return fmt.Sprintf("Memory %s removed.", params.ID), nil
}
