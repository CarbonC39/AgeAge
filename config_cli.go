package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"ageage/agent"
	"ageage/config"

	"github.com/BurntSushi/toml"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// errFormCancelled is deliberately distinct from an operational failure so
// callers can return quietly without committing their draft.
var errFormCancelled = errors.New("form cancelled")

func runHuh(form *huh.Form, cmd *cobra.Command) error {
	if err := form.WithKeyMap(configFormKeyMap()).WithInput(cmd.InOrStdin()).WithOutput(cmd.OutOrStdout()).Run(); err != nil {
		return translateHuhError(err)
	}
	return nil
}

func translateHuhError(err error) error {
	if errors.Is(err, huh.ErrUserAborted) {
		return errFormCancelled
	}
	return err
}

func selectForm(cmd *cobra.Command, title string, choices []huh.Option[string], value *string) error {
	field := huh.NewSelect[string]().Title(title).Options(choices...).Value(value).Filtering(false)
	return runHuh(huh.NewForm(huh.NewGroup(field)), cmd)
}

func configFormKeyMap() *huh.KeyMap {
	keymap := huh.NewDefaultKeyMap()
	// Escape cancels every config form. Disable Huh's filtering shortcuts so
	// Escape cannot be consumed by a select or multi-select filter.
	keymap.Quit.SetKeys("esc", "ctrl+c")
	keymap.Select.Filter.SetEnabled(false)
	keymap.Select.SetFilter.SetEnabled(false)
	keymap.Select.ClearFilter.SetEnabled(false)
	keymap.MultiSelect.Filter.SetEnabled(false)
	keymap.MultiSelect.SetFilter.SetEnabled(false)
	keymap.MultiSelect.ClearFilter.SetEnabled(false)
	keymap.MultiSelect.Toggle.SetKeys(" ", "x")
	keymap.MultiSelect.Toggle.SetHelp("space", "toggle")
	return keymap
}

func inputForm(cmd *cobra.Command, title string, value *string, secret bool) error {
	field := huh.NewInput().Title(title).Value(value)
	if secret {
		field = field.EchoMode(huh.EchoModePassword)
	}
	return runHuh(huh.NewForm(huh.NewGroup(field)), cmd)
}

func optionChoices(values []string) []huh.Option[string] { return huh.NewOptions(values...) }

type configChange struct {
	section string
	key     string
	value   string // TOML encoded scalar/array, not a whole-config serialization.
	label   string
	secret  bool
	unset   bool
}

type configDraft struct {
	path     string
	original []byte
	mode     os.FileMode
	base     map[string]string
	changes  map[string]configChange
}

type draftDecision uint8

const (
	draftKeepEditing draftDecision = iota
	draftSave
	draftDiscard
)

func resolveDraftDecision(choice string, formCancelled, discardConfirmed bool) draftDecision {
	if formCancelled {
		if discardConfirmed {
			return draftDiscard
		}
		return draftKeepEditing
	}
	switch choice {
	case "Save all staged changes":
		return draftSave
	case "Discard all staged changes":
		return draftDiscard
	default:
		return draftKeepEditing
	}
}

const absentConfigValue = "\x00<absent>"

func newConfigDraft(path string) (*configDraft, *config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		return nil, nil, err
	}
	draft := &configDraft{path: path, original: data, mode: info.Mode().Perm(), changes: map[string]configChange{}}
	draft.base = baselineConfigValues(cfg)
	return draft, cfg, nil
}

func changeID(section, key string) string { return section + "." + key }

func (d *configDraft) stage(section, key, value, label string, secret bool) error {
	if full := changeID(section, key); strings.Contains(full, " ") {
		return fmt.Errorf("invalid key")
	}
	if !isEditableConfigKey(changeID(section, key)) {
		return fmt.Errorf("configuration key %s.%s is not editable here", section, key)
	}
	id := changeID(section, key)
	if base, ok := d.base[id]; ok {
		equal := base == value
		if !equal && isSetListField(id) {
			equal = sameTOMLStringSet(base, value)
		}
		if equal {
			delete(d.changes, id)
			return nil
		}
	}
	d.changes[id] = configChange{section, key, value, label, secret, false}
	return nil
}

func isSetListField(id string) bool {
	if id == "agent.tools" || id == "notifications.include" || id == "notifications.exclude" {
		return true
	}
	parts := strings.Split(id, ".")
	return len(parts) == 4 && parts[0] == "notifications" && parts[1] == "channels" && (parts[3] == "include" || parts[3] == "exclude")
}

func sameTOMLStringSet(left, right string) bool {
	decode := func(raw string) ([]string, bool) {
		var value struct {
			Items []string `toml:"value"`
		}
		_, err := toml.Decode("value = "+raw, &value)
		return value.Items, err == nil
	}
	a, ok := decode(left)
	if !ok {
		return false
	}
	b, ok := decode(right)
	if !ok {
		return false
	}
	return sameStringSet(a, b)
}

func (d *configDraft) stageUnset(section, key, label string, secret bool) error {
	id := changeID(section, key)
	if !isEditableConfigKey(id) {
		return fmt.Errorf("configuration key %s.%s is not editable here", section, key)
	}
	if base, ok := d.base[id]; ok && base == absentConfigValue {
		delete(d.changes, id)
		return nil
	}
	d.changes[id] = configChange{section: section, key: key, label: label, secret: secret, unset: true}
	return nil
}

func baselineConfigValues(cfg *config.Config) map[string]string {
	values := make(map[string]string, len(editableConfigKeys)+12)
	for id := range editableConfigKeys {
		value, _, _ := configValue(cfg, id)
		values[id] = value
	}
	values["agent.tools"] = tomlStrings(cfg.Agent.Tools)
	for _, channel := range config.ChannelTypeChoices() {
		section := "notifications.channels." + channel.Value
		override, exists := cfg.Notifications.Channels[channel.Value]
		fields := map[string]string{"preset": absentConfigValue, "include": absentConfigValue, "exclude": absentConfigValue, "throttle_ms": absentConfigValue}
		if exists {
			if override.Preset != "" {
				fields["preset"] = tomlString(override.Preset)
			}
			if override.Include != nil {
				fields["include"] = tomlStrings(override.Include)
			}
			if override.Exclude != nil {
				fields["exclude"] = tomlStrings(override.Exclude)
			}
			if override.ThrottleMS > 0 {
				fields["throttle_ms"] = strconv.Itoa(override.ThrottleMS)
			}
		}
		for key, value := range fields {
			values[changeID(section, key)] = value
		}
	}
	return values
}

// Approved here as targeted UI-owned keys. This is intentionally finite and
// is not a reflected serializer or user-extensible schema.
var editableConfigKeys = map[string]struct{}{
	"llm.api_key": {}, "llm.base_url": {}, "llm.model": {}, "llm.temperature": {}, "llm.max_tokens": {},
	"agent.mode": {}, "agent.max_iterations": {}, "agent.max_parallel_tools": {}, "agent.tools": {},
	"router.enabled": {}, "router.max_history": {}, "router.classifier.model": {}, "router.classifier.api_key": {}, "router.classifier.base_url": {},
	"router.medium.model": {}, "router.medium.api_key": {}, "router.medium.base_url": {}, "router.strong.model": {}, "router.strong.api_key": {}, "router.strong.base_url": {},
	"planner.enabled": {}, "eval.success_threshold": {}, "eval.model.model": {}, "eval.model.api_key": {}, "eval.model.base_url": {},
	"web_search.backend": {}, "web_search.searxng_url": {}, "web_search.tavily_api_key": {}, "web_search.brave_api_key": {},
	"web_fetch.backend": {}, "web_fetch.jina_api_key": {}, "web_fetch.crawl4ai_cmd": {},
	"notifications.preset": {}, "notifications.include": {}, "notifications.exclude": {}, "notifications.throttle_ms": {},
	"security.forbid_rm": {},
}

func isEditableConfigKey(id string) bool {
	if _, ok := editableConfigKeys[id]; ok {
		return true
	}
	parts := strings.Split(id, ".")
	if len(parts) != 4 || parts[0] != "notifications" || parts[1] != "channels" {
		return false
	}
	channelKnown := false
	for _, choice := range config.ChannelTypeChoices() {
		if parts[2] == choice.Value {
			channelKnown = true
			break
		}
	}
	return channelKnown && (parts[3] == "preset" || parts[3] == "include" || parts[3] == "exclude" || parts[3] == "throttle_ms")
}

func tomlString(value string) string { return strconv.Quote(value) }
func tomlStrings(values []string) string {
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = tomlString(value)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func (d *configDraft) dirty() bool { return len(d.changes) != 0 }

func (d *configDraft) merged() ([]byte, error) {
	return applyTOMLChanges(d.original, sortedConfigChanges(d.changes))
}

func sortedConfigChanges(changes map[string]configChange) []configChange {
	keys := make([]string, 0, len(changes))
	for key := range changes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]configChange, 0, len(keys))
	for _, key := range keys {
		result = append(result, changes[key])
	}
	return result
}

func validateConfigBytes(data []byte) error {
	var cfg config.Config
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return err
	}
	return nil
}

func (d *configDraft) validate() error {
	data, err := d.merged()
	if err != nil {
		return err
	}
	return validateConfigBytes(data)
}

func (d *configDraft) save() error {
	data, err := d.merged()
	if err != nil {
		return err
	}
	if err := validateConfigBytes(data); err != nil {
		return fmt.Errorf("draft config is invalid: %w", err)
	}
	return writeTOMLBatch(d.path, d.original, d.mode, data)
}

func (d *configDraft) review(w io.Writer) {
	keys := make([]string, 0, len(d.changes))
	for key := range d.changes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		change := d.changes[key]
		value := change.value
		if change.unset {
			value = "(remove override)"
		}
		if change.secret {
			value = "••••••"
		}
		fmt.Fprintf(w, "  %s: %s\n", change.label, value)
	}
}

func applyTOMLChanges(original []byte, changes []configChange) ([]byte, error) {
	sort.SliceStable(changes, func(i, j int) bool {
		return changeID(changes[i].section, changes[i].key) < changeID(changes[j].section, changes[j].key)
	})
	lines := strings.Split(string(original), "\n")
	for _, change := range changes {
		id := changeID(change.section, change.key)
		if !isEditableConfigKey(id) {
			return nil, fmt.Errorf("unsupported TOML update %s", id)
		}
		sectionHeader := "[" + change.section + "]"
		sectionStart, sectionEnd := -1, len(lines)
		codeLines, inMultiline := tomlCodeLines(lines)
		for i, line := range codeLines {
			stripped := strings.TrimSpace(line)
			if strings.HasPrefix(stripped, "[") && strings.HasSuffix(stripped, "]") {
				if sectionStart >= 0 {
					sectionEnd = i
					break
				}
				if stripped == sectionHeader {
					sectionStart = i
				}
			}
		}
		if sectionStart < 0 {
			if len(lines) > 0 && lines[len(lines)-1] != "" {
				lines = append(lines, "")
			}
			lines = append(lines, sectionHeader, change.key+" = "+change.value)
			continue
		}
		keyLine := -1
		for i := sectionStart + 1; i < sectionEnd; i++ {
			trimmed := strings.TrimSpace(codeLines[i])
			if trimmed == "" && !inMultiline[i] && strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
				trimmed = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "#"))
			}
			if before, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(before) == change.key {
				keyLine = i
				break
			}
		}
		if keyLine >= 0 {
			old := lines[keyLine]
			if change.unset {
				comment := inlineTOMLComment(old)
				if comment == "" {
					lines = append(lines[:keyLine], lines[keyLine+1:]...)
				} else {
					lines[keyLine] = strings.Repeat(" ", len(old)-len(strings.TrimLeft(old, " \t"))) + comment
				}
				continue
			}
			comment := inlineTOMLComment(old)
			if strings.HasPrefix(strings.TrimSpace(old), "#") {
				uncommented := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(old), "#"))
				if _, tail, ok := strings.Cut(uncommented, "="); ok {
					comment = inlineTOMLComment(tail)
				}
			}
			indent := old[:len(old)-len(strings.TrimLeft(old, " \t"))]
			lines[keyLine] = indent + change.key + " = " + change.value
			if comment != "" {
				lines[keyLine] += " " + comment
			}
		} else if !change.unset {
			lines = append(lines[:sectionEnd], append([]string{change.key + " = " + change.value}, lines[sectionEnd:]...)...)
		}
	}
	return []byte(strings.Join(lines, "\n")), nil
}

type tomlLexState struct {
	multiline byte
	basic     bool
}

func tomlCodeLines(lines []string) ([]string, []bool) {
	state := tomlLexState{}
	code := make([]string, len(lines))
	inMultiline := make([]bool, len(lines))
	for i, line := range lines {
		inMultiline[i] = state.multiline != 0
		code[i], _ = tomlCodeLine(line, &state)
	}
	return code, inMultiline
}

// tomlCodeLine blanks string contents while retaining structural TOML syntax.
// Its multiline state prevents apparent section headers and assignments inside
// multiline values from being mistaken for actual TOML statements.
func tomlCodeLine(line string, state *tomlLexState) (string, int) {
	var out strings.Builder
	commentAt := -1
	for i := 0; i < len(line); {
		if state.multiline != 0 {
			quote := state.multiline
			if quote == '"' && state.basic && line[i] == '\\' && i+1 < len(line) {
				out.WriteString("  ")
				i += 2
				continue
			}
			if i+2 < len(line) && line[i] == quote && line[i+1] == quote && line[i+2] == quote {
				out.WriteString("   ")
				i += 3
				state.multiline = 0
				state.basic = false
				continue
			}
			out.WriteByte(' ')
			i++
			continue
		}
		if line[i] == '#' {
			commentAt = i
			break
		}
		if line[i] == '"' || line[i] == '\'' {
			quote := line[i]
			if i+2 < len(line) && line[i+1] == quote && line[i+2] == quote {
				state.multiline = quote
				state.basic = quote == '"'
				out.WriteString("   ")
				i += 3
				continue
			}
			out.WriteByte(' ')
			i++
			for i < len(line) {
				if quote == '"' && line[i] == '\\' && i+1 < len(line) {
					out.WriteString("  ")
					i += 2
					continue
				}
				if line[i] == quote {
					out.WriteByte(' ')
					i++
					break
				}
				out.WriteByte(' ')
				i++
			}
			continue
		}
		out.WriteByte(line[i])
		i++
	}
	return out.String(), commentAt
}

func inlineTOMLComment(line string) string {
	_, index := tomlCodeLine(line, &tomlLexState{})
	if index < 0 {
		return ""
	}
	return strings.TrimSpace(line[index:])
}

func withoutTOMLComment(line string) string {
	code, _ := tomlCodeLine(line, &tomlLexState{})
	return code
}

func writeTOMLBatch(path string, original []byte, mode os.FileMode, replacement []byte) error {
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, original) || info.Mode().Perm() != mode {
		return fmt.Errorf("config changed on disk; reload before saving")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(replacement); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	latest, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err = os.Stat(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(latest, original) || info.Mode().Perm() != mode {
		return fmt.Errorf("config changed on disk; reload before saving")
	}
	return os.Rename(tmpName, path)
}

func configCommand() *cobra.Command {
	root := &cobra.Command{Use: "config", Short: "Review and edit configuration", RunE: runConfig}
	root.PersistentFlags().StringP("config", "c", "", "Path to config.toml")
	root.AddCommand(&cobra.Command{Use: "tools", Short: "Edit the default tool allowlist", RunE: runConfigTools})
	root.AddCommand(&cobra.Command{Use: "validate", Short: "Parse and validate the resolved config file", RunE: runConfigValidate})
	root.AddCommand(&cobra.Command{Use: "edit", Short: "Open config.toml in a text editor", RunE: runConfigEdit})
	return root
}

func configPathFromCmd(cmd *cobra.Command) string {
	value := ""
	if flag := cmd.Flag("config"); flag != nil {
		value = flag.Value.String()
	}
	return findConfigFile(value)
}

func requireInteractive(cmd *cobra.Command) error {
	if !termIsTerminal(cmd) {
		return fmt.Errorf("this command needs a terminal; use `ageage config edit` for plain editing or `ageage config validate` in scripts")
	}
	return nil
}

func termIsTerminal(cmd *cobra.Command) bool {
	// Cobra streams are injectable for tests. The production command uses the
	// process descriptors because Huh needs terminal mode, not just a reader.
	return cmd.InOrStdin() == os.Stdin && cmd.OutOrStdout() == os.Stdout && term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}

func runtimeOSWindows() bool { return runtime.GOOS == "windows" }

func runConfigValidate(cmd *cobra.Command, _ []string) error {
	path := configPathFromCmd(cmd)
	if _, err := config.LoadConfig(path); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Invalid %s: %v\n", path, err)
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Valid: %s\n", path)
	return nil
}

func runConfigEdit(cmd *cobra.Command, _ []string) error {
	path := configPathFromCmd(cmd)
	if _, err := os.Stat(path); err != nil {
		return err
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		if runtimeOSWindows() {
			editor = "notepad"
		} else {
			editor = "vi"
		}
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return fmt.Errorf("editor command is empty")
	}
	args := append(parts[1:], path)
	process := exec.Command(parts[0], args...)
	process.Stdin, process.Stdout, process.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := process.Run(); err != nil {
		return err
	}
	if _, err := config.LoadConfig(path); err != nil {
		return fmt.Errorf("edited config %s is invalid: %w", path, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Updated %s\n", path)
	return nil
}

func runConfig(cmd *cobra.Command, _ []string) error {
	if err := requireInteractive(cmd); err != nil {
		return err
	}
	path := configPathFromCmd(cmd)
	draft, cfg, err := newConfigDraft(path)
	if err != nil {
		return err
	}

menu:
	for {
		section := ""
		choices := []string{"Model & Agent", "Router, Planner & Evaluator", "Web", "Tools", "Notifications", "Basic Safety", "Open raw config", "Validate", "Exit"}
		if err := selectForm(cmd, "Configuration sections", optionChoices(choices), &section); err != nil {
			if errors.Is(err, errFormCancelled) {
				if !draft.dirty() {
					return nil
				}
				if confirmDiscard(cmd) {
					return nil
				}
				continue
			}
			return err
		}
		if section == "Exit" {
			break
		}
		switch section {
		case "Model & Agent":
			err = editConfigSection(cmd, draft, cfg, "Model & Agent", []string{"llm.api_key", "llm.base_url", "llm.model", "llm.temperature", "llm.max_tokens", "agent.mode", "agent.max_iterations", "agent.max_parallel_tools"})
		case "Router, Planner & Evaluator":
			err = editConfigSection(cmd, draft, cfg, section, []string{"router.enabled", "router.max_history", "router.classifier.model", "router.classifier.api_key", "router.classifier.base_url", "router.medium.model", "router.medium.api_key", "router.medium.base_url", "router.strong.model", "router.strong.api_key", "router.strong.base_url", "planner.enabled", "eval.success_threshold", "eval.model.model", "eval.model.api_key", "eval.model.base_url"})
		case "Web":
			err = editWebSection(cmd, draft, cfg)
		case "Tools":
			err = editToolsDraft(cmd, draft, effectiveToolList(draft, cfg.Agent.Tools))
		case "Notifications":
			err = editConfigSection(cmd, draft, cfg, section, []string{"notifications.preset", "notifications.include", "notifications.exclude", "notifications.throttle_ms"})
			if err == nil {
				err = editNotificationOverrides(cmd, draft, cfg)
			}
		case "Basic Safety":
			err = editConfigSection(cmd, draft, cfg, section, []string{"security.forbid_rm"})
		case "Open raw config":
			err = resolveDirtyBeforeExternalEdit(cmd, draft)
			if err == nil {
				if err = runConfigEdit(cmd, nil); err == nil {
					draft, cfg, err = newConfigDraft(path)
				}
			}
		case "Validate":
			if e := draft.validate(); e != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Draft is invalid: %v\n", e)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Draft is valid.")
			}
		}
		if err != nil {
			if errors.Is(err, errFormCancelled) {
				if draft.dirty() && confirmDiscard(cmd) {
					return nil
				}
				continue
			}
			return err
		}
	}
	if !draft.dirty() {
		return nil
	}
	if err := draft.validate(); err != nil {
		return fmt.Errorf("draft is invalid: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "\nChanges to save:")
	draft.review(cmd.OutOrStdout())
	decision := "Back to editing"
	cancelled := false
	if err := selectForm(cmd, "Save staged changes?", optionChoices([]string{"Save all staged changes", "Discard all staged changes", "Back to editing"}), &decision); err != nil {
		if errors.Is(err, errFormCancelled) {
			cancelled = true
		} else {
			return err
		}
	}
	switch resolveDraftDecision(decision, cancelled, cancelled && confirmDiscard(cmd)) {
	case draftSave:
		if err := draft.save(); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Saved %s\n", path)
	case draftDiscard:
		return nil
	default:
		goto menu
	}
	return nil
}

func confirmDiscard(cmd *cobra.Command) bool {
	choice := "Keep editing"
	if err := selectForm(cmd, "Discard all staged changes?", optionChoices([]string{"Discard changes", "Keep editing"}), &choice); err != nil {
		return false
	}
	return choice == "Discard changes"
}

func runConfigTools(cmd *cobra.Command, _ []string) error {
	if err := requireInteractive(cmd); err != nil {
		return err
	}
	path := configPathFromCmd(cmd)
	draft, cfg, err := newConfigDraft(path)
	if err != nil {
		return err
	}

toolsMenu:
	if err := editToolsDraft(cmd, draft, effectiveToolList(draft, cfg.Agent.Tools)); err != nil {
		if errors.Is(err, errFormCancelled) {
			if !draft.dirty() || confirmDiscard(cmd) {
				return nil
			}
			goto toolsMenu
		}
		return err
	}
	if !draft.dirty() {
		return nil
	}
	if err := draft.validate(); err != nil {
		return fmt.Errorf("draft is invalid: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "\nChanges to save:")
	draft.review(cmd.OutOrStdout())
	decision := "Back to editing"
	cancelled := false
	if err := selectForm(cmd, "Save tool selection?", optionChoices([]string{"Save all staged changes", "Discard all staged changes", "Back to editing"}), &decision); err != nil {
		if errors.Is(err, errFormCancelled) {
			cancelled = true
		} else {
			return err
		}
	}
	switch resolveDraftDecision(decision, cancelled, cancelled && confirmDiscard(cmd)) {
	case draftSave:
		if err := draft.save(); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Saved %s\n", path)
	case draftKeepEditing:
		goto toolsMenu
	case draftDiscard:
		return nil
	}
	return nil
}

func resolveDirtyBeforeExternalEdit(cmd *cobra.Command, draft *configDraft) error {
	if !draft.dirty() {
		return nil
	}
	choice := "Back"
	if err := selectForm(cmd, "Staged changes exist", optionChoices([]string{"Save", "Discard", "Back"}), &choice); err != nil {
		return err
	}
	switch choice {
	case "Save":
		return draft.save()
	case "Discard":
		return nil
	default:
		return errFormCancelled
	}
}

func editToolsDraft(cmd *cobra.Command, draft *configDraft, configured []string) error {
	if err := requireInteractive(cmd); err != nil {
		return err
	}
	options := agent.ConfigurableToolOptionsWithConfigured(configured)
	var mode string
	if len(configured) == 0 {
		mode = "Default tool set"
	} else {
		mode = "Custom allowlist"
	}
	if err := selectForm(cmd, "Tool availability", optionChoices([]string{"Default tool set", "Custom allowlist"}), &mode); err != nil {
		return err
	}
	if mode == "Default tool set" {
		return stageToolSelection(draft, mode, nil)
	}
	selected := make([]string, 0, len(configured))
	for _, option := range options {
		if slicesContains(configured, option.Descriptor.Name) {
			selected = append(selected, option.Descriptor.Name)
		}
	}
	items := make([]huh.Option[string], 0, len(options))
	for _, option := range options {
		status := "default"
		if option.Availability == agent.ToolSkillOnly {
			status = "skill-only"
		}
		if option.Availability == agent.ToolExternal {
			status = "custom / unavailable"
		}
		if len(configured) > 0 {
			if !slicesContains(configured, option.Descriptor.Name) {
				status = "excluded by policy"
			} else if option.Availability == agent.ToolDefault {
				status = "custom allowlist"
			} else if option.Availability == agent.ToolSkillOnly {
				status = "skill-only (custom enabled)"
			}
		}
		label := fmt.Sprintf("%s — %s [%s; risk: %s]", option.Descriptor.Name, option.Descriptor.Description, status, option.Descriptor.Metadata.Risk)
		items = append(items, huh.NewOption(label, option.Descriptor.Name).Selected(slicesContains(selected, option.Descriptor.Name)))
	}
	if err := runHuh(huh.NewForm(huh.NewGroup(huh.NewMultiSelect[string]().Title("Tools (Space toggles)").Options(items...).Filterable(false).Value(&selected))), cmd); err != nil {
		return err
	}
	if sameStringSet(selected, configured) {
		selected = append([]string(nil), configured...)
	}
	if mode == "Default tool set" && len(configured) == 0 {
		if _, staged := draft.changes["agent.tools"]; !staged {
			return nil
		}
	}
	if mode == "Custom allowlist" && len(configured) > 0 && sameStringSet(selected, configured) {
		if _, staged := draft.changes["agent.tools"]; !staged {
			return nil
		}
	}
	return stageToolSelection(draft, mode, selected)
}

func stageToolSelection(draft *configDraft, mode string, selected []string) error {
	if mode == "Default tool set" {
		return draft.stage("agent", "tools", "[]", "agent.tools = [] (default tool set)", false)
	}
	if mode != "Custom allowlist" {
		return fmt.Errorf("unknown tool selection mode %q", mode)
	}
	if len(selected) == 0 {
		return fmt.Errorf("custom allowlist cannot be empty; choose Default tool set instead")
	}
	return draft.stage("agent", "tools", tomlStrings(selected), "agent.tools", false)
}

func effectiveToolList(draft *configDraft, configured []string) []string {
	change, ok := draft.changes["agent.tools"]
	if !ok {
		return configured
	}
	var decoded struct {
		Tools *[]string `toml:"tools"`
	}
	if _, err := toml.Decode("tools = "+change.value, &decoded); err != nil || decoded.Tools == nil {
		return configured
	}
	return *decoded.Tools
}

func slicesContains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func editConfigSection(cmd *cobra.Command, draft *configDraft, cfg *config.Config, title string, ids []string) error {
	for _, id := range ids {
		section, key := splitConfigKey(id)
		current, label, secret := configValue(cfg, id)
		if staged, ok := draft.changes[id]; ok {
			current = staged.value
		}
		value := current
		if choices := ownerChoices(id); len(choices) > 0 {
			selected := storedString(current)
			items := make([]huh.Option[string], 0, len(choices)+1)
			found := false
			for _, choice := range choices {
				items = append(items, huh.NewOption(choice.Label+" — "+choice.Description, choice.Value))
				if choice.Value == selected {
					found = true
				}
			}
			if !found && selected != "" {
				items = append(items, huh.NewOption(selected+" (configured, unavailable)", selected))
			}
			if err := selectForm(cmd, label, items, &selected); err != nil {
				return err
			}
			value = tomlString(selected)
		} else if id == "notifications.include" || id == "notifications.exclude" {
			values := cfg.Notifications.Include
			if id == "notifications.exclude" {
				values = cfg.Notifications.Exclude
			}
			if staged, ok := draft.changes[id]; ok {
				values = decodeTOMLStrings(staged.value, values)
			}
			selected := append([]string(nil), values...)
			known := config.ProgressCategoryChoices()
			items := make([]huh.Option[string], 0, len(known)+len(values))
			for _, choice := range known {
				items = append(items, huh.NewOption(choice.Label+" — "+choice.Description, choice.Value))
			}
			for _, item := range values {
				if !slicesContainsChoice(known, item) {
					items = append(items, huh.NewOption(item+" (configured, unavailable)", item))
				}
			}
			for i := range items {
				items[i] = items[i].Selected(slicesContains(selected, items[i].Value))
			}
			field := huh.NewMultiSelect[string]().Title(label + " (Space toggles)").Options(items...).Filterable(false).Value(&selected)
			if err := runHuh(huh.NewForm(huh.NewGroup(field)), cmd); err != nil {
				return err
			}
			if sameStringSet(selected, values) {
				selected = append([]string(nil), values...)
			}
			value = tomlStrings(selected)
		} else {
			if isStringConfigValue(id) {
				value = storedString(value)
			}
			if err := inputForm(cmd, label+" (current: "+masked(current, secret)+")", &value, secret); err != nil {
				return err
			}
		}
		if isStringConfigValue(id) && ownerChoices(id) == nil {
			value = tomlString(value)
		}
		if value == current {
			continue
		}
		if err := draft.stage(section, key, value, label, secret); err != nil {
			return err
		}
	}
	return nil
}

func decodeTOMLStrings(value string, fallback []string) []string {
	var decoded struct {
		Value []string `toml:"value"`
	}
	if _, err := toml.Decode("value = "+value, &decoded); err != nil {
		return fallback
	}
	return decoded.Value
}

func editWebSection(cmd *cobra.Command, draft *configDraft, cfg *config.Config) error {
	if err := editConfigSection(cmd, draft, cfg, "Web", []string{"web_search.backend", "web_fetch.backend"}); err != nil {
		return err
	}
	searchBackend := stagedOr("web_search.backend", tomlString(cfg.WebSearch.Backend), draft)
	fetchBackend := stagedOr("web_fetch.backend", tomlString(cfg.WebFetch.Backend), draft)
	if err := editBackendFollowups(cmd, draft, cfg, searchBackend, config.WebSearchBackendChoices()); err != nil {
		return err
	}
	return editBackendFollowups(cmd, draft, cfg, fetchBackend, config.WebFetchBackendChoices())
}

func stagedOr(id, fallback string, draft *configDraft) string {
	if change, ok := draft.changes[id]; ok {
		return change.value
	}
	return fallback
}

func editBackendFollowups(cmd *cobra.Command, draft *configDraft, cfg *config.Config, backendValue string, choices []config.BackendChoice) error {
	backend := storedString(backendValue)
	for _, choice := range choices {
		if choice.Value != backend {
			continue
		}
		for _, field := range choice.Fields {
			id, ok := backendFieldConfigID(field.ConfigKey)
			if !ok {
				continue
			}
			section, key := splitConfigKey(id)
			current, label, secret := configValue(cfg, id)
			if staged, ok := draft.changes[id]; ok {
				current = staged.value
			}
			value := storedString(current)
			if value == "" && field.DefaultValue != "" {
				value = field.DefaultValue
			}
			if err := inputForm(cmd, field.Label+" (current: "+masked(current, secret)+")", &value, field.Secret || secret); err != nil {
				return err
			}
			encoded := tomlString(value)
			if encoded != current {
				if err := draft.stage(section, key, encoded, labelOr(label, field.Label), field.Secret || secret); err != nil {
					return err
				}
			}
		}
		break
	}
	return nil
}

func backendFieldConfigID(configKey string) (string, bool) {
	switch configKey {
	case config.ConfigKeySearXNGURL:
		return "web_search.searxng_url", true
	case config.ConfigKeyTavilyAPIKey:
		return "web_search.tavily_api_key", true
	case config.ConfigKeyBraveAPIKey:
		return "web_search.brave_api_key", true
	case config.ConfigKeyJinaAPIKey:
		return "web_fetch.jina_api_key", true
	case config.ConfigKeyCrawl4AICmd:
		return "web_fetch.crawl4ai_cmd", true
	default:
		return "", false
	}
}

func labelOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func isStringConfigValue(id string) bool {
	switch id {
	case "llm.api_key", "llm.base_url", "llm.model", "agent.mode",
		"router.classifier.model", "router.classifier.api_key", "router.classifier.base_url",
		"router.medium.model", "router.medium.api_key", "router.medium.base_url",
		"router.strong.model", "router.strong.api_key", "router.strong.base_url",
		"eval.model.model", "eval.model.api_key", "eval.model.base_url",
		"web_search.backend", "web_search.searxng_url", "web_search.tavily_api_key", "web_search.brave_api_key",
		"web_fetch.backend", "web_fetch.jina_api_key", "web_fetch.crawl4ai_cmd", "notifications.preset":
		return true
	default:
		return false
	}
}

func slicesContainsChoice(choices []config.Choice, value string) bool {
	for _, choice := range choices {
		if choice.Value == value {
			return true
		}
	}
	return false
}

type notificationOverrideDraft struct {
	preset                            string
	include, exclude                  []string
	presetSet, includeSet, excludeSet bool
	throttle                          int
	throttleSet                       bool
	exists                            bool
}

func effectiveNotificationOverride(draft *configDraft, cfg *config.Config, channel string) notificationOverrideDraft {
	state := notificationOverrideDraft{}
	base, _ := cfg.Notifications.Channels[channel]
	if base.Preset != "" {
		state.preset, state.presetSet, state.exists = base.Preset, true, true
	}
	if base.Include != nil {
		state.include, state.includeSet, state.exists = append([]string(nil), base.Include...), true, true
	}
	if base.Exclude != nil {
		state.exclude, state.excludeSet, state.exists = append([]string(nil), base.Exclude...), true, true
	}
	if base.ThrottleMS > 0 {
		state.throttle, state.throttleSet, state.exists = base.ThrottleMS, true, true
	}
	section := "notifications.channels." + channel
	readString := func(key string, target *string, set *bool) {
		change, ok := draft.changes[changeID(section, key)]
		if !ok {
			return
		}
		if change.unset {
			*target = ""
			*set = false
			return
		}
		var decoded struct {
			Value string `toml:"value"`
		}
		if _, err := toml.Decode("value = "+change.value, &decoded); err == nil {
			*target = decoded.Value
			*set = true
		}
	}
	readStrings := func(key string, target *[]string, set *bool) {
		change, ok := draft.changes[changeID(section, key)]
		if !ok {
			return
		}
		if change.unset {
			*target = nil
			*set = false
			return
		}
		var decoded struct {
			Value []string `toml:"value"`
		}
		if _, err := toml.Decode("value = "+change.value, &decoded); err == nil {
			*target = decoded.Value
			*set = true
		}
	}
	readInt := func(key string, target *int, set *bool) {
		change, ok := draft.changes[changeID(section, key)]
		if !ok {
			return
		}
		if change.unset {
			*target = 0
			*set = false
			return
		}
		var decoded struct {
			Value int `toml:"value"`
		}
		if _, err := toml.Decode("value = "+change.value, &decoded); err == nil {
			*target = decoded.Value
			*set = true
		}
	}
	readString("preset", &state.preset, &state.presetSet)
	readStrings("include", &state.include, &state.includeSet)
	readStrings("exclude", &state.exclude, &state.excludeSet)
	readInt("throttle_ms", &state.throttle, &state.throttleSet)
	// Existence describes the effective known override fields, not whether an
	// override existed in the loaded config. In particular, staged unsets must
	// make an existing override disappear from the effective draft state.
	state.exists = state.presetSet || state.includeSet || state.excludeSet || state.throttleSet
	return state
}

func editNotificationOverrides(cmd *cobra.Command, draft *configDraft, cfg *config.Config) error {
	for _, channel := range config.ChannelTypeChoices() {
		state := effectiveNotificationOverride(draft, cfg, channel.Value)
		decision := "Use global settings"
		choices := []string{"Create or edit override", "Use global settings"}
		if state.exists {
			decision = "Leave unchanged"
			choices = []string{"Edit override", "Remove override", "Leave unchanged"}
		}
		if err := selectForm(cmd, channel.Label+" notifications", optionChoices(choices), &decision); err != nil {
			return err
		}
		section := "notifications.channels." + channel.Value
		switch decision {
		case "Use global settings", "Leave unchanged":
			continue
		case "Remove override":
			for _, key := range []string{"preset", "include", "exclude", "throttle_ms"} {
				if err := draft.stageUnset(section, key, channel.Label+" notification "+key, false); err != nil {
					return err
				}
			}
			continue
		case "Create or edit override", "Edit override":
			if err := editNotificationOverrideFields(cmd, draft, cfg, channel.Value, channel.Label); err != nil {
				return err
			}
		}
	}
	return nil
}

func editNotificationOverrideFields(cmd *cobra.Command, draft *configDraft, cfg *config.Config, channel, label string) error {
	section := "notifications.channels." + channel
	for {
		field := "Done"
		if err := selectForm(cmd, label+" notification override", optionChoices([]string{"Preset", "Include categories", "Exclude categories", "Throttle", "Done"}), &field); err != nil {
			return err
		}
		if field == "Done" {
			return nil
		}
		state := effectiveNotificationOverride(draft, cfg, channel)
		switch field {
		case "Preset":
			items := append([]huh.Option[string]{huh.NewOption("Inherit global preset", "")}, choiceOptions(config.ProgressPresetChoices())...)
			value := state.preset
			if !state.presetSet {
				value = ""
			}
			if err := selectForm(cmd, label+" notification preset", items, &value); err != nil {
				return err
			}
			if value == "" {
				if err := draft.stageUnset(section, "preset", label+" notification preset", false); err != nil {
					return err
				}
			} else if err := draft.stage(section, "preset", tomlString(value), label+" notification preset", false); err != nil {
				return err
			}
		case "Include categories", "Exclude categories":
			key := "include"
			set := state.includeSet
			values := state.include
			if field == "Exclude categories" {
				key = "exclude"
				set = state.excludeSet
				values = state.exclude
			}
			mode := "Inherit global categories"
			if set {
				mode = "Set explicit category list"
			}
			if err := selectForm(cmd, label+" "+strings.ToLower(field), optionChoices([]string{"Inherit global categories", "Set explicit category list"}), &mode); err != nil {
				return err
			}
			if mode == "Inherit global categories" {
				if err := draft.stageUnset(section, key, label+" "+strings.ToLower(field), false); err != nil {
					return err
				}
				continue
			}
			selected := append([]string(nil), values...)
			known := config.ProgressCategoryChoices()
			items := choiceOptions(known)
			for _, value := range values {
				if !slicesContainsChoice(known, value) {
					items = append(items, huh.NewOption(value+" (configured, unavailable)", value))
				}
			}
			for i := range items {
				items[i] = items[i].Selected(slicesContains(selected, items[i].Value))
			}
			form := huh.NewForm(huh.NewGroup(huh.NewMultiSelect[string]().Title(label + " " + strings.ToLower(field)).Options(items...).Filterable(false).Value(&selected)))
			if err := runHuh(form, cmd); err != nil {
				return err
			}
			if sameStringSet(selected, values) {
				selected = append([]string(nil), values...)
			}
			if err := draft.stage(section, key, tomlStrings(selected), label+" "+strings.ToLower(field), false); err != nil {
				return err
			}
		case "Throttle":
			mode := "Inherit global throttle"
			if state.throttleSet {
				mode = "Set channel throttle"
			}
			if err := selectForm(cmd, label+" notification throttle", optionChoices([]string{"Inherit global throttle", "Set channel throttle"}), &mode); err != nil {
				return err
			}
			if mode == "Inherit global throttle" {
				if err := draft.stageUnset(section, "throttle_ms", label+" notification throttle", false); err != nil {
					return err
				}
				continue
			}
			globalThrottle := cfg.Notifications.ThrottleMS
			if staged, ok := draft.changes["notifications.throttle_ms"]; ok {
				if parsed, e := strconv.Atoi(staged.value); e == nil {
					globalThrottle = parsed
				}
			}
			value := strconv.Itoa(globalThrottle)
			if state.throttleSet {
				value = strconv.Itoa(state.throttle)
			}
			if err := inputForm(cmd, label+" notification throttle (ms)", &value, false); err != nil {
				return err
			}
			number, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || number <= 0 {
				return fmt.Errorf("notification throttle must be a positive integer")
			}
			if err := draft.stage(section, "throttle_ms", strconv.Itoa(number), label+" notification throttle", false); err != nil {
				return err
			}
		}
	}
}

func choiceOptions(choices []config.Choice) []huh.Option[string] {
	items := make([]huh.Option[string], 0, len(choices))
	for _, choice := range choices {
		items = append(items, huh.NewOption(choice.Label+" — "+choice.Description, choice.Value))
	}
	return items
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for _, value := range left {
		if !slicesContains(right, value) {
			return false
		}
	}
	return true
}

func storedString(value string) string {
	parsed, err := strconv.Unquote(value)
	if err == nil {
		return parsed
	}
	return value
}

func ownerChoices(id string) []config.Choice {
	switch id {
	case "agent.mode":
		return config.AgentModeChoices()
	case "notifications.preset":
		return config.ProgressPresetChoices()
	case "web_search.backend":
		return backendChoices(config.WebSearchBackendChoices())
	case "web_fetch.backend":
		return backendChoices(config.WebFetchBackendChoices())
	default:
		return nil
	}
}

func backendChoices(options []config.BackendChoice) []config.Choice {
	result := make([]config.Choice, 0, len(options))
	for _, option := range options {
		result = append(result, option.Choice)
	}
	return result
}

func masked(value string, secret bool) string {
	value = storedString(value)
	if secret && value != "" {
		return "••••••"
	}
	if value == "" {
		return "(empty)"
	}
	return value
}

func configValue(c *config.Config, id string) (string, string, bool) {
	label := strings.ReplaceAll(id, ".", " ")
	secret := strings.Contains(strings.ToLower(id), "key")
	var value string
	switch id {
	case "llm.api_key":
		value = tomlString(c.LLM.APIKey)
	case "llm.base_url":
		value = tomlString(c.LLM.BaseURL)
	case "llm.model":
		value = tomlString(c.LLM.Model)
	case "llm.temperature":
		value = strconv.FormatFloat(c.LLM.Temperature, 'f', -1, 64)
	case "llm.max_tokens":
		value = strconv.Itoa(c.LLM.MaxTokens)
	case "agent.mode":
		value = tomlString(c.Agent.Mode)
	case "agent.max_iterations":
		value = strconv.Itoa(c.Agent.MaxIterations)
	case "agent.max_parallel_tools":
		value = strconv.Itoa(c.Agent.MaxParallelTools)
	case "agent.tools":
		value = tomlStrings(c.Agent.Tools)
	case "router.enabled":
		value = strconv.FormatBool(c.Router.Enabled)
	case "router.max_history":
		value = strconv.Itoa(c.Router.MaxHistory)
	case "router.classifier.model":
		value = tomlString(c.Router.ClassifierModel.Model)
	case "router.classifier.api_key":
		value = tomlString(c.Router.ClassifierModel.APIKey)
	case "router.classifier.base_url":
		value = tomlString(c.Router.ClassifierModel.BaseURL)
	case "router.medium.model":
		value = tomlString(c.Router.MediumModel.Model)
	case "router.medium.api_key":
		value = tomlString(c.Router.MediumModel.APIKey)
	case "router.medium.base_url":
		value = tomlString(c.Router.MediumModel.BaseURL)
	case "router.strong.model":
		value = tomlString(c.Router.StrongModel.Model)
	case "router.strong.api_key":
		value = tomlString(c.Router.StrongModel.APIKey)
	case "router.strong.base_url":
		value = tomlString(c.Router.StrongModel.BaseURL)
	case "planner.enabled":
		value = strconv.FormatBool(c.Planner.Enabled)
	case "eval.success_threshold":
		value = strconv.Itoa(c.Eval.SuccessThreshold)
	case "eval.model.model":
		value = tomlString(c.Eval.Model.Model)
	case "eval.model.api_key":
		value = tomlString(c.Eval.Model.APIKey)
	case "eval.model.base_url":
		value = tomlString(c.Eval.Model.BaseURL)
	case "web_search.backend":
		value = tomlString(c.WebSearch.Backend)
	case "web_search.searxng_url":
		value = tomlString(c.WebSearch.SearXNGURL)
	case "web_search.tavily_api_key":
		value = tomlString(c.WebSearch.TavilyAPIKey)
	case "web_search.brave_api_key":
		value = tomlString(c.WebSearch.BraveAPIKey)
	case "web_fetch.backend":
		value = tomlString(c.WebFetch.Backend)
	case "web_fetch.jina_api_key":
		value = tomlString(c.WebFetch.JinaAPIKey)
	case "web_fetch.crawl4ai_cmd":
		value = tomlString(c.WebFetch.Crawl4AICmd)
	case "notifications.preset":
		value = tomlString(c.Notifications.Preset)
	case "notifications.include":
		value = tomlStrings(c.Notifications.Include)
	case "notifications.exclude":
		value = tomlStrings(c.Notifications.Exclude)
	case "notifications.throttle_ms":
		value = strconv.Itoa(c.Notifications.ThrottleMS)
	case "security.forbid_rm":
		value = strconv.FormatBool(c.Security.ForbidRM)
	}
	return value, label, secret
}

func splitConfigKey(id string) (string, string) {
	index := strings.LastIndexByte(id, '.')
	if index < 0 {
		return "", id
	}
	return id[:index], id[index+1:]
}
