package config

import (
	"path/filepath"
	"testing"
)

// TestExampleConfigParses guards the repo's example.config.toml: it must always
// parse through the real loader and merge with defaults, so a broken example
// never ships. The loader is side-effect free except for workspace resolution.
func TestExampleConfigParses(t *testing.T) {
	path := filepath.Join("..", "example.config.toml")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("example.config.toml failed to parse: %v", err)
	}
	if cfg.LLM.Model != "gpt-4o-mini" || cfg.LLM.BaseURL == "" {
		t.Fatalf("example LLM block not loaded: %#v", cfg.LLM)
	}
	// Newer feature sections must be present and read.
	if !cfg.History.CompressToolTurns || cfg.History.KeepRecentTurns != 2 {
		t.Fatalf("example [history] not loaded: %#v", cfg.History)
	}
	if !cfg.Planner.Enabled {
		t.Fatalf("example [planner] not loaded: %#v", cfg.Planner)
	}
	if cfg.Cron.MaxOutput != 2000 || cfg.Cron.CatchUp {
		t.Fatalf("example [cron] not loaded: %#v", cfg.Cron)
	}
	if cfg.Security.ForbidRM {
		t.Fatalf("example forbid_rm should default to false: %#v", cfg.Security)
	}
	// workspace "./workspace" resolves relative to the repo root.
	want, err := filepath.Abs(filepath.Join("..", "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace != want {
		t.Fatalf("workspace = %q, want %q", cfg.Workspace, want)
	}
}
