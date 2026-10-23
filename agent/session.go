package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"ageage/llm"
)

// SessionManager manages named sessions within .ageage/sessions/.
// Each session has its own CONTEXT.md (injected into the agent's system prompt)
// and history.jsonl (full conversation history, rewritten on every save).
//
// In CLI mode, session IDs are short user-supplied names ("default", "research").
// In channel mode, session IDs are prefixed with a sanitised chatKey so that
// each chat's sessions are namespaced away from CLI sessions and from each other.
type SessionManager struct {
	sessionsDir string // absolute path to .ageage/sessions/
}

// sessionLocks is process-wide so separate SessionManager instances (for
// example the channel server and a cron run) still serialize operations on
// the same on-disk session.
var sessionLocks sync.Map // absolute session directory -> *sync.RWMutex

// SessionInfo describes a single session for listing.
type SessionInfo struct {
	ID          string    // full ID in List; display name in ListWithPrefix for compatibility
	FullID      string    // canonical session ID (directory name)
	DisplayName string    // independent user-facing name; old sessions fall back to ID
	TurnCount   int       // number of completed user→assistant turns
	ModTime     time.Time // last-modified time of the session directory
	Preview     string    // first 50 chars of the last external user message (best-effort)
}

// historyRecord is the on-disk representation of a single llm.Message.
// System messages are intentionally excluded: they are rebuilt fresh on load.
type historyRecord struct {
	Role       string            `json:"role"`
	Content    string            `json:"content,omitempty"`
	Parts      []llm.ContentPart `json:"parts,omitempty"`
	ToolCalls  []llm.ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	InputReply bool              `json:"input_reply,omitempty"`
}

type sessionMetadata struct {
	DisplayName string `json:"display_name,omitempty"`
}

// sanitizeRe matches characters that are not safe in a directory name.
var sanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// IsReservedSessionID reports whether an ID belongs to framework-managed
// session storage rather than the user-visible namespace.
func IsReservedSessionID(id string) bool {
	return strings.HasPrefix(strings.ToLower(id), "cron-")
}

// SanitizeSessionID converts an arbitrary string into a safe directory name.
// Any sequence of characters that is not alphanumeric, a hyphen, or an
// underscore is replaced with a single hyphen.
func SanitizeSessionID(id string) string {
	s := sanitizeRe.ReplaceAllString(id, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "default"
	}
	return s
}

// NewSessionManager creates a SessionManager rooted at <ageageDir>/sessions/.
func NewSessionManager(ageageDir string) *SessionManager {
	return &SessionManager{
		sessionsDir: filepath.Join(ageageDir, "sessions"),
	}
}

// SessionDir returns the directory path for the named session.
func (sm *SessionManager) SessionDir(id string) string {
	return filepath.Join(sm.sessionsDir, id)
}

// ContextPath returns the CONTEXT.md path for the named session.
func (sm *SessionManager) ContextPath(id string) string {
	return filepath.Join(sm.sessionsDir, id, "CONTEXT.md")
}

// HistoryPath returns the history.jsonl path for the named session.
func (sm *SessionManager) HistoryPath(id string) string {
	return filepath.Join(sm.sessionsDir, id, "history.jsonl")
}

// MetadataPath returns the path to the session's display metadata.
func (sm *SessionManager) MetadataPath(id string) string {
	return filepath.Join(sm.SessionDir(id), "metadata.json")
}

// SetDisplayName persists a user-facing session name without changing its
// canonical ID. It only updates existing sessions and uses an atomic replace.
func (sm *SessionManager) SetDisplayName(id, name string) error {
	lock := sm.SessionLock(id)
	lock.Lock()
	defer lock.Unlock()
	info, err := os.Stat(sm.SessionDir(id))
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("session %q is not a directory", id)
	}
	data, err := json.Marshal(sessionMetadata{DisplayName: strings.TrimSpace(name)})
	if err != nil {
		return err
	}
	dir := sm.SessionDir(id)
	tmp, err := os.CreateTemp(dir, ".metadata-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, sm.MetadataPath(id))
}

// DisplayName returns the persisted display name, falling back to the session
// ID for old sessions. It never creates a missing session.
func (sm *SessionManager) DisplayName(id string) (string, error) {
	lock := sm.SessionLock(id)
	lock.RLock()
	defer lock.RUnlock()
	return sm.displayNameSnapshot(id)
}

func (sm *SessionManager) displayNameSnapshot(id string) (string, error) {
	info, err := os.Stat(sm.SessionDir(id))
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("session %q is not a directory", id)
	}
	data, err := os.ReadFile(sm.MetadataPath(id))
	if err != nil {
		if os.IsNotExist(err) {
			return id, nil
		}
		return "", err
	}
	var metadata sessionMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("parse session metadata: %w", err)
	}
	name := strings.TrimSpace(metadata.DisplayName)
	if name == "" {
		return id, nil
	}
	return name, nil
}

// SessionLock returns the process-wide lock for a session. Callers that need
// to coordinate a complete agent run with persistence can hold this lock
// around both operations. SaveHistory and LoadHistory acquire it themselves.
func (sm *SessionManager) SessionLock(id string) *sync.RWMutex {
	path, err := filepath.Abs(sm.SessionDir(id))
	if err != nil {
		path = filepath.Clean(sm.SessionDir(id))
	}
	lock := &sync.RWMutex{}
	actual, _ := sessionLocks.LoadOrStore(path, lock)
	return actual.(*sync.RWMutex)
}

func (sm *SessionManager) lockKey(id string) string {
	path, err := filepath.Abs(sm.SessionDir(id))
	if err != nil {
		return filepath.Clean(sm.SessionDir(id))
	}
	return filepath.Clean(path)
}

// WithSessionLock runs fn while holding the exclusive session lock. It is a
// low-level primitive for operations that do not call LoadHistory or
// SaveHistory. For an atomic load → run → save sequence use
// WithSessionTransaction instead, whose methods are safe under the lock.
func (sm *SessionManager) WithSessionLock(id string, fn func() error) error {
	lock := sm.SessionLock(id)
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

// SessionTransaction holds an exclusive session lock and exposes the
// under-lock load/save operations needed for an atomic agent run. Callers must
// obtain one through WithSessionTransaction; the methods intentionally do not
// reacquire the lock.
type SessionTransaction struct {
	manager *SessionManager
	id      string
}

func (tx *SessionTransaction) SessionID() string { return tx.id }

func (tx *SessionTransaction) LoadHistory() ([]llm.Message, error) {
	return tx.manager.loadHistoryLocked(tx.id)
}

func (tx *SessionTransaction) SaveHistory(msgs []llm.Message) error {
	return tx.manager.saveHistoryLocked(tx.id, msgs)
}

// WithSessionTransaction runs fn while holding the exclusive session lock.
// The transaction methods can safely load, run external code, and save
// without self-deadlocking on the non-reentrant mutex.
func (sm *SessionManager) WithSessionTransaction(id string, fn func(*SessionTransaction) error) error {
	tx, unlock := sm.BeginSessionTransaction(id)
	defer unlock()
	return fn(tx)
}

// BeginSessionTransaction acquires an exclusive lock and returns a transaction
// plus its release function. This form is useful when an existing control flow
// needs to keep the lock around callbacks and an Agent.Run call.
func (sm *SessionManager) BeginSessionTransaction(id string) (*SessionTransaction, func()) {
	lock := sm.SessionLock(id)
	lock.Lock()
	return &SessionTransaction{manager: sm, id: id}, lock.Unlock
}

// BeginSessionTransactionContext acquires an exclusive session lock while
// honoring cancellation. Polling TryLock avoids leaving a goroutine blocked
// on sync.RWMutex after a scheduled run has timed out.
func (sm *SessionManager) BeginSessionTransactionContext(ctx context.Context, id string) (*SessionTransaction, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
	}
	lock := sm.SessionLock(id)
	if lock.TryLock() {
		if err := ctx.Err(); err != nil {
			lock.Unlock()
			return nil, nil, err
		}
		return &SessionTransaction{manager: sm, id: id}, lock.Unlock, nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-ticker.C:
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			default:
			}
			if lock.TryLock() {
				if err := ctx.Err(); err != nil {
					lock.Unlock()
					return nil, nil, err
				}
				return &SessionTransaction{manager: sm, id: id}, lock.Unlock, nil
			}
		}
	}
}

// WithSessionReadLock runs fn while holding the shared session lock.
func (sm *SessionManager) WithSessionReadLock(id string, fn func() error) error {
	lock := sm.SessionLock(id)
	lock.RLock()
	defer lock.RUnlock()
	return fn()
}

func (sm *SessionManager) lockTwoSessions(firstID, secondID string) func() {
	first := sm.SessionLock(firstID)
	second := sm.SessionLock(secondID)
	if first == second {
		first.Lock()
		return first.Unlock
	}
	if sm.lockKey(firstID) > sm.lockKey(secondID) {
		first, second = second, first
	}
	first.Lock()
	second.Lock()
	return func() {
		second.Unlock()
		first.Unlock()
	}
}

// EnsureSession creates the directory and empty placeholder files for a session.
// Safe to call repeatedly; existing files are left untouched.
func (sm *SessionManager) EnsureSession(id string) error {
	lock := sm.SessionLock(id)
	lock.Lock()
	defer lock.Unlock()
	return sm.ensureSessionLocked(id)
}

func (sm *SessionManager) ensureSessionLocked(id string) error {
	dir := sm.SessionDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(sm.sessionsDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"CONTEXT.md", "history.jsonl"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, []byte(""), 0o600); err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
		}
		if err := os.Chmod(p, 0o600); err != nil {
			return fmt.Errorf("protect %s: %w", name, err)
		}
	}
	return nil
}

// CreateSession creates a new session and fails if its directory already
// exists. Unlike EnsureSession, this is suitable for user-facing "new"
// operations where silently attaching to an existing session would be unsafe.
func (sm *SessionManager) CreateSession(id string) error {
	if IsReservedSessionID(id) {
		return fmt.Errorf("session name %q is reserved", id)
	}
	lock := sm.SessionLock(id)
	lock.Lock()
	defer lock.Unlock()
	dir := sm.SessionDir(id)
	if err := os.MkdirAll(sm.sessionsDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(sm.sessionsDir, 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("session %q already exists", id)
		}
		return err
	}
	if err := sm.ensureSessionLocked(id); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	return nil
}

// List returns all sessions sorted by modification time (newest first).
func (sm *SessionManager) List() ([]SessionInfo, error) {
	entries, err := os.ReadDir(sm.sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var infos []SessionInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		// Task-owned cron sessions are an internal execution detail and must
		// not appear in normal user session discovery or operations.
		if IsReservedSessionID(id) {
			continue
		}

		// Count turns and capture a preview of the last user message.
		turns := 0
		preview := ""
		// SaveHistory publishes complete snapshots by atomic rename. Read that
		// committed file directly instead of waiting for a transaction lock: an
		// agent may hold it while paused in ask_user for an arbitrary duration.
		if msgs, err := sm.loadHistorySnapshot(id); err == nil {
			for _, m := range msgs {
				if m.Role == "user" {
					if !m.InputReply {
						turns++
					}
					if m.InputReply {
						continue
					}
					// Keep updating so we end up with the LAST user message.
					t := m.Content
					if strings.TrimSpace(t) == "" {
						t = m.TextContent()
						if strings.HasPrefix(t, "[File ") {
							t = "[File attachment]"
						}
					}
					if strings.TrimSpace(t) == "" {
						for _, part := range m.Parts {
							if part.Type == "image_url" {
								t = "[Image attachment]"
								break
							}
						}
					}
					if len([]rune(t)) > 50 {
						t = string([]rune(t)[:50]) + "…"
					}
					preview = t
				}
			}
		}

		// Use history.jsonl mtime as proxy for last-active time.
		modTime := time.Time{}
		if fi, err := os.Stat(sm.HistoryPath(id)); err == nil {
			modTime = fi.ModTime()
		}

		displayName, err := sm.displayNameSnapshot(id)
		if err != nil || displayName == "" {
			displayName = id
		}
		infos = append(infos, SessionInfo{ID: id, FullID: id, DisplayName: displayName, TurnCount: turns, ModTime: modTime, Preview: preview})
	}

	// Sort newest first.
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].ModTime.After(infos[j].ModTime)
	})
	return infos, nil
}

// FindByPrefix returns sessions whose ID starts with the given prefix.
// Returns (exact match, prefix matches, error). Used for tab-style disambiguation.
func (sm *SessionManager) FindByPrefix(prefix string) (exact *SessionInfo, prefixMatches []SessionInfo, err error) {
	all, err := sm.List()
	if err != nil {
		return nil, nil, err
	}
	for i, si := range all {
		if si.ID == prefix {
			copy := all[i]
			exact = &copy
		} else if strings.HasPrefix(si.ID, prefix) {
			prefixMatches = append(prefixMatches, all[i])
		}
	}
	return exact, prefixMatches, nil
}

// ListWithPrefix returns only sessions whose IDs start with prefix.
// SessionInfo.ID retains the historical prefix-relative canonical ID, while
// DisplayName carries the independent user-facing name.
func (sm *SessionManager) ListWithPrefix(prefix string) ([]SessionInfo, error) {
	all, err := sm.List()
	if err != nil {
		return nil, err
	}
	var filtered []SessionInfo
	for _, si := range all {
		if prefix == "" || si.ID == prefix || strings.HasPrefix(si.ID, prefix+"-") {
			display := si.ID
			if prefix != "" && strings.HasPrefix(si.ID, prefix+"-") {
				display = strings.TrimPrefix(si.ID, prefix+"-")
			} else if si.ID == prefix {
				display = "default"
			}
			displayName := si.DisplayName
			if displayName == "" || displayName == si.FullID {
				// Preserve the historical room-relative name for legacy sessions.
				displayName = display
			}
			filtered = append(filtered, SessionInfo{ID: display, FullID: si.FullID, DisplayName: displayName, TurnCount: si.TurnCount, ModTime: si.ModTime, Preview: si.Preview})
		}
	}
	return filtered, nil
}

// Rename moves a session directory from oldID to newID.
// Returns an error if newID already exists or oldID does not exist.
func (sm *SessionManager) Rename(oldID, newID string) error {
	if IsReservedSessionID(oldID) || IsReservedSessionID(newID) {
		return fmt.Errorf("cron session names are reserved")
	}
	unlock := sm.lockTwoSessions(oldID, newID)
	defer unlock()
	oldDir := sm.SessionDir(oldID)
	newDir := sm.SessionDir(newID)
	if _, err := os.Stat(oldDir); os.IsNotExist(err) {
		return fmt.Errorf("session %q does not exist", oldID)
	}
	if _, err := os.Stat(newDir); err == nil {
		return fmt.Errorf("session %q already exists", newID)
	}
	return os.Rename(oldDir, newDir)
}

// Delete removes a session directory permanently.
// The caller is responsible for ensuring the session is not currently active.
func (sm *SessionManager) Delete(id string) error {
	lock := sm.SessionLock(id)
	lock.Lock()
	defer lock.Unlock()
	return os.RemoveAll(sm.SessionDir(id))
}

// Trash moves a session directory to the system trash.
// Falls back to os.RemoveAll when the trash operation fails.
func (sm *SessionManager) Trash(id string) error {
	lock := sm.SessionLock(id)
	lock.Lock()
	defer lock.Unlock()
	dir := sm.SessionDir(id)
	if err := trashDir(dir); err != nil {
		return os.RemoveAll(dir)
	}
	return nil
}

// trashDir sends a directory to the OS trash/recycle bin.
func trashDir(dir string) error {
	switch runtime.GOOS {
	case "windows":
		// PowerShell: Shell.Application sends the item to the Recycle Bin.
		script := fmt.Sprintf(
			`$sh = New-Object -ComObject Shell.Application; $sh.Namespace(0).ParseName('%s').InvokeVerb('delete')`,
			strings.ReplaceAll(dir, "'", "''"),
		)
		return exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Run()
	case "darwin":
		sanitized := strings.ReplaceAll(dir, `"`, `\"`)
		sanitized = strings.ReplaceAll(sanitized, "\n", "")
		sanitized = strings.ReplaceAll(sanitized, "\r", "")
		script := fmt.Sprintf(`tell application "Finder" to delete POSIX file "%s"`, sanitized)
		return exec.Command("osascript", "-e", script).Run()
	default: // Linux and others
		if _, err := exec.LookPath("trash-put"); err == nil {
			return exec.Command("trash-put", dir).Run()
		}
		if _, err := exec.LookPath("gio"); err == nil {
			return exec.Command("gio", "trash", dir).Run()
		}
		return fmt.Errorf("no trash utility found")
	}
}

// SaveHistory rewrites the history.jsonl for a session with the provided messages.
// System messages are skipped: they are rebuilt fresh on next load.
// This method is safe to call after summarisation — it overwrites the entire file
// rather than appending, so the saved state always matches the in-memory state.
//
// The write is atomic: data is first written to a sibling .tmp file, then renamed
// into place. This prevents history corruption if two callers race (e.g. parallel
// channel handlers both trying to save the same session concurrently).
func (sm *SessionManager) SaveHistory(id string, msgs []llm.Message) error {
	lock := sm.SessionLock(id)
	lock.Lock()
	defer lock.Unlock()
	return sm.saveHistoryLocked(id, msgs)
}

func (sm *SessionManager) saveHistoryLocked(id string, msgs []llm.Message) error {
	if err := sm.ensureSessionLocked(id); err != nil {
		return err
	}
	path := sm.HistoryPath(id)
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".history-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	enc := json.NewEncoder(f)
	writeErr := error(nil)
	for _, m := range msgs {
		if m.Role == "system" {
			continue // rebuilt on load via buildSystemPrompt
		}
		rec := historyRecord{
			Role:       m.Role,
			Content:    m.Content,
			Parts:      m.Parts,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			InputReply: m.InputReply,
		}
		if err := enc.Encode(rec); err != nil {
			writeErr = err
			break
		}
	}
	if writeErr != nil {
		_ = f.Close()
		return writeErr
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadHistory reads history.jsonl for a session and returns the messages.
// Returns nil (no error) when the file is empty or does not exist.
// System messages are never stored on disk and are not returned here.
func (sm *SessionManager) LoadHistory(id string) ([]llm.Message, error) {
	lock := sm.SessionLock(id)
	lock.Lock()
	defer lock.Unlock()
	return sm.loadHistoryLocked(id)
}

func (sm *SessionManager) loadHistoryLocked(id string) ([]llm.Message, error) {
	return sm.readHistoryFile(id, true)
}

// loadHistorySnapshot reads the last fully committed history file without the
// session lock. SaveHistory writes a sibling file and publishes it with one
// atomic rename, so readers see either the previous complete history or the
// new complete history; they never observe a partial write.
func (sm *SessionManager) loadHistorySnapshot(id string) ([]llm.Message, error) {
	return sm.readHistoryFile(id, false)
}

func (sm *SessionManager) readHistoryFile(id string, protectPermissions bool) ([]llm.Message, error) {
	path := sm.HistoryPath(id)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	if protectPermissions {
		if err := f.Chmod(0o600); err != nil {
			return nil, err
		}
	}

	var msgs []llm.Message
	// Attachment budgets belong to the input pipeline. Read one JSONL record
	// at a time without imposing a second, smaller Scanner token limit on
	// base64 images that were already accepted and persisted successfully.
	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		var rec historyRecord
		if json.Unmarshal(line, &rec) == nil {
			msgs = append(msgs, llm.Message{Role: rec.Role, Content: rec.Content, Parts: rec.Parts, ToolCalls: rec.ToolCalls, ToolCallID: rec.ToolCallID, InputReply: rec.InputReply})
		}
		if readErr == io.EOF {
			break
		}
	}
	return msgs, nil
}
