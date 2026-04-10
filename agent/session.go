package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

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

// SessionInfo describes a single session for listing.
type SessionInfo struct {
	ID           string // full session ID (directory name)
	MessageCount int    // number of user+assistant messages in history
}

// historyRecord is the on-disk representation of a single llm.Message.
// System messages are intentionally excluded: they are rebuilt fresh on load.
type historyRecord struct {
	Role       string            `json:"role"`
	Content    string            `json:"content,omitempty"`
	Parts      []llm.ContentPart `json:"parts,omitempty"`
	ToolCalls  []llm.ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

// sanitizeRe matches characters that are not safe in a directory name.
var sanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

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

// EnsureSession creates the directory and empty placeholder files for a session.
// Safe to call repeatedly; existing files are left untouched.
func (sm *SessionManager) EnsureSession(id string) error {
	dir := sm.SessionDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"CONTEXT.md", "history.jsonl"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
		}
	}
	return nil
}

// List returns all sessions present in the sessions directory.
// Sessions are returned in directory-enumeration order (typically alphabetical).
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
		count := 0
		if msgs, err := sm.LoadHistory(id); err == nil {
			for _, m := range msgs {
				if m.Role == "user" || m.Role == "assistant" {
					count++
				}
			}
		}
		infos = append(infos, SessionInfo{ID: id, MessageCount: count})
	}
	return infos, nil
}

// ListWithPrefix returns only sessions whose IDs start with prefix.
// The display name returned in SessionInfo.ID has the prefix stripped.
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
			filtered = append(filtered, SessionInfo{ID: display, MessageCount: si.MessageCount})
		}
	}
	return filtered, nil
}

// Delete removes a session directory permanently.
// The caller is responsible for ensuring the session is not currently active.
func (sm *SessionManager) Delete(id string) error {
	return os.RemoveAll(sm.SessionDir(id))
}

// SaveHistory rewrites the history.jsonl for a session with the provided messages.
// System messages are skipped: they are rebuilt fresh on next load.
// This method is safe to call after summarisation — it overwrites the entire file
// rather than appending, so the saved state always matches the in-memory state.
func (sm *SessionManager) SaveHistory(id string, msgs []llm.Message) error {
	if err := sm.EnsureSession(id); err != nil {
		return err
	}
	path := sm.HistoryPath(id)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
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
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return nil
}

// LoadHistory reads history.jsonl for a session and returns the messages.
// Returns nil (no error) when the file is empty or does not exist.
// System messages are never stored on disk and are not returned here.
func (sm *SessionManager) LoadHistory(id string) ([]llm.Message, error) {
	path := sm.HistoryPath(id)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var msgs []llm.Message
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec historyRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // skip malformed lines rather than failing hard
		}
		msgs = append(msgs, llm.Message{
			Role:       rec.Role,
			Content:    rec.Content,
			Parts:      rec.Parts,
			ToolCalls:  rec.ToolCalls,
			ToolCallID: rec.ToolCallID,
		})
	}
	return msgs, scanner.Err()
}
