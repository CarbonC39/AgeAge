package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ageage/tools"
	"github.com/spf13/cobra"
)

func memoryTestConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("workspace = \".\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func executeMemory(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	cmd := memoryCommandWith(memoryCLI{isTTY: func(*cobra.Command) bool { return false }, newRepo: tools.NewMemoryRepository})
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	return out.String(), stderr.String(), err
}

func TestMemoryListAndSearchAreNonInteractiveAndReportOnlyWarningLocation(t *testing.T) {
	configPath := memoryTestConfig(t)
	dataPath := filepath.Join(filepath.Dir(configPath), "data", "MEMORY.jsonl")
	if err := os.MkdirAll(filepath.Dir(dataPath), 0o755); err != nil {
		t.Fatal(err)
	}
	contents := "{\"id\":\"mem_one\",\"content\":\"private needle\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"tags\":\"tag\"}\nnot valid private unrelated\n"
	if err := os.WriteFile(dataPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := executeMemory(t, "--config", configPath, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ID: mem_one") || !strings.Contains(out, "private needle") {
		t.Fatalf("list output missing record: %q", out)
	}
	if !strings.Contains(stderr, "line 2") || strings.Contains(stderr, "private unrelated") {
		t.Fatalf("warning leaks record content or lacks location: %q", stderr)
	}
	out, _, err = executeMemory(t, "--config", configPath, "search", "needle")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "mem_one") {
		t.Fatalf("search output missing matching record: %q", out)
	}
	if _, _, err := executeMemory(t, "--config", configPath, "add"); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("non-TTY add error = %v", err)
	}
}

func TestMemoryMissingFileListsAsEmptyAndRemoveRequiresConfirmation(t *testing.T) {
	configPath := memoryTestConfig(t)
	out, _, err := executeMemory(t, "--config", configPath, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No memory records") {
		t.Fatalf("empty list output = %q", out)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(configPath), "data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only list created a data directory: %v", err)
	}
	_, _, err = executeMemory(t, "--config", configPath, "rm", "mem_absent")
	if err == nil || !strings.Contains(err.Error(), "terminal confirmation") {
		t.Fatalf("unconfirmed remove error = %v", err)
	}
}

func TestMemoryRemoveYesAndExportNoOverwrite(t *testing.T) {
	configPath := memoryTestConfig(t)
	repo := tools.NewMemoryRepository(filepath.Join(filepath.Dir(configPath), "data", "MEMORY.jsonl"))
	entry, err := repo.Add(context.Background(), "keep this", "tag")
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := executeMemory(t, "--config", configPath, "remove", entry.ID, "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Removed memory IDs: "+entry.ID) {
		t.Fatalf("remove output = %q", out)
	}

	entry, err = repo.Add(context.Background(), "export exactly", "x")
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "backup.jsonl")
	out, _, err = executeMemory(t, "--config", configPath, "export", destination)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, destination) {
		t.Fatalf("export output = %q", out)
	}
	want, err := repo.Export(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("export did not preserve exact JSONL bytes")
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("export permissions = %o", info.Mode().Perm())
	}
	if err := os.WriteFile(destination, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = executeMemory(t, "--config", configPath, "export", destination)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("export overwrite error = %v", err)
	}
	got, err = os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "existing" {
		t.Fatal("refused export changed existing destination")
	}
}

func TestMemoryEmptyStoreExportsWithoutTTY(t *testing.T) {
	configPath := memoryTestConfig(t)
	destination := filepath.Join(t.TempDir(), "empty.jsonl")
	out, _, err := executeMemory(t, "--config", configPath, "export", destination)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, destination) {
		t.Fatalf("export output = %q", out)
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("empty-store export has unexpected contents: %q", data)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("export permissions = %o", info.Mode().Perm())
	}
}

func TestMemoryExportPreservesMalformedAndUnknownJSONLinesExactly(t *testing.T) {
	configPath := memoryTestConfig(t)
	dataPath := filepath.Join(filepath.Dir(configPath), "data", "MEMORY.jsonl")
	if err := os.MkdirAll(filepath.Dir(dataPath), 0o700); err != nil {
		t.Fatal(err)
	}
	contents := []byte("{\"id\":\"mem_one\",\"content\":\"kept\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"future\":{\"x\":1}}\r\nmalformed private line\n")
	if err := os.WriteFile(dataPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "backup.jsonl")
	out, stderr, err := executeMemory(t, "--config", configPath, "export", destination)
	if err != nil {
		t.Fatalf("export failed: %v; stderr=%q", err, stderr)
	}
	if !strings.Contains(out, destination) {
		t.Fatalf("export output = %q", out)
	}
	got, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, contents) {
		t.Fatalf("export changed source bytes:\n got %q\nwant %q", got, contents)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("export mode = %o, want 0600", info.Mode().Perm())
	}
	if strings.Contains(stderr, "malformed private line") {
		t.Fatalf("export warning leaked malformed record contents: %q", stderr)
	}
}

func TestMemoryCommandHelpAndAliasUseInjectedStreams(t *testing.T) {
	cmd := memoryCommandWith(memoryCLI{isTTY: func(*cobra.Command) bool { return false }, newRepo: tools.NewMemoryRepository})
	cmd.SetArgs([]string{"--help"})
	var out, stderr bytes.Buffer
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("memory help failed: %v (%s)", err, stderr.String())
	}
	for _, expected := range []string{"list", "search", "add", "edit", "remove", "rm", "export"} {
		if !strings.Contains(out.String(), expected) {
			t.Errorf("memory help missing %q:\n%s", expected, out.String())
		}
	}
}

func TestMemoryContentValidationAndRemovalIDOrder(t *testing.T) {
	if err := validateMemoryContent("  \n\t"); err == nil {
		t.Fatal("blank content should fail validation")
	}
	if err := validateMemoryContent("valid content"); err != nil {
		t.Fatalf("valid content rejected: %v", err)
	}
	got := orderedUniqueIDs([]string{"mem_b", "mem_a", "mem_b"})
	want := []string{"mem_b", "mem_a"}
	if !slices.Equal(got, want) {
		t.Fatalf("orderedUniqueIDs() = %v, want %v", got, want)
	}
}

func TestMemoryEditConflictMessage(t *testing.T) {
	if err := memoryConflictError(tools.ErrMemoryConflict); err == nil || !strings.Contains(err.Error(), "reload") {
		t.Fatalf("conflict message = %v", err)
	}
	other := errors.New("disk failure")
	if memoryConflictError(other) != other {
		t.Fatal("non-conflict error was changed")
	}
}
