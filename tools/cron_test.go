package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
  {"id":"cron_1","schedule":"0 9 * * *","command":"daily","created":"2026-01-01T00:00:00Z","future_policy":{"mode":"keep"}}
]`)
	entries := store.List()
	if len(entries) != 1 {
		t.Fatalf("entries = %#v", entries)
	}
	if !entries[0].Enabled {
		t.Fatal("legacy entry was not migrated to enabled")
	}
	if entries[0].TaskText() != "daily" || entries[0].Command != "daily" {
		t.Fatalf("legacy task was not preserved: %#v", entries[0])
	}
	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("legacy store mode = %o", got)
	}
	if found, err := store.SetEnabled("cron_1", false); err != nil || !found {
		t.Fatalf("migrate write = (%v, %v)", found, err)
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"future_policy"`) || !strings.Contains(string(data), `"mode": "keep"`) {
		t.Fatalf("migration lost an unknown legacy field: %s", data)
	}
}

func TestCronStoreMalformedFileIsNeverOverwritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron.json")
	original := []byte(`[{"id":"cron_1"}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewCronStore(path)
	if _, err := store.Add("0 9 * * *", "task", "", true); err == nil {
		t.Fatal("Add succeeded against a malformed store")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("malformed store was overwritten: %q", got)
	}
}

func TestCronStoreMutationRollsBackAfterSaveFailure(t *testing.T) {
	store := writeCronStore(t, "")
	entry, err := store.Add("0 9 * * *", "task", "", true)
	if err != nil {
		t.Fatal(err)
	}
	store.loadErr = errors.New("read-only after load failure")
	if found, err := store.SetEnabled(entry.ID, false); err == nil || found {
		t.Fatalf("SetEnabled = (%v, %v), want failed mutation", found, err)
	}
	if got, _ := store.Get(entry.ID); !got.Enabled {
		t.Fatal("failed SetEnabled changed in-memory state")
	}
	if found, err := store.Remove(entry.ID); err == nil || found {
		t.Fatalf("Remove = (%v, %v), want failed mutation", found, err)
	}
	if _, ok := store.Get(entry.ID); !ok {
		t.Fatal("failed Remove changed in-memory state")
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
	info, err := os.Stat(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("written store mode = %o", got)
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

func TestCronAddSchemaProgressiveDisclosure(t *testing.T) {
	properties := func(tool *CronAddTool) map[string]any {
		return tool.Parameters()["properties"].(map[string]any)
	}
	if got := sortedKeys(properties(&CronAddTool{})); !reflect.DeepEqual(got, []string{"schedule", "task"}) {
		t.Fatalf("default properties = %v", got)
	}
	if got := sortedKeys(properties(&CronAddTool{SessionIntegration: true})); !reflect.DeepEqual(got, []string{"continue_current_session", "schedule", "task"}) {
		t.Fatalf("integrated properties = %v", got)
	}
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestCronServiceOwnershipDeliveryAndIsolation(t *testing.T) {
	service := NewCronService(writeCronStore(t, ""), true)
	alice := InteractionScope{ChannelType: "matrix", ChannelID: "!room:example", ThreadID: "$thread", SessionID: "alice-session", SenderID: "@alice:example"}
	bob := InteractionScope{ChannelType: "matrix", ChannelID: "!room:example", ThreadID: "$thread", SessionID: "bob-session", SenderID: "@bob:example"}
	aliceRenamed := alice
	aliceRenamed.SessionID = "renamed-session"
	entry, err := service.Add(CronActor{Scope: alice}, "0 9 * * *", "daily", "", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Delivery != DefaultCronDelivery(alice) || !entry.Owner.equal(alice) || !entry.ContinueCurrentSession {
		t.Fatalf("entry policy not applied: %#v", entry)
	}
	if got := service.List(CronActor{Scope: bob}); len(got) != 0 {
		t.Fatalf("bob listed alice entries: %#v", got)
	}
	if got := service.List(CronActor{Scope: aliceRenamed}); len(got) != 0 {
		t.Fatalf("another session listed source-session entries: %#v", got)
	}
	otherThread := alice
	otherThread.ThreadID = "$other"
	if got := service.List(CronActor{Scope: otherThread}); len(got) != 0 {
		t.Fatalf("another thread listed source-thread entries: %#v", got)
	}
	if found, err := service.Remove(CronActor{Scope: bob}, entry.ID); err != nil || found {
		t.Fatalf("bob remove = (%v, %v)", found, err)
	}
	if found, err := service.SetEnabled(CronActor{Scope: bob}, entry.ID, false); err != nil || found {
		t.Fatalf("bob pause = (%v, %v)", found, err)
	}
	if got := service.List(AdminCronActor()); len(got) != 1 {
		t.Fatalf("admin list = %#v", got)
	}
	if _, err := service.Add(CronActor{}, "0 9 * * *", "orphan", "", true, false); err == nil {
		t.Fatal("unowned Agent entry was accepted")
	}
}

func TestCronPauseResumeToolsUseScopedService(t *testing.T) {
	service := NewCronService(writeCronStore(t, ""), false)
	alice := InteractionScope{ChannelType: "cli", ChannelID: "interactive", SessionID: "alice", SenderID: "local"}
	bob := InteractionScope{ChannelType: "cli", ChannelID: "interactive", SessionID: "bob", SenderID: "local"}
	entry, err := service.Add(CronActor{Scope: alice}, "0 9 * * *", "task", "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	pause := &CronPauseTool{Service: service, ScopeFunc: func() InteractionScope { return bob }}
	if output, err := pause.Execute(context.Background(), json.RawMessage(`{"id":"`+entry.ID+`"}`)); err != nil || !strings.Contains(output, "not found") {
		t.Fatalf("bob pause = (%q, %v)", output, err)
	}
	pause.ScopeFunc = func() InteractionScope { return alice }
	if _, err := pause.Execute(context.Background(), json.RawMessage(`{"id":"`+entry.ID+`"}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := service.Store.Get(entry.ID); got.Enabled {
		t.Fatal("pause did not disable entry")
	}
	resume := &CronResumeTool{Service: service, ScopeFunc: func() InteractionScope { return alice }}
	if _, err := resume.Execute(context.Background(), json.RawMessage(`{"id":"`+entry.ID+`"}`)); err != nil {
		t.Fatal(err)
	}
	if got, _ := service.Store.Get(entry.ID); !got.Enabled {
		t.Fatal("resume did not enable entry")
	}
	for _, schema := range []map[string]any{pause.Parameters(), resume.Parameters()} {
		if keys := sortedKeys(schema["properties"].(map[string]any)); !reflect.DeepEqual(keys, []string{"id"}) {
			t.Fatalf("pause/resume schema = %v", keys)
		}
	}
}

func TestCronServiceRunUsesOneAuditedPath(t *testing.T) {
	store := writeCronStore(t, "")
	service := NewCronService(store, false)
	service.MaxOutput = 3
	service.ExecuteFunc = func(_ context.Context, e CronEntry) (string, error) {
		return "猫狗鳥魚", nil
	}
	entry, err := service.Add(AdminCronActor(), "0 9 * * *", "task", "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	output, err := service.Run(context.Background(), AdminCronActor(), entry.ID)
	if err != nil || output != "猫狗鳥魚" {
		t.Fatalf("Run = (%q, %v)", output, err)
	}
	got, _ := store.Get(entry.ID)
	if got.RunCount != 1 || got.LastStatus != "success" || got.LastOutput != "猫狗鳥" {
		t.Fatalf("audit = %#v", got)
	}

	service.ExecuteFunc = func(context.Context, CronEntry) (string, error) {
		return "partial", errors.New("boom")
	}
	if _, err := service.Run(context.Background(), AdminCronActor(), entry.ID); err == nil {
		t.Fatal("error run succeeded")
	}
	got, _ = store.Get(entry.ID)
	if got.RunCount != 2 || got.LastStatus != "error" || got.LastError != "boom" {
		t.Fatalf("error audit = %#v", got)
	}
}

func TestCronServiceRunHasFiniteTimeoutAndAuditsFailure(t *testing.T) {
	store := writeCronStore(t, "")
	service := NewCronService(store, false)
	service.Timeout = 20 * time.Millisecond
	service.ExecuteFunc = func(ctx context.Context, _ CronEntry) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	entry, err := service.Add(AdminCronActor(), "0 9 * * *", "task", "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := service.Run(context.Background(), AdminCronActor(), entry.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	got, _ := store.Get(entry.ID)
	if got.RunCount != 1 || got.LastStatus != "error" || !strings.Contains(got.LastError, "deadline exceeded") {
		t.Fatalf("timeout audit = %#v", got)
	}
}
