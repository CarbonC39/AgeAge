package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CronEntry represents a scheduled task.
type CronEntry struct {
	ID       string `json:"id"`
	Schedule string `json:"schedule"` // Cron expression (e.g., "*/5 * * * *")
	Command  string `json:"command"`  // Free-text task, or "skill:<name> [args]" to run an existing skill/pipeline
	Created  string `json:"created"`
	// Enabled is false when the entry is paused. Legacy entries without the
	// field (created before the cron rewrite) are migrated to true on load.
	Enabled bool `json:"enabled"`
	// Delivery is an optional IM target for the run result, formatted as
	// "channelType:channelID" or "channelType:channelID:t:threadID"
	// (e.g. "matrix:!room:chat.lomia.uk"). Empty means no delivery.
	Delivery string `json:"delivery,omitempty"`

	// Runtime audit fields, updated after every execution.
	LastRun    string `json:"last_run,omitempty"`
	LastStatus string `json:"last_status,omitempty"` // "success" | "error"
	LastError  string `json:"last_error,omitempty"`
	LastOutput string `json:"last_output,omitempty"` // capped by [cron].max_output
	RunCount   int    `json:"run_count,omitempty"`
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
	// Unmarshal into raw maps so legacy entries (created before the cron
	// rewrite) can be detected by the absence of the "enabled" key.
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		fmt.Printf("⚠️  Warning: cron store %s is malformed, starting empty: %s\n", cs.path, err)
		cs.entries = nil
		return
	}
	entries := make([]CronEntry, 0, len(raw))
	for _, m := range raw {
		b, _ := json.Marshal(m)
		var e CronEntry
		if err := json.Unmarshal(b, &e); err != nil {
			continue
		}
		if e.ID == "" {
			e.ID = fmt.Sprintf("cron_%d", time.Now().UnixNano())
		}
		if _, hasEnabled := m["enabled"]; !hasEnabled {
			e.Enabled = true // legacy entry: enable by default
		}
		entries = append(entries, e)
	}
	cs.entries = entries
}

func (cs *CronStore) save() error {
	data, err := json.MarshalIndent(cs.entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cs.path, data, 0o644)
}

// Add adds a new cron entry.
func (cs *CronStore) Add(schedule, command, delivery string, enabled bool) (CronEntry, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	entry := CronEntry{
		ID:       fmt.Sprintf("cron_%d", time.Now().UnixNano()),
		Schedule: schedule,
		Command:  command,
		Delivery: delivery,
		Enabled:  enabled,
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

// Get returns the entry with the given ID.
func (cs *CronStore) Get(id string) (CronEntry, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, e := range cs.entries {
		if e.ID == id {
			return e, true
		}
	}
	return CronEntry{}, false
}

// SetEnabled pauses (false) or resumes (true) an entry. Returns whether the
// entry exists.
func (cs *CronStore) SetEnabled(id string, enabled bool) (bool, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for i := range cs.entries {
		if cs.entries[i].ID == id {
			cs.entries[i].Enabled = enabled
			return true, cs.save()
		}
	}
	return false, nil
}

// UpdateResult records the outcome of an execution and bumps run_count.
// Returns the updated entry and whether the entry still exists.
func (cs *CronStore) UpdateResult(id string, runAt time.Time, status, errMsg, output string) (CronEntry, bool, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for i := range cs.entries {
		if cs.entries[i].ID == id {
			cs.entries[i].LastRun = runAt.Format(time.RFC3339)
			cs.entries[i].LastStatus = status
			cs.entries[i].LastError = errMsg
			cs.entries[i].LastOutput = output
			cs.entries[i].RunCount++
			if err := cs.save(); err != nil {
				return cs.entries[i], true, err
			}
			return cs.entries[i], true, nil
		}
	}
	return CronEntry{}, false, nil
}

// List returns all cron entries.
func (cs *CronStore) List() []CronEntry {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	result := make([]CronEntry, len(cs.entries))
	copy(result, cs.entries)
	return result
}

// ── Expression matching ───────────────────────────────────────────────────────

// MatchesCronExpr reports whether t matches the 5-field cron expression expr.
// Fields (space-separated): minute hour day-of-month month day-of-week.
// Each field supports: * (any), */n (step from min), n (exact), n-m (range), comma lists.
func MatchesCronExpr(expr string, t time.Time) bool {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return false
	}
	return cronFieldMatch(fields[0], t.Minute(), 0) &&
		cronFieldMatch(fields[1], t.Hour(), 0) &&
		cronFieldMatch(fields[2], t.Day(), 1) &&
		cronFieldMatch(fields[3], int(t.Month()), 1) &&
		cronFieldMatch(fields[4], int(t.Weekday()), 0)
}

// NextRunTime returns the next time strictly after `from` at which the cron
// expression matches. Returns ok=false when no match exists within the search
// window (roughly two years, covering leap-day schedules).
func NextRunTime(expr string, from time.Time) (time.Time, bool) {
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := from.AddDate(2, 0, 0)
	for t.Before(limit) {
		if MatchesCronExpr(expr, t) {
			return t, true
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, false
}

// ValidateCronExpr returns an error if expr is not a valid 5-field cron expression.
func ValidateCronExpr(expr string) error {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return fmt.Errorf("cron expression must have exactly 5 fields (got %d): %q", len(fields), expr)
	}
	limits := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	names := [5]string{"minute", "hour", "day", "month", "weekday"}
	for i, f := range fields {
		if err := validateCronField(f, limits[i][0], limits[i][1], names[i]); err != nil {
			return err
		}
	}
	return nil
}

func cronFieldMatch(token string, val, min int) bool {
	for _, part := range strings.Split(token, ",") {
		if cronPartMatch(strings.TrimSpace(part), val, min) {
			return true
		}
	}
	return false
}

func cronPartMatch(part string, val, min int) bool {
	switch {
	case part == "*":
		return true
	case strings.HasPrefix(part, "*/"):
		step, err := strconv.Atoi(part[2:])
		return err == nil && step > 0 && (val-min)%step == 0
	default:
		if dash := strings.IndexByte(part, '-'); dash != -1 {
			lo, err1 := strconv.Atoi(part[:dash])
			hi, err2 := strconv.Atoi(part[dash+1:])
			return err1 == nil && err2 == nil && val >= lo && val <= hi
		}
		n, err := strconv.Atoi(part)
		return err == nil && val == n
	}
}

func validateCronField(token string, min, max int, name string) error {
	for _, part := range strings.Split(token, ",") {
		part = strings.TrimSpace(part)
		if part == "*" {
			continue
		}
		if strings.HasPrefix(part, "*/") {
			step, err := strconv.Atoi(part[2:])
			if err != nil || step <= 0 {
				return fmt.Errorf("cron %s: invalid step %q", name, part)
			}
			continue
		}
		if dash := strings.IndexByte(part, '-'); dash != -1 {
			lo, err1 := strconv.Atoi(part[:dash])
			hi, err2 := strconv.Atoi(part[dash+1:])
			if err1 != nil || err2 != nil || lo > hi || lo < min || hi > max {
				return fmt.Errorf("cron %s: invalid range %q (allowed %d-%d)", name, part, min, max)
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < min || n > max {
			return fmt.Errorf("cron %s: value %q out of range %d-%d", name, part, min, max)
		}
	}
	return nil
}

// ── Cron Tools ────────────────────────────────────────────────────────────────

// CronAddTool adds a scheduled task.
type CronAddTool struct {
	Store      *CronStore
	Supervised bool
	// ConfirmFunc is called in supervised mode. Returns true to allow execution.
	ConfirmFunc func(operation string) bool
}

func (t *CronAddTool) Name() string { return "cron_add" }

func (t *CronAddTool) Description() string {
	return "Add a new scheduled/recurring task. Specify a cron schedule expression, the task to run " +
		"(free text, or 'skill:<name> [args]' to run an existing skill/pipeline on schedule), " +
		"an optional delivery target for the result, and whether it starts enabled."
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
				"description": "Description of the task to perform on each trigger, OR 'skill:<name> [args]' to run an existing skill/pipeline. With a skill, any text after the skill name is passed as its argument.",
			},
			"delivery": map[string]any{
				"type":        "string",
				"description": "Optional IM delivery target for the result: 'channelType:channelID' or 'channelType:channelID:t:threadID' (e.g. 'matrix:!room:chat.lomia.uk'). Empty = no delivery.",
			},
			"enabled": map[string]any{
				"type":        "boolean",
				"description": "Whether the entry starts enabled. Defaults to true.",
			},
		},
		"required": []string{"schedule", "command"},
	}
}

func (t *CronAddTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Schedule string `json:"schedule"`
		Command  string `json:"command"`
		Delivery string `json:"delivery"`
		Enabled  *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if err := ValidateCronExpr(params.Schedule); err != nil {
		return "", err
	}

	operation := fmt.Sprintf("Add cron task: %s (every %s)", params.Command, params.Schedule)

	if t.Supervised && t.ConfirmFunc != nil {
		if !t.ConfirmFunc(operation) {
			return "Cron task addition denied by user.", nil
		}
	}

	enabled := true
	if params.Enabled != nil {
		enabled = *params.Enabled
	}

	entry, err := t.Store.Add(params.Schedule, params.Command, params.Delivery, enabled)
	if err != nil {
		return "", fmt.Errorf("failed to add cron: %w", err)
	}

	next, _ := NextRunTime(entry.Schedule, time.Now())
	var sb strings.Builder
	fmt.Fprintf(&sb, "Cron task added:\n  ID: %s\n  Schedule: %s\n  Command: %s\n  Enabled: %v",
		entry.ID, entry.Schedule, entry.Command, entry.Enabled)
	if !next.IsZero() {
		fmt.Fprintf(&sb, "\n  Next run: %s", next.Format(time.RFC3339))
	}
	if entry.Delivery != "" {
		fmt.Fprintf(&sb, "\n  Delivery: %s", entry.Delivery)
	}
	return sb.String(), nil
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
	return "List all registered scheduled tasks with their enabled state, next run time, and last run status."
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

	now := time.Now()
	var sb strings.Builder
	fmt.Fprintf(&sb, "Scheduled tasks (%d):\n\n", len(entries))
	for _, e := range entries {
		state := "enabled"
		if !e.Enabled {
			state = "paused"
		}
		status := e.LastStatus
		if status == "" {
			status = "never run"
		}
		next := ""
		if t, ok := NextRunTime(e.Schedule, now); ok {
			next = t.Format(time.RFC3339)
		}
		fmt.Fprintf(&sb, "  ID:       %s\n", e.ID)
		fmt.Fprintf(&sb, "  Schedule: %s\n", e.Schedule)
		fmt.Fprintf(&sb, "  Command:  %s\n", e.Command)
		fmt.Fprintf(&sb, "  State:    %s | Last: %s | Runs: %d\n", state, status, e.RunCount)
		if e.LastRun != "" {
			fmt.Fprintf(&sb, "  Last run: %s\n", e.LastRun)
		}
		if next != "" {
			fmt.Fprintf(&sb, "  Next run: %s\n", next)
		}
		if e.Delivery != "" {
			fmt.Fprintf(&sb, "  Delivery: %s\n", e.Delivery)
		}
		if e.LastError != "" {
			fmt.Fprintf(&sb, "  Last err: %s\n", e.LastError)
		}
		sb.WriteString("\n")
	}

	return sb.String(), nil
}

// CronRunTool immediately executes a scheduled task by ID, outside its schedule.
type CronRunTool struct {
	Store *CronStore
	// RunFunc executes the entry immediately. Wired by the factory so the tool
	// stays free of Agent dependencies.
	RunFunc func(ctx context.Context, id string) (string, error)
}

func (t *CronRunTool) Name() string { return "cron_run" }

func (t *CronRunTool) Description() string {
	return "Immediately execute a scheduled task by its ID, outside of its normal schedule. Returns the run's output."
}

func (t *CronRunTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "The ID of the cron task to execute now.",
			},
		},
		"required": []string{"id"},
	}
}

func (t *CronRunTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if t.RunFunc == nil {
		return "", fmt.Errorf("cron_run is not available in this mode")
	}
	return t.RunFunc(ctx, params.ID)
}
