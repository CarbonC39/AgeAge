package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"ageage/llm"
)

// TmpManager tracks agent-owned temporary files and garbage-collects them
// after each run. All files live under <workspace>/data/tmp/.
type TmpManager struct {
	mu    sync.Mutex
	dir   string   // base directory, created lazily
	files []string // absolute paths of live managed files
}

func newTmpManager(workspace string) *TmpManager {
	return &TmpManager{dir: filepath.Join(workspace, "data", "tmp")}
}

// NewFile creates an empty managed tmp file with the given suffix (e.g. ".md").
// The caller is responsible for writing content to the returned path.
func (t *TmpManager) NewFile(suffix string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := os.MkdirAll(t.dir, 0o755); err != nil {
		return "", fmt.Errorf("tmpmanager: mkdir: %w", err)
	}

	f, err := os.CreateTemp(t.dir, "att-*"+suffix)
	if err != nil {
		return "", fmt.Errorf("tmpmanager: create: %w", err)
	}
	f.Close()

	t.files = append(t.files, f.Name())
	return f.Name(), nil
}

// GC removes managed files whose paths do not appear anywhere in the current
// message history. Call this after each Run() to keep the tmp dir clean.
func (t *TmpManager) GC(messages []llm.Message) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Build a combined text blob of all message content to scan for live paths.
	var all strings.Builder
	for _, m := range messages {
		all.WriteString(m.TextContent())
		for _, p := range m.Parts {
			if p.Type == "image_url" && p.ImageURL != nil {
				all.WriteString(p.ImageURL.URL)
			}
		}
	}
	corpus := all.String()

	var live []string
	for _, path := range t.files {
		if strings.Contains(corpus, path) {
			live = append(live, path)
			continue
		}
		// Keep the path if deletion fails (e.g. permission error, in-use) so
		// GC retries it next time rather than silently orphaning the file.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			live = append(live, path)
		}
	}
	t.files = live
}

// ClearAll removes every managed tmp file unconditionally.
// Call from ClearHistory().
func (t *TmpManager) ClearAll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, path := range t.files {
		os.Remove(path) //nolint:errcheck
	}
	t.files = nil
}
