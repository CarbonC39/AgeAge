package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// CronEntry represents a scheduled task.
type CronEntry struct {
	ID       string `json:"id"`
	Schedule string `json:"schedule"` // Cron expression (e.g., "*/5 * * * *")
	Command  string `json:"command"`  // Tool name and description of what to do
	Created  string `json:"created"`
}

// CronStore manages persistent cron entries in a JSON file.
type CronStore struct {
	path    string
	entries []CronEntry
	mu      sync.Mutex
}

// NewCronStore creates a cron store backed by a JSON file.
func NewCronStore(path string) *CronStore {
	cs := &CronStore{path: path}
	cs.load()
	return cs
}

func (cs *CronStore) load() {
	data, err := os.ReadFile(cs.path)
	if err != nil {
		cs.entries = nil
		return
	}
	if err := json.Unmarshal(data, &cs.entries); err != nil {
		fmt.Printf("⚠️  Warning: cron store %s is malformed, starting empty: %s\n", cs.path, err)
		cs.entries = nil
	}
}

func (cs *CronStore) save() error {
	data, err := json.MarshalIndent(cs.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cs.path, data, 0o644)
}

// Add adds a new cron entry.
func (cs *CronStore) Add(schedule, command string) (CronEntry, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	entry := CronEntry{
		ID:       fmt.Sprintf("cron_%d", time.Now().UnixNano()),
		Schedule: schedule,
		Command:  command,
		Created:  time.Now().Format(time.RFC3339),
	}

	cs.entries = append(cs.entries, entry)
	if err := cs.save(); err != nil {
		return CronEntry{}, err
	}
	return entry, nil
}

// Remove removes a cron entry by ID.
func (cs *CronStore) Remove(id string) (bool, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for i, e := range cs.entries {
		if e.ID == id {
			cs.entries = append(cs.entries[:i], cs.entries[i+1:]...)
			return true, cs.save()
		}
	}
	return false, nil
}

// List returns all cron entries.
func (cs *CronStore) List() []CronEntry {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	result := make([]CronEntry, len(cs.entries))
	copy(result, cs.entries)
	return result
}

// --- Cron Tools ---

// CronAddTool adds a scheduled task.
type CronAddTool struct {
	Store      *CronStore
	Supervised bool
	// ConfirmFunc is called in supervised mode. Returns true to allow execution.
	ConfirmFunc func(operation string) bool
}

func (t *CronAddTool) Name() string { return "cron_add" }

func (t *CronAddTool) Description() string {
	return "Add a new scheduled/recurring task. Specify a cron schedule expression and a description of the task to run."
}

func (t *CronAddTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"schedule": map[string]any{
				"type":        "string",
				"description": "Cron schedule expression (e.g., '*/5 * * * *' for every 5 minutes, '0 9 * * *' for daily at 9am).",
			},
			"command": map[string]any{
				"type":        "string",
				"description": "Description of the task to perform on each trigger.",
			},
		},
		"required": []string{"schedule", "command"},
	}
}

func (t *CronAddTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Schedule string `json:"schedule"`
		Command  string `json:"command"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	operation := fmt.Sprintf("Add cron task: %s (every %s)", params.Command, params.Schedule)

	if t.Supervised && t.ConfirmFunc != nil {
		if !t.ConfirmFunc(operation) {
			return "Cron task addition denied by user.", nil
		}
	}

	entry, err := t.Store.Add(params.Schedule, params.Command)
	if err != nil {
		return "", fmt.Errorf("failed to add cron: %w", err)
	}

	return fmt.Sprintf("Cron task added:\n  ID: %s\n  Schedule: %s\n  Command: %s", entry.ID, entry.Schedule, entry.Command), nil
}

// CronRemoveTool removes a scheduled task.
type CronRemoveTool struct {
	Store      *CronStore
	Supervised bool
	// ConfirmFunc is called in supervised mode. Returns true to allow execution.
	ConfirmFunc func(operation string) bool
}

func (t *CronRemoveTool) Name() string { return "cron_remove" }

func (t *CronRemoveTool) Description() string {
	return "Remove a scheduled task by its ID."
}

func (t *CronRemoveTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "The ID of the cron task to remove.",
			},
		},
		"required": []string{"id"},
	}
}

func (t *CronRemoveTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	operation := fmt.Sprintf("Remove cron task: %s", params.ID)

	if t.Supervised && t.ConfirmFunc != nil {
		if !t.ConfirmFunc(operation) {
			return "Cron task removal denied by user.", nil
		}
	}

	found, err := t.Store.Remove(params.ID)
	if err != nil {
		return "", fmt.Errorf("failed to remove cron: %w", err)
	}
	if !found {
		return fmt.Sprintf("Cron task %s not found.", params.ID), nil
	}
	return fmt.Sprintf("Cron task %s removed.", params.ID), nil
}

// CronListTool lists all scheduled tasks.
type CronListTool struct {
	Store *CronStore
}

func (t *CronListTool) Name() string { return "cron_list" }

func (t *CronListTool) Description() string {
	return "List all registered scheduled tasks."
}

func (t *CronListTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *CronListTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	entries := t.Store.List()

	if len(entries) == 0 {
		return "No scheduled tasks.", nil
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Created < entries[j].Created
	})

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Scheduled tasks (%d):\n\n", len(entries)))
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("  ID:       %s\n", e.ID))
		sb.WriteString(fmt.Sprintf("  Schedule: %s\n", e.Schedule))
		sb.WriteString(fmt.Sprintf("  Command:  %s\n", e.Command))
		sb.WriteString(fmt.Sprintf("  Created:  %s\n\n", e.Created))
	}

	return sb.String(), nil
}
