package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"ageage/agent"
	"ageage/channel"
	"ageage/llm"
	"ageage/security"
	"ageage/server"
	"ageage/tools"

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

	// --- ageage mcp ---
	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Start AgeAge as an MCP server over stdio",
		RunE:  runMCP,
	}
	mcpCmd.Flags().StringP("config", "c", "", "Path to config.toml")
	rootCmd.AddCommand(mcpCmd)

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

	fmt.Println("🚀 AgeAge Setup Wizard")
	fmt.Println(strings.Repeat("─", 40))

	// 1. Workspace directory.
	fmt.Print("\n📁 Workspace directory (default: ./workspace): ")
	wsDir := readLine(reader, "./workspace")
	absWsDir, _ := filepath.Abs(wsDir)

	// 2. API Base URL.
	fmt.Print("\n🌐 LLM API Base URL (default: https://api.openai.com/v1): ")
	baseURL := readLine(reader, "https://api.openai.com/v1")

	// 3. API Key.
	fmt.Print("\n🔑 LLM API Key: ")
	apiKey := readLine(reader, "")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
		if apiKey != "" {
			fmt.Println("   (using OPENAI_API_KEY from environment)")
		}
	}

	// 4. Web Search Backend.
	fmt.Println("\n🔎 Web Search Backend:")
	fmt.Println("   1) DuckDuckGo (Native, no dependencies)")
	fmt.Println("   2) SearXNG (Requires a running instance)")
	fmt.Print("   Select (1-2, default: 1): ")
	searchChoice := readLine(reader, "1")
	searchBackend := "duckduckgo"
	searxngURL := ""
	if searchChoice == "2" {
		searchBackend = "searxng"
		fmt.Print("   SearXNG Instance URL (default: http://localhost:8080): ")
		searxngURL = readLine(reader, "http://localhost:8080")
	}

	// 5. Web Fetch Backend.
	fmt.Println("\n📄 Web Fetch Backend:")
	fmt.Println("   1) Native (Go-based, simple)")
	fmt.Println("   2) Jina (Requires Jina API key)")
	fmt.Println("   3) Crawl4AI (Requires Python & crawl4ai package)")
	fmt.Print("   Select (1-3, default: 1): ")
	fetchChoice := readLine(reader, "1")
	fetchBackend := "native"
	jinaKey := ""
	pythonCmd := ""

	switch fetchChoice {
	case "2":
		fetchBackend = "jina"
		fmt.Print("   Jina API Key (optional): ")
		jinaKey = readLine(reader, "")
	case "3":
		fetchBackend = "crawl4ai"
		pythonCmd = detectPython()
		if pythonCmd == "" {
			fmt.Println("⚠️  Warning: Python not detected. Crawl4AI will require manual setup.")
			pythonCmd = "python"
		} else {
			fmt.Printf("Detected Python: %s\n", pythonCmd)
		}
	}

	// Auto-fill remaining values.
	model := "gpt-4o-mini"
	// Detect provider from URL and adjust defaults.
	if strings.Contains(baseURL, "deepseek") {
		model = "deepseek-chat"
	} else if strings.Contains(baseURL, "generativelanguage.googleapis.com") || strings.Contains(baseURL, "gemini") {
		model = "gemini-2.0-flash"
	}

	// Create directories.
	dirs := []string{
		filepath.Join(absWsDir, "data"),
		filepath.Join(absWsDir, "skills"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", d, err)
		}
	}

	// Generate config.toml.
	configPath := filepath.Join(absWsDir, "config.toml")
	writeConfig := true
	if _, err := os.Stat(configPath); err == nil {
		fmt.Printf("\n⚠️ %s already exists. Overwrite? (y/n): ", configPath)
		if readLine(reader, "n") != "y" {
			fmt.Println("   Skipped config.toml")
			writeConfig = false
		}
	}
	if writeConfig {
		configContent := fmt.Sprintf(`# AgeAge Configuration — Generated by ageage init

workspace = "%s"

[llm]
api_key = "%s"
base_url = "%s"
model = "%s"
temperature = 0.7

[agent]
max_iterations = 20
mode = "supervised"

[subagent]
enabled = true
max_iterations = 10
timeout = 300

[subagent.model]
# model = ""
# api_key = ""
# base_url = ""

[router]
enabled = true
max_history = 8

[router.router]
model = "%s"

[router.medium]
model = "%s"

[router.strong]
model = "%s"

[summarize]
enabled = false
model = "%s"
threshold = 10
keep_recent = 4

[bash]
auto_allow_commands = []

[web_search]
backend = "%s"
searxng_url = "%s"
max_results = 10

[web_fetch]
backend = "%s"
jina_api_key = "%s"
crawl4ai_cmd = "%s"
max_characters = 15000

[mcp]
enabled = false
# Define external MCP servers here
# [mcp.servers.weather]
# command = "npx"
# args = ["-y", "@modelcontextprotocol/server-weather"]
# env = { API_KEY = "..." }

[security]
blocked_commands = [
  "rm -rf /", "rm -rf /*", "mkfs", "dd if=",
  ":(){ :|:& };:", "> /dev/sda", "chmod -R 777 /", "format c:",
]
allowed_roots = []
forbidden_roots = []

[server]
host = "127.0.0.1"
port = 8080

[multimodal]
# vision = true means the LLM accepts image attachments directly.
# Set to false if your model is text-only — images will be rejected or converted.
vision = true
max_image_bytes = 10485760  # 10 MB

# Converters turn non-image files into plain text before sending to the LLM.
# {input} is replaced with the source file path; {output} with a managed .md tmp path.
# Example:
# [[multimodal.converters]]
# extensions = ["pdf"]
# command = "pdftotext {input} {output}"
#
# [[multimodal.converters]]
# extensions = ["docx", "odt"]
# command = "libreoffice --headless --convert-to txt:Text {input} --outdir {output}"
`, absWsDir, apiKey, baseURL, model, model, model, getStrongModel(model), model,
			searchBackend, searxngURL, fetchBackend, jinaKey, pythonCmd)

		if err := os.WriteFile(configPath, []byte(configContent), 0o644); err != nil {
			return fmt.Errorf("failed to write config.toml: %w", err)
		}
		fmt.Printf("Created %s\n", configPath)
	}

	// Generate AGENT.md if not exists (behavioral rules — always injected).
	agentPath := filepath.Join(absWsDir, "data", "AGENT.md")
	if _, err := os.Stat(agentPath); os.IsNotExist(err) {
		agentContent := `# AGENT

## Execution Directives

- Use tools to gather information and perform actions.
- Call finish_task when the task is complete with a FINAL, complete answer.
- Think step by step for complex tasks.
- Always provide complete, detailed answers — never say "see above" or "refer to results".
- Use the memory tool to store and recall important context across sessions.
- Deploy tools efficiently; avoid unnecessary calls.
- Stay honest about limitations.
- Always respond in the same language the user uses.
`
		if err := os.WriteFile(agentPath, []byte(agentContent), 0o644); err != nil {
			fmt.Printf("⚠️  Warning: could not create %s: %s\n", agentPath, err)
		} else {
			fmt.Printf("Created %s\n", agentPath)
		}
	}

	// Generate SOUL.md if not exists (persona — injected in serve/connect, not CLI by default).
	soulPath := filepath.Join(absWsDir, "data", "SOUL.md")
	if _, err := os.Stat(soulPath); os.IsNotExist(err) {
		soulContent := `# SOUL

You are a helpful, friendly, and knowledgeable AI assistant.

## Communication Style

- Match the user's language and tone.
- Use clear formatting (markdown) when helpful.
- Keep responses focused and avoid unnecessary verbosity.
`
		if err := os.WriteFile(soulPath, []byte(soulContent), 0o644); err != nil {
			fmt.Printf("⚠️  Warning: could not create %s: %s\n", soulPath, err)
		} else {
			fmt.Printf("Created %s\n", soulPath)
		}
	}

	// Create empty MEMORY.jsonl if not exists.
	memPath := filepath.Join(absWsDir, "data", "MEMORY.jsonl")
	if _, err := os.Stat(memPath); os.IsNotExist(err) {
		if err := os.WriteFile(memPath, []byte(""), 0o644); err != nil {
			fmt.Printf("⚠️  Warning: could not create %s: %s\n", memPath, err)
		}
	}

	fmt.Println()
	fmt.Println("  " + strings.Repeat("─", 50))
	fmt.Println("  ✨  Setup complete!")
	fmt.Println()
	fmt.Printf("  Run:  ageage cli -c %s\n", configPath)
	fmt.Println()

	return nil
}

func readLine(reader *bufio.Reader, defaultVal string) string {
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)
	if input == "" {
		return defaultVal
	}
	return input
}

func getStrongModel(baseModel string) string {
	switch {
	case strings.Contains(baseModel, "gpt-4o-mini"):
		return "gpt-4o"
	case strings.Contains(baseModel, "deepseek"):
		return "deepseek-reasoner"
	case strings.Contains(baseModel, "gemini") && strings.Contains(baseModel, "flash"):
		return "gemini-2.0-pro"
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
	// Per-chat agent pool: each chat/room gets its own persistent agent.
	agents := make(map[string]*agent.Agent)
	var agentMu sync.Mutex

	confirmMgr := tools.NewConfirmationManager()

	// We'll need a way for NotifyFunc to access the channels to send messages.
	// We'll set this up after creating the manager.
	var managerPtr *channel.Manager
	// channelsByType maps channel name → Channel, for editable-channel lookups.
	channelsByType := make(map[string]channel.Channel)

	getAgent := func(key string, chatKey string) (*agent.Agent, error) {
		agentMu.Lock()
		defer agentMu.Unlock()
		if ag, ok := agents[key]; ok {
			return ag, nil
		}

		ag := factory.CreateAgent(confirmMgr, "")

		parts := strings.SplitN(chatKey, ":", 2)
		channelType, channelID := "", ""
		if len(parts) == 2 {
			channelType, channelID = parts[0], parts[1]
		}

		// Wire up todo send/edit if the channel supports message editing.
		if ch, ok := channelsByType[channelType]; ok {
			if editable, ok := ch.(channel.Editable); ok {
				cID := channelID // capture for closures
				ag.TodoSendFunc = func(text string) string {
					msgID, err := editable.SendMessage(cID, text)
					if err != nil {
						fmt.Printf("[todo] send error: %s\n", err)
					}
					return msgID
				}
				ag.TodoEditFunc = func(msgID, text string) error {
					return editable.EditMessage(cID, msgID, text)
				}
			}
		}

		// NotifyFunc is the fallback for channels without editing support.
		ag.NotifyFunc = func(message string) {
			if managerPtr != nil && channelType != "" {
				managerPtr.Send(channelType, channelID, message)
			}
		}

		// ToolStartCallback: send a Markdown diff block to the channel when the
		// agent writes or edits a file, so IM users can see what changed.
		notifyFn := ag.NotifyFunc
		ag.ToolStartCallback = func(name, args string) {
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

		agents[key] = ag
		return ag, nil
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

	handler := func(msg channel.IncomingMessage) string {
		text := strings.TrimSpace(strings.ToLower(msg.Text))
		chatKey := msg.ChannelType + ":" + msg.ChannelID

		// Confirmation responses (y/n/a) bypass the per-chat mutex to avoid
		// deadlocking with the agent that is waiting for the confirmation.
		if text == "y" || text == "n" || text == "a" {
			pending := confirmMgr.GetAllPending(msg.ChannelID)
			if len(pending) > 0 {
				pc := pending[0]
				allowed := (text == "y" || text == "a")
				confirmMgr.RespondToConfirmation(pc.ID, allowed)
				if !allowed {
					return "❌ Operation denied."
				}
				return ""
			}
		}

		// /stop also bypasses the per-chat mutex: the mutex is held by the
		// running agent, so waiting for it would make /stop arrive too late.
		if text == "/stop" {
			agentMu.Lock()
			if ag, ok := agents[chatKey]; ok {
				ag.Stop()
			}
			agentMu.Unlock()
			return "🛑 Task stopped."
		}

		// All other messages are processed sequentially per chat.
		mu := getChatMutex(chatKey)
		mu.Lock()
		defer mu.Unlock()

		if debugFlag {
			fmt.Printf("\n  ▸ [%s] %s: %s\n", msg.ChannelType, msg.SenderName, msg.Text)
		}

		// Use lowercased `text` so commands are case-insensitive.
		switch text {
		case "/clear":
			agentMu.Lock()
			if ag, ok := agents[chatKey]; ok {
				ag.ClearHistory()
			}
			agentMu.Unlock()
			return "🗑️ Conversation history cleared."

		case "/summarize":
			ag, err := getAgent(chatKey, chatKey)
			if err != nil {
				return fmt.Sprintf("Error: %s", err)
			}
			summary, err := ag.ForceSummarize()
			if err != nil {
				return fmt.Sprintf("❌ %s", err)
			}
			return fmt.Sprintf("📋 Summary:\n%s", summary)

		case "/help":
			return "Available commands:\n" +
				"/clear — Clear conversation history\n" +
				"/stop — Stop the current task\n" +
				"/summarize — Summarize conversation\n" +
				"/help — Show this help"
		}

		ag, err := getAgent(chatKey, chatKey)
		if err != nil {
			return fmt.Sprintf("Error initializing agent: %s", err)
		}

		// Set current channel ID for tools to use.
		ag.SetChannelID(msg.ChannelID)

		result, err := ag.Run(context.Background(), msg.Text, nil)
		if err != nil {
			return fmt.Sprintf("Agent error: %s", err)
		}
		return result
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
			tg := channel.NewTelegram(cfg.Channels.Telegram.BotToken, opts)
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
			manager.Register(channel.NewDiscord(cfg.Channels.Discord.BotToken, cfg.Channels.Discord.ChannelIDs, opts))
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

	ag := factory.CreateAgent(nil, "")

	ui := newCLIUI(factory.Config.LLM.Model)
	ui.printBanner()

	// Read stdin in a dedicated goroutine so the main loop can remain
	// responsive (e.g. to /stop) while the agent is running.
	inputCh := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			inputCh <- scanner.Text()
		}
		close(inputCh)
	}()

	// Ctrl+C: stop the running agent gracefully; if idle, exit.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println()
		ag.Stop()
		ui.printInfo("Interrupted.")
		os.Exit(0)
	}()

	type agentResult struct {
		result   string
		streamed bool
		err      error
	}

	ui.printPrompt()

	var agentCh chan agentResult

	for {
		if agentCh == nil {
			input, ok := <-inputCh
			if !ok {
				break
			}
			input = strings.TrimSpace(input)
			if input == "" {
				ui.printPrompt()
				continue
			}

			switch input {
			case "exit", "quit":
				ui.printInfo("Goodbye!")
				return nil

			case "/clear":
				ag.ClearHistory()
				ui.printOK("History cleared.")
				ui.printPrompt()

			case "/stop":
				ui.printWarn("No task running.")
				ui.printPrompt()

			case "/summarize":
				ui.printStatus("Summarizing…")
				summary, err := ag.ForceSummarize()
				if err != nil {
					ui.printErr(err.Error())
				} else {
					fmt.Println()
					fmt.Println(summary)
					fmt.Println()
				}
				ui.printPrompt()

			default:
				// Parse @path file attachments from input.
				cleanText, parts, warnings := agent.ParseCLIInput(input, factory.Config, ag.TmpManager())
				for _, w := range warnings {
					ui.printWarn(w)
				}
				fmt.Println()
				ui.printAgentHeader()

				spinner := newSpinner()
				spinner.Start("Thinking…")

				// ToolStartCallback: show spinner with tool name; for file_write/
				// file_edit also print a diff preview (spinner is stopped first).
				ag.ToolStartCallback = func(name, args string) {
					switch name {
					case "file_write":
						spinner.Stop()
						var p struct {
							Path    string `json:"path"`
							Content string `json:"content"`
						}
						if json.Unmarshal([]byte(args), &p) == nil && p.Path != "" {
							ui.printFileWrite(p.Path, p.Content)
						}
					case "file_edit":
						spinner.Stop()
						var p struct {
							Path    string `json:"path"`
							Search  string `json:"search"`
							Replace string `json:"replace"`
						}
						if json.Unmarshal([]byte(args), &p) == nil && p.Path != "" {
							ui.printFileEdit(p.Path, p.Search, p.Replace)
						}
					default:
						label := name
						spinner.Update("Running " + label + "…")
					}
				}
				ag.ToolEndCallback = func(name string) {
					// After non-diff tools finish, resume thinking spinner.
					switch name {
					case "file_write", "file_edit":
						// Spinner already stopped for diff display; restart for next step.
						spinner.Start("Thinking…")
					default:
						spinner.Update("Thinking…")
					}
				}

				agentCh = make(chan agentResult, 1)
				go func(text string, ps []llm.ContentPart, ch chan agentResult) {
					streamed := false
					result, err := ag.RunWithParts(context.Background(), text, ps, func(token string) {
						if !streamed {
							spinner.Stop() // clear spinner before first streamed token
							streamed = true
						}
						fmt.Print(token)
					})
					spinner.Stop() // ensure cleared if no tokens were streamed
					ch <- agentResult{result, streamed, err}
				}(cleanText, parts, agentCh)
			}

		} else {
			select {
			case res := <-agentCh:
				agentCh = nil
				if res.err != nil {
					fmt.Println()
					ui.printErr(res.err.Error())
				} else {
					if !res.streamed && res.result != "" {
						fmt.Print(res.result)
					}
					fmt.Println()
					ui.printUsage(ag.LastRunUsage())
				}
				fmt.Println()
				ui.printPrompt()

			case input, ok := <-inputCh:
				if !ok {
					ag.Stop()
					<-agentCh
					return nil
				}
				if strings.TrimSpace(input) == "/stop" {
					ag.Stop()
					fmt.Println()
					ui.printWarn("Stop signal sent.")
				}
			}
		}
	}

	return nil
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

	mcpSrv := server.NewMCPServer(factory)
	return mcpSrv.Start()
}
