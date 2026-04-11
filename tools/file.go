package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ageage/security"
)

// FileReadTool reads file content.
type FileReadTool struct {
	Security *security.Checker
}

func (t *FileReadTool) Name() string { return "file_read" }

func (t *FileReadTool) Description() string {
	return "Read the contents of a file. " +
		"Use start_line and end_line to read a specific range (1-based, inclusive). " +
		"Returns at most 500 lines per call; use start_line to page through larger files."
}

func (t *FileReadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "The path to the file to read.",
			},
			"start_line": map[string]any{
				"type":        "integer",
				"description": "First line to return (1-based, inclusive). Defaults to 1.",
			},
			"end_line": map[string]any{
				"type":        "integer",
				"description": "Last line to return (1-based, inclusive). Defaults to start_line + 499.",
			},
		},
		"required": []string{"path"},
	}
}

const fileReadMaxLines = 500

func (t *FileReadTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// CheckPath resolves symlinks and returns the canonical safe path.
	path, err := t.Security.CheckPath(params.Path)
	if err != nil {
		return "", fmt.Errorf("access denied: %s", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	// Split into lines.
	lines := splitLines(data)
	totalLines := len(lines)

	// Resolve and clamp the requested range.
	start := params.StartLine
	if start <= 0 {
		start = 1
	}
	end := params.EndLine
	if end <= 0 {
		end = start + fileReadMaxLines - 1
	}
	if end-start+1 > fileReadMaxLines {
		end = start + fileReadMaxLines - 1
	}
	if start > totalLines {
		return fmt.Sprintf("(file has %d lines; start_line %d is out of range)", totalLines, start), nil
	}
	if end > totalLines {
		end = totalLines
	}

	// Extract the requested slice (convert to 0-based).
	selected := lines[start-1 : end]
	var sb strings.Builder
	for _, l := range selected {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}

	result := sb.String()
	// Trim the trailing newline added to the last line only if the file didn't end with one.
	if totalLines > 0 && len(data) > 0 && data[len(data)-1] != '\n' && end == totalLines {
		result = strings.TrimRight(result, "\n")
	}

	if end < totalLines {
		result += fmt.Sprintf("\n... (%d-%d of %d lines shown)", start, end, totalLines)
	}
	return result, nil
}

// splitLines splits file content into lines, preserving the original text but
// stripping the line terminator from each entry.
func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	// Remove a trailing newline before splitting so we don't produce a spurious empty last line.
	trimmed := bytes.TrimRight(data, "\n")
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

// FileWriteTool writes content to a file.
type FileWriteTool struct {
	Security   *security.Checker
	Supervised bool
	// ConfirmFunc is called in supervised mode. Returns true to allow execution.
	ConfirmFunc func(operation string) bool
}

func (t *FileWriteTool) Name() string { return "file_write" }

func (t *FileWriteTool) Description() string {
	return "Write content to a file at the given path. Creates the file and parent directories if they don't exist."
}

func (t *FileWriteTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "The path to the file to write.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The content to write to the file.",
			},
		},
		"required": []string{"path", "content"},
	}
}

func (t *FileWriteTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// CheckPath resolves symlinks and returns the canonical safe path.
	// For new files, it resolves the parent directory and appends the filename.
	path, err := t.Security.CheckPath(params.Path)
	if err != nil {
		return "", fmt.Errorf("access denied: %s", err)
	}

	operation := fmt.Sprintf("Write %d bytes to %s", len(params.Content), path)

	if t.Supervised && t.ConfirmFunc != nil {
		if !t.ConfirmFunc(operation) {
			return "File write denied by user.", nil
		}
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("failed to create directory: %w", err)
	}

	if err := os.WriteFile(path, []byte(params.Content), 0o644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(params.Content), path), nil
}

// FileEditTool edits a file by search-and-replace.
type FileEditTool struct {
	Security   *security.Checker
	Supervised bool
	// ConfirmFunc is called in supervised mode. Returns true to allow execution.
	ConfirmFunc func(operation string) bool
}

func (t *FileEditTool) Name() string { return "file_edit" }

func (t *FileEditTool) Description() string {
	return "Edit a file by replacing a specific text pattern with new content. The search text must exactly match a substring in the file."
}

func (t *FileEditTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "The path to the file to edit.",
			},
			"search": map[string]any{
				"type":        "string",
				"description": "The exact text to search for in the file.",
			},
			"replace": map[string]any{
				"type":        "string",
				"description": "The text to replace the search text with.",
			},
		},
		"required": []string{"path", "search", "replace"},
	}
}

func (t *FileEditTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Path    string `json:"path"`
		Search  string `json:"search"`
		Replace string `json:"replace"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if params.Search == "" {
		return "", fmt.Errorf("search text must not be empty")
	}

	// CheckPath resolves symlinks and returns the canonical safe path.
	path, err := t.Security.CheckPath(params.Path)
	if err != nil {
		return "", fmt.Errorf("access denied: %s", err)
	}

	operation := fmt.Sprintf("Edit file %s (replace text)", path)

	if t.Supervised && t.ConfirmFunc != nil {
		if !t.ConfirmFunc(operation) {
			return "File edit denied by user.", nil
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	content := string(data)
	count := strings.Count(content, params.Search)
	if count == 0 {
		return "", fmt.Errorf("search text not found in %s", path)
	}

	newContent := strings.Replace(content, params.Search, params.Replace, 1)

	if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	return fmt.Sprintf("Successfully replaced 1 occurrence in %s (%d total matches found)", path, count), nil
}
