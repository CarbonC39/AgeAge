package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ageage/config"

	"github.com/charmbracelet/huh"
	"github.com/spf13/cobra"
)

func TestHuhAbortIsTypedCancellation(t *testing.T) {
	err := translateHuhError(huh.ErrUserAborted)
	if !errors.Is(err, errFormCancelled) {
		t.Fatalf("translated abort = %v", err)
	}
}

func TestConfigFormKeymapDisablesFilteringAndMapsEscapeToCancel(t *testing.T) {
	keymap := configFormKeyMap()
	if keymap.Select.Filter.Enabled() || keymap.Select.SetFilter.Enabled() || keymap.Select.ClearFilter.Enabled() {
		t.Fatal("config select filtering bindings should all be disabled")
	}
	if keymap.MultiSelect.Filter.Enabled() || keymap.MultiSelect.SetFilter.Enabled() || keymap.MultiSelect.ClearFilter.Enabled() {
		t.Fatal("config multi-select filtering bindings should all be disabled")
	}
	keys := keymap.Quit.Keys()
	if len(keys) != 2 || keys[0] != "esc" || keys[1] != "ctrl+c" {
		t.Fatalf("quit keys = %q, want Escape and Ctrl+C cancellation", keys)
	}
	toggleKeys := keymap.MultiSelect.Toggle.Keys()
	if len(toggleKeys) != 2 || toggleKeys[0] != " " || toggleKeys[1] != "x" {
		t.Fatalf("multi-select toggle keys = %q, want Space and x", toggleKeys)
	}
	if help := keymap.MultiSelect.Toggle.Help(); help.Key != "space" || help.Desc != "toggle" {
		t.Fatalf("multi-select toggle help = %#v, want space/toggle", help)
	}
}

func TestCLIChoiceAdaptersMirrorOwnerProviders(t *testing.T) {
	values := func(choices []config.Choice) []string {
		result := make([]string, len(choices))
		for i, choice := range choices {
			result[i] = choice.Value
		}
		return result
	}
	for _, test := range []struct {
		name string
		got  []config.Choice
		want []config.Choice
	}{
		{"agent modes", ownerChoices("agent.mode"), config.AgentModeChoices()},
		{"notification presets", ownerChoices("notifications.preset"), config.ProgressPresetChoices()},
		{"search backends", ownerChoices("web_search.backend"), backendChoices(config.WebSearchBackendChoices())},
		{"fetch backends", ownerChoices("web_fetch.backend"), backendChoices(config.WebFetchBackendChoices())},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(values(test.got), values(test.want)) {
				t.Fatalf("CLI choices = %v; owner provider = %v", values(test.got), values(test.want))
			}
		})
	}
	if got, want := values(config.ProgressCategoryChoices()), []string{
		config.ProgressLifecycle, config.ProgressPlan, config.ProgressWaiting,
		config.ProgressTool, config.ProgressSubagent, config.ProgressCron,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("notification category provider = %v; runtime categories = %v", got, want)
	}
}

func TestConfigCommandUsesInjectedStreamsAndRejectsNonTTYWithoutWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	before := []byte("[agent]\nmode = \"full\"\n# keep this comment\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := configCommand()
	cmd.SetArgs([]string{"--config", path, "tools"})
	var out, stderr bytes.Buffer
	cmd.SetIn(strings.NewReader("\n"))
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("non-terminal config command error = %v; stderr=%q", err, stderr.String())
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("injected non-TTY command changed config: %q, err=%v", after, readErr)
	}
}

func TestConfigDraftCancellationLeavesBytesUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("[llm]\napi_key = \"secret\" # keep\nmodel = \"old\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	draft, _, err := newConfigDraft(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := draft.stage("llm", "model", `"new"`, "llm.model", false); err != nil {
		t.Fatal(err)
	}
	if resolveDraftDecision("", true, true) != draftDiscard {
		t.Fatal("confirmed cancellation did not discard the draft")
	}
	if !draft.dirty() {
		t.Fatal("test setup did not stage a change")
	}
	// The cancellation decision intentionally does not invoke save.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("cancel changed file:\n%s", after)
	}
}

func TestDraftStageRevertRemovesChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[agent]\nmode = \"full\"\ntools = [\"bash\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	draft, cfg, err := newConfigDraft(path)
	if err != nil {
		t.Fatal(err)
	}
	baseMode := tomlString(cfg.Agent.Mode)
	if err := draft.stage("agent", "mode", `"supervised"`, "agent.mode", false); err != nil {
		t.Fatal(err)
	}
	if !draft.dirty() {
		t.Fatal("changed value did not dirty draft")
	}
	if err := draft.stage("agent", "mode", baseMode, "agent.mode", false); err != nil {
		t.Fatal(err)
	}
	if draft.dirty() {
		t.Fatalf("reverted value remains staged: %#v", draft.changes)
	}
	var review strings.Builder
	draft.review(&review)
	if review.Len() != 0 {
		t.Fatalf("review includes reverted values: %q", review.String())
	}
	if err := stageToolSelection(draft, "Custom allowlist", cfg.Agent.Tools); err != nil {
		t.Fatal(err)
	}
	if draft.dirty() {
		t.Fatalf("choosing current tool list dirtied draft: %#v", draft.changes)
	}
	if err := stageToolSelection(draft, "Default tool set", nil); err != nil {
		t.Fatal(err)
	}
	if !draft.dirty() {
		t.Fatal("changing tool mode to default did not dirty draft")
	}
	if err := stageToolSelection(draft, "Custom allowlist", cfg.Agent.Tools); err != nil {
		t.Fatal(err)
	}
	if draft.dirty() {
		t.Fatalf("reverting tool mode/list left changes: %#v", draft.changes)
	}
}

func TestStagedChangesApplyInStableKeyOrder(t *testing.T) {
	draft := &configDraft{original: []byte("[agent]\n"), changes: map[string]configChange{
		"agent.tools": {section: "agent", key: "tools", value: `["bash"]`},
		"agent.mode":  {section: "agent", key: "mode", value: `"full"`},
	}}
	first, err := draft.merged()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		next, e := draft.merged()
		if e != nil {
			t.Fatal(e)
		}
		if string(next) != string(first) {
			t.Fatalf("merge order varied:\n%s\n%s", first, next)
		}
	}
}

func TestFinalDecisionDefaultsAndCancellationAreNonDestructive(t *testing.T) {
	if got := resolveDraftDecision("", false, false); got != draftKeepEditing {
		t.Fatalf("empty/default choice = %v", got)
	}
	if got := resolveDraftDecision("Discard all staged changes", true, false); got != draftKeepEditing {
		t.Fatalf("unconfirmed cancellation = %v", got)
	}
	if got := resolveDraftDecision("", true, true); got != draftDiscard {
		t.Fatalf("confirmed cancellation = %v", got)
	}
	if got := resolveDraftDecision("Save all staged changes", false, false); got != draftSave {
		t.Fatalf("save choice = %v", got)
	}
}

func TestInvalidDraftCannotBeSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("[agent]\nmode = \"full\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	draft, _, err := newConfigDraft(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := draft.stage("agent", "mode", `not-a-string`, "agent.mode", false); err != nil {
		t.Fatal(err)
	}
	if err := draft.save(); err == nil {
		t.Fatal("invalid TOML value saved")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("invalid draft changed file: %s", got)
	}
}

func TestTOMLBatchInsertsNestedSectionAndKeepsHeaderComment(t *testing.T) {
	original := []byte("[router] # preserve header comment\nenabled = true\n")
	updated, err := applyTOMLChanges(original, []configChange{{section: "router.classifier", key: "model", value: `"fast"`}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[router] # preserve header comment", "[router.classifier]", "model = \"fast\""} {
		if !strings.Contains(string(updated), want) {
			t.Fatalf("missing %q in %s", want, updated)
		}
	}
}

func TestTOMLBatchIgnoresStructureInsideMultilineStrings(t *testing.T) {
	prefix := `[future]
payload = """
[agent]
tools = ["inside-string"]
# tools = ["also-inside-string"]
"""

`
	suffix := "[future_after]\nanswer = 9 # still preserved\n"
	original := []byte(prefix + "[agent]\nmode = \"full\"\ntools = [\"real\"]\n" + suffix)
	updated, err := applyTOMLChanges(original, []configChange{{section: "agent", key: "tools", value: `["bash"]`}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(updated), prefix) {
		t.Fatalf("multiline unknown content changed:\n%s", updated)
	}
	if !strings.HasSuffix(string(updated), suffix) {
		t.Fatalf("content outside real section changed:\n%s", updated)
	}
	if !strings.Contains(string(updated), "tools = [\"bash\"]") || strings.Contains(string(updated), "tools = [\"real\"]") {
		t.Fatalf("real agent assignment not targeted:\n%s", updated)
	}
}

func TestTOMLCommentParserHandlesSingleQuotedLiteral(t *testing.T) {
	original := []byte("[agent]\nmode = 'old # literal' # preserve this comment\n")
	updated, err := applyTOMLChanges(original, []configChange{{section: "agent", key: "mode", value: `"supervised"`}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), `mode = "supervised" # preserve this comment`) {
		t.Fatalf("literal # was treated as comment: %s", updated)
	}
}

func TestToolSelectionDefaultAndCustomSemantics(t *testing.T) {
	draft := &configDraft{changes: map[string]configChange{}}
	if err := stageToolSelection(draft, "Default tool set", nil); err != nil {
		t.Fatal(err)
	}
	if got := draft.changes["agent.tools"].value; got != "[]" {
		t.Fatalf("default list = %q", got)
	}
	if err := stageToolSelection(draft, "Custom allowlist", nil); err == nil {
		t.Fatal("empty custom allowlist accepted")
	}
	if err := stageToolSelection(draft, "Custom allowlist", []string{"bash", "mcp_custom"}); err != nil {
		t.Fatal(err)
	}
	if got := draft.changes["agent.tools"].value; got != `["bash", "mcp_custom"]` {
		t.Fatalf("custom list = %q", got)
	}
}

func TestToolsAndCompatibilityAliasRejectNonTTY(t *testing.T) {
	makeCommand := func() *cobra.Command {
		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader(""))
		cmd.SetOut(&strings.Builder{})
		cmd.Flags().StringP("config", "c", "", "")
		return cmd
	}
	err := runConfigTools(makeCommand(), nil)
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("config tools error = %v", err)
	}
	aliasErr := runTools(makeCommand(), nil)
	if aliasErr == nil || aliasErr.Error() != err.Error() {
		t.Fatalf("alias error = %v; want %v", aliasErr, err)
	}
}

func TestTargetedTOMLBatchPreservesCommentsUnknownDataAndMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("# header\n[agent]\nmode = \"full\" # keep inline\n# tools = []\n\n[future]\nanswer = 42 # unknown\n")
	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatal(err)
	}
	draft, _, err := newConfigDraft(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := draft.stage("agent", "mode", `"supervised"`, "agent.mode", false); err != nil {
		t.Fatal(err)
	}
	if err := draft.stage("agent", "tools", `["bash", "mcp_offline"]`, "agent.tools", false); err != nil {
		t.Fatal(err)
	}
	if err := draft.save(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# header", `mode = "supervised" # keep inline`, `tools = ["bash", "mcp_offline"]`, "[future]", "answer = 42 # unknown"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
}

func TestTargetedTOMLBatchDetectsExternalModification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := []byte("[agent]\nmode = \"full\"\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	draft, _, err := newConfigDraft(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := draft.stage("agent", "mode", `"supervised"`, "agent.mode", false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[agent]\nmode = \"external\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := draft.save(); err == nil || !strings.Contains(err.Error(), "changed on disk") {
		t.Fatalf("save conflict = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "external") {
		t.Fatalf("external content overwritten: %s", got)
	}
}

func TestNotificationOverrideDraftCreateEditReenterAndUnset(t *testing.T) {
	choices := config.ChannelTypeChoices()
	for _, channel := range choices {
		cfg := &config.Config{Notifications: config.NotificationConfig{Preset: config.ProgressPresetBalanced, ThrottleMS: 750}}
		draft := &configDraft{base: baselineConfigValues(cfg), changes: map[string]configChange{}}
		state := effectiveNotificationOverride(draft, cfg, channel.Value)
		if state.exists {
			t.Fatalf("%s unexpectedly starts with override", channel.Value)
		}
		section := "notifications.channels." + channel.Value
		if err := draft.stage(section, "preset", tomlString(config.ProgressPresetQuiet), "preset", false); err != nil {
			t.Fatal(err)
		}
		if err := draft.stage(section, "include", tomlStrings([]string{"custom_category", config.ProgressTool}), "include", false); err != nil {
			t.Fatal(err)
		}
		if err := draft.stage(section, "exclude", tomlStrings([]string{config.ProgressPlan}), "exclude", false); err != nil {
			t.Fatal(err)
		}
		if err := draft.stage(section, "throttle_ms", "1200", "throttle", false); err != nil {
			t.Fatal(err)
		}
		state = effectiveNotificationOverride(draft, cfg, channel.Value)
		if !state.exists || state.preset != config.ProgressPresetQuiet || !state.includeSet || !sameStrings(state.include, []string{"custom_category", config.ProgressTool}) || !sameStrings(state.exclude, []string{config.ProgressPlan}) || state.throttle != 1200 {
			t.Fatalf("staged override was not restored for %s: %#v", channel.Value, state)
		}
		for _, key := range []string{"preset", "include", "exclude", "throttle_ms"} {
			if err := draft.stageUnset(section, key, key, false); err != nil {
				t.Fatal(err)
			}
		}
		state = effectiveNotificationOverride(draft, cfg, channel.Value)
		if state.exists || draft.dirty() {
			t.Fatalf("unset did not restore absent override for %s: state=%#v changes=%#v", channel.Value, state, draft.changes)
		}
	}
}

func TestNotificationOverrideExistingAllFieldsStagedUnsetAndRevert(t *testing.T) {
	channel := "telegram"
	section := "notifications.channels." + channel
	baseOverride := config.NotificationChannelConfig{
		Preset:     config.ProgressPresetQuiet,
		Include:    []string{config.ProgressTool, "vendor_category"},
		Exclude:    []string{config.ProgressPlan},
		ThrottleMS: 1250,
	}
	cfg := &config.Config{Notifications: config.NotificationConfig{
		Channels: map[string]config.NotificationChannelConfig{channel: baseOverride},
	}}
	draft := &configDraft{base: baselineConfigValues(cfg), changes: map[string]configChange{}}
	for _, key := range []string{"preset", "include", "exclude", "throttle_ms"} {
		if err := draft.stageUnset(section, key, key, false); err != nil {
			t.Fatal(err)
		}
	}
	state := effectiveNotificationOverride(draft, cfg, channel)
	if state.exists || !draft.dirty() {
		t.Fatalf("all staged unsets should hide effective override but remain pending: state=%#v changes=%#v", state, draft.changes)
	}
	if err := draft.stage(section, "preset", tomlString(baseOverride.Preset), "preset", false); err != nil {
		t.Fatal(err)
	}
	state = effectiveNotificationOverride(draft, cfg, channel)
	if !state.exists || !state.presetSet || state.preset != baseOverride.Preset {
		t.Fatalf("reverted preset did not restore effective field: %#v", state)
	}
	if _, stillStaged := draft.changes[changeID(section, "preset")]; stillStaged {
		t.Fatal("reverting preset to baseline left an unnecessary staged unset")
	}
	if !draft.dirty() {
		t.Fatal("other staged unsets should remain pending after reverting preset")
	}
}

func TestRunConfigValidateWorksWithoutTTY(t *testing.T) {
	validPath := filepath.Join(t.TempDir(), "valid.toml")
	if err := os.WriteFile(validPath, []byte("[agent]\nmode = \"full\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	validCmd, validOut, validErr := configTestCommand(validPath)
	if err := runConfigValidate(validCmd, nil); err != nil {
		t.Fatalf("valid non-TTY config rejected: %v (%s)", err, validErr.String())
	}
	if !strings.Contains(validOut.String(), "Valid:") {
		t.Fatalf("valid output = %q", validOut.String())
	}

	invalidPath := filepath.Join(t.TempDir(), "invalid.toml")
	if err := os.WriteFile(invalidPath, []byte("[agent\nmode = \"full\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalidCmd, invalidOut, invalidErr := configTestCommand(invalidPath)
	if err := runConfigValidate(invalidCmd, nil); err == nil {
		t.Fatal("invalid non-TTY config was accepted")
	}
	if invalidOut.Len() != 0 || !strings.Contains(invalidErr.String(), "Invalid") {
		t.Fatalf("invalid output=%q error=%q", invalidOut.String(), invalidErr.String())
	}
}

func TestRunConfigEditUsesVisualArgvAndValidatesResult(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "path with spaces")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(configDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("[agent]\nmode = \"full\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	argvPath := filepath.Join(t.TempDir(), "editor-argv.json")
	helper, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGEAGE_TEST_EDITOR_HELPER", "1")
	t.Setenv("AGEAGE_TEST_EDITOR_MODE", "valid")
	t.Setenv("AGEAGE_TEST_EDITOR_ARGV", argvPath)
	t.Setenv("VISUAL", helper+" -test.run=TestEditorHelperProcess")
	t.Setenv("EDITOR", "missing-editor-should-not-run")
	cmd, out, stderr := configTestCommand(configPath)
	if err := runConfigEdit(cmd, nil); err != nil {
		t.Fatalf("runConfigEdit failed (stderr %q): %v", stderr.String(), err)
	}
	var argv []string
	data, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &argv); err != nil {
		t.Fatal(err)
	}
	if got := argv[len(argv)-1]; got != configPath {
		t.Fatalf("editor final argv = %q, want exact config path %q; argv=%q", got, configPath, argv)
	}
	if !strings.Contains(out.String(), "Updated") {
		t.Fatalf("edit output = %q", out.String())
	}

	t.Setenv("AGEAGE_TEST_EDITOR_MODE", "invalid")
	if err := runConfigEdit(cmd, nil); err == nil || !strings.Contains(err.Error(), "is invalid") {
		t.Fatalf("invalid TOML after editor returned error = %v", err)
	}
}

// TestEditorHelperProcess is launched as the test executable by runConfigEdit.
// It records argv and optionally replaces the target with invalid TOML.
func TestEditorHelperProcess(t *testing.T) {
	if os.Getenv("AGEAGE_TEST_EDITOR_HELPER") != "1" {
		return
	}
	args := os.Args
	target := args[len(args)-1]
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("AGEAGE_TEST_EDITOR_ARGV"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("AGEAGE_TEST_EDITOR_MODE") == "invalid" {
		if err := os.WriteFile(target, []byte("[agent\nmode = \"full\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func configTestCommand(path string) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	cmd.Flags().String("config", path, "")
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetIn(strings.NewReader(""))
	return cmd, &out, &stderr
}

func TestNotificationOverrideNoOpPreservesUnknownCategoryOrder(t *testing.T) {
	values := []string{"vendor_future", "tool"}
	cfg := &config.Config{Notifications: config.NotificationConfig{Channels: map[string]config.NotificationChannelConfig{"telegram": {Preset: "quiet", Include: values}}}}
	draft := &configDraft{base: baselineConfigValues(cfg), changes: map[string]configChange{}}
	state := effectiveNotificationOverride(draft, cfg, "telegram")
	if !state.exists || !sameStrings(state.include, values) {
		t.Fatalf("configured order/unknown value lost: %#v", state)
	}
	if err := draft.stage("notifications.channels.telegram", "include", tomlStrings(values), "include", false); err != nil {
		t.Fatal(err)
	}
	if draft.dirty() {
		t.Fatalf("no-op category edit dirtied draft: %#v", draft.changes)
	}
	if err := draft.stage("notifications.channels.telegram", "include", tomlStrings([]string{"tool", "vendor_future"}), "include", false); err != nil {
		t.Fatal(err)
	}
	if draft.dirty() {
		t.Fatalf("set-equivalent no-op reordered category list: %#v", draft.changes)
	}
}
