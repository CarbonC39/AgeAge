package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestInitDraftUsesConfigOwnedDefaults(t *testing.T) {
	defaults := config.DefaultConfig()
	draft := newInitDraft(filepath.Join(t.TempDir(), "workspace"))
	if draft.Workspace != defaults.Workspace || draft.BaseURL != defaults.LLM.BaseURL || draft.Model != defaults.LLM.Model {
		t.Fatalf("init LLM/storage defaults drifted: draft=%#v defaults=%#v", draft, defaults)
	}
	if draft.AgentMode != defaults.Agent.Mode || draft.Planner != defaults.Planner.Enabled || draft.EvalThreshold != defaults.Eval.SuccessThreshold {
		t.Fatalf("init Agent defaults drifted: draft=%#v defaults=%#v", draft, defaults)
	}
	if draft.SearchBackend != defaults.WebSearch.Backend || draft.FetchBackend != defaults.WebFetch.Backend {
		t.Fatalf("init backend defaults drifted: draft search=%q fetch=%q", draft.SearchBackend, draft.FetchBackend)
	}
	for _, option := range config.WebFetchBackendChoices() {
		if option.Value != draft.FetchBackend {
			continue
		}
		for _, field := range option.Fields {
			if field.ConfigKey == config.ConfigKeyCrawl4AICmd && draft.PythonCommand != field.DefaultValue {
				t.Fatalf("init Python command default = %q; owner provider = %q", draft.PythonCommand, field.DefaultValue)
			}
		}
	}
	if _, ok := initProfile(draft.Provider); !ok {
		t.Fatalf("init default provider %q has no owner profile", draft.Provider)
	}
	if !configChoiceExists(config.AgentModeChoices(), draft.AgentMode) ||
		!configBackendChoiceExists(config.WebSearchBackendChoices(), draft.SearchBackend) ||
		!configBackendChoiceExists(config.WebFetchBackendChoices(), draft.FetchBackend) {
		t.Fatal("init defaults are missing from the owner-provided selector choices")
	}
}

func configChoiceExists(choices []config.Choice, value string) bool {
	for _, choice := range choices {
		if choice.Value == value {
			return true
		}
	}
	return false
}

func configBackendChoiceExists(choices []config.BackendChoice, value string) bool {
	for _, choice := range choices {
		if choice.Value == value {
			return true
		}
	}
	return false
}

func TestRootHelpShowsAdministrativeCommandSurface(t *testing.T) {
	root := newRootCommand()
	root.SetArgs([]string{"--help"})
	var out, stderr bytes.Buffer
	root.SetIn(strings.NewReader(""))
	root.SetOut(&out)
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatalf("root help failed: %v (%s)", err, stderr.String())
	}
	for _, expected := range []string{"init", "config", "tools", "memory", "scheduled tasks"} {
		if !strings.Contains(out.String(), expected) {
			t.Errorf("root help missing %q:\n%s", expected, out.String())
		}
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

func TestInitCancellationAndPlainNonTTYDoNotWrite(t *testing.T) {
	target := filepath.Join(t.TempDir(), "not-created")
	cmd := initCommand()
	if err := cmd.Flags().Set("dir", target); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	if err := runInit(cmd, nil); err != nil {
		t.Fatalf("non-TTY cancellation returned error: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel created target directory: stat err=%v", err)
	}
}

func TestInitExistingConfigAndNonEmptyDirectoryRefuseWithoutOverwrite(t *testing.T) {
	t.Run("existing config", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		before := []byte("existing config bytes\n")
		if err := os.WriteFile(path, before, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := commitInit(newInitDraft(dir), false); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
			t.Fatalf("commit existing config error = %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, before) {
			t.Fatalf("existing config changed: %q, err=%v", got, err)
		}
	})
	t.Run("non-empty directory", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "keep.txt")
		if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := commitInit(newInitDraft(dir), false); err == nil || !strings.Contains(err.Error(), "non-empty existing directory") {
			t.Fatalf("commit existing directory error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "config.toml")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refusal created config: %v", err)
		}
	})
}

func TestInitBackNavigationRetainsDraftAndMasksSecret(t *testing.T) {
	draft := newInitDraft(filepath.Join(t.TempDir(), "target"))
	draft.APIKey = "secret-should-not-appear"
	draft.Workspace = "retained workspace"
	draft.EvalThresholdText = "9"
	flow := &initFlow{draft: draft, page: 4}
	if !flow.navigate("Back") || flow.page != 3 {
		t.Fatalf("Back navigation state = page %d", flow.page)
	}
	if flow.draft.Workspace != "retained workspace" || flow.draft.APIKey != "secret-should-not-appear" || flow.draft.EvalThresholdText != "9" {
		t.Fatal("Back navigation lost edited draft values")
	}
	if !flow.navigate("Continue") || flow.page != 4 {
		t.Fatalf("Continue navigation state = page %d", flow.page)
	}
	first := &initFlow{draft: draft}
	if !first.navigate("Back") || first.page != 0 {
		t.Fatalf("Back from first page = %d", first.page)
	}
	review := initReview(draft)
	if strings.Contains(review, draft.APIKey) || !strings.Contains(review, masked(draft.APIKey, true)) {
		t.Fatalf("review did not mask secret: %s", review)
	}
	if draft.Workspace != "retained workspace" {
		t.Fatal("draft value was lost during back navigation")
	}
	if draft.EvalThresholdText != "9" {
		t.Fatal("evaluator threshold edit was lost during back navigation")
	}
}

func TestInitToolSelectionAndEvaluatorThresholdValidation(t *testing.T) {
	if err := validateInitCustomTools(nil); err == nil {
		t.Fatal("empty custom allowlist passed page validation")
	}
	if err := validateInitCustomTools([]string{"bash"}); err != nil {
		t.Fatalf("non-empty custom allowlist rejected: %v", err)
	}
	for _, test := range []struct {
		value string
		want  int
		valid bool
	}{{"3", 3, true}, {"12", 12, true}, {"0", 0, false}, {"-1", 0, false}, {"abc", 0, false}} {
		got, err := parsePositiveThreshold(test.value)
		if test.valid && (err != nil || got != test.want) {
			t.Errorf("threshold %q = %d, %v", test.value, got, err)
		}
		if !test.valid && err == nil {
			t.Errorf("threshold %q unexpectedly accepted", test.value)
		}
	}
	draft := newInitDraft(filepath.Join(t.TempDir(), "target"))
	draft.EvalEnabled = true
	draft.EvalThreshold = 7
	draft.EvalThresholdText = "7"
	if !strings.Contains(draft.configContent(), "success_threshold = 7") {
		t.Fatal("configured evaluator threshold was not generated")
	}
}

func TestInitPlainAndTTYEquivalentDraftAnswersBuildIdenticalConfig(t *testing.T) {
	for _, name := range config.APIKeyEnvironmentNames() {
		t.Setenv(name, "")
	}
	dir := filepath.Join(t.TempDir(), "plain-created")
	cmd := initCommand()
	if err := cmd.Flags().Set("dir", dir); err != nil {
		t.Fatal(err)
	}
	answers := []string{
		"", "workspace-choice", "", "", "", "n", "custom-model", "full", "Default tool set",
		"n", "", "y", "5", "", "", "y", "n", "n", "y",
	}
	var out, stderr bytes.Buffer
	cmd.SetIn(strings.NewReader(strings.Join(answers, "\n") + "\n"))
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	if err := runInit(cmd, nil); err != nil {
		t.Fatalf("plain init: %v; stderr=%s", err, stderr.String())
	}
	created, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// These are the same answers held by the TTY form's shared initDraft;
	// comparing against the actual plain-mode output exercises prompt parsing
	// as well as the common generated-config builder.
	ttyDraft := newInitDraft(filepath.Join(t.TempDir(), "tty-equivalent"))
	ttyDraft.Workspace = "workspace-choice"
	ttyDraft.Model = "custom-model"
	ttyDraft.AgentMode = config.AgentModeFull
	ttyDraft.EvalEnabled = true
	ttyDraft.EvalThreshold = 5
	ttyDraft.EvalThresholdText = "5"
	ttyDraft.ForbidRM = true
	if got := ttyDraft.configContent(); string(created) != got {
		t.Fatalf("plain config differs from equivalent TTY draft\n--- plain ---\n%s\n--- TTY ---\n%s", created, got)
	}
}

func TestInitModelDiscoveryFailureFallsBackWithoutDiscardingDraft(t *testing.T) {
	draft := newInitDraft(filepath.Join(t.TempDir(), "target"))
	draft.APIKey = "retained-key"
	draft.Workspace = "retained-workspace"
	models, err := discoverInitModels(func() ([]string, error) { return nil, errors.New("offline") })
	if err == nil || len(models) != 0 {
		t.Fatalf("failed discovery result = %v, %v", models, err)
	}
	var available bool
	models, draft.Model, available = initModelDiscoveryResult(draft.BaseURL, models, err)
	if available || len(models) != 0 || draft.Model != config.SuggestModel(draft.BaseURL) {
		t.Fatalf("failed discovery did not fall back to provider suggestion: models=%v model=%q available=%t", models, draft.Model, available)
	}
	if draft.APIKey != "retained-key" || draft.Workspace != "retained-workspace" || draft.Model == "" {
		t.Fatalf("model fallback lost other draft values: %#v", draft)
	}
}

func TestInitPlainConfirmedCreationAndDefaultRefusal(t *testing.T) {
	for _, tc := range []struct {
		name         string
		confirmation string
		wantCreated  bool
	}{
		{name: "confirmation creates", confirmation: "y\n", wantCreated: true},
		{name: "default refusal", confirmation: "n\n", wantCreated: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range config.APIKeyEnvironmentNames() {
				t.Setenv(name, "")
			}
			t.Setenv("AGEAGE_API_KEY", "init-secret-not-for-output")
			dir := filepath.Join(t.TempDir(), "new target")
			cmd := initCommand()
			if err := cmd.Flags().Set("dir", dir); err != nil {
				t.Fatal(err)
			}
			input := strings.Repeat("\n", 17) + tc.confirmation
			var out, stderr bytes.Buffer
			cmd.SetIn(strings.NewReader(input))
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			if err := runInit(cmd, nil); err != nil {
				t.Fatalf("plain init failed: %v; stderr=%s", err, stderr.String())
			}
			if strings.Contains(out.String(), "init-secret-not-for-output") {
				t.Fatal("setup output exposed API key")
			}
			_, statErr := os.Stat(filepath.Join(dir, "config.toml"))
			if tc.wantCreated && statErr != nil {
				t.Fatalf("confirmed setup did not create config: %v", statErr)
			}
			if !tc.wantCreated && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("default refusal created config: %v", statErr)
			}
			if tc.wantCreated {
				if _, err := config.LoadConfig(filepath.Join(dir, "config.toml")); err != nil {
					t.Fatalf("created config invalid: %v", err)
				}
				info, err := os.Stat(filepath.Join(dir, "config.toml"))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o600 {
					t.Fatalf("config mode = %v, err=%v; want 0600", info.Mode().Perm(), err)
				}
				for _, relative := range []string{"data/AGENT.md", "data/SOUL.md", "skills"} {
					if _, err := os.Stat(filepath.Join(dir, relative)); err != nil {
						t.Errorf("missing starter target %s: %v", relative, err)
					}
				}
			}
		})
	}
}

func TestInitExistingConfigPromptDefaultsToNoAndAllowsExplicitReplace(t *testing.T) {
	for _, tc := range []struct {
		name        string
		answer      string
		wantReplace bool
	}{
		{name: "default no", answer: "n\n"},
		{name: "explicit yes", answer: "y\n", wantReplace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range config.APIKeyEnvironmentNames() {
				t.Setenv(name, "")
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			original := []byte("keep this until explicit replace\n")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := initCommand()
			if err := cmd.Flags().Set("dir", dir); err != nil {
				t.Fatal(err)
			}
			cmd.SetIn(strings.NewReader(strings.Repeat("\n", 17) + tc.answer))
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			if err := runInit(cmd, nil); err != nil {
				t.Fatalf("runInit: %v; stderr=%s", err, stderr.String())
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantReplace {
				if bytes.Equal(got, original) {
					t.Fatal("explicit replace left old config unchanged")
				}
				if _, err := config.LoadConfig(path); err != nil {
					t.Fatalf("replacement config invalid: %v", err)
				}
			} else if !bytes.Equal(got, original) {
				t.Fatalf("default refusal changed config: %q", got)
			}
		})
	}
}

func TestModelSuggestionsForGemini(t *testing.T) {
	const gemini = "https://generativelanguage.googleapis.com/v1beta/openai/"
	if got := config.SuggestModel(gemini); got != "gemini-3.5-flash" {
		t.Fatalf("SuggestModel(gemini) = %q", got)
	}
	if got := config.SuggestStrongModel(gemini, "gemini-3.5-flash"); got != "gemini-3.1-pro" {
		t.Fatalf("SuggestStrongModel(gemini) = %q", got)
	}
	if got := config.SuggestModel("http://localhost:11434/v1"); got != "llama3.3" {
		t.Fatalf("SuggestModel(ollama) = %q", got)
	}
}

func TestFindEnvAPIKeyUsesOwnedPrecedence(t *testing.T) {
	for _, name := range config.APIKeyEnvironmentNames() {
		t.Setenv(name, "")
	}
	t.Setenv("GEMINI_API_KEY", "gemini-key")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	if key, name := findEnvAPIKey(); key != "openai-key" || name != "OPENAI_API_KEY" {
		t.Fatalf("findEnvAPIKey() = (%q, %q)", key, name)
	}
}
