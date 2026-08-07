package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCronStore(t *testing.T, content string) *CronStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cron.json")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return NewCronStore(path)
}

func TestCronStoreMigratesLegacyEntriesToEnabled(t *testing.T) {
	store := writeCronStore(t, `[
  {"id":"cron_1","schedule":"0 9 * * *","command":"daily","created":"2026-01-01T00:00:00Z"}
]`)
	entries := store.List()
	if len(entries) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	if !entries[0].Enabled {
		t.Fatal("legacy entry was not migrated to enabled")
	}
}

func TestCronStorePreservesExplicitlyDisabledEntry(t *testing.T) {
	store := writeCronStore(t, `[
  {"id":"cron_1","schedule":"0 9 * * *","command":"daily","created":"2026-01-01T00:00:00Z","enabled":false}
]`)
	entries := store.List()
	if len(entries) != 1 || entries[0].Enabled {
		t.Fatalf("explicitly disabled entry was changed: %#v", entries)
	}
}

func TestCronStoreAddAndSetEnabled(t *testing.T) {
	store := writeCronStore(t, "")
	entry, err := store.Add("*/5 * * * *", "skill:check", "matrix:!room", true)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID == "" || !entry.Enabled {
		t.Fatalf("added entry = %#v", entry)
	}

	got, ok := store.Get(entry.ID)
	if !ok || got.Command != "skill:check" {
		t.Fatalf("Get = (%#v, %v)", got, ok)
	}

	if found, err := store.SetEnabled(entry.ID, false); err != nil || !found {
		t.Fatalf("SetEnabled = (%v, %v)", found, err)
	}
	if e, _ := store.Get(entry.ID); e.Enabled {
		t.Fatal("entry still enabled after pause")
	}
}

func TestCronStoreUpdateResult(t *testing.T) {
	store := writeCronStore(t, "")
	entry, _ := store.Add("0 9 * * *", "task", "", true)

	updated, ok, err := store.UpdateResult(entry.ID, time.Now(), "success", "", "hello world")
	if err != nil || !ok {
		t.Fatalf("UpdateResult = (%v, %v, %v)", updated, ok, err)
	}
	if updated.LastStatus != "success" || updated.RunCount != 1 || updated.LastOutput != "hello world" {
		t.Fatalf("updated = %#v", updated)
	}

	// Store survives reload.
	reloaded := NewCronStore(store.path)
	e, _ := reloaded.Get(entry.ID)
	if e.RunCount != 1 || e.LastStatus != "success" {
		t.Fatalf("reloaded = %#v", e)
	}
}

func TestNextRunTime(t *testing.T) {
	base := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		expr string
		want string
	}{
		{"*/5 * * * *", "2026-08-06T10:05:00Z"},
		{"0 9 * * *", "2026-08-07T09:00:00Z"},
		{"30 23 31 12 *", "2026-12-31T23:30:00Z"},
	}
	for _, tt := range tests {
		got, ok := NextRunTime(tt.expr, base)
		if !ok {
			t.Fatalf("NextRunTime(%q) = no match", tt.expr)
		}
		if got.UTC().Format(time.RFC3339) != tt.want {
			t.Fatalf("NextRunTime(%q) = %s, want %s", tt.expr, got.UTC().Format(time.RFC3339), tt.want)
		}
	}
}

func TestCronListToolOutputIncludesStateAndNextRun(t *testing.T) {
	store := writeCronStore(t, "")
	store.Add("0 9 * * *", "daily task", "", true)
	store.Add("*/5 * * * *", "paused task", "", false)

	tool := &CronListTool{Store: store}
	out, err := tool.Execute(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "paused") || !strings.Contains(out, "enabled") {
		t.Fatalf("list output missing state markers: %s", out)
	}
	if !strings.Contains(out, "Next run:") {
		t.Fatalf("list output missing next run: %s", out)
	}
}

func TestCronRunToolUsesRunFunc(t *testing.T) {
	store := writeCronStore(t, "")
	entry, _ := store.Add("0 9 * * *", "task", "", true)

	tool := &CronRunTool{
		Store: store,
		RunFunc: func(_ context.Context, id string) (string, error) {
			if id != entry.ID {
				t.Fatalf("run func got id %q", id)
			}
			return "ran ok", nil
		},
	}
	out, err := tool.Execute(nil, json.RawMessage(`{"id":"`+entry.ID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ran ok") {
		t.Fatalf("output = %q", out)
	}

	missing := &CronRunTool{Store: store, RunFunc: func(context.Context, string) (string, error) { return "x", nil }}
	if _, err := missing.Execute(nil, json.RawMessage(`{"id":"nope"}`)); err != nil {
		t.Fatalf("expected a result even for a missing run func, got %v", err)
	}
}
