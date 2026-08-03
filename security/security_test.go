package security

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckPathWorkspaceAndAllowedRoots(t *testing.T) {
	workspace := t.TempDir()
	allowed := t.TempDir()
	checker := NewChecker(workspace, nil, []string{allowed}, nil)

	got, err := checker.CheckPath("nested/new.txt")
	if err != nil {
		t.Fatalf("workspace path rejected: %v", err)
	}
	want := filepath.Join(workspace, "nested", "new.txt")
	if got != want {
		t.Fatalf("resolved path = %q, want %q", got, want)
	}

	got, err = checker.CheckPath(filepath.Join(allowed, "new.txt"))
	if err != nil {
		t.Fatalf("allowed root rejected: %v", err)
	}
	if got != filepath.Join(allowed, "new.txt") {
		t.Fatalf("allowed path = %q", got)
	}
}

func TestCheckPathRejectsOutsideForbiddenAndBlocked(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	forbidden := filepath.Join(workspace, "private")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{workspace, forbidden, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	checker := NewChecker(workspace, nil, []string{root}, []string{forbidden})
	blocked := filepath.Join(workspace, "credentials.toml")
	checker.BlockFile(blocked)

	if _, err := checker.CheckPath(filepath.Join(forbidden, "x")); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("forbidden path error = %v", err)
	}
	if _, err := checker.CheckPath(blocked); err == nil || !strings.Contains(err.Error(), "system-protected") {
		t.Fatalf("blocked file error = %v", err)
	}

	strict := NewChecker(workspace, nil, nil, nil)
	if _, err := strict.CheckPath(filepath.Join(outside, "x")); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("outside path error = %v", err)
	}
}

func TestCheckPathResolvesMissingSuffixAfterSymlink(t *testing.T) {
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

	checker := NewChecker(workspace, nil, nil, nil)
	_, err := checker.CheckPath("link/new/deep/file.txt")
	if err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("symlink escape error = %v", err)
	}
}

func TestCommandBlocklistIsCaseInsensitive(t *testing.T) {
	checker := NewChecker(t.TempDir(), []string{"rm -rf", "shutdown"}, nil, nil)
	if safe, _ := checker.IsCommandSafe("sudo SHUTDOWN now"); safe {
		t.Fatal("blocked command was considered safe")
	}
	if safe, reason := checker.IsCommandSafe("go test ./..."); !safe {
		t.Fatalf("safe command rejected: %s", reason)
	}
}
