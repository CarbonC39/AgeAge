package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ageage/security"
)

// defaultMaxOutputBytes is the default cap on combined stdout+stderr.
// Output beyond this limit is silently discarded; a truncation notice is appended.
// This prevents OOM when the agent runs commands that emit large volumes of data
// (e.g. log tails, large file conversions, recursive directory listings).
const defaultMaxOutputBytes = 4 * 1024 * 1024 // 4 MB

// maxAgentRunes is the maximum number of runes returned to the agent.
// The subprocess buffer may hold up to defaultMaxOutputBytes; the agent only
// sees this smaller slice to avoid bloating conversation history.
const maxAgentRunes = 8000

// BashTool executes shell commands.
type BashTool struct {
	Security          *security.Checker
	Timeout           time.Duration
	Supervised        bool
	AutoAllowCommands []string // Commands that skip supervised confirmation
	// MaxOutputBytes caps combined stdout+stderr before it enters memory.
	// Zero means use defaultMaxOutputBytes.
	MaxOutputBytes int
	// WorkDir is the working directory for the subprocess.
	// Defaults to the process CWD when empty.
	WorkDir string
	// PassthroughEnvVars holds additional env var name prefixes that are
	// allowed through the safe-env filter (case-insensitive prefix match).
	PassthroughEnvVars []string
	// ConfirmFunc is called in supervised mode. Returns true to allow execution.
	ConfirmFunc     func(cmd string) bool
	ConfirmationMgr *ConfirmationManager
	ChannelID       string
}

func (t *BashTool) Name() string { return "bash" }

func (t *BashTool) SetChannelID(channelID string) { t.ChannelID = channelID }

func (t *BashTool) Description() string {
	if runtime.GOOS == "windows" {
		return "Execute a PowerShell command and return its output. Use PowerShell syntax."
	}
	return "Execute a shell command and return its output. Use sh/bash syntax."
}

func (t *BashTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute.",
			},
		},
		"required": []string{"command"},
	}
}

func (t *BashTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	cmd := strings.TrimSpace(params.Command)
	if cmd == "" {
		return "", fmt.Errorf("command cannot be empty")
	}

	// Security check.
	if safe, reason := t.Security.IsCommandSafe(cmd); !safe {
		return "", fmt.Errorf("command blocked: %s", reason)
	}

	// Supervised mode confirmation.
	if t.Supervised && !t.isAutoAllowed(cmd) {
		if t.ConfirmationMgr != nil && t.ChannelID != "" {
			confirmID, _ := t.ConfirmationMgr.RequestConfirmation(
				fmt.Sprintf("Execute: %s", cmd),
				t.ChannelID,
				5*time.Minute,
			)
			return fmt.Sprintf("[CONFIRMATION_REQUIRED:%s]", confirmID), nil
		}
		if t.ConfirmFunc != nil && !t.ConfirmFunc(cmd) {
			return "Command execution denied by user.", nil
		}
	}

	timeout := t.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	execCmd := t.buildCmd(ctx, cmd)

	// OOM protection: cap combined stdout+stderr in memory.
	cap := t.MaxOutputBytes
	if cap <= 0 {
		cap = defaultMaxOutputBytes
	}
	lw := &limitedWriter{limit: cap}
	execCmd.Stdout = lw
	execCmd.Stderr = lw

	// Restricted environment: agent cannot read sensitive env vars.
	execCmd.Env = buildSafeEnv(t.PassthroughEnvVars)

	if t.WorkDir != "" {
		execCmd.Dir = t.WorkDir
	}

	runErr := execCmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		return lw.string(), fmt.Errorf("command timed out after %s", timeout)
	}

	result := lw.string()
	if lw.truncated {
		result += fmt.Sprintf("\n... (output truncated at %d bytes)", cap)
	}

	// Further trim to the agent-visible rune limit.
	if utf8.RuneCountInString(result) > maxAgentRunes {
		b := []rune(result)
		result = string(b[:maxAgentRunes]) + "\n... (truncated)"
	}

	if runErr != nil {
		return result, fmt.Errorf("command failed: %w", runErr)
	}
	return result, nil
}

// buildCmd constructs the platform-appropriate exec.Cmd.
// On Windows: prefers pwsh (PowerShell Core 7+) for better POSIX-command
// compatibility; falls back to Windows PowerShell 5.1.
// On Unix: uses sh -c.
func (t *BashTool) buildCmd(ctx context.Context, cmd string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		shell := windowsShell()
		// -NonInteractive prevents prompts; -NoProfile skips slow profile loading.
		// Wrap in a script block so exit codes from native commands propagate.
		script := fmt.Sprintf(
			"$ErrorActionPreference='Continue'; $PSNativeCommandUseErrorActionPreference=$false; %s; exit $LASTEXITCODE",
			cmd,
		)
		return exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-Command", script)
	}
	return exec.CommandContext(ctx, "sh", "-c", cmd)
}

// isAutoAllowed checks if a command matches any auto-allow pattern (prefix match).
func (t *BashTool) isAutoAllowed(cmd string) bool {
	cmdLower := strings.ToLower(strings.TrimSpace(cmd))
	for _, pattern := range t.AutoAllowCommands {
		pl := strings.ToLower(strings.TrimSpace(pattern))
		if pl == "" {
			continue
		}
		if cmdLower == pl || strings.HasPrefix(cmdLower, pl+" ") {
			return true
		}
	}
	return false
}

// windowsShell returns "pwsh" if PowerShell Core is available, else "powershell".
var windowsShell = sync.OnceValue(func() string {
	if _, err := exec.LookPath("pwsh"); err == nil {
		return "pwsh"
	}
	return "powershell"
})

// ── limitedWriter ─────────────────────────────────────────────────────────────

// limitedWriter buffers at most limit bytes of combined stdout+stderr.
// Writes beyond the limit are discarded; the truncated flag is set.
// A mutex guards concurrent writes from stdout and stderr goroutines.
type limitedWriter struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	limit    int
	truncated bool
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	cur := lw.buf.Len()
	if cur >= lw.limit {
		lw.truncated = true
		return len(p), nil // discard without error so the command keeps running
	}
	rem := lw.limit - cur
	if len(p) > rem {
		p = p[:rem]
		lw.truncated = true
	}
	n, err := lw.buf.Write(p)
	return n, err
}

func (lw *limitedWriter) string() string {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.buf.String()
}

// ── safe environment ──────────────────────────────────────────────────────────

// safeEnvPrefixes enumerates env var name prefixes that are safe to forward.
// This is an allowlist: only vars whose names (case-insensitively) match one
// of these prefixes are passed to the subprocess. Everything else is dropped,
// including API keys, tokens, and other credentials that the parent process
// might hold.
var safeEnvPrefixes = []string{
	// Core POSIX / shell
	"PATH", "HOME", "USER", "SHELL", "TERM", "COLORTERM",
	"LANG", "LC_", "TMPDIR", "TEMP", "TMP",
	// Windows system
	"USERNAME", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
	"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "PSMODULEPATH",
	"PROCESSOR_",
	// Go
	"GOPATH", "GOROOT", "GOENV", "GOBIN", "GOPROXY", "GONOSUMCHECK",
	// Java
	"JAVA_HOME", "JAVA_OPTS", "JVM_",
	// Python
	"VIRTUAL_ENV", "CONDA_", "PYTHONPATH", "PYTHONHOME",
	// Node / npm
	"NODE_PATH", "NODE_ENV", "NPM_CONFIG_PREFIX",
	// Rust
	"CARGO_HOME", "RUSTUP_HOME",
	// Git (identity only, not credentials)
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
	"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	"GIT_CONFIG_", "GIT_EDITOR",
	// SSH agent socket (needed for git-over-SSH)
	"SSH_AUTH_SOCK", "SSH_AGENT_PID",
	// Display / XDG (Linux desktop environments)
	"DISPLAY", "WAYLAND_DISPLAY", "XDG_",
}

// credentialSuffixes are always blocked regardless of prefix allowlist.
// Any env var whose name ends with one of these is considered a credential.
var credentialSuffixes = []string{
	"_KEY", "_APIKEY", "_API_KEY",
	"_TOKEN", "_ACCESS_TOKEN", "_REFRESH_TOKEN",
	"_SECRET", "_SECRET_KEY",
	"_PASSWORD", "_PASSWD", "_PASS",
	"_CREDENTIAL", "_CREDENTIALS",
}

// buildSafeEnv constructs a clean environment for subprocess execution.
// extra contains additional name prefixes (from config) to allow through.
func buildSafeEnv(extra []string) []string {
	allowed := make([]string, 0, len(safeEnvPrefixes)+len(extra))
	for _, p := range safeEnvPrefixes {
		allowed = append(allowed, strings.ToUpper(p))
	}
	for _, p := range extra {
		allowed = append(allowed, strings.ToUpper(p))
	}

	var env []string
	for _, kv := range os.Environ() {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		name := strings.ToUpper(kv[:idx])

		// Hard block: anything that looks like a credential.
		if isCredentialVar(name) {
			continue
		}

		// Allowlist: name must start with one of the safe prefixes.
		for _, pfx := range allowed {
			if strings.HasPrefix(name, pfx) {
				env = append(env, kv)
				break
			}
		}
	}
	return env
}

// isCredentialVar returns true when the env var name looks like a credential.
func isCredentialVar(nameUpper string) bool {
	for _, sfx := range credentialSuffixes {
		if strings.HasSuffix(nameUpper, sfx) {
			return true
		}
	}
	return false
}
