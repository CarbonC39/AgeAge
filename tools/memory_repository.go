package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	memoryLockTimeout      = 5 * time.Second
	memoryLockPollInterval = 20 * time.Millisecond
)

var (
	ErrMemoryLockTimeout = errors.New("timed out waiting for memory file lock")
	ErrMemoryConflict    = errors.New("memory entry changed since it was selected")
	ErrMemoryAmbiguous   = errors.New("memory ID is ambiguous")
	ErrMemoryNotFound    = errors.New("memory entry not found")
)

var memoryLocks sync.Map // canonical path -> chan struct{} semaphore

func memoryProcessLock(path string) chan struct{} {
	canonical, err := filepath.Abs(path)
	if err != nil {
		canonical = filepath.Clean(path)
	}
	lock := make(chan struct{}, 1)
	lock <- struct{}{}
	actual, _ := memoryLocks.LoadOrStore(canonical, lock)
	return actual.(chan struct{})
}

// MemoryEntry represents a single memory record in MEMORY.jsonl.
type MemoryEntry struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Tags      string `json:"tags,omitempty"`
}

// MemoryRecord carries the public record metadata and an opaque revision used
// to reject edits made from a stale administrative snapshot.
type MemoryRecord struct {
	Entry    MemoryEntry
	Revision string
}

type MemoryWarning struct {
	Line    int
	Message string
}

type MemorySnapshot struct {
	Records  []MemoryRecord
	Warnings []MemoryWarning
}

// MemoryRepository is the shared, concurrency-safe owner of a JSONL memory
// file. It provides domain operations to both Agent tools and administration.
type MemoryRepository struct {
	path string
}

func NewMemoryRepository(path string) *MemoryRepository {
	return &MemoryRepository{path: path}
}

func (r *MemoryRepository) Path() string { return r.path }

func (r *MemoryRepository) withLock(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return fmt.Errorf("create memory directory: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, memoryLockTimeout)
	defer cancel()
	local := memoryProcessLock(r.path)
	select {
	case <-waitCtx.Done():
		return memoryLockWaitError(ctx, waitCtx.Err())
	case <-local:
	}
	defer func() { local <- struct{}{} }()
	if err := waitCtx.Err(); err != nil {
		return memoryLockWaitError(ctx, err)
	}
	lockPath := r.path + ".lock"
	unlock, err := lockMemoryFile(waitCtx, lockPath)
	if err != nil {
		return memoryLockWaitError(ctx, err)
	}
	defer unlock()
	if err := ensureMemoryPermissions(r.path); err != nil {
		return fmt.Errorf("protect memory file: %w", err)
	}
	return fn()
}

func memoryLockWaitError(parent context.Context, err error) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrMemoryLockTimeout
	}
	return err
}

func ensureMemoryPermissions(path string) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return os.Chmod(path, 0o600)
}

type memoryLine struct {
	raw      []byte
	body     []byte
	ending   []byte
	entry    *MemoryEntry
	revision string
}

func splitMemoryLines(data []byte) []memoryLine {
	if len(data) == 0 {
		return nil
	}
	var lines []memoryLine
	for len(data) > 0 {
		n := bytes.IndexByte(data, '\n')
		var raw, body, ending []byte
		if n < 0 {
			raw, body = data, data
			data = nil
		} else {
			raw, body, ending = data[:n+1], data[:n], data[n:n+1]
			data = data[n+1:]
		}
		line := memoryLine{raw: append([]byte(nil), raw...), body: append([]byte(nil), body...), ending: append([]byte(nil), ending...)}
		if len(bytes.TrimSpace(body)) > 0 {
			var obj map[string]json.RawMessage
			var entry MemoryEntry
			if json.Unmarshal(body, &obj) == nil && obj != nil && json.Unmarshal(body, &entry) == nil {
				line.entry = &entry
				sum := sha256.Sum256(raw)
				line.revision = hex.EncodeToString(sum[:])
			}
		}
		lines = append(lines, line)
	}
	return lines
}

func readMemoryFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

func snapshotMemory(data []byte) MemorySnapshot {
	lines := splitMemoryLines(data)
	result := MemorySnapshot{Records: make([]MemoryRecord, 0), Warnings: make([]MemoryWarning, 0)}
	for i, line := range lines {
		if len(bytes.TrimSpace(line.body)) == 0 {
			continue
		}
		if line.entry == nil {
			result.Warnings = append(result.Warnings, MemoryWarning{Line: i + 1, Message: "malformed JSONL record"})
			continue
		}
		result.Records = append(result.Records, MemoryRecord{Entry: *line.entry, Revision: line.revision})
	}
	return result
}

func (r *MemoryRepository) List(ctx context.Context) (MemorySnapshot, error) {
	var result MemorySnapshot
	if _, err := os.Stat(filepath.Dir(r.path)); errors.Is(err, os.ErrNotExist) {
		return MemorySnapshot{Records: make([]MemoryRecord, 0), Warnings: make([]MemoryWarning, 0)}, nil
	} else if err != nil {
		return result, err
	}
	err := r.withLock(ctx, func() error {
		data, err := readMemoryFile(r.path)
		if err != nil {
			return fmt.Errorf("read memory file: %w", err)
		}
		result = snapshotMemory(data)
		return nil
	})
	return result, err
}

// Search applies the Agent's established case-insensitive OR matching across
// content and tags. It intentionally returns the full result set for callers;
// Agent-facing limits remain in MemoryRecallTool.
func (r *MemoryRepository) Search(ctx context.Context, query string) ([]MemoryRecord, error) {
	snapshot, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	keywords := strings.Fields(strings.ToLower(query))
	results := make([]MemoryRecord, 0)
	for _, record := range snapshot.Records {
		text := strings.ToLower(record.Entry.Content + " " + record.Entry.Tags)
		for _, keyword := range keywords {
			if strings.Contains(text, keyword) {
				results = append(results, record)
				break
			}
		}
	}
	return results, nil
}

func (r *MemoryRepository) Export(ctx context.Context) ([]byte, error) {
	var data []byte
	err := r.withLock(ctx, func() error {
		var err error
		data, err = readMemoryFile(r.path)
		if err != nil {
			return fmt.Errorf("read memory file: %w", err)
		}
		return nil
	})
	return data, err
}

// WriteExport writes an exact snapshot to destination with mode 0600. The
// destination is replaced atomically so readers never observe a partial
// backup.
func (r *MemoryRepository) WriteExport(ctx context.Context, destination string) error {
	data, err := r.Export(ctx)
	if err != nil {
		return err
	}
	if err := atomicWriteMemory(destination, data); err != nil {
		return fmt.Errorf("write memory export: %w", err)
	}
	return nil
}

// WriteExportIfAbsent writes an exact snapshot with mode 0600 and fails if the
// destination already exists. The temporary file is linked into place so the
// no-overwrite rule remains true even if another process creates the path
// after the caller checks it.
func (r *MemoryRepository) WriteExportIfAbsent(ctx context.Context, destination string) error {
	data, err := r.Export(ctx)
	if err != nil {
		return err
	}
	if err := atomicCreateMemory(destination, data); err != nil {
		return fmt.Errorf("write memory export: %w", err)
	}
	return nil
}

func (r *MemoryRepository) HasData(ctx context.Context) (bool, error) {
	var hasData bool
	err := r.withLock(ctx, func() error {
		info, err := os.Stat(r.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		hasData = info.Size() > 0
		return nil
	})
	return hasData, err
}

func (r *MemoryRepository) Add(ctx context.Context, content, tags string) (MemoryEntry, error) {
	entry := MemoryEntry{
		ID: fmt.Sprintf("mem_%d", time.Now().UnixNano()), Content: content,
		Timestamp: time.Now().Format(time.RFC3339), Tags: tags,
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return MemoryEntry{}, fmt.Errorf("marshal memory entry: %w", err)
	}
	err = r.withLock(ctx, func() error {
		f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return fmt.Errorf("open memory file: %w", err)
		}
		defer f.Close()
		if err := f.Chmod(0o600); err != nil {
			return fmt.Errorf("protect memory file: %w", err)
		}
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Size() > 0 {
			last := []byte{0}
			if _, err := f.ReadAt(last, info.Size()-1); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			if last[0] != '\n' {
				if _, err := f.Write([]byte{'\n'}); err != nil {
					return err
				}
			}
		}
		if _, err := f.Write(append(encoded, '\n')); err != nil {
			return fmt.Errorf("write memory entry: %w", err)
		}
		return f.Sync()
	})
	return entry, err
}

func atomicCreateMemory(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".memory-export-*.tmp")
	if err != nil {
		return fmt.Errorf("create export temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("protect export temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write export temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync export temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close export temp file: %w", err)
	}
	if err := os.Link(tmpName, path); err != nil {
		return err
	}
	return nil
}

func (r *MemoryRepository) Edit(ctx context.Context, id, revision, content, tags string) error {
	return r.withLock(ctx, func() error {
		data, err := readMemoryFile(r.path)
		if err != nil {
			return fmt.Errorf("read memory file: %w", err)
		}
		lines := splitMemoryLines(data)
		idx, err := locateMemoryID(lines, id)
		if err != nil {
			return err
		}
		if lines[idx].revision != revision {
			return ErrMemoryConflict
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(lines[idx].body, &fields); err != nil {
			return err
		}
		fields["content"], _ = json.Marshal(content)
		if tags == "" {
			delete(fields, "tags")
		} else {
			fields["tags"], _ = json.Marshal(tags)
		}
		updated, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		lines[idx].raw = append(updated, lines[idx].ending...)
		return atomicWriteMemory(r.path, joinMemoryLines(lines))
	})
}

func (r *MemoryRepository) Remove(ctx context.Context, revisions map[string]string) error {
	if len(revisions) == 0 {
		return nil
	}
	return r.withLock(ctx, func() error {
		data, err := readMemoryFile(r.path)
		if err != nil {
			return fmt.Errorf("read memory file: %w", err)
		}
		lines := splitMemoryLines(data)
		indexes := make(map[int]struct{}, len(revisions))
		for id, revision := range revisions {
			idx, err := locateMemoryID(lines, id)
			if err != nil {
				return err
			}
			if lines[idx].revision != revision {
				return ErrMemoryConflict
			}
			indexes[idx] = struct{}{}
		}
		kept := make([]memoryLine, 0, len(lines)-len(indexes))
		for i, line := range lines {
			if _, remove := indexes[i]; !remove {
				kept = append(kept, line)
			}
		}
		return atomicWriteMemory(r.path, joinMemoryLines(kept))
	})
}

func locateMemoryID(lines []memoryLine, id string) (int, error) {
	found := -1
	for i, line := range lines {
		if line.entry == nil || line.entry.ID != id {
			continue
		}
		if found >= 0 {
			return -1, fmt.Errorf("%w: %s", ErrMemoryAmbiguous, id)
		}
		found = i
	}
	if found < 0 {
		return -1, fmt.Errorf("%w: %s", ErrMemoryNotFound, id)
	}
	return found, nil
}

func joinMemoryLines(lines []memoryLine) []byte {
	var out []byte
	for _, line := range lines {
		out = append(out, line.raw...)
	}
	return out
}

func atomicWriteMemory(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".memory-*.tmp")
	if err != nil {
		return fmt.Errorf("create memory temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("protect memory temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write memory temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync memory temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close memory temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace memory file: %w", err)
	}
	return nil
}
