package security

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Checker provides security checking for commands and file paths.
type Checker struct {
	blockedCommands []string
	allowedRoots    []string
	forbiddenRoots  []string
	workspace       string
}

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

	for _, blocked := range c.blockedCommands {
		if strings.Contains(lower, strings.ToLower(blocked)) {
			return false, fmt.Sprintf("command contains blocked pattern: %q", blocked)
		}
	}

	return true, ""
}

// Workspace returns the configured workspace root (canonical absolute path).
func (c *Checker) Workspace() string { return c.workspace }

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

	// Step 2: resolve all symlinks to obtain the canonical path.
	// If the file does not exist yet (e.g., a new file to be written), EvalSymlinks
	// fails; in that case we resolve as many components as possible by evaluating
	// the parent directory and appending the filename.
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// File doesn't exist yet — resolve the parent directory instead.
		parent, err2 := filepath.EvalSymlinks(filepath.Dir(absPath))
		if err2 != nil {
			// Parent also doesn't exist; use the cleaned absolute path.
			resolved = absPath
		} else {
			resolved = filepath.Join(parent, filepath.Base(absPath))
		}
	}

	// Step 3: check forbidden roots (highest priority).
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
