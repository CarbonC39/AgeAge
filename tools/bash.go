package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"ageage/security"
)

// BashTool executes shell commands.
type BashTool struct {
	Security          *security.Checker
	Timeout           time.Duration
	Supervised        bool
	AutoAllowCommands []string // Commands that skip supervised confirmation
	// ConfirmFunc is called in supervised mode. Returns true to allow execution.
	// In CLI mode, it reads from stdin. In channel mode, it should be nil.
	ConfirmFunc func(cmd string) bool
	// ConfirmationMgr manages async confirmations for channel mode
	ConfirmationMgr *ConfirmationManager
	// ChannelID is the current channel ID (set per-message)
	ChannelID string
}

func (t *BashTool) Name() string { return "bash" }

// SetChannelID sets the channel ID for async confirmations.
// This should be called before Execute in channel mode.
func (t *BashTool) SetChannelID(channelID string) {
	t.ChannelID = channelID
}

func (t *BashTool) Description() string {
	return "Execute a shell command and return its output."
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

	// Supervised mode: check auto-allow list first, then ask for confirmation.
	if t.Supervised {
		if !t.isAutoAllowed(cmd) {
			// Try async confirmation (channel mode)
			if t.ConfirmationMgr != nil && t.ChannelID != "" {
				confirmID, _ := t.ConfirmationMgr.RequestConfirmation(
					fmt.Sprintf("Execute: %s", cmd),
					t.ChannelID,
					5*time.Minute, // 5 minute timeout for user response
				)
				// Return the confirmation ID in the result so the caller can display it
				return fmt.Sprintf("[CONFIRMATION_REQUIRED:%s]", confirmID), nil
			}
			// CLI mode: use sync confirmation
			if t.ConfirmFunc != nil {
				if !t.ConfirmFunc(cmd) {
					return "Command execution denied by user.", nil
				}
			}
		}
	}

	timeout := t.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var execCmd *exec.Cmd
	if runtime.GOOS == "windows" {
		execCmd = exec.CommandContext(ctx, "cmd", "/C", cmd)
	} else {
		execCmd = exec.CommandContext(ctx, "sh", "-c", cmd)
	}

	output, err := execCmd.CombinedOutput()
	result := string(output)

	if ctx.Err() == context.DeadlineExceeded {
		return result, fmt.Errorf("command timed out after %s", timeout)
	}

	if err != nil {
		return result, fmt.Errorf("command failed: %w\nOutput: %s", err, result)
	}

	// Truncate very large outputs.
	const maxOutput = 10000
	if len(result) > maxOutput {
		result = result[:maxOutput] + "\n... (output truncated)"
	}

	return result, nil
}

// isAutoAllowed checks if a command matches any auto-allow pattern.
// Patterns support prefix matching: "ls" matches "ls -la", "git" matches "git status", etc.
func (t *BashTool) isAutoAllowed(cmd string) bool {
	cmdLower := strings.ToLower(strings.TrimSpace(cmd))
	for _, pattern := range t.AutoAllowCommands {
		patternLower := strings.ToLower(strings.TrimSpace(pattern))
		if patternLower == "" {
			continue
		}
		// Exact match or prefix match (command starts with pattern followed by space or end).
		if cmdLower == patternLower {
			return true
		}
		if strings.HasPrefix(cmdLower, patternLower+" ") {
			return true
		}
	}
	return false
}
