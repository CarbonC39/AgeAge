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

// EnsureAgeAgeDir creates the .ageage directory structure in workDir.
// Idempotent: safe to call on every startup.
//
// Layout:
//
//	.ageage/
//	  .gitignore      — ignores tmp/
//	  CONTEXT.md      — working notes injected into the system prompt
//	  settings.json   — persists always-allow command prefixes
//	  tmp/            — scratch space for temporary data
func EnsureAgeAgeDir(workDir string) error {
	ageageDir := filepath.Join(workDir, ".ageage")
	tmpDir := filepath.Join(ageageDir, "tmp")

	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}

	// .gitignore — only tmp/ is ignored; CONTEXT.md and settings.json are tracked.
	gitignorePath := filepath.Join(ageageDir, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		if err := os.WriteFile(gitignorePath, []byte("tmp/\n"), 0o644); err != nil {
			return err
		}
	}

	// CONTEXT.md — empty placeholder (populated by the agent over time).
	contextPath := filepath.Join(ageageDir, "CONTEXT.md")
	if _, err := os.Stat(contextPath); os.IsNotExist(err) {
		if err := os.WriteFile(contextPath, []byte(""), 0o644); err != nil {
			return err
		}
	}

	// settings.json — initially empty auto_allow_commands list.
	settingsPath := filepath.Join(ageageDir, "settings.json")
	if _, err := os.Stat(settingsPath); os.IsNotExist(err) {
		data, _ := json.Marshal(workspaceSettings{AutoAllowCommands: []string{}})
		if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
			return err
		}
	}

	return nil
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
