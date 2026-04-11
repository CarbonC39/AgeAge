package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// TreeTool lists directory contents in a tree format, similar to `tree -L <depth>`.
// It is a skill-only tool (not registered globally).
type TreeTool struct {
	WorkDir string // default root when no path is provided
}

func (t *TreeTool) Name() string { return "tree" }

func (t *TreeTool) Description() string {
	return "List directory contents as an indented tree (like `tree -L <depth>`). " +
		"Returns files and sub-directories up to the specified depth."
}

func (t *TreeTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Directory to list. Defaults to the current working directory.",
			},
			"depth": map[string]interface{}{
				"type":        "integer",
				"description": "Maximum recursion depth (default 2, max 6).",
			},
			"all": map[string]interface{}{
				"type":        "boolean",
				"description": "Include hidden files and directories (names starting with '.'). Default false.",
			},
		},
		"required": []string{},
	}
}

func (t *TreeTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
		All   bool   `json:"all"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	root := params.Path
	if root == "" {
		root = t.WorkDir
	}
	if root == "" {
		root = "."
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(t.WorkDir, root)
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("cannot access %q: %w", abs, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", abs)
	}

	depth := params.Depth
	if depth <= 0 {
		depth = 2
	}
	if depth > 6 {
		depth = 6
	}

	var sb strings.Builder
	fmt.Fprintln(&sb, filepath.Base(abs))

	var dirs, files int
	var walk func(dir, prefix string, curDepth int)
	walk = func(dir, prefix string, curDepth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}

		// Filter hidden unless -all.
		filtered := entries[:0]
		for _, e := range entries {
			if !params.All && strings.HasPrefix(e.Name(), ".") {
				continue
			}
			filtered = append(filtered, e)
		}

		// Sort: directories first, then files, both alphabetically.
		sort.Slice(filtered, func(i, j int) bool {
			di, dj := filtered[i].IsDir(), filtered[j].IsDir()
			if di != dj {
				return di
			}
			return filtered[i].Name() < filtered[j].Name()
		})

		for i, e := range filtered {
			last := i == len(filtered)-1
			connector := "├── "
			childPrefix := prefix + "│   "
			if last {
				connector = "└── "
				childPrefix = prefix + "    "
			}
			fmt.Fprintf(&sb, "%s%s%s\n", prefix, connector, e.Name())
			if e.IsDir() {
				dirs++
				if curDepth < depth {
					walk(filepath.Join(dir, e.Name()), childPrefix, curDepth+1)
				}
			} else {
				files++
			}
		}
	}
	walk(abs, "", 1)

	fmt.Fprintf(&sb, "\n%d %s, %d %s",
		dirs, pluralWord(dirs, "directory", "directories"),
		files, pluralWord(files, "file", "files"),
	)
	return sb.String(), nil
}

func pluralWord(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
