package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ageage/security"
)

func TestFileReadRangesAndVirtualDocs(t *testing.T) {
	workspace := t.TempDir()
	checker := security.NewChecker(workspace, nil, nil, nil)
	path := filepath.Join(workspace, "lines.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &FileReadTool{Security: checker}
	got, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"lines.txt","start_line":2,"end_line":3}`))
	if err != nil || got != "two\nthree\n\n... (2-3 of 4 lines shown)" {
		t.Fatalf("range read = %q, %v", got, err)
	}
	got, err = tool.Execute(context.Background(), json.RawMessage(`{"path":"lines.txt","start_line":9}`))
	if err != nil || !strings.Contains(got, "out of range") {
		t.Fatalf("out-of-range read = %q, %v", got, err)
	}

	docsDir := filepath.Join(workspace, ".ageage", "docs")
	virtual := &FileReadTool{Security: checker, DocsDir: docsDir}
	args, _ := json.Marshal(map[string]any{"path": filepath.Join(docsDir, "pipeline.md"), "start_line": 1, "end_line": 2})
	got, err = virtual.Execute(context.Background(), args)
	if err != nil || !strings.Contains(got, "Pipeline Skills") {
		t.Fatalf("virtual doc read = %q, %v", got, err)
	}
}

func TestFileWriteAndEditLifecycle(t *testing.T) {
	workspace := t.TempDir()
	checker := security.NewChecker(workspace, nil, nil, nil)
	write := &FileWriteTool{Security: checker}
	edit := &FileEditTool{Security: checker}

	if _, err := write.Execute(context.Background(), json.RawMessage(`{"path":"nested/file.txt","content":"old old"}`)); err != nil {
		t.Fatal(err)
	}
	got, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"nested/file.txt","search":"old","replace":"new"}`))
	if err != nil || !strings.Contains(got, "2 total matches") {
		t.Fatalf("edit = %q, %v", got, err)
	}
	data, _ := os.ReadFile(filepath.Join(workspace, "nested", "file.txt"))
	if string(data) != "new old" {
		t.Fatalf("file content = %q", data)
	}
	if _, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"nested/file.txt","search":"","replace":"x"}`)); err == nil {
		t.Fatal("empty search was accepted")
	}
	if _, err := edit.Execute(context.Background(), json.RawMessage(`{"path":"nested/file.txt","search":"missing","replace":"x"}`)); err == nil {
		t.Fatal("missing search text was accepted")
	}
}

func TestSupervisedFileMutationsCanBeDenied(t *testing.T) {
	workspace := t.TempDir()
	checker := security.NewChecker(workspace, nil, nil, nil)
	denied := func(string) bool { return false }
	write := &FileWriteTool{Security: checker, Supervised: true, ConfirmFunc: denied}
	got, err := write.Execute(context.Background(), json.RawMessage(`{"path":"file.txt","content":"x"}`))
	if err != nil || !strings.Contains(got, "denied") {
		t.Fatalf("denied write = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied write created file: %v", err)
	}
}

func TestGlobMatchAndTool(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "agent/agent.go", true},
		{"src/?ain.go", "src/main.go", true},
		{"*.md", "docs/readme.md", false},
	}
	for _, tt := range tests {
		got, err := globMatch(tt.pattern, tt.path)
		if err != nil || got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, %v", tt.pattern, tt.path, got, err)
		}
	}

	workspace := t.TempDir()
	for _, relative := range []string{"main.go", "agent/agent.go", ".hidden/secret.go", "README.md"} {
		path := filepath.Join(workspace, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(relative), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool := &GlobTool{Security: security.NewChecker(workspace, nil, nil, nil), Workspace: workspace}
	got, err := tool.Execute(context.Background(), json.RawMessage(`{"pattern":"**/*.go"}`))
	if err != nil || !strings.Contains(got, "main.go") || !strings.Contains(got, "agent.go") || strings.Contains(got, "secret.go") {
		t.Fatalf("glob output = %q, %v", got, err)
	}
}

func TestGrepCaseContextAndInvalidPattern(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "sample.txt")
	if err := os.WriteFile(path, []byte("before\nNeedle here\nafter\nneedle again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &GrepTool{Security: security.NewChecker(workspace, nil, nil, nil)}
	got, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"sample.txt","pattern":"needle","context_lines":1}`))
	if err != nil || !strings.Contains(got, "2 found") || !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("grep output = %q, %v", got, err)
	}
	got, err = tool.Execute(context.Background(), json.RawMessage(`{"path":"sample.txt","pattern":"needle","case_sensitive":true}`))
	if err != nil || !strings.Contains(got, "1 found") {
		t.Fatalf("case-sensitive grep = %q, %v", got, err)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"sample.txt","pattern":"["}`)); err == nil {
		t.Fatal("invalid regexp was accepted")
	}
}
