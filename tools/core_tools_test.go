package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"ageage/security"
)

type stubTool struct {
	name   string
	result string
	err    error
	calls  int
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "stub" }
func (s *stubTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (s *stubTool) Execute(context.Context, json.RawMessage) (string, error) {
	s.calls++
	return s.result, s.err
}

func TestRegistryLifecycleAndFiltering(t *testing.T) {
	r := NewRegistry()
	a := &stubTool{name: "a", result: "ok"}
	b := &stubTool{name: "b"}
	r.Register(a)
	r.Register(b)

	names := r.List()
	slices.Sort(names)
	if !slices.Equal(names, []string{"a", "b"}) {
		t.Fatalf("List = %#v", names)
	}
	defs := r.ToOpenAIToolsFiltered([]string{"b"})
	if len(defs) != 1 || defs[0].Function.Name != "b" {
		t.Fatalf("filtered definitions = %#v", defs)
	}
	if got, err := r.Execute(context.Background(), "a", nil); err != nil || got != "ok" || a.calls != 1 {
		t.Fatalf("Execute = (%q, %v), calls=%d", got, err, a.calls)
	}
	r.Unregister("a")
	if _, err := r.Execute(context.Background(), "a", nil); err == nil {
		t.Fatal("expected unknown tool error")
	}
}

func TestRegistryPropagatesToolError(t *testing.T) {
	want := errors.New("boom")
	r := NewRegistry()
	r.Register(&stubTool{name: "fail", result: "partial", err: want})
	got, err := r.Execute(context.Background(), "fail", nil)
	if got != "partial" || !errors.Is(err, want) {
		t.Fatalf("Execute = (%q, %v)", got, err)
	}
}

func TestMemoryStoreRecallAndForget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	store := &MemoryStoreTool{MemoryPath: path}
	recall := &MemoryRecallTool{MemoryPath: path}
	forget := &MemoryForgetTool{MemoryPath: path}

	if recall.HasMemories() {
		t.Fatal("empty memory store reported data")
	}
	if _, err := store.Execute(context.Background(), json.RawMessage(`{"content":"Prefers dark mode","tags":"ui preference"}`)); err != nil {
		t.Fatal(err)
	}
	if !recall.HasMemories() {
		t.Fatal("stored memory was not detected")
	}
	got, err := recall.Execute(context.Background(), json.RawMessage(`{"query":"dark ui"}`))
	if err != nil || !strings.Contains(got, "Prefers dark mode") {
		t.Fatalf("recall = (%q, %v)", got, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entry MemoryEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatal(err)
	}
	if _, err := forget.Execute(context.Background(), json.RawMessage(`{"id":"`+entry.ID+`"}`)); err != nil {
		t.Fatal(err)
	}
	if recall.HasMemories() {
		t.Fatal("forgotten memory still reported data")
	}
}

func TestSupervisedMemoryStoreCanBeDenied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	called := false
	store := &MemoryStoreTool{
		MemoryPath: path,
		Supervised: true,
		ConfirmFunc: func(operation string) bool {
			called = true
			return false
		},
	}
	got, err := store.Execute(context.Background(), json.RawMessage(`{"content":"secret"}`))
	if err != nil || !called || !strings.Contains(got, "denied") {
		t.Fatalf("denied store = (%q, %v), called=%v", got, err, called)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("denied store created file: %v", err)
	}
}

func TestTreeHonorsSecurityAndHiddenFlag(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(filepath.Join(workspace, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"visible.txt", ".hidden"} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tool := &TreeTool{WorkDir: workspace, Security: security.NewChecker(workspace, nil, nil, nil)}
	got, err := tool.Execute(context.Background(), json.RawMessage(`{"depth":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "visible.txt") || strings.Contains(got, ".hidden") {
		t.Fatalf("unexpected tree output:\n%s", got)
	}
	got, err = tool.Execute(context.Background(), json.RawMessage(`{"depth":2,"all":true}`))
	if err != nil || !strings.Contains(got, ".hidden") {
		t.Fatalf("hidden tree output = %q, %v", got, err)
	}
	args, _ := json.Marshal(map[string]any{"path": outside})
	if _, err := tool.Execute(context.Background(), args); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("outside tree error = %v", err)
	}
}

func TestFileWriteRejectsSymlinkEscapeWithMissingDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated privileges on Windows")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "link")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	tool := &FileWriteTool{Security: security.NewChecker(workspace, nil, nil, nil)}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"path":"link/new/file.txt","content":"escape"}`))
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("write escape error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new", "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("escape target was created: %v", err)
	}
}
