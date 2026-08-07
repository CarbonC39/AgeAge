package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Checker provides security checking for commands and file paths.
type Checker struct {
	blockedCommands []string
	allowedRoots    []string
	forbiddenRoots  []string
	workspace       string
	blockedFiles    []string // hardcoded per-file blocks (e.g. credentials.toml)
	forbidRM        bool     // blocks rm and its aliases in bash commands
}

// SetForbidRM enables or disables blocking of rm (permanent deletion) commands.
// The agent is steered towards recoverable system-trash deletion instead.
func (c *Checker) SetForbidRM(enabled bool) { c.forbidRM = enabled }

// NewChecker creates a new security checker.
func NewChecker(workspace string, blockedCmds, allowedRoots, forbiddenRoots []string) *Checker {
	// Resolve a directory path to its canonical absolute form, following symlinks.
	// Falls back to filepath.Abs if the directory does not exist yet.
	resolveDir := func(path string) string {
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = filepath.Clean(path)
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			return resolved
		}
		return abs
	}

	normalized := make([]string, len(allowedRoots))
	for i, r := range allowedRoots {
		normalized[i] = resolveDir(r)
	}

	normalizedForbidden := make([]string, len(forbiddenRoots))
	for i, r := range forbiddenRoots {
		normalizedForbidden[i] = resolveDir(r)
	}

	return &Checker{
		blockedCommands: blockedCmds,
		allowedRoots:    normalized,
		forbiddenRoots:  normalizedForbidden,
		workspace:       resolveDir(workspace),
	}
}

// IsCommandSafe checks if a shell command is safe to execute.
// Returns (true, "") if safe, or (false, reason) if blocked.
func (c *Checker) IsCommandSafe(cmd string) (bool, string) {
	lower := strings.ToLower(strings.TrimSpace(cmd))

	// Hard rule: never allow permanent deletion via rm.
	if c.forbidRM && isRMCommand(lower) {
		return false, "command permanently deletes files (rm), which is disabled by security.forbid_rm. " +
			"Use the system trash for recoverable deletion: `gio trash <path>` (Linux), " +
			"`trash <path>` (macOS), or move to the Recycle Bin on Windows."
	}

	for _, blocked := range c.blockedCommands {
		if strings.Contains(lower, strings.ToLower(blocked)) {
			return false, fmt.Sprintf("command contains blocked pattern: %q", blocked)
		}
	}

	return true, ""
}

// isRMCommand reports whether a lowercased shell command invokes rm (or its
// platform aliases) as the command word — never as an argument. It splits the
// command on shell control operators so `cat rm` (reading a file named "rm")
// stays allowed while `rm -rf x` and `echo a && rm x` are blocked.
func isRMCommand(cmd string) bool {
	tokens := shellTokens(cmd)
	segmentStart := true // true at command start and right after a control operator
	prev := ""
	envPrefix := false // after `env`, skip NAME=VALUE assignments

	for _, tok := range tokens {
		lower := tok

		if isShellControlOp(lower) {
			segmentStart = true
			prev = ""
			envPrefix = false
			continue
		}

		// `env` may be followed by arbitrary NAME=VALUE assignments before the
		// real command; those are transparent to command detection.
		if envPrefix {
			if strings.Contains(lower, "=") {
				prev = lower
				continue
			}
			envPrefix = false
			if isRMWord(lower) {
				return true
			}
			prev = lower
			segmentStart = false
			continue
		}

		if segmentStart && isRMWord(lower) {
			return true
		}
		if (prev == "sudo" || prev == "command") && isRMWord(lower) {
			return true
		}
		if lower == "env" {
			envPrefix = true
		}

		segmentStart = false
		prev = lower
	}
	return false
}

// isRMWord matches rm and its common permanent-deletion aliases.
// Linux/macOS: rm (+ absolute paths). Windows PowerShell: Remove-Item, del,
// erase (all Remove-Item aliases). rmdir/rd are intentionally not matched —
// they only remove empty directories and are not "rm".
func isRMWord(lower string) bool {
	switch lower {
	case "rm", "/bin/rm", "/usr/bin/rm", "remove-item", "del", "erase":
		return true
	}
	return false
}

// isShellControlOp reports whether a token is a shell control operator that
// starts a new command (and therefore resets command-word position).
func isShellControlOp(tok string) bool {
	switch tok {
	case ";", "&", "&&", "||", "|", "(", ")":
		return true
	}
	return false
}

// shellTokens splits a shell command into words and control operators,
// keeping quoted strings intact. Multi-character operators (&&, ||) are
// emitted as single tokens so command boundaries are detectable.
func shellTokens(cmd string) []string {
	var tokens []string
	var cur strings.Builder
	var quote byte
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	i := 0
	for i < len(cmd) {
		ch := cmd[i]
		switch {
		case quote != 0:
			cur.WriteByte(ch)
			if ch == quote {
				quote = 0
			}
			i++
		case ch == '\'' || ch == '"':
			quote = ch
			cur.WriteByte(ch)
			i++
		case ch == '&' || ch == '|':
			flush()
			if i+1 < len(cmd) && cmd[i+1] == ch {
				tokens = append(tokens, cmd[i:i+2])
				i += 2
			} else {
				tokens = append(tokens, string(ch))
				i++
			}
		case strings.ContainsRune(" \t\n;<()", rune(ch)):
			flush()
			if strings.ContainsRune(";()", rune(ch)) {
				tokens = append(tokens, string(ch))
			}
			i++
		default:
			cur.WriteByte(ch)
			i++
		}
	}
	flush()
	return tokens
}

// Workspace returns the configured workspace root (canonical absolute path).
func (c *Checker) Workspace() string { return c.workspace }

// BlockFile permanently blocks access to the given file path.
// This is a hardcoded protection that cannot be overridden by config.
// Canonicalises the path via filepath.Clean before storing.
func (c *Checker) BlockFile(path string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	c.blockedFiles = append(c.blockedFiles, abs)
}

// resolvePath converts path to an absolute path without resolving symlinks.
// Relative paths are resolved against the workspace root, NOT the process CWD.
// Absolute paths are cleaned and returned unchanged.
func (c *Checker) resolvePath(path string) string {
	// Normalise separators so both "/" and "\" work on Windows.
	path = filepath.FromSlash(path)
	path = strings.TrimSpace(path)
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(c.workspace, path)
}

// CheckPath resolves path to its canonical form, verifies it is within allowed
// scope, and returns the symlink-resolved path that is safe to pass to OS calls.
//
// Using the returned path (instead of the original) for OS operations prevents
// TOCTOU attacks where a symlink is swapped between the security check and the
// actual file operation.
//
// Returns an error if the path is empty, forbidden, or outside the allowed scope.
func (c *Checker) CheckPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path must not be empty")
	}

	// Step 1: resolve relative paths against workspace.
	absPath := c.resolvePath(path)

	// Step 2: resolve all symlinks to obtain the canonical path. For a path
	// containing components that do not exist yet, resolve the nearest existing
	// ancestor and append the missing suffix. Never fall back to the unresolved
	// path: MkdirAll would otherwise follow an earlier symlink outside the
	// workspace when creating the missing directories.
	resolved, err := resolveFromExistingAncestor(absPath)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path %q: %w", path, err)
	}

	// Step 3a: check hardcoded blocked files (highest priority, not configurable).
	cleanResolved := filepath.Clean(resolved)
	for _, blocked := range c.blockedFiles {
		if cleanResolved == blocked {
			return "", fmt.Errorf("path %q is system-protected and cannot be accessed by the agent", resolved)
		}
	}

	// Step 3b: check forbidden roots (high priority).
	for _, forbidden := range c.forbiddenRoots {
		if isSubPath(resolved, forbidden) {
			return "", fmt.Errorf("path %q is inside forbidden root %q", resolved, forbidden)
		}
	}

	// Step 4: check workspace.
	if isSubPath(resolved, c.workspace) {
		return resolved, nil
	}

	// Step 5: check additional allowed roots.
	for _, root := range c.allowedRoots {
		if isSubPath(resolved, root) {
			return resolved, nil
		}
	}

	return "", fmt.Errorf("path %q is outside workspace %q and not in any allowed root", resolved, c.workspace)
}

// resolveFromExistingAncestor canonicalises path even when one or more trailing
// components do not exist. Existing symlinks are always resolved before the
// missing suffix is appended.
func resolveFromExistingAncestor(path string) (string, error) {
	current := filepath.Clean(path)
	var missing []string

	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing ancestor")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// isSubPath checks if child is inside (or equal to) parent directory.
func isSubPath(child, parent string) bool {
	// Clean both paths to remove redundant separators.
	child = filepath.Clean(child)
	parent = filepath.Clean(parent)

	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}

	// Rel must not escape the parent via "..".
	if strings.HasPrefix(rel, "..") {
		return false
	}

	// Must not be a drive-rooted absolute path on Windows (Rel shouldn't produce one).
	if filepath.IsAbs(rel) {
		return false
	}

	return true
}
