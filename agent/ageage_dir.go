package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// workspaceSettings is the schema of .ageage/settings.json.
type workspaceSettings struct {
	AutoAllowCommands []string `json:"auto_allow_commands"`
}

// EnsureAgeAgeDir creates the .ageage directory structure inside workDir.
// Idempotent: safe to call on every startup.
//
// Layout:
//
//	<workDir>/
//	  .gitignore               — .ageage/ is appended here if the file exists
//	  .ageage/
//	    settings.json          — persists always-allow command prefixes (global)
//	    tmp/                   — scratch space for temporary data
//	    sessions/
//	      default/
//	        CONTEXT.md         — session-specific notes injected into system prompt
//	        history.jsonl      — full conversation history (local only)
func EnsureAgeAgeDir(workDir string) error {
	ageageDir := filepath.Join(workDir, ".ageage")
	tmpDir := filepath.Join(ageageDir, "tmp")

	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}

	// If the workspace has a .gitignore, add .ageage/ to it so the entire
	// metadata directory stays out of the project's git history.
	addToGitignore(workDir, ".ageage/")

	// settings.json — initially empty auto_allow_commands list.
	settingsPath := filepath.Join(ageageDir, "settings.json")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		data, _ := json.Marshal(workspaceSettings{AutoAllowCommands: []string{}})
		if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
			return err
		}
	}

	// Ensure the default session exists.
	sm := NewSessionManager(ageageDir)
	if err := sm.EnsureSession("default"); err != nil {
		return err
	}

	return nil
}

// addToGitignore appends entry to workDir/.gitignore if that file exists and
// does not already contain the entry. A no-op when the file is absent.
func addToGitignore(workDir, entry string) {
	gitignorePath := filepath.Join(workDir, ".gitignore")
	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		return // file doesn't exist or unreadable — skip silently
	}

	// Check if the entry is already present (line-by-line, ignoring whitespace and
	// trailing slashes so both ".ageage" and ".ageage/" are treated as equivalent).
	normalized := strings.TrimRight(strings.TrimSpace(entry), "/")
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimRight(strings.TrimSpace(line), "/") == normalized {
			return // already there
		}
	}

	// Append with a leading newline if the file doesn't already end with one.
	suffix := entry + "\n"
	if len(data) > 0 && data[len(data)-1] != '\n' {
		suffix = "\n" + suffix
	}
	_ = os.WriteFile(gitignorePath, append(data, []byte(suffix)...), 0o644)
}

// LoadWorkspaceAutoAllowCommands reads the auto-allow command prefixes from
// .ageage/settings.json. Returns nil if the file is missing or unreadable.
func LoadWorkspaceAutoAllowCommands(settingsPath string) []string {
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil
	}
	var s workspaceSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil
	}
	return s.AutoAllowCommands
}

// AppendAlwaysAllow records a new command prefix in .ageage/settings.json.
// The prefix is extracted as the first whitespace-separated token of operation.
// Duplicate prefixes are silently ignored.
func AppendAlwaysAllow(settingsPath, operation string) {
	prefix := strings.SplitN(strings.TrimSpace(operation), " ", 2)[0]
	if prefix == "" {
		return
	}

	var s workspaceSettings
	if data, err := os.ReadFile(settingsPath); err == nil {
		_ = json.Unmarshal(data, &s)
	}

	for _, cmd := range s.AutoAllowCommands {
		if cmd == prefix {
			return // already recorded
		}
	}
	s.AutoAllowCommands = append(s.AutoAllowCommands, prefix)

	if data, err := json.MarshalIndent(s, "", "  "); err == nil {
		_ = os.WriteFile(settingsPath, data, 0o644)
	}
}
