package main

import (
	"os"
	"path/filepath"
	"testing"

	"ageage/config"
)

// TestBuildInitConfigProducesValidToml guarantees the config emitted by
// `ageage init` always parses through the real loader and carries the newer
// feature sections ([history], [planner], [cron], forbid_rm).
func TestBuildInitConfigProducesValidToml(t *testing.T) {
	content := buildInitConfig(
		".",
		"sk-test",
		"https://api.openai.com/v1",
		"gpt-4o-mini",
		"supervised",
		"",
		true, "gemini-flash", "gpt-4o-mini", "gpt-4o",
		true, 3,
		"duckduckgo", "", "", "",
		"native", "", "",
	)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("init-generated config failed to parse: %v\n---\n%s", err, content)
	}
	if !cfg.Planner.Enabled {
		t.Fatal("init config missing [planner] enabled = true")
	}
	if cfg.History.CompressToolTurns != true || cfg.History.KeepRecentTurns != 2 {
		t.Fatalf("init config [history] = %#v", cfg.History)
	}
	if cfg.Cron.MaxOutput != 2000 {
		t.Fatalf("init config [cron] = %#v", cfg.Cron)
	}
	if cfg.Security.ForbidRM {
		t.Fatal("init config forbid_rm should default to false")
	}
	if !cfg.Router.Enabled || cfg.Router.ClassifierModel.Model != "gemini-flash" {
		t.Fatalf("init config router = %#v", cfg.Router)
	}
}
