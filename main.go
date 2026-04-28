package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ageage/agent"
	"ageage/channel"
	"ageage/config"
	"ageage/creds"
	"ageage/llm"
	"ageage/security"
	"ageage/server"
	"ageage/tools"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

var debugFlag bool

func main() {
	rootCmd := &cobra.Command{
		Use:   "ageage",
		Short: "AgeAge - A mini Golang Agent framework",
		Long:  "AgeAge is a lightweight, modular AI agent framework with token optimization and enterprise-grade security.",
	}

	rootCmd.PersistentFlags().BoolVar(&debugFlag, "debug", false, "Enable debug output (print model raw output and tool flow)")

	// --- ageage init ---
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Interactive setup: create workspace directory and config",
		RunE:  runInit,
	}
	rootCmd.AddCommand(initCmd)

	// --- ageage serve ---
	serveCmd := &cobra.Command{
		Use:   "serve [data-dir]",
		Short: "Start the AgeAge API server",
		Args:  cobra.ExactArgs(1),
		RunE:  runServe,
	}
	rootCmd.AddCommand(serveCmd)

	// --- ageage cli ---
	cliCmd := &cobra.Command{
		Use:   "cli",
		Short: "Start the interactive CLI session",
		RunE:  runCLI,
	}
	cliCmd.Flags().StringP("config", "c", "", "Path to config.toml (default: ./config.toml or ./workspace/config.toml)")
	cliCmd.Flags().Bool("soul", false, "Inject SOUL.md personality (default false in CLI mode)")
	cliCmd.Flags().BoolP("think", "T", false, "Show reasoning model think-blocks inline (default: show summary only)")
	rootCmd.AddCommand(cliCmd)

	// --- ageage connect ---
	connectCmd := &cobra.Command{
		Use:   "connect",
		Short: "Connect to configured IM channels (Telegram, Discord, Matrix)",
		RunE:  runConnect,
	}
	connectCmd.Flags().StringP("config", "c", "", "Path to config.toml")
	rootCmd.AddCommand(connectCmd)

	// --- ageage skills ---
	skillsCmd := &cobra.Command{
		Use:   "skills",
		Short: "List all loaded skills",
		RunE:  runSkills,
	}
	skillsCmd.Flags().StringP("config", "c", "", "Path to config.toml")
	rootCmd.AddCommand(skillsCmd)

	// --- ageage tools ---
	toolsCmd := &cobra.Command{
		Use:   "tools",
		Short: "Interactively select which tools the agent uses by default",
		RunE:  runTools,
	}
	toolsCmd.Flags().StringP("config", "c", "", "Path to config.toml")
	rootCmd.AddCommand(toolsCmd)

	// --- ageage mcp ---
	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Start AgeAge as an MCP server over stdio",
		RunE:  runMCP,
	}
	mcpCmd.Flags().StringP("config", "c", "", "Path to config.toml")
	rootCmd.AddCommand(mcpCmd)

	// --- ageage cred ---
	credCmd := &cobra.Command{
		Use:   "cred",
		Short: "Manage stored credentials",
	}
	credCmd.PersistentFlags().StringP("config", "c", "", "Path to config.toml")

	credKeygenCmd := &cobra.Command{
		Use:   "keygen",
		Short: "Show the path of the auto-generated master key",
		RunE:  runCredKeygen,
	}

	credListCmd := &cobra.Command{
		Use:   "list",
		Short: "List stored credential names",
		RunE:  runCredList,
	}

	credAddCmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Add or update a credential (prompts for value, no terminal echo)",
		Args:  cobra.ExactArgs(1),
		RunE:  runCredAdd,
	}

	credSetCmd := &cobra.Command{
		Use:   "set <name> <value>",
		Short: "Add or update a credential (value inline — use 'add' for sensitive input)",
		Args:  cobra.ExactArgs(2),
		RunE:  runCredSet,
	}

	credRemoveCmd := &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Remove a stored credential",
		Args:    cobra.ExactArgs(1),
		RunE:    runCredRemove,
	}

	credCmd.AddCommand(credKeygenCmd, credListCmd, credAddCmd, credSetCmd, credRemoveCmd)
	rootCmd.AddCommand(credCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// findConfigFile locates the config file.
func findConfigFile(explicit string) string {
	if explicit != "" {
		return explicit
	}
	candidates := []string{
		"config.toml",
		filepath.Join("workspace", "config.toml"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "config.toml"
}

// --- ageage init ---

func runInit(cmd *cobra.Command, args []string) error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("AgeAge Setup Wizard")
	fmt.Println(strings.Repeat("═", 52))

	// ─── 1/7  Storage ────────────────────────────────────────
	printInitSection("1/7  Storage")
	fmt.Println("AgeAge directory — one folder for config.toml, AGENT.md, SOUL.md,")
	fmt.Println("memories, skills, and session data.")
	fmt.Println("Launch with: ageage cli -c <dir>/config.toml")
	fmt.Print("AgeAge directory (default: ./ageage): ")
	ageageDir, _ := filepath.Abs(readLine(reader, "./ageage"))
	cfgPath := filepath.Join(ageageDir, "config.toml")

	fmt.Println()
	fmt.Println("Workspace — the directory the agent reads and writes files in.")
	fmt.Println("CLI mode always uses the shell's launch directory, regardless of this setting.")
	fmt.Println("Channel/serve mode uses the workspace path below.")
	fmt.Print("Workspace (default: . — runtime launch directory): ")
	workspace := readLine(reader, ".")

	// ─── 2/7  LLM Provider ───────────────────────────────────
	printInitSection("2/7  LLM Provider")
	fmt.Println("Base URL examples:")
	fmt.Println("  OpenAI:    https://api.openai.com/v1")
	fmt.Println("  Anthropic: https://api.anthropic.com/v1")
	fmt.Println("  DeepSeek:  https://api.deepseek.com/v1")
	fmt.Println("  Gemini:    https://generativelanguage.googleapis.com/v1beta/openai")
	fmt.Println("  Mistral:   https://api.mistral.ai/v1")
	fmt.Println("  Ollama:    http://localhost:11434/v1  (no API key needed)")
	fmt.Print("Base URL (default: https://api.openai.com/v1): ")
	baseURL := readLine(reader, "https://api.openai.com/v1")

	envKey, envName := findEnvAPIKey()
	if envKey != "" {
		fmt.Printf("API Key (found %s — press Enter to use it): ", envName)
	} else {
		fmt.Print("API Key (press Enter to skip for keyless providers like Ollama): ")
	}
	apiKey := readLine(reader, envKey)
	if apiKey == envKey && envKey != "" {
		fmt.Printf("  Using %s.\n", envName)
	}

	model := pickModel(reader, baseURL, apiKey)

	// ─── 3/7  Agent Behavior ─────────────────────────────────
	printInitSection("3/7  Agent Behavior")
	fmt.Println("Mode:")
	fmt.Println("  1) supervised — pause for confirmation before every tool call")
	fmt.Println("                  Recommended for CLI use; you review each action")
	fmt.Println("  2) full       — fully autonomous, no confirmation prompts")
	fmt.Println("                  Required for channel mode (Telegram/Discord/Matrix)")
	fmt.Print("Select mode (default: 1): ")
	agentMode := "supervised"
	if readLine(reader, "1") == "2" {
		agentMode = "full"
	}

	// ─── 4/7  Intent Router ──────────────────────────────────
	printInitSection("4/7  Intent Router  (optional)")
	fmt.Println("The router classifies each request by answering 3 factual checks")
	fmt.Println("(needs tools? needs multiple steps? needs synthesis?) and routes to")
	fmt.Println("the right model tier — direct, atomic, or workflow.")
	fmt.Println("Requires at least a cheap classifier model; strong model is optional.")
	fmt.Println("Skip if you use a single model for everything.")
	fmt.Print("Configure router? (y/N): ")
	routerEnabled := strings.ToLower(readLine(reader, "n")) == "y"
	var routerClassifier, routerMedium, routerStrong string
	if routerEnabled {
		classDefault := suggestModel(baseURL)
		fmt.Printf("  Classifier model (cheap; used for intent classification; default: %s): ", classDefault)
		routerClassifier = readLine(reader, classDefault)
		fmt.Printf("  Atomic model (single-tool tasks; default: %s): ", model)
		routerMedium = readLine(reader, model)
		strongDefault := getStrongModel(baseURL, model)
		fmt.Printf("  Workflow model (multi-step tasks; default: %s): ", strongDefault)
		routerStrong = readLine(reader, strongDefault)
	}

	// ─── 5/7  Skill Quality  ─────────────────────────────────
	printInitSection("5/7  Skill Quality  (optional)")
	fmt.Println("The Evaluator reviews auto-generated skills after they run,")
	fmt.Println("patching deficiencies in the background. It stops once a skill")
	fmt.Println("passes N consecutive times (success threshold).")
	fmt.Print("Enable Evaluator? (y/N): ")
	evalEnabled := strings.ToLower(readLine(reader, "n")) == "y"
	evalThreshold := 3
	if evalEnabled {
		fmt.Print("  Success threshold (default: 3): ")
		if t := readLine(reader, "3"); t != "3" {
			if _, err := fmt.Sscanf(t, "%d", &evalThreshold); err != nil || evalThreshold < 1 {
				evalThreshold = 3
			}
		}
	}

	// ─── 6/7  Web Tools ──────────────────────────────────────
	printInitSection("6/7  Web Tools")
	fmt.Println("Search backend:")
	fmt.Println("  1) DuckDuckGo  — no API key, works immediately")
	fmt.Println("  2) Brave       — higher quality results (Brave Search API key required)")
	fmt.Println("  3) Tavily      — optimized for LLM agents (Tavily API key required)")
	fmt.Println("  4) SearXNG     — self-hosted, privacy-friendly")
	fmt.Print("Select (default: 1): ")
	searchBackend, searxngURL, tavilyKey, braveKey := parseSearchChoice(reader, readLine(reader, "1"))

	fmt.Println("\nFetch backend (used when the agent reads web pages):")
	fmt.Println("  1) Native   — built-in Go HTTP client, no setup")
	fmt.Println("  2) Jina     — cleaner extraction; optional API key for higher rate limits")
	fmt.Println("  3) Crawl4AI — best content quality; requires Python + crawl4ai package")
	fmt.Print("Select (default: 1): ")
	fetchBackend, jinaKey, pythonCmd := parseFetchChoice(reader, readLine(reader, "1"))

	// ─── 7/7  Default Tools ───────────────────────────────────
	printInitSection("7/7  Default Tools")
	fmt.Println("All tools are enabled by default. An allowlist restricts the agent to")
	fmt.Println("only the tools you name (useful for leaner or constrained deployments).")
	fmt.Print("Customize tool allowlist? (y/N): ")
	var selectedTools []string
	if strings.ToLower(readLine(reader, "n")) == "y" {
		selectedTools = selectTools(reader, nil)
	}
	toolsLine := toolsLineFromSlice(selectedTools)

	// ─── Generate files ───────────────────────────────────────
	for _, d := range []string{
		filepath.Join(ageageDir, "data"),
		filepath.Join(ageageDir, "skills"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", d, err)
		}
	}

	writeConfig := true
	if _, err := os.Stat(cfgPath); err == nil {
		fmt.Printf("\n%s already exists. Overwrite? (y/N): ", cfgPath)
		if strings.ToLower(readLine(reader, "n")) != "y" {
			fmt.Println("  Skipped.")
			writeConfig = false
		}
	}
	if writeConfig {
		content := buildInitConfig(workspace, apiKey, baseURL, model, agentMode, toolsLine,
			routerEnabled, routerClassifier, routerMedium, routerStrong,
			evalEnabled, evalThreshold,
			searchBackend, searxngURL, tavilyKey, braveKey,
			fetchBackend, jinaKey, pythonCmd)
		if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("failed to write config: %w", err)
		}
		fmt.Printf("Created %s\n", cfgPath)
	}

	writeFileMD(filepath.Join(ageageDir, "data", "AGENT.md"), `# AGENT

## Execution Directives

- Use tools to gather information and perform actions.
- Call finish_task(status="success", summary=...) when done with a complete answer.
- Use status="failure" for early exit (missing information, unrecoverable error).
- If you used update_todos, all todos must be done before calling status="success".
- Think step by step for complex tasks; use delegate or escalate for heavy subtasks.
- Never say "see above" or "refer to results" — always include the full answer inline.
- Use memory_store and memory_recall to persist important context across sessions.
- Minimize unnecessary tool calls; batch independent reads in a single response.
- Stay honest about limitations and uncertainty.
- Always respond in the same language the user uses.
`)

	writeFileMD(filepath.Join(ageageDir, "data", "SOUL.md"), `# SOUL

You are a helpful, friendly, and knowledgeable AI assistant.

## Communication Style

- Match the user's language and tone.
- Use clear markdown formatting when it aids readability.
- Keep responses focused and avoid unnecessary verbosity.
`)

	fmt.Println()
	fmt.Println(strings.Repeat("─", 52))
	fmt.Println("Setup complete!")
	fmt.Println()
	fmt.Printf("  Start chatting:  ageage cli -c %s\n", cfgPath)
	fmt.Printf("  Channel mode:    ageage connect -c %s\n", cfgPath)
	fmt.Printf("  Tool select:     ageage tools -c %s\n", cfgPath)
	fmt.Println()
	fmt.Println("Useful commands once running:")
	fmt.Println("  /build [description] — create a reusable skill or pipeline")
	fmt.Println("  /session new [name]  — start a fresh named session")
	fmt.Println("  /undo                — roll back the last turn")
	fmt.Println("  /help                — list all commands")
	fmt.Println()
	fmt.Println("More options in config.toml:")
	fmt.Println("  [summarize]    — auto-compress long conversation history")
	fmt.Println("  [channels.*]   — Telegram, Discord, Matrix connectors")
	fmt.Println("  [mcp.servers]  — connect external MCP tool servers")
	fmt.Println("  [multimodal]   — vision and document converter settings")
	fmt.Println("  [bash]         — auto-allow commands, env var passthrough")
	fmt.Println("  [security]     — restrict allowed paths and blocked commands")
	fmt.Println()
	return nil
}

// printInitSection prints a bold section header for the init wizard.
func printInitSection(title string) {
	fmt.Println()
	fmt.Println("── " + title + " " + strings.Repeat("─", max(0, 48-len(title))))
}

// findEnvAPIKey returns the first API key found in known environment variables.
func findEnvAPIKey() (key, name string) {
	for _, n := range []string{"AGEAGE_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "DEEPSEEK_API_KEY", "GEMINI_API_KEY"} {
		if v := os.Getenv(n); v != "" {
			return v, n
		}
	}
	return "", ""
}

// pickModel asks the user for a model, optionally fetching the list from the API.
func pickModel(reader *bufio.Reader, baseURL, apiKey string) string {
	suggested := suggestModel(baseURL)
	fmt.Printf("Model (default: %s; press Enter to fetch list from API): ", suggested)
	input := readLine(reader, "")
	if input != "" {
		return input
	}
	// Try fetching the model list.
	fmt.Print("  Fetching models from API...")
	models, err := fetchModels(baseURL, apiKey)
	if err != nil || len(models) == 0 {
		if err != nil {
			fmt.Printf(" failed (%s)\n", err)
		} else {
			fmt.Println(" no models returned.")
		}
		fmt.Printf("  Enter model name (default: %s): ", suggested)
		return readLine(reader, suggested)
	}
	fmt.Printf(" %d found.\n", len(models))
	const maxShow = 40
	for i, m := range models {
		if i >= maxShow {
			fmt.Printf("  ... and %d more (type name manually)\n", len(models)-maxShow)
			break
		}
		fmt.Printf("  %3d. %s\n", i+1, m)
	}
	fmt.Printf("Select (number or name, default: %s): ", suggested)
	choice := readLine(reader, "")
	if choice == "" {
		return suggested
	}
	var n int
	if _, err := fmt.Sscanf(choice, "%d", &n); err == nil && n >= 1 && n <= len(models) {
		return models[n-1]
	}
	return choice
}

// fetchModels calls the /models endpoint and returns sorted model IDs.
func fetchModels(baseURL, apiKey string) ([]string, error) {
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("x-api-key", apiKey) // Anthropic
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
	ids := make([]string, len(result.Data))
	for i, d := range result.Data {
		ids[i] = d.ID
	}
	sort.Strings(ids)
	return ids, nil
}

// parseSearchChoice returns search config fields from the user's choice string.
func parseSearchChoice(reader *bufio.Reader, choice string) (backend, searxngURL, tavilyKey, braveKey string) {
	backend = "duckduckgo"
	switch choice {
	case "2":
		backend = "brave"
		fmt.Print("  Brave Search API Key: ")
		braveKey = readLine(reader, "")
	case "3":
		backend = "tavily"
		fmt.Print("  Tavily API Key: ")
		tavilyKey = readLine(reader, "")
	case "4":
		backend = "searxng"
		fmt.Print("  SearXNG instance URL (default: http://localhost:8888): ")
		searxngURL = readLine(reader, "http://localhost:8888")
	}
	return
}

// parseFetchChoice returns fetch config fields from the user's choice string.
func parseFetchChoice(reader *bufio.Reader, choice string) (backend, jinaKey, pythonCmd string) {
	backend = "native"
	switch choice {
	case "2":
		backend = "jina"
		fmt.Print("  Jina API Key (optional — press Enter to skip): ")
		jinaKey = readLine(reader, "")
	case "3":
		backend = "crawl4ai"
		pythonCmd = detectPython()
		if pythonCmd == "" {
			fmt.Println("  Warning: Python not detected. Crawl4AI will need manual setup.")
			pythonCmd = "python"
		} else {
			fmt.Printf("  Detected Python: %s\n", pythonCmd)
		}
	}
	return
}

// buildInitConfig builds the config.toml content using a strings.Builder.
func buildInitConfig(
	workspace, apiKey, baseURL, model, agentMode, toolsLine string,
	routerEnabled bool, routerClassifier, routerMedium, routerStrong string,
	evalEnabled bool, evalThreshold int,
	searchBackend, searxngURL, tavilyKey, braveKey string,
	fetchBackend, jinaKey, pythonCmd string,
) string {
	if pythonCmd == "" {
		pythonCmd = "python"
	}
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	p("# AgeAge Configuration — generated by ageage init\n\n")
	p("workspace = %q\n\n", workspace)

	p("[llm]\n")
	p("api_key     = %q\n", apiKey)
	p("base_url    = %q\n", baseURL)
	p("model       = %q\n", model)
	p("temperature = 0.7\n")
	p("# max_tokens = 8192\n\n")

	p("[agent]\n")
	p("max_iterations    = 20\n")
	p("mode              = %q\n", agentMode)
	p("%s\n", toolsLine)
	p("# non_include_tools  = []  # tool names to always exclude\n")
	p("# max_parallel_tools = 0   # >1 enables parallel tool dispatch within one response\n\n")

	p("[subagent]\n")
	p("max_iterations = 10\n")
	p("timeout        = 300\n")
	p("# [subagent.model]\n")
	p("# model   = \"\"  # independent model for sub-agents; defaults to [llm].model\n")
	p("# api_key = \"\"\n\n")

	p("[pipeline]\n")
	p("# foreach_concurrency = 4  # max parallel foreach iterations; 0 = sequential\n")
	p("# [pipeline.models.simple]\n# model = \"\"  # → direct complexity\n")
	p("# [pipeline.models.medium]\n# model = \"\"  # → atomic complexity\n")
	p("# [pipeline.models.complex]\n# model = \"\"  # → workflow complexity\n\n")

	if routerEnabled {
		p("[router]\n")
		p("# Routes requests to different model tiers by task complexity.\n")
		p("enabled     = true\n")
		p("max_history = 8\n\n")
		p("[router.classifier]\n")
		p("model = %q  # lightweight model for intent classification\n\n", routerClassifier)
		p("[router.medium]\n")
		p("model = %q\n\n", routerMedium)
		p("[router.strong]\n")
		p("model = %q\n\n", routerStrong)
	} else {
		p("[router]\n")
		p("# Routes requests to model tiers by task complexity.\n")
		p("# Set enabled = true and configure the sub-sections below to activate.\n")
		p("enabled     = false\n")
		p("# max_history = 8\n")
		p("# [router.classifier]\n# model = \"gpt-4o-mini\"  # cheap intent classifier\n")
		p("# [router.medium]\n# model = %q\n", model)
		p("# [router.strong]\n# model = %q\n\n", getStrongModel(baseURL, model))
	}

	p("[eval]\n")
	if evalEnabled {
		p("# Evaluator reviews auto-generated skills after they run and patches deficiencies.\n")
		p("success_threshold = %d  # stop evaluating after N consecutive passes\n\n", evalThreshold)
	} else {
		p("# Evaluator reviews auto-generated skills after they run and patches deficiencies.\n")
		p("# enabled           = true\n")
		p("# success_threshold = 3\n\n")
	}

	p("[summarize]\n")
	p("# Auto-compress long conversation history to stay within context limits.\n")
	p("enabled     = false\n")
	p("# model     = \"\"  # defaults to [llm].model; use a cheaper model to save cost\n")
	p("threshold   = 10  # compress after this many message pairs\n")
	p("keep_recent = 4   # keep N most recent messages intact after compression\n\n")

	p("[bash]\n")
	p("auto_allow_commands = []  # command prefixes that skip supervised confirmation\n")
	p("# max_output_bytes   = 4194304  # 4 MB cap on combined stdout+stderr\n")
	p("# passthrough_env_vars = []     # env var names/prefixes forwarded to subprocesses\n\n")

	p("[web_search]\n")
	p("backend        = %q\n", searchBackend)
	p("searxng_url    = %q\n", searxngURL)
	p("tavily_api_key = %q\n", tavilyKey)
	p("brave_api_key  = %q\n", braveKey)
	p("max_results    = 10\n")
	p("# blocked_domains = []\n\n")

	p("[web_fetch]\n")
	p("backend        = %q\n", fetchBackend)
	p("jina_api_key   = %q\n", jinaKey)
	p("crawl4ai_cmd   = %q\n", pythonCmd)
	p("max_characters = 15000\n\n")

	p("[browser]\n")
	p("# Browser automation tools (browser_navigate, browser_click, etc.).\n")
	p("# backend = \"playwright\"  # \"playwright\" or \"agent-browser\"\n")
	p("# headless    = true\n")
	p("# browser_type = \"chromium\"  # \"chromium\", \"firefox\", or \"webkit\"\n")
	p("# timeout     = 30           # seconds per browser action\n\n")

	p("[mcp]\n")
	p("# Connect external MCP tool servers (launched as subprocesses).\n")
	p("enabled = false\n")
	p("# [mcp.servers.example]\n")
	p("# command = \"npx\"\n")
	p("# args    = [\"-y\", \"@modelcontextprotocol/server-filesystem\", \"/tmp\"]\n")
	p("# env     = { API_KEY = \"...\" }\n\n")

	p("[security]\n")
	p("blocked_commands = [\n")
	p("  \"rm -rf /\", \"rm -rf /*\", \"mkfs\", \"dd if=\",\n")
	p("  \":(){ :|:& };:\", \"> /dev/sda\", \"chmod -R 777 /\",\n")
	p("  \"format c:\", \"del /f /s /q c:\\\\\",\n")
	p("]\n")
	p("allowed_roots   = []  # extra roots the agent may access; empty = workspace only\n")
	p("forbidden_roots = []  # paths the agent can never access regardless of allowed_roots\n\n")

	p("[multimodal]\n")
	p("vision          = true        # false if your model does not support images\n")
	p("max_image_bytes = 10485760    # 10 MB\n")
	p("# [[multimodal.converters]]\n")
	p("# extensions = [\"pdf\"]\n")
	p("# command    = \"pdftotext {input} {output}\"\n\n")

	p("[server]\n")
	p("# HTTP API server (used by: ageage serve)\n")
	p("host = \"127.0.0.1\"\n")
	p("port = 8080\n\n")

	p("# ── IM Channel Connectors (ageage connect) ──────────────────────────────────\n")
	p("# Uncomment and fill in the relevant section to enable a channel.\n")
	p("#\n")
	p("# [channels]\n")
	p("# parallel = false  # true = handle multiple incoming messages concurrently\n")
	p("#\n")
	p("# [channels.telegram]\n")
	p("# enabled       = true\n")
	p("# bot_token     = \"...\"\n")
	p("# allowed_users = []  # Telegram user IDs (as strings); empty = allow all\n")
	p("#\n")
	p("# [channels.discord]\n")
	p("# enabled       = true\n")
	p("# bot_token     = \"...\"\n")
	p("# channel_ids   = []  # Discord channel IDs to monitor\n")
	p("# allowed_users = []\n")
	p("#\n")
	p("# [channels.matrix]\n")
	p("# enabled      = true\n")
	p("# homeserver   = \"https://matrix.org\"\n")
	p("# user_id      = \"@bot:matrix.org\"\n")
	p("# access_token = \"...\"\n")
	p("# room_ids     = []  # rooms to monitor; empty = all joined rooms\n")
	p("# allowed_users = []\n")
	p("# auto_thread  = true  # reply in a new thread per conversation\n")

	return b.String()
}

// writeFileMD writes content to path only if path does not exist yet.
func writeFileMD(path, content string) {
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		return
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Printf("Warning: could not create %s: %s\n", path, err)
	} else {
		fmt.Printf("Created %s\n", path)
	}
}

// suggestModel returns a sensible default model for the given API base URL.
func suggestModel(baseURL string) string {
	switch {
	case strings.Contains(baseURL, "anthropic"):
		return "claude-haiku-4-5"
	case strings.Contains(baseURL, "deepseek"):
		return "deepseek-chat"
	case strings.Contains(baseURL, "generativelanguage") || strings.Contains(baseURL, "gemini"):
		return "gemini-2.0-flash"
	case strings.Contains(baseURL, "mistral"):
		return "mistral-small-latest"
	case strings.Contains(baseURL, "11434"): // Ollama
		return "llama3.2"
	default:
		return "gpt-4o-mini"
	}
}

func readLine(reader *bufio.Reader, defaultVal string) string {
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)
	if input == "" {
		return defaultVal
	}
	return input
}

func getStrongModel(baseURL, baseModel string) string {
	switch {
	case strings.Contains(baseURL, "anthropic"):
		return "claude-opus-4-7"
	case strings.Contains(baseURL, "mistral"):
		return "mistral-large-latest"
	case strings.Contains(baseURL, "generativelanguage") || strings.Contains(baseURL, "gemini"):
		return strings.ReplaceAll(baseModel, "flash", "pro")
	case strings.Contains(baseURL, "deepseek"):
		return "deepseek-reasoner"
	case strings.Contains(baseModel, "gpt-4o-mini"):
		return "gpt-4o"
	default:
		return baseModel
	}
}

func checkCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func detectPython() string {
	if checkCommand("python3") {
		return "python3"
	}
	if checkCommand("python") {
		return "python"
	}
	return ""
}

// startChannels registers and starts channel connectors based on config.
// Returns the manager (for shutdown) and count of registered channels.
// If no channels are enabled, returns nil manager and 0 count.
func startChannels(factory *agent.AgentFactory) (*channel.Manager, int) {
	cfg := factory.Config

	// Session manager — one per .ageage directory (shared across all chats).
	sm := agent.NewSessionManager(factory.Config.AgeAgeDirPath())

	// Per-session agent pool. In channel mode, session IDs are prefixed with a
	// sanitised chatKey so each chat's sessions are independent.
	agents := make(map[string]*agent.Agent)       // sessionID → agent
	activeSessions := make(map[string]string)     // chatKey → active sessionID
	chatKeyBySessionID := make(map[string]string) // sessionID → original chatKey (for matrix.to links)
	var agentMu sync.Mutex

	// activeReactions tracks the pending ⏳ reaction for each running task so
	// the /stop handler (which bypasses the per-chat mutex) can remove it.
	type reactionInfo struct {
		channelType string
		channelID   string
		msgID       string // original task message ID (to React 🛑 onto)
		eventID     string // reaction event ID returned by React (to Unreact)
	}
	activeReactions := make(map[string]reactionInfo) // chatKey → pending reaction

	confirmMgr := tools.NewConfirmationManager()

	var managerPtr *channel.Manager
	channelsByType := make(map[string]channel.Channel)

	// roomChatKey strips the ":t:<threadID>" suffix so we always have a
	// plain "channelType:channelID" key for session-prefix and callback wiring.
	roomChatKey := func(chatKey string) string {
		if idx := strings.LastIndex(chatKey, ":t:"); idx >= 0 {
			return chatKey[:idx]
		}
		return chatKey
	}

	// chatSessionID returns the active session ID for a chatKey, creating the
	// default session on first access. Must be called with agentMu held.
	// Thread chatKeys ("type:id:t:threadID") produce a session under the room's
	// prefix so all thread sessions are visible in the room's session list.
	chatSessionID := func(chatKey string) string {
		if id, ok := activeSessions[chatKey]; ok {
			return id
		}
		var id string
		if idx := strings.LastIndex(chatKey, ":t:"); idx >= 0 {
			rKey := chatKey[:idx]
			threadID := chatKey[idx+3:]
			id = agent.SanitizeSessionID(rKey) + "-" + agent.SanitizeSessionID(threadID)
		} else {
			id = agent.SanitizeSessionID(chatKey)
		}
		_ = sm.EnsureSession(id)
		activeSessions[chatKey] = id
		chatKeyBySessionID[id] = chatKey
		return id
	}

	// makeChatAgent creates an agent for a session, wires IM callbacks, and
	// optionally loads existing history. Must be called with agentMu held.
	makeChatAgent := func(chatKey, sessionID string) *agent.Agent {
		parts := strings.SplitN(roomChatKey(chatKey), ":", 2)
		channelType, channelID := "", ""
		if len(parts) == 2 {
			channelType, channelID = parts[0], parts[1]
		}

		ag := factory.CreateAgent(confirmMgr, "")
		ag.SessionDir = sm.SessionDir(sessionID)

		if ch, ok := channelsByType[channelType]; ok {
			if editable, ok := ch.(channel.Editable); ok {
				cID := channelID
				ag.Callbacks.TodoSend = func(text string) string {
					msgID, err := editable.SendMessage(cID, text)
					if err != nil {
						fmt.Printf("[todo] send error: %s\n", err)
					}
					return msgID
				}
				ag.Callbacks.TodoEdit = func(msgID, text string) error {
					return editable.EditMessage(cID, msgID, text)
				}
			}
		}
		ag.Callbacks.Notify = func(message string) {
			if managerPtr != nil && channelType != "" {
				managerPtr.Send(channelType, channelID, message)
			}
		}
		ag.Callbacks.AskUser = func(question string, options []string) {
			if managerPtr != nil && channelType != "" {
				managerPtr.SendQuestion(channelType, channelID, question, options)
			}
		}
		notifyFn := ag.Callbacks.Notify
		ag.Callbacks.ToolStart = func(name, args string) {
			if notifyFn == nil {
				return
			}
			var msg string
			switch name {
			case "file_write":
				var p struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				}
				if json.Unmarshal([]byte(args), &p) == nil && p.Path != "" {
					msg = imDiffWrite(p.Path, p.Content)
				}
			case "file_edit":
				var p struct {
					Path    string `json:"path"`
					Search  string `json:"search"`
					Replace string `json:"replace"`
				}
				if json.Unmarshal([]byte(args), &p) == nil && p.Path != "" {
					msg = imDiffEdit(p.Path, p.Search, p.Replace)
				}
			}
			if msg != "" {
				notifyFn(msg)
			}
		}

		// Load existing conversation history.
		if msgs, err := sm.LoadHistory(sessionID); err == nil && len(msgs) > 0 {
			ag.SetMessages(msgs)
		}

		return ag
	}

	// getAgent returns the agent for the chatKey's active session, creating it
	// on first access.
	getAgent := func(chatKey string) (*agent.Agent, string) {
		agentMu.Lock()
		defer agentMu.Unlock()
		sessionID := chatSessionID(chatKey)
		if ag, ok := agents[sessionID]; ok {
			return ag, sessionID
		}
		ag := makeChatAgent(chatKey, sessionID)
		agents[sessionID] = ag
		return ag, sessionID
	}

	// Per-chat mutexes to ensure sequential processing in each chat.
	chatMu := make(map[string]*sync.Mutex)
	var chatMuLock sync.Mutex
	getChatMutex := func(chatKey string) *sync.Mutex {
		chatMuLock.Lock()
		defer chatMuLock.Unlock()
		if mu, ok := chatMu[chatKey]; ok {
			return mu
		}
		mu := &sync.Mutex{}
		chatMu[chatKey] = mu
		return mu
	}

	// handleSessionCmd processes /session sub-commands for a room.
	// roomKey is always "channelType:channelID" (no :t: suffix) — all sessions
	// in the room share this prefix. chatKey may include ":t:threadID" and is
	// used only to resolve the currently active session.
	handleSessionCmd := func(rKey, chatKey, rawInput string) string {
		defaultPrefix := agent.SanitizeSessionID(rKey)

		// /session display name ↔ full session ID mapping helpers.
		toFullID := func(name string) string {
			if name == "default" || name == "" {
				return defaultPrefix
			}
			return defaultPrefix + "-" + agent.SanitizeSessionID(name)
		}
		toDisplayName := func(fullID string) string {
			if fullID == defaultPrefix {
				return "default"
			}
			return strings.TrimPrefix(fullID, defaultPrefix+"-")
		}

		agentMu.Lock()
		currentSessionID := chatSessionID(chatKey)
		agentMu.Unlock()

		parts := strings.Fields(rawInput)
		sub := ""
		if len(parts) >= 2 {
			sub = strings.ToLower(parts[1])
		}

		// Normalize aliases.
		switch sub {
		case "ls":
			sub = "list"
		case "n":
			sub = "new"
		case "sw":
			sub = "switch"
		case "rm", "delete":
			sub = "remove"
		}

		switch sub {
		case "": // /session — show current session
			infos, _ := sm.ListWithPrefix(defaultPrefix)
			cur := toDisplayName(currentSessionID)
			var sb strings.Builder
			fmt.Fprintf(&sb, "**Current session:** %s\n", cur)
			if len(infos) > 1 {
				sb.WriteString("_Use /session list to see all sessions._")
			}
			return sb.String()

		case "list":
			infos, err := sm.ListWithPrefix(defaultPrefix)
			if err != nil || len(infos) == 0 {
				return "No sessions found."
			}
			cur := toDisplayName(currentSessionID)
			var sb strings.Builder
			sb.WriteString("**Sessions:**\n")
			for _, si := range infos {
				marker := ""
				if si.ID == cur {
					marker = " ← current"
				}
				fmt.Fprintf(&sb, "• %s (%d turns)%s\n", si.ID, si.TurnCount, marker)
			}
			return strings.TrimRight(sb.String(), "\n")

		case "new":
			name := ""
			if len(parts) >= 3 {
				name = strings.Join(parts[2:], "-")
			} else {
				name = fmt.Sprintf("session-%d", len(agents))
			}
			newFullID := toFullID(name)

			// Save current agent history.
			agentMu.Lock()
			if curAg, ok := agents[currentSessionID]; ok {
				_ = sm.SaveHistory(currentSessionID, curAg.Messages())
			}
			// Create new agent for new session.
			if err := sm.EnsureSession(newFullID); err != nil {
				agentMu.Unlock()
				return fmt.Sprintf("❌ Failed to create session: %s", err)
			}
			newAg := makeChatAgent(chatKey, newFullID)
			agents[newFullID] = newAg
			activeSessions[chatKey] = newFullID
			agentMu.Unlock()
			return fmt.Sprintf("✅ Switched to new session **%s**.", toDisplayName(newFullID))

		case "switch":
			if len(parts) < 3 {
				return "Usage: /session switch <name>"
			}
			name := strings.Join(parts[2:], "-")
			newFullID := toFullID(name)
			if _, err := sm.LoadHistory(newFullID); err != nil {
				// Check if directory exists
				if _, statErr := os.Stat(sm.SessionDir(newFullID)); os.IsNotExist(statErr) {
					return fmt.Sprintf("❌ Session '%s' does not exist. Use /session new %s to create it.", name, name)
				}
			}
			agentMu.Lock()
			if curAg, ok := agents[currentSessionID]; ok {
				_ = sm.SaveHistory(currentSessionID, curAg.Messages())
			}
			_ = sm.EnsureSession(newFullID)
			if _, ok := agents[newFullID]; !ok {
				newAg := makeChatAgent(chatKey, newFullID)
				agents[newFullID] = newAg
			}
			activeSessions[chatKey] = newFullID
			agentMu.Unlock()
			return fmt.Sprintf("✅ Switched to session **%s**.", toDisplayName(newFullID))

		case "remove":
			if len(parts) < 3 {
				return "Usage: /session remove <name>"
			}
			name := strings.Join(parts[2:], "-")
			delFullID := toFullID(name)
			if delFullID == currentSessionID {
				return "❌ Cannot remove the active session."
			}
			agentMu.Lock()
			delete(agents, delFullID)
			agentMu.Unlock()
			if err := sm.Trash(delFullID); err != nil {
				return fmt.Sprintf("❌ Failed to remove session: %s", err)
			}
			return fmt.Sprintf("🗑️ Removed session **%s**.", toDisplayName(delFullID))

		default:
			return "Usage: /session [list|ls | new|n [name] | switch|sw <name> | remove|rm <name>]"
		}
	}

	// respond is a helper that sends a reply via msg.Respond (if set) or returns
	// the text for the channel to send. It always returns "" when Respond is used.
	respond := func(msg channel.IncomingMessage, text string) string {
		if msg.Respond != nil {
			_ = msg.Respond(text)
			return ""
		}
		return text
	}

	handler := func(msg channel.IncomingMessage) string {
		text := strings.TrimSpace(msg.Text)
		textLow := strings.ToLower(text)

		// In group chats, only respond when the bot is @mentioned or replied to.
		// Exceptions: confirmation replies (y/n/a) and /stop are always handled.
		if msg.IsGroupChat && !msg.BotMentioned {
			if textLow != "y" && textLow != "n" && textLow != "a" && textLow != "/stop" {
				return ""
			}
		}

		// rKey is always the room-level key ("type:channelID"), used for session prefix.
		// chatKey additionally encodes the thread when msg.ThreadID is set.
		rKey := msg.ChannelType + ":" + msg.ChannelID
		chatKey := rKey
		if msg.ThreadID != "" {
			chatKey += ":t:" + msg.ThreadID
		}

		// Confirmation responses bypass the per-chat mutex.
		if textLow == "y" || textLow == "n" || textLow == "a" {
			pending := confirmMgr.GetAllPending(msg.ChannelID)
			if len(pending) > 0 {
				pc := pending[0]
				allowed := (textLow == "y" || textLow == "a")
				confirmMgr.RespondToConfirmation(pc.ID, allowed)
				if !allowed {
					return respond(msg, "❌ Operation denied.")
				}
				return ""
			}
		}

		// /stop and /session abort bypass the per-chat mutex (may be held by the agent).
		if textLow == "/stop" || textLow == "/session abort" {
			agentMu.Lock()
			if sessionID, ok := activeSessions[chatKey]; ok {
				if ag, ok := agents[sessionID]; ok {
					ag.Stop()
				}
			}
			react := activeReactions[chatKey]
			agentMu.Unlock()
			factory.UserInputMgr.Cancel(msg.ChannelID)
			if react.eventID != "" {
				if r, ok := channelsByType[react.channelType].(channel.Reactor); ok {
					_ = r.Unreact(react.channelID, react.eventID)
					_, _ = r.React(react.channelID, react.msgID, "🛑")
				}
			}
			return respond(msg, "🛑 Task stopped.")
		}

		// If a pipeline node is waiting for user input, route this message to it
		// instead of starting a new agent run.
		if factory.UserInputMgr.HasPending(msg.ChannelID) {
			factory.UserInputMgr.Respond(msg.ChannelID, text)
			return respond(msg, "✅ Got it.")
		}

		// Send read receipt promptly (before acquiring the per-chat mutex).
		if rr, ok := channelsByType[msg.ChannelType].(channel.ReadReceiptSender); ok {
			_ = rr.SendReadReceipt(msg.ChannelID, msg.ReplyTo)
		}

		// All other messages are processed sequentially per chat.
		mu := getChatMutex(chatKey)
		mu.Lock()
		defer mu.Unlock()

		if debugFlag {
			fmt.Printf("\n  ▸ [%s] %s: %s\n", msg.ChannelType, msg.SenderName, msg.Text)
		}

		// /cred commands — never routed through the agent.
		// /cred set and /cred add are blocked in IM to prevent passwords appearing in chat logs.
		if strings.HasPrefix(textLow, "/cred") {
			return respond(msg, handleCredChanCmd(msg, factory.CredMgr, text))
		}

		// /sessions — list sessions for this room with matrix.to links.
		// Must come before the /session prefix check below.
		if textLow == "/sessions" {
			roomPrefix := agent.SanitizeSessionID(msg.ChannelType + ":" + msg.ChannelID)
			infos, err := sm.ListWithPrefix(roomPrefix)
			if err != nil || len(infos) == 0 {
				return respond(msg, "No sessions found for this room.")
			}
			agentMu.Lock()
			currentSessionID := ""
			if id, ok := activeSessions[chatKey]; ok {
				currentSessionID = id
			}
			agentMu.Unlock()
			var sb strings.Builder
			fmt.Fprintf(&sb, "**Sessions in %s** — newest first:\n", msg.ChannelID)
			for _, si := range infos {
				marker := ""
				fullID := roomPrefix + "-" + si.ID
				if si.ID == "default" {
					fullID = roomPrefix
				}
				if fullID == currentSessionID {
					marker = " ← current"
				}
				line := fmt.Sprintf("• %s (%d turns, %s ago)%s", si.ID, si.TurnCount, fmtAge(si.ModTime), marker)
				// Append matrix.to link when we can recover the thread event ID.
				agentMu.Lock()
				origChatKey := chatKeyBySessionID[fullID]
				agentMu.Unlock()
				if _, threadEventID, ok := strings.Cut(origChatKey, ":t:"); ok {
					line += fmt.Sprintf(" → https://matrix.to/#/%s/%s", msg.ChannelID, threadEventID)
				}
				sb.WriteString(line + "\n")
			}
			return respond(msg, strings.TrimRight(sb.String(), "\n"))
		}

		// /session commands.
		if strings.HasPrefix(textLow, "/session") {
			parts := strings.Fields(text)
			sub := ""
			if len(parts) >= 2 {
				sub = strings.ToLower(parts[1])
			}
			// Normalize alias before any checks.
			if sub == "n" {
				sub = "new"
			}

			// No nesting: /session new from within a thread is not allowed.
			if sub == "new" && msg.ThreadID != "" {
				return respond(msg, "❌ Cannot create a session from within a thread. Use the main chat window.")
			}

			// Matrix: /session new in the main chat creates a thread-backed session.
			// The user's command event becomes the thread root; replies go inside it.
			// Session ID follows the room-prefix scheme: roomPrefix + "-" + sanitize(threadID).
			if sub == "new" && msg.ChannelType == "matrix" && msg.ThreadID == "" {
				threadChatKey := rKey + ":t:" + msg.ReplyTo
				roomPrefix := agent.SanitizeSessionID(rKey)
				newFullID := roomPrefix + "-" + agent.SanitizeSessionID(msg.ReplyTo)

				agentMu.Lock()
				currentSessionID := chatSessionID(chatKey)
				if curAg, ok := agents[currentSessionID]; ok {
					_ = sm.SaveHistory(currentSessionID, curAg.Messages())
				}
				if err := sm.EnsureSession(newFullID); err != nil {
					agentMu.Unlock()
					return respond(msg, fmt.Sprintf("❌ Failed to create session: %s", err))
				}
				newAg := makeChatAgent(threadChatKey, newFullID)
				agents[newFullID] = newAg
				activeSessions[threadChatKey] = newFullID
				chatKeyBySessionID[newFullID] = threadChatKey
				agentMu.Unlock()

				if mx, ok := channelsByType["matrix"].(*channel.MatrixChannel); ok {
					_ = mx.SendInThread(msg.ChannelID, msg.ReplyTo, msg.ReplyTo, "✅ New session started. Continue in this thread.")
				}
				return ""
			}

			return respond(msg, handleSessionCmd(rKey, chatKey, text))
		}

		// /build [description] — create a skill or pipeline without entering the agent loop.
		// The planner runs isolated; the main conversation history is untouched.
		if textLow == "/build" || strings.HasPrefix(textLow, "/build ") {
			task := strings.TrimSpace(msg.Text[len("/build"):])
			ag, _ := getAgent(chatKey)
			history := ag.Messages()
			docsDir := filepath.Join(factory.Config.AgeAgeDirPath(), "docs")
			planner := agent.NewPlanner(factory, docsDir)
			skill, err := planner.CreateSkill(context.Background(), task, history)
			if err != nil {
				return respond(msg, fmt.Sprintf("❌ Build failed: %s", err))
			}
			return respond(msg, fmt.Sprintf("✅ Built `%s` — use `/%s` to activate.", skill.Name, skill.CommandName()))
		}

		switch textLow {
		case "/clear":
			ag, sessionID := getAgent(chatKey)
			ag.ClearHistory()
			_ = sm.SaveHistory(sessionID, ag.Messages())
			return respond(msg, "🗑️ Conversation history cleared.")

		case "/summarize":
			ag, sessionID := getAgent(chatKey)
			summary, err := ag.ForceSummarize()
			if err != nil {
				return respond(msg, fmt.Sprintf("❌ %s", err))
			}
			_ = sm.SaveHistory(sessionID, ag.Messages())
			return respond(msg, fmt.Sprintf("📋 Summary:\n%s", summary))

		case "/undo":
			ag, sessionID := getAgent(chatKey)
			n := ag.RollbackLastTurn()
			if n == 0 {
				return respond(msg, "Nothing to undo.")
			}
			_ = sm.SaveHistory(sessionID, ag.Messages())
			return respond(msg, "↩️ Last turn undone.")

		case "/help":
			return respond(msg, "Available commands:\n"+
				"/clear — Clear conversation history (keeps session)\n"+
				"/build [description] — Create a skill or pipeline (uses conversation context)\n"+
				"/stop — Stop the current task\n"+
				"/summarize — Summarize conversation\n"+
				"/undo — Remove the last turn from history\n"+
				"/retry [text] — Re-run the last message (optionally modified)\n"+
				"/sessions — List sessions for this room\n"+
				"/session list|ls — List sessions\n"+
				"/session new|n [name] — Start a new session\n"+
				"/session switch|sw <name> — Switch to a session\n"+
				"/session remove|rm <name> — Remove a session (moves to trash)\n"+
				"/help — Show this help")
		}

		// /retry [modifier] — roll back the last turn and re-run with optional extra text.
		// Must NOT return here; fall through to the agent execution block below.
		var retryText string
		var retryParts []llm.ContentPart
		if textLow == "/retry" || strings.HasPrefix(textLow, "/retry ") {
			ag, _ := getAgent(chatKey)
			lastMsg, ok := ag.LastTurnUserMessage()
			if !ok {
				return respond(msg, "Nothing to retry.")
			}
			retryText = lastMsg.TextContent()
			retryParts = lastMsg.Parts
			modifier := strings.TrimSpace(text[len("/retry"):])
			if modifier != "" {
				retryText += "\n\n" + modifier
			}
			ag.RollbackLastTurn()
		}

		// Typing indicator: on while the agent runs.
		if ti, ok := channelsByType[msg.ChannelType].(channel.TypingIndicator); ok {
			_ = ti.SendTyping(msg.ChannelID, true)
			defer func() { _ = ti.SendTyping(msg.ChannelID, false) }()
		}

		// Reaction: ⏳ while processing; silently removed on success, ❌ on error.
		var reactEventID string
		if r, ok := channelsByType[msg.ChannelType].(channel.Reactor); ok {
			reactEventID, _ = r.React(msg.ChannelID, msg.ReplyTo, "⏳")
			if reactEventID != "" {
				agentMu.Lock()
				activeReactions[chatKey] = reactionInfo{msg.ChannelType, msg.ChannelID, msg.ReplyTo, reactEventID}
				agentMu.Unlock()
			}
		}

		ag, sessionID := getAgent(chatKey)
		ag.SetChannelID(msg.ChannelID)

		runText := msg.Text
		if retryText != "" {
			runText = retryText
		}
		// In group chats, prefix with a clean sender name so the agent can
		// distinguish participants. DMs are unlabelled (single user).
		if msg.IsGroupChat && msg.SenderID != "" && retryText == "" {
			displayName := senderDisplayName(msg.ChannelType, msg.SenderID, msg.SenderName)
			runText = fmt.Sprintf("[%s]: %s", displayName, runText)
		}
		var result string
		var err error
		if len(retryParts) > 0 {
			result, err = ag.RunWithParts(context.Background(), runText, retryParts, nil)
		} else {
			result, err = ag.Run(context.Background(), runText, nil)
		}
		// Save history after every run (best-effort; ignore errors).
		_ = sm.SaveHistory(sessionID, ag.Messages())

		// Remove ⏳; add ❌ only on error.
		agentMu.Lock()
		delete(activeReactions, chatKey)
		agentMu.Unlock()
		if reactEventID != "" {
			if r, ok := channelsByType[msg.ChannelType].(channel.Reactor); ok {
				_ = r.Unreact(msg.ChannelID, reactEventID)
				if err != nil {
					_, _ = r.React(msg.ChannelID, msg.ReplyTo, "❌")
				}
			}
		}

		if err != nil {
			return respond(msg, fmt.Sprintf("Agent error: %s", err))
		}
		return respond(msg, result)
	}

	manager := channel.NewManager(handler)
	managerPtr = manager
	registered := 0

	opts := channel.ChannelOptions{
		Parallel: cfg.Channels.Parallel,
	}

	if cfg.Channels.Telegram.Enabled {
		if cfg.Channels.Telegram.BotToken == "" {
			fmt.Println("  ⚠  Telegram: bot_token not set, skipping")
		} else {
			tg := channel.NewTelegram(cfg.Channels.Telegram.BotToken, cfg.Channels.Telegram.AllowedUsers, opts)
			tg.AnswerCallback = func(channelID, answer string) { factory.UserInputMgr.Respond(channelID, answer) }
			manager.Register(tg)
			channelsByType["telegram"] = tg
			fmt.Println("  ✓  Telegram")
			registered++
		}
	}

	if cfg.Channels.Discord.Enabled {
		if cfg.Channels.Discord.BotToken == "" {
			fmt.Println("  ⚠  Discord: bot_token not set, skipping")
		} else if len(cfg.Channels.Discord.ChannelIDs) == 0 {
			fmt.Println("  ⚠  Discord: no channel_ids configured, skipping")
		} else {
			dc := channel.NewDiscord(cfg.Channels.Discord.BotToken, cfg.Channels.Discord.ChannelIDs, cfg.Channels.Discord.AllowedUsers, opts)
			manager.Register(dc)
			channelsByType["discord"] = dc
			fmt.Println("  ✓  Discord")
			registered++
		}
	}

	if cfg.Channels.Matrix.Enabled {
		if cfg.Channels.Matrix.AccessToken == "" {
			fmt.Println("  ⚠  Matrix: access_token not set, skipping")
		} else {
			mx := channel.NewMatrix(
				cfg.Channels.Matrix.Homeserver,
				cfg.Channels.Matrix.UserID,
				cfg.Channels.Matrix.AccessToken,
				cfg.Channels.Matrix.RoomIDs,
				cfg.Channels.Matrix.AllowedUsers,
				opts,
			)
			manager.Register(mx)
			channelsByType["matrix"] = mx
			fmt.Println("  ✓  Matrix")
			registered++
		}
	}

	if registered == 0 {
		return nil, 0
	}
	return manager, registered
}

// --- ageage connect ---

func runConnect(cmd *cobra.Command, args []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	configPath = findConfigFile(configPath)

	factory, err := agent.NewFactory(configPath, debugFlag)
	if err != nil {
		return err
	}
	factory.InjectSoul = true
	if err := agent.EnsureAgeAgeDir(factory.Config.EffectiveWorkDir()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create .ageage directory: %s\n", err)
	}

	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	go factory.WatchSkills(watchCtx)
	go agent.NewCronScheduler(factory.CronStore, factory).Run(watchCtx)

	manager, registered := startChannels(factory)
	if registered == 0 {
		return fmt.Errorf("no channels enabled. Enable at least one channel in [channels] config")
	}

	fmt.Printf("\n  ▸ %d channel(s) running  ·  Ctrl+C to stop\n\n", registered)

	// Handle graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		fmt.Println("\n  ⊘  Shutting down…")
		manager.StopAll()
		os.Exit(0)
	}()

	return manager.StartAll()
}

// --- ageage serve ---

func runServe(cmd *cobra.Command, args []string) error {
	dataDir := args[0]
	configPath := filepath.Join(dataDir, "config.toml")

	factory, err := agent.NewFactory(configPath, debugFlag)
	if err != nil {
		return err
	}
	factory.InjectSoul = true
	if err := agent.EnsureAgeAgeDir(factory.Config.EffectiveWorkDir()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create .ageage directory: %s\n", err)
	}

	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	go factory.WatchSkills(watchCtx)
	go agent.NewCronScheduler(factory.CronStore, factory).Run(watchCtx)

	// Start channel connectors in the background.
	manager, chCount := startChannels(factory)
	if chCount > 0 {
		fmt.Printf("  ▸ %d channel(s) starting alongside API server\n", chCount)
		go func() {
			if err := manager.StartAll(); err != nil {
				fmt.Printf("  ⚠  Channel error: %s\n", err)
			}
		}()

		// Graceful shutdown: stop channels on SIGINT/SIGTERM.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Println("\n  ⊘  Shutting down…")
			manager.StopAll()
			os.Exit(0)
		}()
	}

	srv := server.NewServer(factory, factory.Config.Server.Host, factory.Config.Server.Port)
	return srv.Start()
}

func runCLI(cmd *cobra.Command, args []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	configPath = findConfigFile(configPath)
	soulFlag, _ := cmd.Flags().GetBool("soul")
	showThink, _ := cmd.Flags().GetBool("think")

	factory, err := agent.NewFactory(configPath, debugFlag)
	if err != nil {
		return err
	}

	// CLI: working directory = launch directory (not config workspace).
	if cwd, err := os.Getwd(); err == nil {
		factory.Config.WorkDir = cwd
		factory.SecurityChecker = security.NewChecker(
			cwd,
			factory.Config.Security.BlockedCommands,
			factory.Config.Security.AllowedRoots,
			factory.Config.Security.ForbiddenRoots,
		)
	} else {
		fmt.Fprintf(os.Stderr, "warning: could not determine working directory: %s — using workspace as fallback\n", err)
	}
	factory.InjectSoul = soulFlag

	// Ensure .ageage directory exists in the working directory.
	if err := agent.EnsureAgeAgeDir(factory.Config.EffectiveWorkDir()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create .ageage directory: %s\n", err)
	}

	// Merge workspace-local always-allow commands from .ageage/settings.json.
	settingsPath := factory.Config.WorkspaceSettingsPath()
	if cmds := agent.LoadWorkspaceAutoAllowCommands(settingsPath); len(cmds) > 0 {
		factory.Config.Bash.AutoAllowCommands = append(factory.Config.Bash.AutoAllowCommands, cmds...)
	}

	// Persist "always allow" choices to .ageage/settings.json for future sessions.
	factory.OnAlwaysAllow = func(operation string) {
		agent.AppendAlwaysAllow(settingsPath, operation)
	}

	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	go factory.WatchSkills(watchCtx)
	go agent.NewCronScheduler(factory.CronStore, factory).Run(watchCtx)

	// ── Session setup ─────────────────────────────────────────────────────────
	sm := agent.NewSessionManager(factory.Config.AgeAgeDirPath())
	activeSessionID := "default"
	if err := sm.EnsureSession(activeSessionID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not initialise session: %s\n", err)
	}

	// createSessionAgent builds a fresh agent wired to a session directory.
	createSessionAgent := func(sessionID string) *agent.Agent {
		newAg := factory.CreateAgent(nil, "")
		newAg.SessionDir = sm.SessionDir(sessionID)
		newAg.Callbacks.AskUser = func(question string, options []string) {
			fmt.Println()
			fmt.Printf("  ❓ %s\n", question)
			for i, opt := range options {
				fmt.Printf("     %d. %s\n", i+1, opt)
			}
			fmt.Println("  (Type your answer, or /stop to cancel)")
		}
		// Pipeline node progress: print each status update on its own line.
		newAg.Callbacks.Notify = func(msg string) {
			fmt.Println()
			fmt.Println(stGray.Render("  ◈ ") + stDim.Render(msg))
		}
		return newAg
	}

	ag := createSessionAgent(activeSessionID)

	// Load existing history so the conversation resumes after a restart.
	if msgs, err := sm.LoadHistory(activeSessionID); err == nil && len(msgs) > 0 {
		ag.SetMessages(msgs)
	}

	// switchSession saves the current agent's history, then loads (or creates)
	// the target session and returns a fresh agent for it.
	switchSession := func(newID string) (*agent.Agent, error) {
		_ = sm.SaveHistory(activeSessionID, ag.Messages())
		if err := sm.EnsureSession(newID); err != nil {
			return nil, err
		}
		newAg := createSessionAgent(newID)
		if msgs, err := sm.LoadHistory(newID); err == nil && len(msgs) > 0 {
			newAg.SetMessages(msgs)
		}
		activeSessionID = newID
		return newAg, nil
	}

	ui := newCLIUI(factory.Config.LLM.Model)
	thinkFilter := &ThinkStreamFilter{showThink: showThink}
	ui.printBanner()
	if showThink {
		ui.printInfo("Think-blocks: expanded  (--think)")
	}
	if len(ag.Messages()) > 0 {
		ui.printInfo(fmt.Sprintf("Resumed session '%s'.", activeSessionID))
	}

	// rl handles line editing (history, cursor movement, raw mode).
	rl := &Readline{}
	historyFile := filepath.Join(factory.Config.AgeAgeDirPath(), "cli_history")
	rl.LoadHistory(historyFile)
	promptMain := stPink.Render("You") + stGray.Render(" ▸ ")
	promptCont := stGray.Render("... ▸ ")

	// readMultiLine reads one logical input line, handling:
	//   - lines ending with \ → continue on next line
	//   - ``` blocks → accumulate until closing ```
	// History is managed inside rl; each logical line is added once.
	readMultiLine := func() (string, error) {
		var accum strings.Builder
		inBlock := false
		for {
			if inBlock || accum.Len() > 0 {
				rl.PromptAnsi = promptCont
			} else {
				rl.PromptAnsi = promptMain
			}
			raw, err := rl.ReadLine()
			if err != nil {
				return accum.String(), err
			}
			if inBlock {
				if strings.TrimSpace(raw) == "```" {
					return strings.TrimRight(accum.String(), "\n"), nil
				}
				accum.WriteString(raw + "\n")
				continue
			}
			if strings.TrimSpace(raw) == "```" {
				inBlock = true
				continue
			}
			if trimmed, ok := strings.CutSuffix(raw, "\\"); ok {
				accum.WriteString(trimmed + "\n")
				continue
			}
			if accum.Len() > 0 {
				accum.WriteString(raw)
				return accum.String(), nil
			}
			return raw, nil
		}
	}

	// readyForInput is a semaphore: the goroutine waits for a token before
	// printing the next prompt. The main loop sends a token when it has finished
	// all output and is ready for the next user turn. This prevents readline's
	// prompt from interleaving with agent streaming output.
	readyForInput := make(chan struct{}, 1)
	readyForInput <- struct{}{} // initial: ready immediately

	// signalReady sends the readiness token; safe to call multiple times.
	signalReady := func() {
		select {
		case readyForInput <- struct{}{}:
		default:
		}
	}

	// Read stdin in a dedicated goroutine so the main loop can remain
	// responsive (e.g. to /stop) while the agent is running.
	inputCh := make(chan string)
	go func() {
		for {
			<-readyForInput // wait until the main loop is done printing
			line, err := readMultiLine()
			if err == ErrInterrupt {
				// Ctrl+C while idle → exit. The signal handler handles Ctrl+C
				// when the agent is running (terminal is then in cooked mode).
				close(inputCh)
				return
			}
			if err != nil {
				close(inputCh)
				return
			}
			if line != "" {
				rl.AddHistory(line)
			}
			inputCh <- line
			// Main loop calls signalReady() when its output is complete.
		}
	}()

	// agentActive is true while the agent goroutine is running.
	// The signal handler reads this to decide whether to stop or exit.
	var agentActive atomic.Bool

	// Ctrl+C handling:
	//   SIGTERM        → always save + exit immediately
	//   SIGINT, active → stop the running agent
	//   SIGINT, idle   → ignored here; readline returns ErrInterrupt which sends
	//                    "/stop" through inputCh, avoiding a double-exit race
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			if sig == syscall.SIGTERM {
				fmt.Println()
				_ = sm.SaveHistory(activeSessionID, ag.Messages())
				os.Exit(0)
			}
			// SIGINT: only act when the agent is running.
			if agentActive.Load() {
				fmt.Println()
				ui.printWarn("Interrupted. Press Ctrl+C again to exit.")
				ag.Stop()
			}
		}
	}()

	type agentResult struct {
		result string
		err    error
	}

	var agentCh chan agentResult

	for {
		if agentCh == nil {
			input, ok := <-inputCh
			if !ok {
				break
			}
			input = strings.TrimSpace(input)
			if input == "" {
				signalReady()
				continue
			}

			// ── /session and /s commands ───────────────────────────────────────
			lower := strings.ToLower(input)
			if strings.HasPrefix(lower, "/session") || strings.HasPrefix(lower, "/s ") || lower == "/s" {
				parts := strings.Fields(input)
				sub := ""
				if len(parts) >= 2 {
					sub = strings.ToLower(parts[1])
				}
				switch sub {
				case "": // /session — show current session + short list
					infos, _ := sm.List()
					fmt.Println()
					printSessionList(infos, activeSessionID)
					fmt.Println()

				case "list", "ls":
					infos, err := sm.List()
					if err != nil || len(infos) == 0 {
						ui.printInfo("No sessions found.")
					} else {
						fmt.Println()
						printSessionList(infos, activeSessionID)
						fmt.Println()
					}

				case "new", "n":
					var newName string
					if len(parts) >= 3 {
						newName = agent.SanitizeSessionID(strings.Join(parts[2:], "-"))
					} else {
						// Use existing-session count to generate a unique default name.
						if existing, err := sm.List(); err == nil {
							newName = fmt.Sprintf("session-%d", len(existing)+1)
						} else {
							newName = fmt.Sprintf("session-%d", 2)
						}
					}
					if newName == activeSessionID {
						ui.printInfo(fmt.Sprintf("Already on session '%s'.", activeSessionID))
						break
					}
					newAg, err := switchSession(newName)
					if err != nil {
						ui.printErr(fmt.Sprintf("Failed to create session: %s", err))
					} else {
						ag = newAg
						ui.printOK(fmt.Sprintf("Created and switched to session '%s'.", activeSessionID))
					}

				case "switch", "sw":
					if len(parts) < 3 {
						ui.printWarn("Usage: /session switch <name>")
					} else {
						query := agent.SanitizeSessionID(strings.Join(parts[2:], "-"))
						if query == activeSessionID {
							ui.printInfo(fmt.Sprintf("Already on session '%s'.", activeSessionID))
							break
						}
						targetID, err := resolveSession(sm, query)
						if err != nil {
							ui.printErr(err.Error())
						} else {
							newAg, switchErr := switchSession(targetID)
							if switchErr != nil {
								ui.printErr(fmt.Sprintf("Failed to switch: %s", switchErr))
							} else {
								ag = newAg
								ui.printOK(fmt.Sprintf("Switched to session '%s'.", activeSessionID))
							}
						}
					}

				case "rename", "mv":
					switch len(parts) {
					case 2: // /session rename — rename current session
						ui.printWarn("Usage: /session rename <new-name>  OR  /session rename <old> <new>")
					case 3: // /session rename <new-name> — rename current session
						newID := agent.SanitizeSessionID(parts[2])
						if err := sm.Rename(activeSessionID, newID); err != nil {
							ui.printErr(fmt.Sprintf("Rename failed: %s", err))
						} else {
							// Update the active session pointer and agent's session dir.
							activeSessionID = newID
							ag.SessionDir = sm.SessionDir(newID)
							ui.printOK(fmt.Sprintf("Session renamed to '%s'.", newID))
						}
					default: // /session rename <old> <new>
						oldID := agent.SanitizeSessionID(parts[2])
						newID := agent.SanitizeSessionID(parts[3])
						if oldID == activeSessionID {
							// Renaming the active session: update pointer too.
							if err := sm.Rename(oldID, newID); err != nil {
								ui.printErr(fmt.Sprintf("Rename failed: %s", err))
							} else {
								activeSessionID = newID
								ag.SessionDir = sm.SessionDir(newID)
								ui.printOK(fmt.Sprintf("Session renamed to '%s'.", newID))
							}
						} else {
							if err := sm.Rename(oldID, newID); err != nil {
								ui.printErr(fmt.Sprintf("Rename failed: %s", err))
							} else {
								ui.printOK(fmt.Sprintf("Session '%s' renamed to '%s'.", oldID, newID))
							}
						}
					}

				case "delete", "del", "rm":
					if len(parts) < 3 {
						ui.printWarn("Usage: /session delete <name>")
					} else {
						delID := agent.SanitizeSessionID(strings.Join(parts[2:], "-"))
						if delID == activeSessionID {
							ui.printErr("Cannot delete the active session. Switch away first.")
							break
						}
						// Find the actual session (supports prefix matching).
						resolved, resolveErr := resolveSession(sm, delID)
						if resolveErr != nil {
							ui.printErr(resolveErr.Error())
							break
						}
						// Confirm when the session has conversation history.
						infos, _ := sm.List()
						var turns int
						for _, si := range infos {
							if si.ID == resolved {
								turns = si.TurnCount
								break
							}
						}
						if turns > 0 {
							rl.PromptAnsi = fmt.Sprintf("  %s Delete session '%s' (%d turns)? [y/N] ",
								stAmber.Render("⚠"), resolved, turns)
							line, _ := rl.ReadLine()
							rl.PromptAnsi = promptMain
							if strings.ToLower(strings.TrimSpace(line)) != "y" {
								ui.printInfo("Cancelled.")
								break
							}
						}
						if err := sm.Delete(resolved); err != nil {
							ui.printErr(fmt.Sprintf("Failed to delete: %s", err))
						} else {
							ui.printOK(fmt.Sprintf("Deleted session '%s'.", resolved))
						}
					}

				default:
					ui.printWarn("Usage: /session [list | new [name] | switch <name> | rename <new> | delete <name>]")
					ui.printWarn("       Short forms: /s, ls, n, sw, mv, del")
				}
				signalReady()
				continue
			}

			// /undo — remove the last user→assistant exchange.
			if lower == "/undo" {
				n := ag.RollbackLastTurn()
				if n == 0 {
					ui.printWarn("Nothing to undo.")
				} else {
					_ = sm.SaveHistory(activeSessionID, ag.Messages())
					ui.printOK("Last turn undone.")
				}
				signalReady()
				continue
			}

			// /retry [modifier] — re-run last message, optionally with extra text.
			// directRun bypasses the switch so a retried message that happens to match
			// a slash command (e.g. user originally typed "/clear") is never intercepted.
			var retryParts []llm.ContentPart
			directRun := false
			if lower == "/retry" || strings.HasPrefix(lower, "/retry ") {
				lastMsg, ok := ag.LastTurnUserMessage()
				if !ok {
					ui.printWarn("Nothing to retry.")
					signalReady()
					continue
				}
				modifier := strings.TrimSpace(input[len("/retry"):])
				input = lastMsg.TextContent()
				retryParts = lastMsg.Parts
				if modifier != "" {
					input += "\n\n" + modifier
				}
				ag.RollbackLastTurn()
				directRun = true
			}

			shouldRun := directRun
			if !directRun {
				switch input {
				case "exit", "quit":
					_ = sm.SaveHistory(activeSessionID, ag.Messages())
					_ = rl.SaveHistory(historyFile, 1000)
					ui.printInfo("Goodbye!")
					return nil

				case "/clear":
					ag.ClearHistory()
					_ = sm.SaveHistory(activeSessionID, ag.Messages())
					ui.printOK("History cleared.")
					signalReady()

				case "/stop":
					ui.printWarn("No task running.")
					signalReady()

				case "/summarize":
					ui.printStatus("Summarizing…")
					summary, err := ag.ForceSummarize()
					if err != nil {
						ui.printErr(err.Error())
					} else {
						fmt.Println()
						fmt.Println(summary)
						fmt.Println()
						_ = sm.SaveHistory(activeSessionID, ag.Messages())
					}
					signalReady()

				case "/think":
					if thinkFilter.LastThink == "" {
						ui.printInfo("No think-block captured yet.")
					} else {
						fmt.Println()
						fmt.Println(stGray.Render("┌── last thinking ") + stDim.Render(line(34)))
						for l := range strings.SplitSeq(strings.TrimSpace(thinkFilter.LastThink), "\n") {
							fmt.Println(stDim.Render("│ " + l))
						}
						fmt.Println(stGray.Render("└" + line(50)))
						fmt.Println()
					}
					signalReady()

				case "/skills":
					skills := factory.GetSkills()
					if len(skills) == 0 {
						ui.printInfo("No skills loaded.")
					} else {
						fmt.Println()
						for _, s := range skills {
							tag := ""
							if s.IsPipeline() {
								tag = stDim.Render(" [pipeline]")
							}
							fmt.Printf("  %s  %s%s\n",
								stBlue.Render("/"+s.CommandName()),
								stGray.Render(s.Description),
								tag,
							)
						}
						fmt.Println()
					}
					signalReady()

				case "/help":
					fmt.Println()
					sections := []struct {
						header string
						rows   [][]string
					}{
						{"General", [][]string{
							{"/help", "Show this help"},
							{"/clear", "Clear conversation history"},
							{"/stop", "Interrupt a running task"},
							{"/summarize", "Compress conversation history"},
							{"/undo", "Remove the last turn from history"},
							{"/retry [text]", "Re-run the last message (optionally modified)"},
							{"/think", "Show the last reasoning think-block"},
							{"/skills", "List available skills"},
							{"exit / quit", "Exit AgeAge"},
						}},
						{"Sessions  (/s is a shorthand for /session)", [][]string{
							{"/s  or  /session", "List sessions with timestamps"},
							{"/s new [name]", "Create and switch to a new session"},
							{"/s switch <name>", "Switch (prefix matching supported)"},
							{"/s rename <new>", "Rename the current session"},
							{"/s rename <old> <new>", "Rename any session"},
							{"/s delete <name>", "Delete a session (confirms if non-empty)"},
						}},
						{"Input", [][]string{
							{"@/path/to/file", "Attach a file to your message"},
							{"line ending with \\", "Continue input on next line"},
							{"``` … ```", "Multi-line block input"},
						}},
					}
					for _, sec := range sections {
						fmt.Printf("  %s\n", stGray.Render(sec.header))
						for _, row := range sec.rows {
							fmt.Printf("    %s  %s\n",
								stBlue.Render(fmt.Sprintf("%-30s", row[0])),
								stGray.Render(row[1]),
							)
						}
						fmt.Println()
					}
					signalReady()

				default:
					shouldRun = true
				}
			}

			if shouldRun {
				// Parse @path file attachments from input.
				cleanText, parts, warnings := agent.ParseCLIInput(input, factory.Config, ag.TmpManager())
				for _, w := range warnings {
					ui.printWarn(w)
				}
				// /retry: restore the original parts (attachments) from the rolled-back turn.
				if retryParts != nil {
					parts = retryParts
				}
				fmt.Println()
				ui.printAgentHeader()

				spinner := newSpinner()
				spinner.Start("Thinking…")

				// ToolStartCallback: always stop the spinner before any tool output so
				// the spinner's \r never overwrites tool result lines (rendering race).
				ag.Callbacks.ToolStart = func(name, args string) {
					spinner.Stop()
					switch name {
					case "bash":
						var p struct {
							Command string `json:"command"`
						}
						if json.Unmarshal([]byte(args), &p) == nil && p.Command != "" {
							ui.printBashCommand(p.Command)
						}
					case "file_write":
						var p struct {
							Path    string `json:"path"`
							Content string `json:"content"`
						}
						if json.Unmarshal([]byte(args), &p) == nil && p.Path != "" {
							ui.printFileWrite(p.Path, p.Content)
						}
					case "file_edit":
						var p struct {
							Path    string `json:"path"`
							Search  string `json:"search"`
							Replace string `json:"replace"`
						}
						if json.Unmarshal([]byte(args), &p) == nil && p.Path != "" {
							ui.printFileEdit(p.Path, p.Search, p.Replace)
						}
					}
				}
				ag.Callbacks.ToolResult = func(name, result string) {
					if name == "bash" {
						ui.printBashOutput(result)
						return
					}
					ui.printToolResult(name, result)
				}
				ag.Callbacks.ToolEnd = func(name string) {
					spinner.Start("Thinking…")
				}

				thinkFilter.Reset()
				// Pause/resume spinner around think-block output to prevent
				// the spinner's \r from overwriting the think summary lines.
				thinkFilter.OnThinkBegin = func() { spinner.Stop() }
				thinkFilter.OnThinkEnd = func() { spinner.Start("Thinking…") }
				agentCh = make(chan agentResult, 1)
				agentActive.Store(true)
				go func(text string, ps []llm.ContentPart, ch chan agentResult) {
					var buf strings.Builder
					thinkFilter.inner = func(token string) {
						buf.WriteString(token)
						approxTokens := buf.Len() / 4
						if approxTokens > 0 {
							spinner.Update(fmt.Sprintf("Writing… (~%d tokens)", approxTokens))
						}
					}
					result, err := ag.RunWithParts(context.Background(), text, ps, thinkFilter.Wrap())
					thinkFilter.Flush()
					spinner.Stop()
					if buf.Len() > 0 {
						result = buf.String()
					}
					ch <- agentResult{result, err}
				}(cleanText, parts, agentCh)
			}

		} else {
			select {
			case res := <-agentCh:
				agentCh = nil
				agentActive.Store(false)
				// Save history after every completed turn (best-effort).
				_ = sm.SaveHistory(activeSessionID, ag.Messages())
				// Auto-rename auto-generated session names (session-N) to a slug
				// derived from the first user message so sessions are easy to identify.
				if isAutoSessionName(activeSessionID) && res.err == nil {
					if slug := firstMessageSlug(ag.Messages()); slug != "" && slug != activeSessionID {
						if err := sm.Rename(activeSessionID, slug); err == nil {
							ag.SessionDir = sm.SessionDir(slug)
							activeSessionID = slug
						}
					}
				}
				if res.err != nil {
					fmt.Println()
					ui.printErr(res.err.Error())
				} else {
					if res.result != "" {
						fmt.Println()
						fmt.Print(ui.renderMarkdown(res.result))
					}
					ui.printUsage(ag.LastRunUsage())
				}
				fmt.Println()
				signalReady()

			case input, ok := <-inputCh:
				if !ok {
					ag.Stop()
					agentActive.Store(false)
					factory.UserInputMgr.Cancel("")
					<-agentCh
					_ = sm.SaveHistory(activeSessionID, ag.Messages())
					return nil
				}
				trimmed := strings.TrimSpace(input)
				if trimmed == "/stop" {
					ag.Stop()
					factory.UserInputMgr.Cancel("")
					fmt.Println()
					ui.printWarn("Stop signal sent.")
				} else if factory.UserInputMgr.HasPending("") {
					// A pipeline node is waiting for user input — deliver the answer.
					factory.UserInputMgr.Respond("", trimmed)
				}
			}
		}
	}

	_ = sm.SaveHistory(activeSessionID, ag.Messages())
	_ = rl.SaveHistory(historyFile, 1000)
	return nil
}

// handleCredChanCmd processes /cred commands received from an IM channel.
// /cred set and /cred add are always rejected in IM to prevent passwords
// appearing in chat histories.
func handleCredChanCmd(_ channel.IncomingMessage, mgr *creds.Manager, rawInput string) string {
	if mgr == nil {
		return "❌ Credentials unavailable (initialization failed at startup)."
	}

	parts := strings.Fields(rawInput)
	sub := ""
	if len(parts) >= 2 {
		sub = strings.ToLower(parts[1])
	}

	// Normalize aliases.
	switch sub {
	case "ls":
		sub = "list"
	case "rm", "delete":
		sub = "remove"
	}

	switch sub {
	case "list":
		names := mgr.List()
		if len(names) == 0 {
			return "No credentials stored."
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "**Stored credentials** (%d):\n", len(names))
		for _, n := range names {
			fmt.Fprintf(&sb, "• `%s`\n", n)
		}
		return strings.TrimRight(sb.String(), "\n")

	case "set", "add":
		// Hardcoded block — never allow credential values to travel over IM.
		return "❌ Adding credentials via IM is not permitted (passwords must not appear in chat logs).\nUse `ageage cred add <name>` on the command line."

	case "remove":
		if len(parts) < 3 {
			return "Usage: /cred remove <name>"
		}
		name := parts[2]
		if err := mgr.Remove(name); err != nil {
			return fmt.Sprintf("❌ %s", err)
		}
		return fmt.Sprintf("✅ Credential `%s` removed.", name)

	case "reload":
		if err := mgr.Reload(); err != nil {
			return fmt.Sprintf("❌ Reload failed: %s", err)
		}
		names := mgr.List()
		return fmt.Sprintf("✅ Credentials reloaded (%d stored).", len(names))

	case "":
		return "Usage: /cred [list|ls | remove|rm <name> | reload]"

	default:
		return "Usage: /cred [list|ls | remove|rm <name> | reload]\n_(Adding credentials via IM is not allowed — use `ageage cred add` on the CLI.)_"
	}
}

// fmtAge returns a short human-readable description of how long ago t was.
// Used in session listings (e.g. "2h ago", "3d ago", "just now").
// senderDisplayName returns a clean, platform-neutral label for a group chat
// sender. Platform-specific identifiers (Matrix homeservers, @ sigils, numeric
// Discord snowflakes, etc.) are stripped so the agent sees a short human name.
func senderDisplayName(channelType, senderID, senderName string) string {
	switch channelType {
	case "matrix":
		// @localpart:homeserver.org → localpart
		if strings.HasPrefix(senderID, "@") {
			if i := strings.Index(senderID, ":"); i > 1 {
				return senderID[1:i]
			}
			return senderID[1:]
		}
	}
	// For all other platforms: prefer the human-readable display name,
	// strip a leading @ that some platforms include in usernames.
	name := strings.TrimPrefix(senderName, "@")
	if name != "" {
		return name
	}
	return senderID
}

func fmtAge(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

// resolveSession resolves a user-supplied session name to an actual session ID.
// It first tries an exact match, then falls back to unambiguous prefix matching.
// Returns an error when the name is not found or is ambiguous.
func resolveSession(sm *agent.SessionManager, query string) (string, error) {
	exact, prefixMatches, err := sm.FindByPrefix(query)
	if err != nil {
		return "", err
	}
	if exact != nil {
		return exact.ID, nil
	}
	switch len(prefixMatches) {
	case 0:
		return "", fmt.Errorf("session %q not found", query)
	case 1:
		return prefixMatches[0].ID, nil
	default:
		names := make([]string, len(prefixMatches))
		for i, m := range prefixMatches {
			names[i] = m.ID
		}
		return "", fmt.Errorf("ambiguous prefix %q — matches: %s", query, strings.Join(names, ", "))
	}
}

// printSessionList prints a formatted session list with dynamic column widths.
// activeID marks the currently active session with a ▶ indicator.
func printSessionList(infos []agent.SessionInfo, activeID string) {
	// Compute the widest session ID so columns stay aligned regardless of name length.
	maxW := 7 // minimum width ("default")
	for _, si := range infos {
		if len(si.ID) > maxW {
			maxW = len(si.ID)
		}
	}
	if maxW > 42 {
		maxW = 42
	}
	for _, si := range infos {
		marker := "  "
		if si.ID == activeID {
			marker = stBlue.Render("▶ ")
		}
		id := si.ID
		if len(id) > maxW {
			id = id[:maxW-1] + "…"
		}
		preview := ""
		if si.Preview != "" {
			preview = "  " + stDim.Render(`"`+si.Preview+`"`)
		}
		fmt.Printf("  %s%-*s  %s  %s%s\n",
			marker,
			maxW,
			id,
			stDim.Render(fmt.Sprintf("%2d turns", si.TurnCount)),
			stDim.Render(fmtAge(si.ModTime)),
			preview,
		)
	}
}

// isAutoSessionName reports whether s is an auto-generated name like "session-3".
func isAutoSessionName(s string) bool {
	if !strings.HasPrefix(s, "session-") {
		return false
	}
	suffix := s[len("session-"):]
	if suffix == "" {
		return false
	}
	for _, c := range suffix {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// firstMessageSlug derives a short slug from the first user message in a conversation.
// Returns "" if no user message is found or the slug would be empty after sanitizing.
func firstMessageSlug(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role == "user" {
			words := strings.Fields(m.TextContent())
			if len(words) > 5 {
				words = words[:5]
			}
			s := agent.SanitizeSessionID(strings.Join(words, "-"))
			if len(s) > 32 {
				s = s[:32]
			}
			s = strings.TrimRight(s, "-")
			return s
		}
	}
	return ""
}

// imDiffWrite builds a Markdown diff block for a file_write operation.
func imDiffWrite(path, content string) string {
	const maxLines = 30
	lines := strings.Split(content, "\n")
	var sb strings.Builder
	fmt.Fprintf(&sb, "**📝 Writing `%s`**\n```diff\n", path)
	for i, l := range lines {
		if i >= maxLines {
			fmt.Fprintf(&sb, "... (%d more lines)\n", len(lines)-maxLines)
			break
		}
		fmt.Fprintf(&sb, "+ %s\n", l)
	}
	sb.WriteString("```")
	return sb.String()
}

// imDiffEdit builds a Markdown diff block for a file_edit operation.
func imDiffEdit(path, oldStr, newStr string) string {
	const maxLines = 15
	oldLines := strings.Split(oldStr, "\n")
	newLines := strings.Split(newStr, "\n")
	var sb strings.Builder
	fmt.Fprintf(&sb, "**✏️ Editing `%s`**\n```diff\n", path)
	for i, l := range oldLines {
		if i >= maxLines {
			fmt.Fprintf(&sb, "... (%d more lines)\n", len(oldLines)-maxLines)
			break
		}
		fmt.Fprintf(&sb, "- %s\n", l)
	}
	for i, l := range newLines {
		if i >= maxLines {
			fmt.Fprintf(&sb, "... (%d more lines)\n", len(newLines)-maxLines)
			break
		}
		fmt.Fprintf(&sb, "+ %s\n", l)
	}
	sb.WriteString("```")
	return sb.String()
}

func runSkills(cmd *cobra.Command, args []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	configPath = findConfigFile(configPath)

	factory, err := agent.NewFactory(configPath, debugFlag)
	if err != nil {
		return err
	}

	loadedSkills := factory.Skills
	if len(loadedSkills) == 0 {
		fmt.Println("  No skills found.")
		return nil
	}

	fmt.Printf("\n  Skills (%d loaded)\n", len(loadedSkills))
	fmt.Println("  " + strings.Repeat("─", 48))
	for _, s := range loadedSkills {
		fmt.Printf("\n  ◆ %s", s.Name)
		if s.Version != "" {
			fmt.Printf("  v%s", s.Version)
		}
		fmt.Println()
		if s.Description != "" {
			fmt.Printf("    %s\n", s.Description)
		}
		if len(s.RequiredTools) > 0 {
			fmt.Printf("    tools: %s\n", strings.Join(s.RequiredTools, ", "))
		}
	}
	fmt.Println()

	return nil
}

func runMCP(cmd *cobra.Command, args []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	configPath = findConfigFile(configPath)

	factory, err := agent.NewFactory(configPath, debugFlag)
	if err != nil {
		return err
	}
	if err := agent.EnsureAgeAgeDir(factory.Config.EffectiveWorkDir()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create .ageage directory: %s\n", err)
	}

	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	go factory.WatchSkills(watchCtx)
	go agent.NewCronScheduler(factory.CronStore, factory).Run(watchCtx)

	mcpSrv := server.NewMCPServer(factory)
	return mcpSrv.Start()
}

// --- ageage cred ---

// credMgrFromCmd loads config and initialises a CredentialManager.
// The cred subcommands use this instead of a full AgentFactory.
func credMgrFromCmd(cmd *cobra.Command) (*creds.Manager, error) {
	configPath, _ := cmd.Flags().GetString("config")
	if configPath == "" {
		// Walk up to the parent to find the persistent flag.
		configPath, _ = cmd.Root().PersistentFlags().GetString("config")
	}
	configPath = findConfigFile(configPath)
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return creds.NewManager(cfg.CredentialsPath())
}

func runCredKeygen(cmd *cobra.Command, args []string) error {
	path, err := creds.KeyFilePath()
	if err != nil {
		return err
	}
	fmt.Printf("Master key location: %s\n", path)
	fmt.Println("(Auto-generated on first use. Keep this file private.)")
	return nil
}

func runCredList(cmd *cobra.Command, args []string) error {
	mgr, err := credMgrFromCmd(cmd)
	if err != nil {
		return err
	}
	names := mgr.List()
	if len(names) == 0 {
		fmt.Println("No credentials stored.")
		return nil
	}
	fmt.Printf("Stored credentials (%d):\n", len(names))
	for _, n := range names {
		fmt.Printf("  • %s\n", n)
	}
	return nil
}

func runCredAdd(cmd *cobra.Command, args []string) error {
	name := args[0]
	mgr, err := credMgrFromCmd(cmd)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Value for %q (input hidden): ", name)
	valBytes, err := term.ReadPassword(os.Stdin.Fd())
	fmt.Fprintln(os.Stderr) // newline after hidden input
	if err != nil {
		// Fallback: read normally when not on a TTY (e.g. piped input).
		fmt.Fprintf(os.Stderr, "(no TTY — reading value from stdin): ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		valBytes = []byte(strings.TrimRight(line, "\r\n"))
	}
	if len(valBytes) == 0 {
		return fmt.Errorf("value must not be empty")
	}
	if err := mgr.Set(name, string(valBytes)); err != nil {
		return err
	}
	fmt.Printf("✓ Credential %q saved.\n", name)
	return nil
}

func runCredSet(cmd *cobra.Command, args []string) error {
	name, value := args[0], args[1]
	mgr, err := credMgrFromCmd(cmd)
	if err != nil {
		return err
	}
	if err := mgr.Set(name, value); err != nil {
		return err
	}
	fmt.Printf("✓ Credential %q saved.\n", name)
	return nil
}

func runCredRemove(cmd *cobra.Command, args []string) error {
	name := args[0]
	mgr, err := credMgrFromCmd(cmd)
	if err != nil {
		return err
	}
	if err := mgr.Remove(name); err != nil {
		return err
	}
	fmt.Printf("✓ Credential %q removed.\n", name)
	return nil
}

// --- ageage tools ---

// toolEntry describes a tool available for selection.
type toolEntry struct {
	name      string
	desc      string
	skillOnly bool // normally registered only when a skill requires it
}

var knownTools = []toolEntry{
	{"bash", "Execute shell commands", false},
	{"file_read", "Read files", false},
	{"file_write", "Create/overwrite files", false},
	{"file_edit", "Edit files (diff-based)", false},
	{"web_fetch", "Fetch web pages", false},
	{"web_search", "Search the web", false},
	{"memory_store", "Store memories", false},
	{"memory_recall", "Recall memories", false},
	{"memory_forget", "Delete memories", false},
	{"cron_add", "Schedule cron tasks", false},
	{"cron_remove", "Remove cron tasks", false},
	{"cron_list", "List cron tasks", false},
	{"delegate", "Delegate to a sub-agent", false},
	{"grep", "Search file content", true},
	{"glob", "Find files by pattern", true},
	{"tree", "Show directory tree", true},
	{"ask_user", "Ask the user a question", true},
	{"escalate", "Escalate task to user", true},
	{"browser_navigate", "Browser navigation", true},
	{"browser_action", "Browser click/type/scroll actions", true},
	{"browser_content", "Get browser page content", true},
	{"update_todos", "Manage todo list", true},
}

// selectTools presents an interactive checklist and returns the selected tool names.
// Pass nil for initialSelected to start with all tools enabled.
// Returns nil when all tools are selected (empty config = all tools).
func selectTools(reader *bufio.Reader, initialSelected []string) []string {
	selected := make([]bool, len(knownTools))
	if len(initialSelected) == 0 {
		// nil or empty → all enabled
		for i := range selected {
			selected[i] = true
		}
	} else {
		for i, t := range knownTools {
			selected[i] = slices.Contains(initialSelected, t.name)
		}
	}

	for {
		fmt.Println()
		fmt.Println("   Tools  ([x] = enabled   [ ] = disabled)")
		fmt.Println("   " + strings.Repeat("─", 54))
		for i, t := range knownTools {
			mark := "[ ]"
			if selected[i] {
				mark = "[x]"
			}
			note := ""
			if t.skillOnly {
				note = "  (skill-only by default)"
			}
			fmt.Printf("   %2d. %s  %-22s %s%s\n", i+1, mark, t.name, t.desc, note)
		}
		fmt.Println()
		fmt.Print("   Toggle (e.g. 1,3,5), 'a' = all, 'd' = none, Enter = confirm: ")
		line := strings.TrimSpace(readLine(reader, ""))
		if line == "" {
			break
		}
		switch line {
		case "a":
			for i := range selected {
				selected[i] = true
			}
		case "d":
			for i := range selected {
				selected[i] = false
			}
		default:
			for part := range strings.SplitSeq(line, ",") {
				var n int
				if _, err := fmt.Sscanf(strings.TrimSpace(part), "%d", &n); err == nil && n >= 1 && n <= len(knownTools) {
					selected[n-1] = !selected[n-1]
				}
			}
		}
	}

	result := make([]string, 0, len(knownTools))
	allOn := true
	for i, t := range knownTools {
		if selected[i] {
			result = append(result, t.name)
		} else {
			allOn = false
		}
	}
	if allOn {
		return nil // empty config means all tools enabled
	}
	return result
}

// toolsLineFromSlice formats a tools slice as a TOML config line.
func toolsLineFromSlice(tools []string) string {
	if len(tools) == 0 {
		return "# tools = []  # Positive allowlist; empty = all tools enabled"
	}
	quoted := make([]string, len(tools))
	for i, t := range tools {
		quoted[i] = fmt.Sprintf("%q", t)
	}
	return fmt.Sprintf("tools = [%s]", strings.Join(quoted, ", "))
}

// updateConfigTools replaces or inserts the tools line in the [agent] section of a TOML file.
func updateConfigTools(configPath, toolsLine string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")

	agentIdx := -1
	toolsIdx := -1
	for i, line := range lines {
		stripped := strings.TrimSpace(line)
		if stripped == "[agent]" {
			agentIdx = i
			continue
		}
		if agentIdx >= 0 && toolsIdx < 0 {
			// Stop at the next section header.
			if strings.HasPrefix(stripped, "[") {
				break
			}
			isTools := strings.HasPrefix(stripped, "# tools") ||
				(strings.HasPrefix(stripped, "tools") && len(stripped) > 5 && (stripped[5] == ' ' || stripped[5] == '='))
			if isTools {
				toolsIdx = i
			}
		}
	}

	if toolsIdx >= 0 {
		lines[toolsIdx] = toolsLine
	} else if agentIdx >= 0 {
		// Insert after the [agent] line.
		newLines := make([]string, 0, len(lines)+1)
		newLines = append(newLines, lines[:agentIdx+1]...)
		newLines = append(newLines, toolsLine)
		newLines = append(newLines, lines[agentIdx+1:]...)
		lines = newLines
	} else {
		// No [agent] section — append one.
		lines = append(lines, "", "[agent]", toolsLine)
	}

	return os.WriteFile(configPath, []byte(strings.Join(lines, "\n")), 0o644)
}

func runTools(cmd *cobra.Command, args []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	configPath = findConfigFile(configPath)

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	reader := bufio.NewReader(os.Stdin)

	fmt.Println("\n🔧 Tool Selection")
	fmt.Println(strings.Repeat("─", 40))
	if len(cfg.Agent.Tools) == 0 {
		fmt.Println("Current: all tools enabled (no allowlist set)")
	} else {
		fmt.Printf("Current allowlist: %s\n", strings.Join(cfg.Agent.Tools, ", "))
	}

	selected := selectTools(reader, cfg.Agent.Tools)
	newLine := toolsLineFromSlice(selected)

	fmt.Printf("\nNew setting: %s\n", newLine)
	fmt.Print("Write to config? (Y/n): ")
	if strings.ToLower(readLine(reader, "y")) == "n" {
		fmt.Println("Aborted — config not changed.")
		return nil
	}
	if err := updateConfigTools(configPath, newLine); err != nil {
		return fmt.Errorf("failed to update config: %w", err)
	}
	fmt.Printf("Updated %s\n", configPath)
	return nil
}
