package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ageage/security"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

type metadataStubTool struct{ stubTool }

func (s *metadataStubTool) Metadata() ToolMetadata {
	return ToolMetadata{
		ReadOnly:       true,
		Idempotent:     true,
		Risk:           RiskLow,
		DefaultTimeout: time.Second,
		NetworkAccess:  true,
		ConcurrencyKey: "test",
	}
}

func TestRegistryMetadataIsOptionalAndConservative(t *testing.T) {
	r := NewRegistry()
	legacy := &stubTool{name: "legacy"}
	r.Register(legacy)
	metadata, ok := r.Metadata("legacy")
	if !ok || metadata.ReadOnly || metadata.Idempotent || metadata.Risk != RiskMedium {
		t.Fatalf("legacy metadata = %#v, registered=%v", metadata, ok)
	}
	if _, ok := r.Metadata("missing"); ok {
		t.Fatal("missing tool reported as registered")
	}

	described := &metadataStubTool{stubTool: stubTool{name: "described"}}
	r.Register(described)
	metadata, ok = r.GetMetadata("described")
	if !ok || !metadata.ReadOnly || !metadata.Idempotent || metadata.Risk != RiskLow || metadata.ConcurrencyKey != "test" {
		t.Fatalf("described metadata = %#v, registered=%v", metadata, ok)
	}

	r.RegisterWithMetadata(legacy, ToolMetadata{ReadOnly: true, Risk: RiskLow})
	metadata, _ = r.Metadata("legacy")
	if !metadata.ReadOnly || metadata.Risk != RiskLow {
		t.Fatalf("explicit metadata was not applied: %#v", metadata)
	}

	// Replacing a tool through the minimal registration API must not retain
	// policy metadata that belonged to the previous implementation.
	r.Register(&stubTool{name: "legacy"})
	metadata, _ = r.Metadata("legacy")
	if metadata.ReadOnly || metadata.Idempotent || metadata.Risk != RiskMedium {
		t.Fatalf("replacement inherited stale metadata: %#v", metadata)
	}
}

func TestMCPToolMetadataMapsAnnotationsConservatively(t *testing.T) {
	legacy := &MCPTool{Tool: &mcp.Tool{Name: "legacy"}}
	metadata := legacy.Metadata()
	if metadata.ReadOnly || metadata.Idempotent || metadata.Risk != RiskMedium || metadata.NetworkAccess {
		t.Fatalf("unannotated MCP metadata = %#v", metadata)
	}
	destructive := true
	openWorld := true
	annotated := &MCPTool{Tool: &mcp.Tool{
		Name: "annotated",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    true,
			IdempotentHint:  false,
			DestructiveHint: &destructive,
			OpenWorldHint:   &openWorld,
		},
	}}
	metadata = annotated.Metadata()
	if metadata.ReadOnly || !metadata.Idempotent || metadata.Risk != RiskHigh || !metadata.NetworkAccess {
		t.Fatalf("annotated MCP metadata = %#v", metadata)
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

func TestMemoryFileIsPrivateAndRecallIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	store := &MemoryStoreTool{MemoryPath: path}
	recall := &MemoryRecallTool{MemoryPath: path}
	for i := 0; i < maxMemoryRecallEntries+10; i++ {
		content := fmt.Sprintf("common memory entry %d %s", i, strings.Repeat("x", 300))
		args, err := json.Marshal(map[string]string{"content": content})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Execute(context.Background(), args); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("memory mode = %o, want 600", got)
	}
	got, err := recall.Execute(context.Background(), json.RawMessage(`{"query":"common"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > maxMemoryRecallChars {
		t.Fatalf("recall output length = %d, want <= %d", len(got), maxMemoryRecallChars)
	}
	if !strings.Contains(got, "memory recall truncated") {
		t.Fatalf("bounded recall did not report truncation: %q", got)
	}
}

func TestMemoryRecallRepairsExistingPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	entry := MemoryEntry{ID: "old", Content: "legacy", Timestamp: time.Now().Format(time.RFC3339)}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	recall := &MemoryRecallTool{MemoryPath: path}
	if !recall.HasMemories() {
		t.Fatal("legacy memory was not detected")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("recalled memory mode = %o, want 600", got)
	}
}

func TestMemoryConcurrentStoresRemainValidJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	store := &MemoryStoreTool{MemoryPath: path}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := store.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"content":"entry %d"}`, i))); err != nil {
				t.Errorf("store %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 32 {
		t.Fatalf("stored lines = %d, want 32", len(lines))
	}
	for i, line := range lines {
		var entry MemoryEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line %d invalid JSON: %v", i, err)
		}
	}
}

func TestMemoryConcurrentStoreForgetRecallRemainValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	store := &MemoryStoreTool{MemoryPath: path}
	forget := &MemoryForgetTool{MemoryPath: path}
	recall := &MemoryRecallTool{MemoryPath: path}
	ids := make([]string, 20)
	for i := range ids {
		if _, err := store.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"content":"seed %d"}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var entry MemoryEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		ids[i] = entry.ID
	}

	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			if i%2 == 0 {
				if _, err := forget.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"id":%q}`, id))); err != nil {
					t.Errorf("forget %d: %v", i, err)
				}
			} else if _, err := store.Execute(context.Background(), json.RawMessage(fmt.Sprintf(`{"content":"new %d"}`, i))); err != nil {
				t.Errorf("store %d: %v", i, err)
			}
		}(i, id)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := recall.Execute(context.Background(), json.RawMessage(`{"query":"seed new"}`)); err != nil {
				t.Errorf("recall: %v", err)
			}
		}()
	}
	wg.Wait()
	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(strings.TrimSpace(string(final)), "\n") {
		var entry MemoryEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("final line %d invalid JSON: %v", i, err)
		}
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
