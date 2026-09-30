package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemoryRepositoryPreservesMalformedLinesAndUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	malformed := []byte("{broken: keep exactly\r\n")
	known := []byte(`{"id":"one","content":"before","timestamp":"2026-01-02T03:04:05Z","extra":{"keep":[1,2]}}` + "\n")
	if err := os.WriteFile(path, append(append(append([]byte(nil), malformed...), known...), []byte("tail-invalid")...), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository(path)
	snapshot, err := repo.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) != 1 || len(snapshot.Warnings) != 2 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if err := repo.Edit(context.Background(), "one", snapshot.Records[0].Revision, "after", "tag"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, malformed) || !bytes.HasSuffix(data, []byte("tail-invalid")) {
		t.Fatalf("malformed bytes changed: %q", data)
	}
	var line map[string]json.RawMessage
	if err := json.Unmarshal(bytes.Split(data[len(malformed):], []byte("\n"))[0], &line); err != nil {
		t.Fatal(err)
	}
	if _, ok := line["extra"]; !ok {
		t.Fatalf("unknown field was dropped: %s", data)
	}
	var entry MemoryEntry
	if err := json.Unmarshal(bytes.Split(data[len(malformed):], []byte("\n"))[0], &entry); err != nil {
		t.Fatal(err)
	}
	if entry.ID != "one" || entry.Timestamp != "2026-01-02T03:04:05Z" || entry.Content != "after" || entry.Tags != "tag" {
		t.Fatalf("edited entry = %#v", entry)
	}
	if err := repo.Remove(context.Background(), map[string]string{"one": snapshot.Records[0].Revision}); !errors.Is(err, ErrMemoryConflict) {
		t.Fatalf("stale remove error = %v, want conflict", err)
	}
	if err := repo.Remove(context.Background(), map[string]string{"one": mustRevision(t, repo, "one")}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, append(append([]byte(nil), malformed...), []byte("tail-invalid")...)) {
		t.Fatalf("delete failed to preserve malformed bytes: %q", data)
	}
}

func mustRevision(t *testing.T, repo *MemoryRepository, id string) string {
	t.Helper()
	snapshot, err := repo.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range snapshot.Records {
		if record.Entry.ID == id {
			return record.Revision
		}
	}
	t.Fatalf("memory %q missing", id)
	return ""
}

func TestMemoryRepositoryRejectsAmbiguousAndStaleMutations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	data := "{\"id\":\"duplicate\",\"content\":\"a\",\"timestamp\":\"t\"}\n" +
		"{\"id\":\"duplicate\",\"content\":\"b\",\"timestamp\":\"t\"}\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository(path)
	snapshot, err := repo.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Edit(context.Background(), "duplicate", snapshot.Records[0].Revision, "edit", ""); !errors.Is(err, ErrMemoryAmbiguous) {
		t.Fatalf("ambiguous edit = %v", err)
	}
	if err := repo.Remove(context.Background(), map[string]string{"duplicate": snapshot.Records[0].Revision}); !errors.Is(err, ErrMemoryAmbiguous) {
		t.Fatalf("ambiguous remove = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != data {
		t.Fatalf("ambiguous mutation changed file: %q, %v", got, err)
	}
}

func TestMemoryRepositorySearchAndExactExport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	original := []byte("{\"id\":\"one\",\"content\":\"Alpha fact\",\"timestamp\":\"t\",\"tags\":\"blue\",\"future\":true}\r\ninvalid\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	repo := NewMemoryRepository(path)
	got, err := repo.Search(context.Background(), "ALPHA blue")
	if err != nil || len(got) != 1 {
		t.Fatalf("Search = (%#v, %v)", got, err)
	}
	data, err := repo.Export(context.Background())
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("Export = (%q, %v)", data, err)
	}
	destination := filepath.Join(t.TempDir(), "backup.jsonl")
	if err := repo.WriteExport(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(backup, original) {
		t.Fatalf("backup = (%q, %v)", backup, err)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, %v", info, err)
	}
}

func TestMemoryRepositoryCrossProcessAppends(t *testing.T) {
	if path := os.Getenv("AGEAGE_MEMORY_HELPER_PATH"); path != "" {
		for i := 0; i < 8; i++ {
			if _, err := NewMemoryRepository(path).Add(context.Background(), "from child", ""); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	if _, err := NewMemoryRepository(path).Add(context.Background(), "parent", ""); err != nil {
		t.Fatal(err)
	}
	commands := make([]*exec.Cmd, 4)
	outputs := make([]bytes.Buffer, len(commands))
	for i := range commands {
		commands[i] = exec.Command(os.Args[0], "-test.run=^TestMemoryRepositoryCrossProcessAppends$")
		commands[i].Env = append(os.Environ(), "AGEAGE_MEMORY_HELPER_PATH="+path)
		commands[i].Stdout = &outputs[i]
		commands[i].Stderr = &outputs[i]
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, cmd := range commands {
		err := cmd.Wait()
		if err != nil {
			t.Fatalf("child append: %v: %s", err, outputs[i].String())
		}
	}
	snapshot, err := NewMemoryRepository(path).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) != 33 {
		t.Fatalf("records after child appends = %d, want 33", len(snapshot.Records))
	}
}

func TestMemoryRepositoryLockWaitIsCancellableAndBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl.lock")
	unlock, err := lockMemoryFile(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = lockMemoryFile(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("lock wait did not respect caller cancellation")
	}
}

func TestMemoryRepositorySameProcessWaitHonorsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	repo := NewMemoryRepository(path)
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- repo.withLock(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := repo.List(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repository operation error = %v, want context deadline", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("same-process lock wait did not honor its context")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatalf("initial lock holder: %v", err)
	}
}

func TestMemoryRepositoryConcurrentAppendRecallEditAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	repo := NewMemoryRepository(path)
	ctx := context.Background()
	for _, item := range []struct{ content, tags string }{
		{"edit me", "selected"}, {"remove me", "selected"}, {"keep me", "unrelated"},
	} {
		if _, err := repo.Add(ctx, item.content, item.tags); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var editRecord, removeRecord, keepRecord MemoryRecord
	for _, record := range snapshot.Records {
		switch record.Entry.Content {
		case "edit me":
			editRecord = record
		case "remove me":
			removeRecord = record
		case "keep me":
			keepRecord = record
		}
	}
	if editRecord.Entry.ID == "" || removeRecord.Entry.ID == "" || keepRecord.Entry.ID == "" {
		t.Fatalf("seed snapshot missing entries: %#v", snapshot.Records)
	}

	firstAppend := make(chan struct{})
	appendErr := make(chan error, 1)
	appenderDone := make(chan struct{})
	go func() {
		defer close(appenderDone)
		for i := 0; i < 24; i++ {
			if _, err := repo.Add(ctx, fmt.Sprintf("concurrent append %02d", i), "generated"); err != nil {
				if i == 0 {
					close(firstAppend)
				}
				appendErr <- err
				return
			}
			if i == 0 {
				close(firstAppend)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	<-firstAppend

	var wg sync.WaitGroup
	operationErrors := make(chan error, 3)
	wg.Add(3)
	go func() {
		defer wg.Done()
		operationErrors <- repo.Edit(ctx, editRecord.Entry.ID, editRecord.Revision, "edited content", "updated")
	}()
	go func() {
		defer wg.Done()
		operationErrors <- repo.Remove(ctx, map[string]string{removeRecord.Entry.ID: removeRecord.Revision})
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 12; i++ {
			if _, err := repo.Search(ctx, "append edited keep"); err != nil {
				operationErrors <- err
				return
			}
		}
		operationErrors <- nil
	}()
	wg.Wait()
	<-appenderDone
	close(operationErrors)
	for err := range operationErrors {
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-appendErr:
		t.Fatal(err)
	default:
	}

	final, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Warnings) != 0 {
		t.Fatalf("final JSONL warnings: %#v", final.Warnings)
	}
	byContent := make(map[string]MemoryEntry, len(final.Records))
	for _, record := range final.Records {
		byContent[record.Entry.Content] = record.Entry
	}
	if entry, ok := byContent["edited content"]; !ok || entry.ID != editRecord.Entry.ID || entry.Timestamp != editRecord.Entry.Timestamp || entry.Tags != "updated" {
		t.Fatalf("edited memory missing or metadata changed: %#v", entry)
	}
	if _, ok := byContent["remove me"]; ok {
		t.Fatal("selected deletion survived")
	}
	if entry, ok := byContent["keep me"]; !ok || entry.ID != keepRecord.Entry.ID {
		t.Fatalf("unrelated memory lost: %#v", entry)
	}
	for i := 0; i < 24; i++ {
		content := fmt.Sprintf("concurrent append %02d", i)
		if _, ok := byContent[content]; !ok {
			t.Fatalf("new append lost: %q", content)
		}
	}
	data, err := repo.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var entry MemoryEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("final line %d is invalid JSON: %v", i+1, err)
		}
	}
}

func TestMemoryRepositoryAddMaintainsJSONLAndPrivateMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.jsonl")
	if err := os.WriteFile(path, []byte("legacy-without-newline"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMemoryRepository(path).Add(context.Background(), "new", "tag"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "legacy-without-newline\n{") {
		t.Fatalf("append corrupted missing delimiter input: %q", data)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("memory permissions = %v, %v", info, err)
	}
}
