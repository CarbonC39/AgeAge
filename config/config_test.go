package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigSafetyAndHistoryDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Agent.Mode != "supervised" || cfg.Agent.MaxIterations <= 0 {
		t.Fatalf("unsafe agent defaults: %#v", cfg.Agent)
	}
	if !cfg.History.CompressToolTurns || cfg.History.KeepRecentTurns != 2 {
		t.Fatalf("history defaults: %#v", cfg.History)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("server should bind localhost by default: %#v", cfg.Server)
	}
	if cfg.Bash.MaxOutputBytes <= 0 {
		t.Fatalf("bash output cap disabled: %#v", cfg.Bash)
	}
}

func TestLoadConfigMergesDefaultsAndResolvesWorkspace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := `
workspace = "project"

[llm]
model = "local-model"

[router]
enabled = true

[router.classifier]
model = "small-classifier"

[history]
compress_tool_turns = false
keep_recent_turns = 7
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Workspace != filepath.Join(dir, "project") || cfg.EffectiveWorkDir() != cfg.Workspace {
		t.Fatalf("workspace = %q, workdir = %q", cfg.Workspace, cfg.EffectiveWorkDir())
	}
	if cfg.LLM.Model != "local-model" || cfg.LLM.BaseURL == "" {
		t.Fatalf("LLM defaults not merged: %#v", cfg.LLM)
	}
	if !cfg.Router.Enabled || cfg.Router.ClassifierModel.Model != "small-classifier" {
		t.Fatalf("router config = %#v", cfg.Router)
	}
	if cfg.History.CompressToolTurns || cfg.History.KeepRecentTurns != 7 {
		t.Fatalf("history config = %#v", cfg.History)
	}
}

func TestConfigPathsAndEnsureDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("workspace = \".\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(dir, "data")); err != nil || !info.IsDir() {
		t.Fatalf("data directory missing: %v", err)
	}
	if cfg.SkillsDir() != filepath.Join(dir, "skills") || cfg.CredentialsPath() != filepath.Join(dir, "credentials.toml") {
		t.Fatalf("derived paths are wrong: skills=%q creds=%q", cfg.SkillsDir(), cfg.CredentialsPath())
	}
}

func TestShouldExcludeToolUsesCaseInsensitivePrefix(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Agent.NonIncludeTools = []string{"memory_", "BASH"}
	for _, name := range []string{"memory_store", "memory_recall", "bash", "Bash"} {
		if !cfg.ShouldExcludeTool(name) {
			t.Errorf("expected %q to be excluded", name)
		}
	}
	if cfg.ShouldExcludeTool("web_fetch") {
		t.Fatal("unrelated tool was excluded")
	}
}
