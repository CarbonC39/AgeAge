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
		false, true, false, false, // forbidRM, planner, summarize, keepRawToolCalls
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
	if cfg.Summarize.Enabled {
		t.Fatal("init config summarize should be off by default")
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

// TestBuildInitConfigAdvancedSwitches verifies the advanced-settings choices
// are written to the generated TOML.
func TestBuildInitConfigAdvancedSwitches(t *testing.T) {
	content := buildInitConfig(
		".",
		"sk-test",
		"https://api.openai.com/v1",
		"gpt-4o-mini",
		"full",
		"",
		false, "", "", "",
		false, 3,
		"duckduckgo", "", "", "",
		"native", "", "",
		true, false, true, true, // forbidRM, planner, summarize, keepRawToolCalls
	)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("advanced init config failed to parse: %v\n---\n%s", err, content)
	}
	if !cfg.Security.ForbidRM {
		t.Fatal("forbid_rm was not enabled")
	}
	if cfg.Planner.Enabled {
		t.Fatal("planner should be disabled")
	}
	if !cfg.Summarize.Enabled {
		t.Fatal("summarize should be enabled")
	}
	if cfg.History.CompressToolTurns {
		t.Fatal("compress_tool_turns should be disabled when raw tool calls requested")
	}
	if cfg.Agent.Mode != "full" {
		t.Fatalf("agent mode = %q", cfg.Agent.Mode)
	}
}

func TestModelSuggestionsForGemini(t *testing.T) {
	const gemini = "https://generativelanguage.googleapis.com/v1beta/openai/"
	if got := suggestModel(gemini); got != "gemini-3.5-flash" {
		t.Fatalf("suggestModel(gemini) = %q", got)
	}
	if got := getStrongModel(gemini, "gemini-3.5-flash"); got != "gemini-3.1-pro" {
		t.Fatalf("getStrongModel(gemini) = %q", got)
	}
	if got := suggestModel("http://localhost:11434/v1"); got != "llama3.3" {
		t.Fatalf("suggestModel(ollama) = %q", got)
	}
}
