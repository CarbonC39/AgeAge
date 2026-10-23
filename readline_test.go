package main

import (
	"io"
	"os"
	"testing"
)

func TestReadlinePipeRetainsBufferedLinesAcrossCalls(t *testing.T) {
	oldStdin := os.Stdin
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(write, "first\nsecond\n"); err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	os.Stdin = read
	defer func() {
		os.Stdin = oldStdin
		_ = read.Close()
	}()

	reader := &Readline{}
	for _, want := range []string{"first", "second"} {
		got, err := reader.ReadLine()
		if err != nil || got != want {
			t.Fatalf("ReadLine() = (%q, %v), want (%q, nil)", got, err, want)
		}
	}
	if got, err := reader.ReadLine(); err != io.EOF || got != "" {
		t.Fatalf("ReadLine() at EOF = (%q, %v), want empty line and io.EOF", got, err)
	}
}
