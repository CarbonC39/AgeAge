package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"ageage/agent"
	"ageage/config"

	"github.com/BurntSushi/toml"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

var errInitCancelled = errors.New("setup cancelled")

const (
	initAgentTemplate = `# AGENT

## Execution Directives

- Use tools to gather information and perform actions.
- Call finish_task(status="success", summary=...) when done with a complete answer.
- The summary field IS your only message to the user. It must contain the COMPLETE answer with all relevant data, files, and details. Never use status-only phrases like "completed", "已 完成", or "task done" as the summary. If the user asked for data, put the data in the summary.
- Use status="failure" for early exit (missing information, unrecoverable error).
- If you used update_todos, all todos must be done before calling status="success".
- Think step by step for complex tasks; use delegate or escalate for heavy work.
- Never say "see above" or "refer to results" — always include the full answer inline.
- Use memory_store and memory_recall to persist important context across sessions.
- Minimize unnecessary tool calls; batch independent reads in a single response.
- Stay honest about limitations and uncertainty.
- Always respond in the same language the user uses.
`
	initSoulTemplate = `# SOUL

You are a helpful, friendly, and knowledgeable AI assistant.

## Communication Style

- Match the user's language and tone.
- Use clear markdown formatting when it aids readability.
- Keep responses focused and avoid unnecessary verbosity.
`
)

type initDraft struct {
	AgeageDir              string
	Workspace              string
	Provider               string
	SelectedProfileBaseURL string
	SelectedProfileModel   string
	APIKey                 string
	BaseURL                string
	Model                  string
	AgentMode              string

	RouterEnabled     bool
	RouterClass       string
	RouterMedium      string
	RouterStrong      string
	Planner           bool
	EvalEnabled       bool
	EvalThreshold     int
	EvalThresholdText string

	SearchBackend string
	SearXNGURL    string
	TavilyKey     string
	BraveKey      string
	FetchBackend  string
	JinaKey       string
	PythonCommand string

	ToolMode     string
	Tools        []string
	ForbidRM     bool
	Summarize    bool
	KeepRawTools bool
}

func initCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Set up an AgeAge workspace",
		Long:  "Create a workspace with config.toml and starter files. Use --plain for scripted setup.",
		RunE:  runInit,
	}
	cmd.Flags().Bool("plain", false, "Use line-oriented prompts instead of terminal forms")
	cmd.Flags().String("dir", "", "Workspace data directory (default: ./ageage)")
	return cmd
}

func newInitDraft(dir string) *initDraft {
	defaults := config.DefaultConfig()
	if dir == "" {
		dir = "./ageage"
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = filepath.Clean(dir)
	}
	envKey, _ := findEnvAPIKey()
	profiles := config.LLMProviderProfiles()
	provider := ""
	for _, profile := range profiles {
		if profile.BaseURL == defaults.LLM.BaseURL {
			provider = profile.Name
			break
		}
	}
	evalThreshold := defaults.Eval.SuccessThreshold
	return &initDraft{
		AgeageDir: absDir,
		Workspace: defaults.Workspace,
		Provider:  provider, SelectedProfileBaseURL: defaults.LLM.BaseURL, SelectedProfileModel: defaults.LLM.Model,
		APIKey: envKey, BaseURL: defaults.LLM.BaseURL, Model: defaults.LLM.Model,
		AgentMode:   defaults.Agent.Mode,
		RouterClass: config.SuggestModel(defaults.LLM.BaseURL), RouterMedium: defaults.LLM.Model,
		RouterStrong: config.SuggestStrongModel(defaults.LLM.BaseURL, defaults.LLM.Model),
		Planner:      defaults.Planner.Enabled, EvalThreshold: evalThreshold, EvalThresholdText: strconv.Itoa(evalThreshold),
		SearchBackend: defaults.WebSearch.Backend, SearXNGURL: "",
		TavilyKey: defaults.WebSearch.TavilyAPIKey, BraveKey: defaults.WebSearch.BraveAPIKey,
		FetchBackend: defaults.WebFetch.Backend, JinaKey: defaults.WebFetch.JinaAPIKey,
		PythonCommand: defaults.WebFetch.Crawl4AICmd,
		ToolMode:      "Default tool set",
	}
}

func runInit(cmd *cobra.Command, _ []string) error {
	dir := "./ageage"
	if flag := cmd.Flag("dir"); flag != nil && flag.Value.String() != "" {
		dir = flag.Value.String()
	}
	draft := newInitDraft(dir)
	plain := false
	if flag := cmd.Flag("plain"); flag != nil {
		plain, _ = strconv.ParseBool(flag.Value.String())
	}
	if plain || !termIsTerminal(cmd) {
		return runPlainInit(cmd, draft)
	}
	return runInteractiveInit(cmd, draft)
}

func runInteractiveInit(cmd *cobra.Command, draft *initDraft) error {
	flow := &initFlow{draft: draft}
	for flow.page < 8 {
		if err := runInitPage(cmd, flow.draft, flow.page); err != nil {
			if errors.Is(err, errFormCancelled) {
				return nil
			}
			return err
		}
		move, err := initPageNavigation(cmd, flow.page)
		if err != nil {
			if errors.Is(err, errFormCancelled) {
				return nil
			}
			return err
		}
		if !flow.navigate(move) {
			return nil
		}
	}
	return runInitReviewAndCommit(cmd, flow.draft)
}

type initFlow struct {
	draft *initDraft
	page  int
}

func (flow *initFlow) navigate(action string) bool {
	switch action {
	case "Cancel setup":
		return false
	case "Back":
		if flow.page > 0 {
			flow.page--
		}
	default:
		flow.page++
	}
	return true
}

func runInitPage(cmd *cobra.Command, draft *initDraft, page int) error {
	switch page {
	case 0:
		return runHuh(huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("AgeAge directory").Value(&draft.AgeageDir),
			huh.NewInput().Title("Workspace directory").Value(&draft.Workspace),
		)), cmd)
	case 1:
		profiles := config.LLMProviderProfiles()
		options := make([]huh.Option[string], 0, len(profiles)+1)
		for _, profile := range profiles {
			options = append(options, huh.NewOption(profile.Name+" — "+profile.BaseURL, profile.Name))
		}
		options = append(options, huh.NewOption("Custom endpoint", "custom"))
		previousProvider := draft.Provider
		if err := runHuh(huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("LLM provider").Options(options...).Value(&draft.Provider))), cmd); err != nil {
			return err
		}
		previousProfile, previousProfileOK := initProfile(previousProvider)
		modelWasProfileDefault := previousProfileOK && draft.Model == previousProfile.DefaultModel
		if profile, ok := initProfile(draft.Provider); ok {
			if previousProvider != draft.Provider || draft.SelectedProfileBaseURL == "" {
				draft.BaseURL = profile.BaseURL
			}
			draft.SelectedProfileBaseURL = profile.BaseURL
			draft.SelectedProfileModel = profile.DefaultModel
			if draft.Model == "" || (previousProvider != draft.Provider && modelWasProfileDefault) {
				draft.Model = profile.DefaultModel
			}
		}
		apiKey := huh.NewInput().Title("API key (optional for keyless providers)").EchoMode(huh.EchoModePassword).Value(&draft.APIKey)
		return runHuh(huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("Provider base URL").Value(&draft.BaseURL), apiKey,
		)), cmd)
	case 2:
		return runInteractiveModelPage(cmd, draft)
	case 3:
		choices := make([]huh.Option[string], 0)
		for _, c := range config.AgentModeChoices() {
			choices = append(choices, huh.NewOption(c.Label+" — "+c.Description, c.Value))
		}
		return runHuh(huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Agent mode").Options(choices...).Value(&draft.AgentMode))), cmd)
	case 4:
		if err := runHuh(huh.NewForm(huh.NewGroup(
			huh.NewConfirm().Title("Enable the intent router?").Value(&draft.RouterEnabled),
			huh.NewInput().Title("Router classifier model").Value(&draft.RouterClass),
			huh.NewInput().Title("Router medium model").Value(&draft.RouterMedium),
			huh.NewInput().Title("Router strong model").Value(&draft.RouterStrong),
			huh.NewConfirm().Title("Enable automatic planner?").Value(&draft.Planner),
			huh.NewConfirm().Title("Enable skill evaluator?").Value(&draft.EvalEnabled),
		)), cmd); err != nil {
			return err
		}
		if !draft.EvalEnabled {
			return nil
		}
		for {
			if err := runHuh(huh.NewForm(huh.NewGroup(huh.NewInput().Title("Evaluator success threshold (positive integer)").Value(&draft.EvalThresholdText))), cmd); err != nil {
				return err
			}
			threshold, err := parsePositiveThreshold(draft.EvalThresholdText)
			if err == nil {
				draft.EvalThreshold = threshold
				return nil
			}
			fmt.Fprintln(cmd.ErrOrStderr(), err)
		}
	case 5:
		if err := runBackendInitPage(cmd, "Web search backend", config.WebSearchBackendChoices(), &draft.SearchBackend); err != nil {
			return err
		}
		if err := runSearchBackendFields(cmd, draft); err != nil {
			return err
		}
		if err := runBackendInitPage(cmd, "Web fetch backend", config.WebFetchBackendChoices(), &draft.FetchBackend); err != nil {
			return err
		}
		return runFetchBackendFields(cmd, draft)
	case 6:
		return runInitToolPage(cmd, draft)
	case 7:
		return runHuh(huh.NewForm(huh.NewGroup(
			huh.NewConfirm().Title("Forbid rm in the shell tool?").Value(&draft.ForbidRM),
			huh.NewConfirm().Title("Enable conversation summarization?").Value(&draft.Summarize),
			huh.NewConfirm().Title("Preserve raw tool calls in history?").Value(&draft.KeepRawTools),
		)), cmd)
	default:
		return nil
	}
}

func initPageNavigation(cmd *cobra.Command, page int) (string, error) {
	move := "Continue"
	choices := []huh.Option[string]{huh.NewOption("Continue", "Continue")}
	if page > 0 {
		choices = append(choices, huh.NewOption("Back", "Back"))
	}
	choices = append(choices, huh.NewOption("Cancel setup", "Cancel setup"))
	err := runHuh(huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Setup navigation").Options(choices...).Value(&move))), cmd)
	return move, err
}

func runInteractiveModelPage(cmd *cobra.Command, draft *initDraft) error {
	modelAction := "manual"
	if err := runHuh(huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Model selection").Options(
		huh.NewOption("Enter model manually", "manual"),
		huh.NewOption("Fetch model list from provider", "fetch"),
	).Value(&modelAction))), cmd); err != nil {
		return err
	}
	if modelAction == "fetch" {
		models, err := discoverInitModels(func() ([]string, error) {
			return fetchModelsWithProgress(cmd, draft.BaseURL, draft.APIKey)
		})
		if errors.Is(err, errFormCancelled) {
			return err
		}
		models, suggested, available := initModelDiscoveryResult(draft.BaseURL, models, err)
		if !available {
			fmt.Fprintf(cmd.ErrOrStderr(), "Model discovery unavailable; enter a model manually.\n")
			draft.Model = suggested
		} else {
			options := make([]huh.Option[string], 0, len(models))
			for _, model := range models {
				options = append(options, huh.NewOption(model, model))
			}
			options = append(options, huh.NewOption("Enter a different model manually", "manual"))
			if err := runHuh(huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Available models").Options(options...).Value(&draft.Model))), cmd); err != nil {
				return err
			}
			if draft.Model != "manual" {
				return nil
			}
			draft.Model = config.SuggestModel(draft.BaseURL)
		}
	}
	return runHuh(huh.NewForm(huh.NewGroup(huh.NewInput().Title("Model name").Value(&draft.Model))), cmd)
}

func discoverInitModels(fetch func() ([]string, error)) ([]string, error) {
	models, err := fetch()
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("provider returned no models")
	}
	sort.Strings(models)
	return models, nil
}

func initModelDiscoveryResult(baseURL string, models []string, err error) ([]string, string, bool) {
	if err != nil || len(models) == 0 {
		return nil, config.SuggestModel(baseURL), false
	}
	return models, config.SuggestModel(baseURL), true
}

func fetchModelsWithProgress(cmd *cobra.Command, baseURL, apiKey string) ([]string, error) {
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	type result struct {
		models []string
		err    error
	}
	finished := make(chan result, 1)
	go func() {
		models, err := fetchModelsContext(ctx, baseURL, apiKey)
		finished <- result{models: models, err: err}
	}()
	cancelFetch := false
	progress := huh.NewForm(huh.NewGroup(huh.NewConfirm().Title("⠋ Fetching models. Select Yes to cancel, No to wait.").Value(&cancelFetch)))
	if err := runHuh(progress, cmd); err != nil {
		cancel()
		return nil, err
	}
	if cancelFetch {
		cancel()
		return nil, context.Canceled
	}
	resultValue := <-finished
	return resultValue.models, resultValue.err
}

func fetchModelsContext(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(result.Data))
	for _, item := range result.Data {
		if item.ID != "" {
			models = append(models, item.ID)
		}
	}
	sort.Strings(models)
	return models, nil
}

func runBackendInitPage(cmd *cobra.Command, title string, providers []config.BackendChoice, value *string) error {
	options := make([]huh.Option[string], 0, len(providers))
	for _, provider := range providers {
		options = append(options, huh.NewOption(provider.Label+" — "+provider.Description, provider.Value))
	}
	return runHuh(huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title(title).Options(options...).Value(value))), cmd)
}

func runSearchBackendFields(cmd *cobra.Command, draft *initDraft) error {
	return runBackendFollowups(cmd, config.WebSearchBackendChoices(), draft.SearchBackend, draft)
}

func runFetchBackendFields(cmd *cobra.Command, draft *initDraft) error {
	return runBackendFollowups(cmd, config.WebFetchBackendChoices(), draft.FetchBackend, draft)
}

func runBackendFollowups(cmd *cobra.Command, providers []config.BackendChoice, selected string, draft *initDraft) error {
	var fields []config.BackendField
	for _, option := range providers {
		if option.Value == selected {
			fields = option.Fields
			break
		}
	}
	if len(fields) == 0 {
		return nil
	}
	inputs := make([]huh.Field, 0, len(fields))
	for _, field := range fields {
		value := initBackendFieldValue(draft, field.ConfigKey)
		if value == nil {
			continue
		}
		if *value == "" {
			*value = field.DefaultValue
		}
		input := huh.NewInput().Title(field.Label).Value(value)
		if field.Secret {
			input = input.EchoMode(huh.EchoModePassword)
		}
		inputs = append(inputs, input)
	}
	if len(inputs) == 0 {
		return nil
	}
	return runHuh(huh.NewForm(huh.NewGroup(inputs...)), cmd)
}

func initBackendFieldValue(draft *initDraft, key string) *string {
	switch key {
	case config.ConfigKeySearXNGURL:
		return &draft.SearXNGURL
	case config.ConfigKeyTavilyAPIKey:
		return &draft.TavilyKey
	case config.ConfigKeyBraveAPIKey:
		return &draft.BraveKey
	case config.ConfigKeyJinaAPIKey:
		return &draft.JinaKey
	case config.ConfigKeyCrawl4AICmd:
		return &draft.PythonCommand
	default:
		return nil
	}
}

func runInitToolPage(cmd *cobra.Command, draft *initDraft) error {
	if draft.ToolMode == "" {
		draft.ToolMode = "Default tool set"
	}
	if err := runHuh(huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Tool availability").Options(
		huh.NewOption("Default tool set — empty config enables defaults", "Default tool set"),
		huh.NewOption("Custom allowlist", "Custom allowlist"),
	).Value(&draft.ToolMode))), cmd); err != nil {
		return err
	}
	if draft.ToolMode == "Default tool set" {
		draft.Tools = nil
		return nil
	}
	options := agent.ConfigurableToolOptionsWithConfigured(draft.Tools)
	choices := make([]huh.Option[string], 0, len(options))
	selected := append([]string(nil), draft.Tools...)
	for _, item := range options {
		status := string(item.Availability)
		if !item.Configurable {
			continue
		}
		choices = append(choices, huh.NewOption(fmt.Sprintf("%s — %s [%s; risk: %s]", item.Descriptor.Name, item.Descriptor.Description, status, item.Descriptor.Metadata.Risk), item.Descriptor.Name).Selected(slices.Contains(selected, item.Descriptor.Name)))
	}
	field := huh.NewMultiSelect[string]().Title("Tools (Space toggles; choose at least one)").Options(choices...).Filterable(false).Value(&draft.Tools).Validate(validateInitCustomTools)
	return runHuh(huh.NewForm(huh.NewGroup(field)), cmd)
}

func validateInitCustomTools(selected []string) error {
	if len(selected) == 0 {
		return fmt.Errorf("custom allowlist cannot be empty; choose at least one tool")
	}
	return nil
}

func parsePositiveThreshold(value string) (int, error) {
	threshold, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || threshold < 1 {
		return 0, fmt.Errorf("success threshold must be a positive integer")
	}
	return threshold, nil
}

func runInitReviewAndCommit(cmd *cobra.Command, draft *initDraft) error {
	if err := validateInitDraft(draft); err != nil {
		return err
	}
	review := initReview(draft)
	fmt.Fprintln(cmd.OutOrStdout(), review)
	configPath := filepath.Join(draft.AgeageDir, "config.toml")
	if err := checkInitConfigTarget(configPath); err != nil {
		return err
	}
	if err := checkInitDirectory(draft); err != nil {
		return err
	}
	decision := "No — cancel"
	if _, err := os.Stat(configPath); err == nil {
		decision = "No — cancel"
		if err := runInitFinalChoice(cmd, "Config already exists. Replace it?", &decision, "Yes — explicitly replace config"); err != nil {
			if errors.Is(err, errFormCancelled) {
				return nil
			}
			return err
		}
		if decision != "Yes — explicitly replace config" {
			return nil
		}
		if err := commitInit(draft, true); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Setup complete: %s\n", configPath)
		return nil
	}
	if err := runInitFinalChoice(cmd, "Create this setup?", &decision, "Yes — create setup"); err != nil {
		if errors.Is(err, errFormCancelled) {
			return nil
		}
		return err
	}
	if decision != "Yes — create setup" {
		return nil
	}
	if err := commitInit(draft, false); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Setup complete: %s\n", configPath)
	return nil
}

func runInitFinalChoice(cmd *cobra.Command, title string, decision *string, affirmative string) error {
	options := []huh.Option[string]{huh.NewOption("No — cancel", "No — cancel"), huh.NewOption(affirmative, affirmative)}
	return runHuh(huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title(title).Options(options...).Value(decision))), cmd)
}

func initReview(draft *initDraft) string {
	toolSummary := "default tool set"
	if draft.ToolMode == "Custom allowlist" {
		toolSummary = "custom allowlist: " + strings.Join(draft.Tools, ", ")
	}
	routerSummary := "disabled"
	if draft.RouterEnabled {
		routerSummary = fmt.Sprintf("enabled (classifier %s; medium %s; strong %s)", draft.RouterClass, draft.RouterMedium, draft.RouterStrong)
	}
	return fmt.Sprintf("Setup review\n  Config: %s\n  Data directory: %s/data\n  Skills directory: %s/skills\n  Workspace: %s\n  Provider: %s\n  Base URL: %s\n  API key: %s\n  Model: %s\n  Agent mode: %s\n  Tools: %s\n  Router: %s\n  Planner: %t; evaluator: %t (threshold %d)\n  Search backend: %s\n  Fetch backend: %s\n  Safety: forbid rm=%t; summarize=%t; preserve raw tool calls=%t\n  Starter files: data/AGENT.md, data/SOUL.md",
		filepath.Join(draft.AgeageDir, "config.toml"), draft.AgeageDir, draft.AgeageDir, draft.Workspace, draft.Provider, draft.BaseURL, masked(draft.APIKey, true), draft.Model, draft.AgentMode, toolSummary, routerSummary, draft.Planner, draft.EvalEnabled, draft.EvalThreshold, draft.SearchBackend, draft.FetchBackend, draft.ForbidRM, draft.Summarize, draft.KeepRawTools)
}

func validateInitDraft(draft *initDraft) error {
	if strings.TrimSpace(draft.AgeageDir) == "" || strings.TrimSpace(draft.Workspace) == "" {
		return fmt.Errorf("workspace and AgeAge directory are required")
	}
	if strings.TrimSpace(draft.BaseURL) == "" || strings.TrimSpace(draft.Model) == "" {
		return fmt.Errorf("provider base URL and model are required")
	}
	if draft.ToolMode == "Custom allowlist" && len(draft.Tools) == 0 {
		return fmt.Errorf("custom allowlist cannot be empty; choose Default tool set instead")
	}
	if !containsConfigChoice(config.AgentModeChoices(), draft.AgentMode) {
		return fmt.Errorf("unsupported agent mode %q", draft.AgentMode)
	}
	if !containsBackendChoice(config.WebSearchBackendChoices(), draft.SearchBackend) {
		return fmt.Errorf("unsupported web search backend %q", draft.SearchBackend)
	}
	if !containsBackendChoice(config.WebFetchBackendChoices(), draft.FetchBackend) {
		return fmt.Errorf("unsupported web fetch backend %q", draft.FetchBackend)
	}
	if draft.ToolMode != "Default tool set" && draft.ToolMode != "Custom allowlist" {
		return fmt.Errorf("unsupported tool mode %q", draft.ToolMode)
	}
	if draft.ToolMode == "Custom allowlist" {
		allowed := make(map[string]bool)
		for _, option := range agent.ConfigurableToolOptions() {
			if option.Configurable {
				allowed[option.Descriptor.Name] = true
			}
		}
		for _, name := range draft.Tools {
			if !allowed[name] {
				return fmt.Errorf("tool %q is not in the configurable tool catalog", name)
			}
		}
	}
	if draft.EvalEnabled && draft.EvalThreshold < 1 {
		return fmt.Errorf("evaluator success threshold must be a positive integer")
	}
	if draft.EvalThreshold < 1 {
		draft.EvalThreshold = 3
	}
	validated := config.DefaultConfig()
	if err := toml.Unmarshal([]byte(draft.configContent()), validated); err != nil {
		return fmt.Errorf("generated configuration is invalid: %w", err)
	}
	return nil
}

func containsConfigChoice(options []config.Choice, value string) bool {
	for _, option := range options {
		if option.Value == value {
			return true
		}
	}
	return false
}

func containsBackendChoice(options []config.BackendChoice, value string) bool {
	for _, option := range options {
		if option.Value == value {
			return true
		}
	}
	return false
}

func (draft *initDraft) configContent() string {
	toolsLine := toolsLineFromSlice(draft.Tools)
	if draft.ToolMode == "Default tool set" {
		toolsLine = "# tools = []  # Positive allowlist; empty = all tools enabled"
	}
	return buildInitConfig(
		draft.Workspace, draft.APIKey, draft.BaseURL, draft.Model, draft.AgentMode, toolsLine,
		draft.RouterEnabled, draft.RouterClass, draft.RouterMedium, draft.RouterStrong,
		draft.EvalEnabled, draft.EvalThreshold,
		draft.SearchBackend, draft.SearXNGURL, draft.TavilyKey, draft.BraveKey,
		draft.FetchBackend, draft.JinaKey, draft.PythonCommand,
		draft.ForbidRM, draft.Planner, draft.Summarize, draft.KeepRawTools,
	)
}

func commitInit(draft *initDraft, overwrite bool) error {
	dir := filepath.Clean(draft.AgeageDir)
	configPath := filepath.Join(dir, "config.toml")
	_, statErr := os.Stat(configPath)
	configExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if configExists && !overwrite {
		return fmt.Errorf("refusing to overwrite existing config: %s", configPath)
	}
	if err := checkInitConfigTarget(configPath); err != nil {
		return err
	}
	if !configExists {
		if err := checkInitDirectory(draft); err != nil {
			return err
		}
	}
	if err := validateInitDraft(draft); err != nil {
		return err
	}
	for _, path := range []string{
		filepath.Join(dir, "data"),
		filepath.Join(dir, "skills"),
		filepath.Join(dir, "data", "AGENT.md"),
		filepath.Join(dir, "data", "SOUL.md"),
	} {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		wantDir := path == filepath.Join(dir, "data") || path == filepath.Join(dir, "skills")
		if wantDir != info.IsDir() {
			return fmt.Errorf("setup target has incompatible existing path: %s", path)
		}
	}
	content := []byte(draft.configContent())
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Join(dir, "data"), err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Join(dir, "skills"), err)
	}
	if err := writeInitFile(configPath, content, 0o600, overwrite); err != nil {
		return err
	}
	if err := writeInitFileIfMissing(filepath.Join(dir, "data", "AGENT.md"), []byte(initAgentTemplate), 0o644); err != nil {
		return err
	}
	if err := writeInitFileIfMissing(filepath.Join(dir, "data", "SOUL.md"), []byte(initSoulTemplate), 0o644); err != nil {
		return err
	}
	return nil
}

func checkInitConfigTarget(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to replace non-regular config target: %s", path)
	}
	return nil
}

func checkInitDirectory(draft *initDraft) error {
	dir := filepath.Clean(draft.AgeageDir)
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("refusing to use existing non-directory target: %s", dir)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		if _, configErr := os.Stat(filepath.Join(dir, "config.toml")); errors.Is(configErr, os.ErrNotExist) {
			return fmt.Errorf("refusing to initialize non-empty existing directory: %s", dir)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func writeInitFile(path string, data []byte, mode os.FileMode, overwrite bool) error {
	if _, err := os.Stat(path); err == nil && !overwrite {
		return fmt.Errorf("refusing to overwrite existing file: %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return atomicWriteFile(path, data, mode)
}

func writeInitFileIfMissing(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Stat(path); err == nil {
		if info.Mode().IsRegular() {
			return nil
		}
		return fmt.Errorf("starter target is not a regular file: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := atomicWriteFile(path, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ageage-init-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	ok = true
	return nil
}

func runPlainInit(cmd *cobra.Command, draft *initDraft) error {
	reader := bufio.NewReader(cmd.InOrStdin())
	out := cmd.OutOrStdout()
	prompt := func(label string, value *string, fallback string, secret bool) error {
		displayFallback := fallback
		if secret && fallback != "" {
			displayFallback = "••••••"
		}
		fmt.Fprintf(out, "%s (default %s): ", label, displayFallback)
		var line string
		if secret && cmd.InOrStdin() == os.Stdin && term.IsTerminal(os.Stdin.Fd()) {
			secretBytes, err := term.ReadPassword(os.Stdin.Fd())
			fmt.Fprintln(out)
			if err != nil {
				return err
			}
			line = string(secretBytes)
		} else {
			var err error
			line, err = reader.ReadString('\n')
			if err != nil && err != io.EOF {
				return err
			}
			wasEmpty := strings.TrimSpace(line) == ""
			line = strings.TrimSpace(line)
			if line == "" {
				line = fallback
			}
			*value = line
			if err == io.EOF && wasEmpty {
				return errInitCancelled
			}
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			line = fallback
		}
		*value = line
		return nil
	}
	promptBool := func(label string, value *bool) error {
		fallback := "n"
		if *value {
			fallback = "y"
		}
		text := fallback
		if err := prompt(label+" [y/N]", &text, fallback, false); err != nil {
			return err
		}
		*value = strings.EqualFold(text, "y") || strings.EqualFold(text, "yes")
		return nil
	}
	promptChoice := func(label string, value *string, choices []config.Choice) error {
		fmt.Fprintf(out, "%s options:\n", label)
		for _, option := range choices {
			fmt.Fprintf(out, "  %s — %s\n", option.Value, option.Description)
		}
		if err := prompt(label, value, *value, false); err != nil {
			return err
		}
		for _, option := range choices {
			if option.Value == *value {
				return nil
			}
		}
		return fmt.Errorf("unsupported %s %q", label, *value)
	}
	finishPrompt := func(err error) error {
		if errors.Is(err, errInitCancelled) {
			return nil
		}
		return err
	}
	if err := prompt("AgeAge directory", &draft.AgeageDir, draft.AgeageDir, false); err != nil {
		return finishPrompt(err)
	}
	if err := prompt("Workspace", &draft.Workspace, draft.Workspace, false); err != nil {
		return finishPrompt(err)
	}
	profiles := config.LLMProviderProfiles()
	fmt.Fprintln(out, "LLM providers:")
	for _, profile := range profiles {
		fmt.Fprintf(out, "  %s: %s\n", profile.Name, profile.BaseURL)
	}
	if err := prompt("Provider name", &draft.Provider, draft.Provider, false); err != nil {
		return finishPrompt(err)
	}
	if profile, ok := initProfile(draft.Provider); ok {
		draft.BaseURL = profile.BaseURL
		if draft.Model == config.DefaultConfig().LLM.Model {
			draft.Model = profile.DefaultModel
		}
	}
	if err := prompt("Provider base URL", &draft.BaseURL, draft.BaseURL, false); err != nil {
		return finishPrompt(err)
	}
	if err := prompt("API key", &draft.APIKey, draft.APIKey, true); err != nil {
		return finishPrompt(err)
	}
	fetchText := "n"
	if err := prompt("Fetch models from provider? [y/N]", &fetchText, "n", false); err != nil {
		return finishPrompt(err)
	}
	if strings.EqualFold(fetchText, "y") || strings.EqualFold(fetchText, "yes") {
		models, err := discoverInitModels(func() ([]string, error) {
			return fetchModelsContext(cmd.Context(), draft.BaseURL, draft.APIKey)
		})
		models, suggested, available := initModelDiscoveryResult(draft.BaseURL, models, err)
		if !available {
			fmt.Fprintln(out, "Model discovery unavailable; enter a model manually.")
			draft.Model = suggested
		} else {
			fmt.Fprintf(out, "Available models: %s\n", strings.Join(models, ", "))
		}
	}
	if err := prompt("Model", &draft.Model, config.SuggestModel(draft.BaseURL), false); err != nil {
		return finishPrompt(err)
	}
	modeChoices := config.AgentModeChoices()
	choices := make([]config.Choice, len(modeChoices))
	copy(choices, modeChoices)
	if err := promptChoice("Agent mode", &draft.AgentMode, choices); err != nil {
		return err
	}
	if err := prompt("Tool mode (Default tool set or Custom allowlist)", &draft.ToolMode, "Default tool set", false); err != nil {
		return finishPrompt(err)
	}
	if draft.ToolMode == "Custom allowlist" {
		options := agent.ConfigurableToolOptionsWithConfigured(nil)
		names := make([]string, 0, len(options))
		for _, item := range options {
			if item.Configurable {
				status := string(item.Availability)
				names = append(names, fmt.Sprintf("%s — %s [%s; risk: %s]", item.Descriptor.Name, item.Descriptor.Description, status, item.Descriptor.Metadata.Risk))
			}
		}
		fmt.Fprintf(out, "Configurable tools: %s\n", strings.Join(names, ", "))
		var selected string
		if err := prompt("Comma-separated tool names", &selected, "", false); err != nil {
			return finishPrompt(err)
		}
		for _, name := range strings.Split(selected, ",") {
			if strings.TrimSpace(name) != "" {
				draft.Tools = append(draft.Tools, strings.TrimSpace(name))
			}
		}
	}
	if err := promptBool("Enable intent router?", &draft.RouterEnabled); err != nil {
		return finishPrompt(err)
	}
	if draft.RouterEnabled {
		if err := prompt("Router classifier model", &draft.RouterClass, config.SuggestModel(draft.BaseURL), false); err != nil {
			return finishPrompt(err)
		}
		if err := prompt("Router medium model", &draft.RouterMedium, draft.Model, false); err != nil {
			return finishPrompt(err)
		}
		if err := prompt("Router strong model", &draft.RouterStrong, config.SuggestStrongModel(draft.BaseURL, draft.Model), false); err != nil {
			return finishPrompt(err)
		}
	}
	if err := promptBool("Enable automatic planner?", &draft.Planner); err != nil {
		return finishPrompt(err)
	}
	if err := promptBool("Enable skill evaluator?", &draft.EvalEnabled); err != nil {
		return finishPrompt(err)
	}
	if draft.EvalEnabled {
		for {
			if err := prompt("Evaluator success threshold (positive integer)", &draft.EvalThresholdText, "3", false); err != nil {
				return finishPrompt(err)
			}
			threshold, err := parsePositiveThreshold(draft.EvalThresholdText)
			if err == nil {
				draft.EvalThreshold = threshold
				break
			}
			fmt.Fprintln(out, err)
		}
	}
	if err := promptChoice("Search backend", &draft.SearchBackend, backendChoiceValues(config.WebSearchBackendChoices())); err != nil {
		return err
	}
	if err := promptBackendFields(prompt, config.WebSearchBackendChoices(), draft.SearchBackend, draft); err != nil {
		return finishPrompt(err)
	}
	if err := promptChoice("Fetch backend", &draft.FetchBackend, backendChoiceValues(config.WebFetchBackendChoices())); err != nil {
		return err
	}
	if err := promptBackendFields(prompt, config.WebFetchBackendChoices(), draft.FetchBackend, draft); err != nil {
		return finishPrompt(err)
	}
	if err := promptBool("Forbid rm in the shell tool?", &draft.ForbidRM); err != nil {
		return finishPrompt(err)
	}
	if err := promptBool("Enable conversation summarization?", &draft.Summarize); err != nil {
		return finishPrompt(err)
	}
	if err := promptBool("Preserve raw tool calls in history?", &draft.KeepRawTools); err != nil {
		return finishPrompt(err)
	}
	fmt.Fprintln(out, initReview(draft))
	if err := validateInitDraft(draft); err != nil {
		return err
	}
	if err := checkInitDirectory(draft); err != nil {
		return err
	}
	configPath := filepath.Join(draft.AgeageDir, "config.toml")
	if _, err := os.Stat(configPath); err == nil {
		fmt.Fprintf(out, "%s exists. Replace it? [y/N]: ", configPath)
		line, _ := reader.ReadString('\n')
		if !strings.EqualFold(strings.TrimSpace(line), "y") {
			return nil
		}
		if err := commitInit(draft, true); err != nil {
			return err
		}
		fmt.Fprintf(out, "Setup complete: %s\n", configPath)
		return nil
	}
	fmt.Fprint(out, "Create setup? [y/N]: ")
	line, _ := reader.ReadString('\n')
	if !strings.EqualFold(strings.TrimSpace(line), "y") {
		return nil
	}
	if err := commitInit(draft, false); err != nil {
		return err
	}
	fmt.Fprintf(out, "Setup complete: %s\n", configPath)
	return nil
}

func backendChoiceValues(options []config.BackendChoice) []config.Choice {
	choices := make([]config.Choice, len(options))
	for i, option := range options {
		choices[i] = option.Choice
	}
	return choices
}

func promptBackendFields(prompt func(string, *string, string, bool) error, providers []config.BackendChoice, selected string, draft *initDraft) error {
	for _, provider := range providers {
		if provider.Value != selected {
			continue
		}
		for _, field := range provider.Fields {
			value := initBackendFieldValue(draft, field.ConfigKey)
			if value == nil {
				continue
			}
			if *value == "" {
				*value = field.DefaultValue
			}
			if err := prompt(field.Label, value, field.DefaultValue, field.Secret); err != nil {
				return err
			}
		}
	}
	return nil
}

func initProfile(name string) (config.LLMProviderProfile, bool) {
	for _, profile := range config.LLMProviderProfiles() {
		if strings.EqualFold(profile.Name, name) {
			return profile, true
		}
	}
	return config.LLMProviderProfile{}, false
}
