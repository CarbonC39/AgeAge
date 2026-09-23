package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	// Task is the preferred name for the free-text task. Command is retained
	// for compatibility with existing stores and callers.
	Task    string `json:"task,omitempty"`
	Command string `json:"command,omitempty"`
	Created string `json:"created"`
	// Enabled is false when the entry is paused. Legacy entries without the
	// field (created before the cron rewrite) are migrated to true on load.
	Enabled bool `json:"enabled"`
	// Delivery is an optional IM target for the run result, formatted as
	// "channelType:channelID" or "channelType:channelID:t:threadID"
	// (e.g. "matrix:!room:chat.lomia.uk"). Empty means no delivery.
	Delivery               string           `json:"delivery,omitempty"`
	Owner                  InteractionScope `json:"owner,omitempty"`
	ContinueCurrentSession bool             `json:"continue_current_session,omitempty"`

	// Runtime audit fields, updated after every execution.
	LastRun    string `json:"last_run,omitempty"`
	LastStatus string `json:"last_status,omitempty"` // "success" | "error"
	LastError  string `json:"last_error,omitempty"`
	LastOutput string `json:"last_output,omitempty"` // capped by [cron].max_output
	RunCount   int    `json:"run_count,omitempty"`
}

func (e CronEntry) TaskText() string {
	if strings.TrimSpace(e.Task) != "" {
		return e.Task
	}
	return e.Command
}

// CronStore manages persistent cron entries in a JSON file.
type CronStore struct {
	path    string
	entries []CronEntry
	loadErr error
	extras  map[string]map[string]json.RawMessage
	mu      sync.Mutex
}

// NewCronStore creates a cron store backed by a JSON file.
func NewCronStore(path string) *CronStore {
	cs := &CronStore{path: path, extras: make(map[string]map[string]json.RawMessage)}
	cs.load()
	return cs
}

func (cs *CronStore) load() {
	data, err := os.ReadFile(cs.path)
	if err != nil {
		if !os.IsNotExist(err) {
			cs.loadErr = fmt.Errorf("read cron store: %w", err)
		}
		cs.entries = nil
		return
	}
	if strings.TrimSpace(string(data)) == "null" {
		cs.loadErr = fmt.Errorf("parse cron store: top-level null is not an entry array")
		fmt.Printf("⚠️  Warning: cron store %s is malformed; mutations are disabled to preserve it\n", cs.path)
		cs.tightenPermissions()
		cs.entries = nil
		return
	}
	// Unmarshal into raw maps so legacy entries (created before the cron
	// rewrite) can be detected by the absence of the "enabled" key.
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		cs.loadErr = fmt.Errorf("parse cron store: %w", err)
		fmt.Printf("⚠️  Warning: cron store %s is malformed; mutations are disabled to preserve it: %s\n", cs.path, err)
		cs.tightenPermissions()
		cs.entries = nil
		return
	}
	cs.loadErr = nil
	entries := make([]CronEntry, 0, len(raw))
	seenIDs := make(map[string]struct{}, len(raw))
	for i, m := range raw {
		b, _ := json.Marshal(m)
		var e CronEntry
		if err := json.Unmarshal(b, &e); err != nil {
			cs.loadErr = fmt.Errorf("parse cron entry %d: %w", i, err)
			cs.entries = nil
			fmt.Printf("⚠️  Warning: cron store %s contains an invalid entry; mutations are disabled to preserve it: %s\n", cs.path, err)
			cs.tightenPermissions()
			return
		}
		if e.ID == "" {
			e.ID = fmt.Sprintf("cron_%d", time.Now().UnixNano())
		}
		if _, duplicate := seenIDs[e.ID]; duplicate {
			cs.loadErr = fmt.Errorf("parse cron entry %d: duplicate ID %q", i, e.ID)
			cs.entries = nil
			fmt.Printf("⚠️  Warning: cron store %s contains duplicate IDs; mutations are disabled to preserve it\n", cs.path)
			cs.tightenPermissions()
			return
		}
		seenIDs[e.ID] = struct{}{}
		if _, hasEnabled := m["enabled"]; !hasEnabled {
			e.Enabled = true // legacy entry: enable by default
		}
		if e.Task == "" {
			e.Task = e.Command
		}
		if e.Command == "" {
			e.Command = e.Task
		}
		extras := make(map[string]json.RawMessage)
		for key, value := range m {
			if !knownCronEntryField(key) {
				extras[key] = append(json.RawMessage(nil), value...)
			}
		}
		if len(extras) > 0 {
			cs.extras[e.ID] = extras
		}
		entries = append(entries, e)
	}
	cs.entries = entries
	// Existing stores may have been created with the old, world-readable mode.
	// Tighten the file on load without changing its contents.
	cs.tightenPermissions()
}

func (cs *CronStore) tightenPermissions() {
	if err := os.Chmod(cs.path, 0o600); err != nil {
		fmt.Printf("⚠️  Warning: could not set cron store %s to mode 0600: %s\n", cs.path, err)
	}
}

func (cs *CronStore) save() error {
	if cs.loadErr != nil {
		return cs.loadErr
	}
	rawEntries := make([]map[string]json.RawMessage, 0, len(cs.entries))
	for _, entry := range cs.entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &raw); err != nil {
			return err
		}
		for key, value := range cs.extras[entry.ID] {
			if _, exists := raw[key]; !exists {
				raw[key] = value
			}
		}
		rawEntries = append(rawEntries, raw)
	}
	data, err := json.MarshalIndent(rawEntries, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(cs.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".cron-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, cs.path); err != nil {
		return err
	}
	return nil
}

func knownCronEntryField(name string) bool {
	switch name {
	case "id", "schedule", "task", "command", "created", "enabled", "delivery", "owner", "continue_current_session",
		"last_run", "last_status", "last_error", "last_output", "run_count":
		return true
	default:
		return false
	}
}

// Add adds a new cron entry.
func (cs *CronStore) Add(schedule, command, delivery string, enabled bool) (CronEntry, error) {
	entry := CronEntry{
		ID:       fmt.Sprintf("cron_%d", time.Now().UnixNano()),
		Schedule: schedule,
		Task:     command,
		Command:  command,
		Delivery: delivery,
		Enabled:  enabled,
		Created:  time.Now().Format(time.RFC3339),
	}

	return cs.addEntry(entry)
}

// Remove removes a cron entry by ID.
func (cs *CronStore) Remove(id string) (bool, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	for i, e := range cs.entries {
		if e.ID == id {
			previous := append([]CronEntry(nil), cs.entries...)
			cs.entries = append(cs.entries[:i], cs.entries[i+1:]...)
			if err := cs.save(); err != nil {
				cs.entries = previous
				return false, err
			}
			return true, nil
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
			previous := cs.entries[i].Enabled
			cs.entries[i].Enabled = enabled
			if err := cs.save(); err != nil {
				cs.entries[i].Enabled = previous
				return false, err
			}
			return true, nil
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
			previous := cs.entries[i]
			cs.entries[i].LastRun = runAt.Format(time.RFC3339)
			cs.entries[i].LastStatus = status
			cs.entries[i].LastError = errMsg
			cs.entries[i].LastOutput = output
			cs.entries[i].RunCount++
			if err := cs.save(); err != nil {
				cs.entries[i] = previous
				return previous, true, err
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

// CronActor identifies the caller of the shared cron service. Admin actors are
// used by the CLI and scheduler; interactive agents are restricted to entries
// owned by their complete interaction scope.
type CronActor struct {
	Scope InteractionScope
	Admin bool
}

func AdminCronActor() CronActor { return CronActor{Admin: true} }

func DefaultCronDelivery(scope InteractionScope) string {
	if scope.ChannelType == "" || scope.ChannelID == "" {
		return ""
	}
	target := scope.ChannelType + ":" + scope.ChannelID
	if scope.ThreadID != "" {
		target += ":t:" + scope.ThreadID
	}
	return target
}

// ValidateCronDelivery prevents an Agent from redirecting scheduled output to
// another room. CLI/admin callers may provide an explicit target.
func ValidateCronDelivery(target string, scope InteractionScope, admin bool) error {
	if target == "" {
		return nil
	}
	if admin {
		parts := strings.SplitN(target, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return fmt.Errorf("invalid cron delivery target %q", target)
		}
		return nil
	}
	if expected := DefaultCronDelivery(scope); expected == "" || target != expected {
		return fmt.Errorf("cron delivery must remain in the source room/thread")
	}
	return nil
}

// CronService is the single authorization path used by Agent tools, the CLI,
// and the scheduler. The store remains available for legacy admin callers.
type CronService struct {
	Store              *CronStore
	SessionIntegration bool
	Timeout            time.Duration
	MaxOutput          int
	ExecuteFunc        func(context.Context, CronEntry) (string, error)
	Now                func() time.Time
}

func NewCronService(store *CronStore, sessionIntegration bool) *CronService {
	return &CronService{
		Store:              store,
		SessionIntegration: sessionIntegration,
		Timeout:            5 * time.Minute,
		MaxOutput:          2000,
		Now:                time.Now,
	}
}

func (s *CronService) Add(actor CronActor, schedule, task, delivery string, enabled, continueCurrent bool) (CronEntry, error) {
	if s == nil || s.Store == nil {
		return CronEntry{}, fmt.Errorf("cron service is unavailable")
	}
	if err := ValidateCronExpr(schedule); err != nil {
		return CronEntry{}, err
	}
	if strings.TrimSpace(task) == "" {
		return CronEntry{}, fmt.Errorf("cron task must not be empty")
	}
	if !actor.Admin && (actor.Scope.SessionID == "" || actor.Scope.SenderID == "") {
		return CronEntry{}, fmt.Errorf("cron tasks require an authenticated owner and source session")
	}
	if !actor.Admin && delivery == "" {
		delivery = DefaultCronDelivery(actor.Scope)
	}
	if err := ValidateCronDelivery(delivery, actor.Scope, actor.Admin); err != nil {
		return CronEntry{}, err
	}
	if !s.SessionIntegration {
		continueCurrent = false
	}
	entry := CronEntry{Schedule: schedule, Task: task, Command: task, Delivery: delivery,
		Enabled: enabled, Owner: actor.Scope, ContinueCurrentSession: continueCurrent,
		Created: time.Now().Format(time.RFC3339)}
	if actor.Admin {
		entry.Owner = InteractionScope{}
	}
	return s.Store.addEntry(entry)
}

func (s *CronService) owned(actor CronActor, e CronEntry) bool {
	return actor.Admin || (!e.Owner.isLegacy() && e.Owner.equal(actor.Scope))
}

func (s *CronService) List(actor CronActor) []CronEntry {
	if s == nil || s.Store == nil {
		return nil
	}
	entries := s.Store.List()
	if actor.Admin {
		return entries
	}
	filtered := entries[:0]
	for _, e := range entries {
		if s.owned(actor, e) {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func (s *CronService) Get(actor CronActor, id string) (CronEntry, bool) {
	if s == nil || s.Store == nil {
		return CronEntry{}, false
	}
	e, ok := s.Store.Get(id)
	if !ok || !s.owned(actor, e) {
		return CronEntry{}, false
	}
	return e, true
}

func (s *CronService) Remove(actor CronActor, id string) (bool, error) {
	if _, ok := s.Get(actor, id); !ok {
		return false, nil
	}
	return s.Store.Remove(id)
}

func (s *CronService) SetEnabled(actor CronActor, id string, enabled bool) (bool, error) {
	if _, ok := s.Get(actor, id); !ok {
		return false, nil
	}
	return s.Store.SetEnabled(id, enabled)
}

// Run executes and audits an entry through the same path for Agent tools, CLI
// requests, and scheduler triggers. Authorization is checked before execution.
func (s *CronService) Run(ctx context.Context, actor CronActor, id string) (string, error) {
	entry, ok := s.Get(actor, id)
	if !ok {
		return "", fmt.Errorf("cron task %s not found", id)
	}
	if s.ExecuteFunc == nil {
		return "", fmt.Errorf("cron execution is unavailable")
	}
	// An Agent tool call already runs while holding its source session lock.
	// Waiting for that same lock here would deadlock the conversation until the
	// cron timeout. Scheduled and admin runs can safely queue behind it.
	if !actor.Admin && entry.ContinueCurrentSession && entry.Owner.SessionID != "" &&
		entry.Owner.SessionID == actor.Scope.SessionID {
		return "", fmt.Errorf("a task that continues this session cannot be run from inside the same active session")
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	output, runErr := s.ExecuteFunc(runCtx, entry)
	status, errMsg := "success", ""
	if runErr != nil {
		status = "error"
		errMsg = runErr.Error()
	}
	maxOutput := s.MaxOutput
	if maxOutput <= 0 {
		maxOutput = 2000
	}
	auditOutput := output
	if runes := []rune(auditOutput); len(runes) > maxOutput {
		auditOutput = string(runes[:maxOutput])
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	if _, exists, auditErr := s.Store.UpdateResult(entry.ID, now(), status, errMsg, auditOutput); auditErr != nil {
		if runErr != nil {
			return output, fmt.Errorf("%w (cron audit failed: %v)", runErr, auditErr)
		}
		return output, fmt.Errorf("cron audit failed: %w", auditErr)
	} else if !exists {
		if runErr != nil {
			return output, runErr
		}
		return output, fmt.Errorf("cron task %s was removed before its result could be audited", entry.ID)
	}
	return output, runErr
}

func (cs *CronStore) addEntry(entry CronEntry) (CronEntry, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if entry.ID == "" {
		entry.ID = fmt.Sprintf("cron_%d", time.Now().UnixNano())
	}
	if entry.Created == "" {
		entry.Created = time.Now().Format(time.RFC3339)
	}
	if entry.Task == "" {
		entry.Task = entry.Command
	}
	if entry.Command == "" {
		entry.Command = entry.Task
	}
	cs.entries = append(cs.entries, entry)
	if err := cs.save(); err != nil {
		cs.entries = cs.entries[:len(cs.entries)-1]
		return CronEntry{}, err
	}
	return entry, nil
}

// ── Cron Tools ────────────────────────────────────────────────────────────────

// CronAddTool adds a scheduled task.
type CronAddTool struct {
	Store              *CronStore
	Service            *CronService
	ScopeFunc          func() InteractionScope
	SessionIntegration bool
	Supervised         bool
	// ConfirmFunc is called in supervised mode. Returns true to allow execution.
	ConfirmFunc func(operation string) bool
}

func (t *CronAddTool) Name() string { return "cron_add" }

func (t *CronAddTool) Description() string {
	return "Add a new scheduled/recurring task. Specify a cron schedule expression, the task to run " +
		"(free text, or 'skill:<name> [args]' to run an existing skill/pipeline on schedule)."
}

func (t *CronAddTool) Parameters() map[string]any {
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"schedule": map[string]any{
				"type":        "string",
				"description": "Cron schedule expression (e.g., '*/5 * * * *' for every 5 minutes, '0 9 * * *' for daily at 9am).",
			},
			"task": map[string]any{
				"type":        "string",
				"description": "Description of the task to perform on each trigger, OR 'skill:<name> [args]' to run an existing skill/pipeline. With a skill, any text after the skill name is passed as its argument.",
			},
		},
		"required": []string{"schedule", "task"},
	}
	if t.SessionIntegration {
		resultProperties(params)["continue_current_session"] = map[string]any{
			"type":        "boolean",
			"description": "Continue the source conversation session.",
		}
	}
	return params
}

func resultProperties(schema map[string]any) map[string]any {
	return schema["properties"].(map[string]any)
}

func (t *CronAddTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Schedule               string `json:"schedule"`
		Task                   string `json:"task"`
		ContinueCurrentSession bool   `json:"continue_current_session"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if err := ValidateCronExpr(params.Schedule); err != nil {
		return "", err
	}
	if strings.TrimSpace(params.Task) == "" {
		return "", fmt.Errorf("cron task must not be empty")
	}

	operation := fmt.Sprintf("Add cron task: %s (every %s)", params.Task, params.Schedule)

	if t.Supervised && t.ConfirmFunc != nil {
		if !t.ConfirmFunc(operation) {
			return "Cron task addition denied by user.", nil
		}
	}

	var entry CronEntry
	var err error
	if t.Service != nil {
		scope := InteractionScope{}
		if t.ScopeFunc != nil {
			scope = t.ScopeFunc()
		}
		entry, err = t.Service.Add(CronActor{Scope: scope}, params.Schedule, params.Task, "", true, params.ContinueCurrentSession)
	} else {
		entry, err = t.Store.Add(params.Schedule, params.Task, "", true)
	}
	if err != nil {
		return "", fmt.Errorf("failed to add cron: %w", err)
	}

	next, _ := NextRunTime(entry.Schedule, time.Now())
	var sb strings.Builder
	fmt.Fprintf(&sb, "Cron task added:\n  ID: %s\n  Schedule: %s\n  Task: %s\n  Enabled: %v",
		entry.ID, entry.Schedule, entry.TaskText(), entry.Enabled)
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
	Service    *CronService
	ScopeFunc  func() InteractionScope
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

	var found bool
	var err error
	if t.Service != nil {
		scope := InteractionScope{}
		if t.ScopeFunc != nil {
			scope = t.ScopeFunc()
		}
		found, err = t.Service.Remove(CronActor{Scope: scope}, params.ID)
	} else {
		found, err = t.Store.Remove(params.ID)
	}
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
	Store     *CronStore
	Service   *CronService
	ScopeFunc func() InteractionScope
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
	var entries []CronEntry
	if t.Service != nil {
		scope := InteractionScope{}
		if t.ScopeFunc != nil {
			scope = t.ScopeFunc()
		}
		entries = t.Service.List(CronActor{Scope: scope})
	} else {
		entries = t.Store.List()
	}

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
	Store     *CronStore
	Service   *CronService
	ScopeFunc func() InteractionScope
	// RunFunc executes the entry immediately. Wired by the factory so the tool
	// stays free of Agent dependencies.
	RunFunc func(ctx context.Context, id string) (string, error)
}

// CronPauseTool disables an owned scheduled task without deleting it.
type CronPauseTool struct {
	Store     *CronStore
	Service   *CronService
	ScopeFunc func() InteractionScope
}

func (t *CronPauseTool) Name() string { return "cron_pause" }
func (t *CronPauseTool) Description() string {
	return "Pause one of your scheduled tasks by its ID."
}
func (t *CronPauseTool) Parameters() map[string]any { return cronIDParameters() }
func (t *CronPauseTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	return setCronEnabledFromTool(t.Store, t.Service, t.ScopeFunc, args, false, "paused")
}

// CronResumeTool enables a previously paused owned scheduled task.
type CronResumeTool struct {
	Store     *CronStore
	Service   *CronService
	ScopeFunc func() InteractionScope
}

func (t *CronResumeTool) Name() string { return "cron_resume" }
func (t *CronResumeTool) Description() string {
	return "Resume one of your paused scheduled tasks by its ID."
}
func (t *CronResumeTool) Parameters() map[string]any { return cronIDParameters() }
func (t *CronResumeTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	return setCronEnabledFromTool(t.Store, t.Service, t.ScopeFunc, args, true, "resumed")
}

func cronIDParameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{"type": "string", "description": "The scheduled task ID."},
		},
		"required": []string{"id"},
	}
}

func setCronEnabledFromTool(store *CronStore, service *CronService, scopeFunc func() InteractionScope, args json.RawMessage, enabled bool, verb string) (string, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	var found bool
	var err error
	if service != nil {
		scope := InteractionScope{}
		if scopeFunc != nil {
			scope = scopeFunc()
		}
		found, err = service.SetEnabled(CronActor{Scope: scope}, params.ID, enabled)
	} else {
		found, err = store.SetEnabled(params.ID, enabled)
	}
	if err != nil {
		return "", fmt.Errorf("failed to %s cron task: %w", verb, err)
	}
	if !found {
		return fmt.Sprintf("Cron task %s not found.", params.ID), nil
	}
	return fmt.Sprintf("Cron task %s %s.", params.ID, verb), nil
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
	if t.Service != nil {
		scope := InteractionScope{}
		if t.ScopeFunc != nil {
			scope = t.ScopeFunc()
		}
		return t.Service.Run(ctx, CronActor{Scope: scope}, params.ID)
	}
	if t.RunFunc == nil {
		return "", fmt.Errorf("cron_run is not available in this mode")
	}
	return t.RunFunc(ctx, params.ID)
}
