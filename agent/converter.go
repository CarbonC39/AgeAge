package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ageage/config"
)

// boundedConverterWriter limits child output while it is being produced. It
// never buffers an unbounded CombinedOutput, even for a noisy failing converter.
type boundedConverterWriter struct {
	mu       sync.Mutex
	data     bytes.Buffer
	limit    int
	overflow bool
}

func (w *boundedConverterWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	available := w.limit - w.data.Len()
	if len(p) > available {
		p = p[:available]
		w.overflow = true
	}
	_, _ = w.data.Write(p)
	return n, nil
}
func (w *boundedConverterWriter) snapshot() ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.data.Bytes()...), w.overflow
}

// runConverterContext accepts only a staged input bounded by the incoming
// pipeline. In-memory output is bounded; large output is truncated after the
// converter finishes, allowing it to complete its own file writes normally.
func runConverterContext(parent context.Context, inputPath string, conv *config.ConverterConfig, mgr *TmpManager) (text, path string, truncated bool, err error) {
	if mgr == nil {
		return "", "", false, fmt.Errorf("converter requires temporary storage")
	}
	path, err = mgr.NewFile(".md")
	if err != nil {
		return "", "", false, err
	}
	success := false
	defer func() {
		if !success {
			mgr.Remove(path)
		}
	}()
	absInput, err := filepath.Abs(inputPath)
	if err != nil {
		return "", path, false, err
	}
	tokens := strings.Fields(conv.Command)
	if len(tokens) == 0 {
		return "", path, false, fmt.Errorf("converter command is empty")
	}
	for i, tok := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(tok, "{input}", absInput), "{output}", path)
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tokens[0], tokens[1:]...)
	// A child which inherits output pipes must not hold Wait forever.
	cmd.WaitDelay = time.Second
	stdout := &boundedConverterWriter{limit: maxAttachmentTextBytes + 1}
	stderr := &boundedConverterWriter{limit: 8 * 1024}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	out, outTruncated := stdout.snapshot()
	// Reading a converter-created file is bounded independently of the process
	// limit. Reject symlinks or non-regular output instead of following them.
	info, statErr := os.Lstat(path)
	if statErr != nil || !info.Mode().IsRegular() {
		return "", path, false, fmt.Errorf("converter output is not a regular file")
	}
	f, openErr := os.Open(path)
	if openErr != nil {
		return "", path, false, fmt.Errorf("cannot read converter output")
	}
	content, readErr := io.ReadAll(&io.LimitedReader{R: f, N: maxAttachmentTextBytes + 1})
	_ = f.Close()
	if readErr != nil {
		return "", path, false, fmt.Errorf("cannot read converter output")
	}
	usedStdout := false
	if len(bytes.TrimSpace(content)) == 0 {
		content = out
		usedStdout = true
	}
	truncated = len(content) > maxAttachmentTextBytes || (usedStdout && outTruncated)
	if err := parent.Err(); err != nil {
		return "", path, false, err
	}
	if runErr != nil {
		return "", path, false, fmt.Errorf("converter failed: %w", runErr)
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return "", path, false, fmt.Errorf("converter produced no output")
	}
	text = truncateAttachmentText(string(content), maxAttachmentTextBytes)
	if !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
		return "", path, false, fmt.Errorf("converter produced binary output instead of text")
	}
	// Keep only the bounded result in managed storage. This also makes stdout
	// converters and file converters have the same lifetime and tool behavior.
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		return "", path, false, fmt.Errorf("cannot store converter output")
	}
	success = true
	return text, path, truncated, nil
}

// runConverter preserves the local helper for callers with a local input.
func runConverter(inputPath string, conv *config.ConverterConfig, mgr *TmpManager) (string, string, error) {
	info, err := os.Stat(inputPath)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() || info.Size() > defaultAttachmentBytes {
		return "", "", fmt.Errorf("converter input exceeds attachment limit or is not a regular file")
	}
	text, path, _, err := runConverterContext(context.Background(), inputPath, conv, mgr)
	return text, path, err
}
