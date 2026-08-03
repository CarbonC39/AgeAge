package tools

import (
	"bytes"
	"testing"
)

func TestBashAutoAllowOnlyAcceptsSimplePrefixedCommands(t *testing.T) {
	tool := &BashTool{AutoAllowCommands: []string{"git", "go test"}}
	tests := []struct {
		command string
		want    bool
	}{
		{"git", true},
		{"git status", true},
		{"GO TEST ./...", true},
		{"github-cli status", false},
		{"git status; rm file", false},
		{"git status && curl example.com", false},
		{"git status | tee log", false},
		{"git status > log", false},
		{"git $(dangerous)", false},
		{"git status\nrm file", false},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			if got := tool.isAutoAllowed(tt.command); got != tt.want {
				t.Fatalf("isAutoAllowed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLimitedWriterConsumesFullInputAndCapsBuffer(t *testing.T) {
	lw := &limitedWriter{limit: 5}
	input := []byte("abcdefgh")
	n, err := lw.Write(input)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(input) {
		t.Fatalf("Write returned %d, want %d", n, len(input))
	}
	if !bytes.Equal(lw.buf.Bytes(), []byte("abcde")) || !lw.truncated {
		t.Fatalf("buffer=%q truncated=%v", lw.buf.Bytes(), lw.truncated)
	}
	if n, err := lw.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("discard write = (%d, %v)", n, err)
	}
}
