package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// TodoItem is a single task entry.
type TodoItem struct {
	Task   string `json:"task"`
	Status string `json:"status"` // pending | in_progress | done | skipped
}

// TodoStore is a thread-safe, per-run task list shared between the agent and
// the UpdateTodosTool. The agent holds a pointer to clear it after finish_task.
type TodoStore struct {
	mu         sync.Mutex
	items      []TodoItem
	todoMsgID  string // platform message ID of the first todo notification sent this session

	// NotifyFunc is a simple fire-and-forget notifier (for non-editable channels).
	NotifyFunc func(string)

	// SendFunc sends a notification and returns the platform message ID.
	// Takes priority over NotifyFunc when set.
	SendFunc func(text string) string

	// EditFunc edits a previously sent notification identified by msgID.
	// Returns an error if editing failed; the store will then fall back to SendFunc.
	// Only used when SendFunc is also set.
	EditFunc func(msgID, text string) error
}

// Update replaces the entire task list and notifies the user.
func (s *TodoStore) Update(items []TodoItem) {
	s.mu.Lock()
	s.items = make([]TodoItem, len(items))
	copy(s.items, items)
	s.mu.Unlock()

	formatted := s.Format()
	if formatted == "" {
		return
	}

	if s.SendFunc != nil {
		s.mu.Lock()
		msgID := s.todoMsgID
		s.mu.Unlock()

		if msgID != "" && s.EditFunc != nil {
			if err := s.EditFunc(msgID, formatted); err != nil {
				// Edit failed — send a new message and track its ID.
				newID := s.SendFunc(formatted)
				s.mu.Lock()
				s.todoMsgID = newID
				s.mu.Unlock()
			}
		} else {
			newID := s.SendFunc(formatted)
			if newID != "" {
				s.mu.Lock()
				s.todoMsgID = newID
				s.mu.Unlock()
			}
		}
	} else if s.NotifyFunc != nil {
		s.NotifyFunc(formatted)
	}
}

// Clear empties the task list and resets the tracked todo message ID.
func (s *TodoStore) Clear() {
	s.mu.Lock()
	s.items = nil
	s.todoMsgID = ""
	s.mu.Unlock()
}

// IsEmpty reports whether there are no tasks.
func (s *TodoStore) IsEmpty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items) == 0
}

// IsComplete reports whether every task is in a terminal state (done or skipped).
// Returns true when the list is empty (no tasks = no unfinished work).
func (s *TodoStore) IsComplete() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.items {
		switch strings.ToLower(strings.TrimSpace(item.Status)) {
		case "done", "completed", "finished", "skipped", "cancelled", "canceled":
			// terminal — ok
		default:
			return false
		}
	}
	return true
}

// PendingList returns a newline-separated list of tasks that are not yet in a
// terminal state (done/skipped/cancelled). Returns "" when all are done.
func (s *TodoStore) PendingList() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lines []string
	for _, item := range s.items {
		switch strings.ToLower(strings.TrimSpace(item.Status)) {
		case "done", "completed", "finished", "skipped", "cancelled", "canceled":
			// terminal — skip
		default:
			lines = append(lines, fmt.Sprintf("- [%s] %s", item.Status, item.Task))
		}
	}
	return strings.Join(lines, "\n")
}

// Format returns the task list as a human-readable string, or "" when empty.
func (s *TodoStore) Format() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.items) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("Current Tasks:\n")
	for _, item := range s.items {
		fmt.Fprintf(&sb, "%s %s\n", todoStatusMark(item.Status), item.Task)
	}
	return sb.String()
}

func todoStatusMark(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "done", "completed", "finished":
		return "[x]"
	case "in_progress", "doing", "wip":
		return "[~]"
	case "skipped", "cancelled", "canceled":
		return "[-]"
	default: // pending, todo, ""
		return "[ ]"
	}
}

// ── UpdateTodosTool ──────────────────────────────────────────────────────────

// UpdateTodosTool lets the agent overwrite its current task list in a single call.
// It is a skill-only tool — not registered in the global registry.
type UpdateTodosTool struct {
	Store *TodoStore
}

func (t *UpdateTodosTool) Name() string { return "update_todos" }

func (t *UpdateTodosTool) Description() string {
	return "Replace the current task list with a new one (full overwrite). " +
		"The list is displayed to the user and injected into every subsequent LLM context so you always know your progress. " +
		"Call with an empty array to clear the list. " +
		"Use statuses: pending, in_progress, done, skipped."
}

func (t *UpdateTodosTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"todos": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"task": map[string]interface{}{
							"type":        "string",
							"description": "Short description of the task.",
						},
						"status": map[string]interface{}{
							"type":        "string",
							"description": "One of: pending, in_progress, done, skipped.",
						},
					},
					"required": []string{"task", "status"},
				},
				"description": "Complete replacement task list. Omit or pass [] to clear.",
			},
		},
		"required": []string{"todos"},
	}
}

type updateTodosArgs struct {
	Todos []TodoItem `json:"todos"`
}

func (t *UpdateTodosTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var a updateTodosArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	t.Store.Update(a.Todos)

	if len(a.Todos) == 0 {
		return "Todo list cleared.", nil
	}
	return t.Store.Format(), nil
}
