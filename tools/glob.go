package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ageage/security"
)

// GlobTool finds files matching a glob pattern, with ** cross-directory support.
// It is a skill-only tool — not registered in the global registry.
type GlobTool struct {
	Security  *security.Checker
	Workspace string
}

func (t *GlobTool) Name() string { return "glob" }

func (t *GlobTool) Description() string {
	return "Find files matching a glob pattern under a directory. " +
		"Supports * (any chars within a path segment), ** (any number of segments), and ? (single char). " +
		"Returns absolute paths of matching files."
}

func (t *GlobTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"pattern": map[string]interface{}{
				"type":        "string",
				"description": "Glob pattern, e.g. \"**/*.go\", \"src/**/*.ts\", \"config.*\".",
			},
			"base_path": map[string]interface{}{
				"type":        "string",
				"description": "Directory to search from. Defaults to the workspace root.",
			},
		},
		"required": []string{"pattern"},
	}
}

type GlobArgs struct {
	Pattern  string `json:"pattern"`
	BasePath string `json:"base_path"`
}

func (t *GlobTool) Execute(args json.RawMessage) (string, error) {
	var a GlobArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}

	base := a.BasePath
	if base == "" {
		base = t.Workspace
	}
	// Resolve relative paths against workspace so the agent can say e.g. "src/".
	if t.Security != nil {
		var err error
		base, err = t.Security.CheckPath(base)
		if err != nil {
			return "", fmt.Errorf("access denied: %s", err)
		}
	} else {
		if !filepath.IsAbs(base) {
			base = filepath.Join(t.Workspace, base)
		}
		base = filepath.Clean(base)
	}

	// Walk directory and collect matches.
	var matches []string
	walkErr := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		// Skip hidden directories (except the base itself).
		if d.IsDir() && path != base && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(base, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel) // normalise for pattern matching

		matched, matchErr := globMatch(a.Pattern, rel)
		if matchErr != nil {
			return matchErr // bubble up invalid-pattern errors
		}
		if matched {
			matches = append(matches, path)
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("search error: %w", walkErr)
	}

	if len(matches) == 0 {
		return fmt.Sprintf("No files matching %q under %s", a.Pattern, base), nil
	}

	const maxResults = 500
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d file(s) matching %q:\n", len(matches), a.Pattern)
	for i, p := range matches {
		if i >= maxResults {
			fmt.Fprintf(&sb, "... (%d more)\n", len(matches)-maxResults)
			break
		}
		sb.WriteString(p)
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}

// globMatch reports whether a relative path matches the given glob pattern.
// The pattern is split on "/" and each segment is matched with filepath.Match.
// A "**" segment matches zero or more path segments.
func globMatch(pattern, path string) (bool, error) {
	patParts := strings.Split(filepath.ToSlash(pattern), "/")
	pathParts := strings.Split(path, "/")
	return globMatchParts(patParts, pathParts)
}

func globMatchParts(pat, path []string) (bool, error) {
	for {
		if len(pat) == 0 {
			return len(path) == 0, nil
		}
		seg := pat[0]
		pat = pat[1:]

		if seg == "**" {
			// ** matches zero or more segments — try all possible split points.
			for i := 0; i <= len(path); i++ {
				if ok, err := globMatchParts(pat, path[i:]); err != nil {
					return false, err
				} else if ok {
					return true, nil
				}
			}
			return false, nil
		}

		if len(path) == 0 {
			return false, nil
		}
		ok, err := filepath.Match(seg, path[0])
		if err != nil {
			return false, fmt.Errorf("invalid pattern segment %q: %w", seg, err)
		}
		if !ok {
			return false, nil
		}
		path = path[1:]
	}
}
