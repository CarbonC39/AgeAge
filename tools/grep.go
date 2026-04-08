package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"ageage/security"
)

// GrepTool searches a file for lines matching a regular expression.
// It is a skill-only tool — not registered in the global registry.
type GrepTool struct {
	Security *security.Checker
}

func (t *GrepTool) Name() string { return "grep" }

func (t *GrepTool) Description() string {
	return "Search for a pattern within a file and return matching lines with line numbers. " +
		"Use plain text for keyword search or a regular expression for pattern matching. " +
		"Returns line numbers, matching lines, and optional surrounding context."
}

func (t *GrepTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Path to the file to search.",
			},
			"pattern": map[string]interface{}{
				"type":        "string",
				"description": "Regex or plain-text keyword to search for.",
			},
			"case_sensitive": map[string]interface{}{
				"type":        "boolean",
				"description": "Case-sensitive matching. Defaults to false.",
			},
			"context_lines": map[string]interface{}{
				"type":        "integer",
				"description": "Lines of context to show before and after each match (0–5). Defaults to 0.",
			},
		},
		"required": []string{"path", "pattern"},
	}
}

type GrepArgs struct {
	Path          string `json:"path"`
	Pattern       string `json:"pattern"`
	CaseSensitive bool   `json:"case_sensitive"`
	ContextLines  int    `json:"context_lines"`
}

func (t *GrepTool) Execute(args json.RawMessage) (string, error) {
	var a GrepArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}

	// Resolve, verify, and obtain the canonical safe path.
	path := a.Path
	if t.Security != nil {
		var err error
		path, err = t.Security.CheckPath(a.Path)
		if err != nil {
			return "", fmt.Errorf("access denied: %s", err)
		}
	}

	// Cap context lines.
	ctx := a.ContextLines
	if ctx < 0 {
		ctx = 0
	} else if ctx > 5 {
		ctx = 5
	}

	// Compile pattern.
	pat := a.Pattern
	if !a.CaseSensitive {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return "", fmt.Errorf("invalid pattern %q: %w", a.Pattern, err)
	}

	// Read file.
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot open file: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var raw []string
	for scanner.Scan() {
		raw = append(raw, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read error: %w", err)
	}

	// Mark matching lines and their context windows.
	type entry struct {
		lineNum int
		content string
		isMatch bool
		include bool
	}
	entries := make([]entry, len(raw))
	for i, l := range raw {
		entries[i] = entry{lineNum: i + 1, content: l}
	}

	matchCount := 0
	for i := range entries {
		if re.MatchString(entries[i].content) {
			entries[i].isMatch = true
			entries[i].include = true
			matchCount++
			lo := i - ctx
			if lo < 0 {
				lo = 0
			}
			hi := i + ctx
			if hi >= len(entries) {
				hi = len(entries) - 1
			}
			for j := lo; j <= hi; j++ {
				entries[j].include = true
			}
		}
	}

	if matchCount == 0 {
		return fmt.Sprintf("No matches for %q in %s", a.Pattern, path), nil
	}

	const maxOutputLines = 200
	var sb strings.Builder
	fmt.Fprintf(&sb, "Matches for %q in %s (%d found):\n\n", a.Pattern, path, matchCount)

	prevIncluded := true
	outputLines := 0
	for _, e := range entries {
		if !e.include {
			prevIncluded = false
			continue
		}
		if !prevIncluded {
			sb.WriteString("  ...\n")
		}
		prevIncluded = true

		if outputLines >= maxOutputLines {
			sb.WriteString("  ... (output truncated)\n")
			break
		}

		mark := "  "
		if e.isMatch {
			mark = "> "
		}
		fmt.Fprintf(&sb, "%s%4d: %s\n", mark, e.lineNum, e.content)
		outputLines++
	}

	return sb.String(), nil
}
