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
max_iterations = 10
timeout = 300

[subagent.model]
# model = ""
# api_key = ""
# base_url = ""

[pipeline]
# foreach_concurrency = 4  # max parallel foreach iterations; 0 or 1 = sequential

[pipeline.models.simple]
# model = ""
# api_key = ""
# base_url = ""

[pipeline.models.medium]
# model = ""
# api_key = ""
# base_url = ""

[pipeline.models.complex]
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

	// Session manager — one per .ageage directory (shared across all chats).
	sm := agent.NewSessionManager(factory.Config.AgeAgeDirPath())

	// Per-session agent pool. In channel mode, session IDs are prefixed with a
	// sanitised chatKey so each chat's sessions are independent.
	agents := make(map[string]*agent.Agent) // sessionID → agent
	activeSessions := make(map[string]string) // chatKey → active sessionID
	var agentMu sync.Mutex

	confirmMgr := tools.NewConfirmationManager()

	var managerPtr *channel.Manager
	channelsByType := make(map[string]channel.Channel)

	// chatSessionID returns the active session ID for a chatKey, creating the
	// default session on first access. Must be called with agentMu held.
	chatSessionID := func(chatKey string) string {
		if id, ok := activeSessions[chatKey]; ok {
			return id
		}
		id := agent.SanitizeSessionID(chatKey)
		_ = sm.EnsureSession(id)
		activeSessions[chatKey] = id
		return id
	}

	// makeChatAgent creates an agent for a session, wires IM callbacks, and
	// optionally loads existing history. Must be called with agentMu held.
	makeChatAgent := func(chatKey, sessionID string) *agent.Agent {
		parts := strings.SplitN(chatKey, ":", 2)
		channelType, channelID := "", ""
		if len(parts) == 2 {
			channelType, channelID = parts[0], parts[1]
		}

		ag := factory.CreateAgent(confirmMgr, "")
		ag.SessionDir = sm.SessionDir(sessionID)

		if ch, ok := channelsByType[channelType]; ok {
			if editable, ok := ch.(channel.Editable); ok {
				cID := channelID
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
		ag.NotifyFunc = func(message string) {
			if managerPtr != nil && channelType != "" {
				managerPtr.Send(channelType, channelID, message)
			}
		}
		ag.AskUserNotify = func(question string, options []string) {
			if managerPtr != nil && channelType != "" {
				managerPtr.SendQuestion(channelType, channelID, question, options)
			}
		}
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

	// handleSessionCmd processes /session sub-commands for a given chatKey.
	// The defaultPrefix is the sanitised chatKey (session ID prefix for this chat).
	handleSessionCmd := func(chatKey, rawInput string) string {
		defaultPrefix := agent.SanitizeSessionID(chatKey)

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
				fmt.Fprintf(&sb, "• %s (%d messages)%s\n", si.ID, si.MessageCount, marker)
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

		case "delete":
			if len(parts) < 3 {
				return "Usage: /session delete <name>"
			}
			name := strings.Join(parts[2:], "-")
			delFullID := toFullID(name)
			if delFullID == currentSessionID {
				return "❌ Cannot delete the active session. Switch to another session first."
			}
			agentMu.Lock()
			delete(agents, delFullID)
			agentMu.Unlock()
			if err := sm.Delete(delFullID); err != nil {
				return fmt.Sprintf("❌ Failed to delete session: %s", err)
			}
			return fmt.Sprintf("🗑️ Deleted session **%s**.", toDisplayName(delFullID))

		default:
			return "Usage: /session [list | new [name] | switch <name> | delete <name>]"
		}
	}

	handler := func(msg channel.IncomingMessage) string {
		text := strings.TrimSpace(msg.Text)
		textLow := strings.ToLower(text)
		chatKey := msg.ChannelType + ":" + msg.ChannelID

		// Confirmation responses bypass the per-chat mutex.
		if textLow == "y" || textLow == "n" || textLow == "a" {
			pending := confirmMgr.GetAllPending(msg.ChannelID)
			if len(pending) > 0 {
				pc := pending[0]
				allowed := (textLow == "y" || textLow == "a")
				confirmMgr.RespondToConfirmation(pc.ID, allowed)
				if !allowed {
					return "❌ Operation denied."
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
			agentMu.Unlock()
			factory.UserInputMgr.Cancel(msg.ChannelID)
			return "🛑 Task stopped."
		}

		// If a pipeline node is waiting for user input, route this message to it
		// instead of starting a new agent run.
		if factory.UserInputMgr.HasPending(msg.ChannelID) {
			factory.UserInputMgr.Respond(msg.ChannelID, text)
			return "✅ Got it."
		}

		// All other messages are processed sequentially per chat.
		mu := getChatMutex(chatKey)
		mu.Lock()
		defer mu.Unlock()

		if debugFlag {
			fmt.Printf("\n  ▸ [%s] %s: %s\n", msg.ChannelType, msg.SenderName, msg.Text)
		}

		// /session commands.
		if strings.HasPrefix(textLow, "/session") {
			return handleSessionCmd(chatKey, text)
		}

		switch textLow {
		case "/clear":
			agentMu.Lock()
			sessionID := chatSessionID(chatKey)
			if ag, ok := agents[sessionID]; ok {
				ag.ClearHistory()
				_ = sm.SaveHistory(sessionID, ag.Messages())
			}
			agentMu.Unlock()
			return "🗑️ Conversation history cleared."

		case "/summarize":
			ag, sessionID := getAgent(chatKey)
			summary, err := ag.ForceSummarize()
			if err != nil {
				return fmt.Sprintf("❌ %s", err)
			}
			_ = sm.SaveHistory(sessionID, ag.Messages())
			return fmt.Sprintf("📋 Summary:\n%s", summary)

		case "/help":
			return "Available commands:\n" +
				"/clear — Clear conversation history\n" +
				"/stop — Stop the current task\n" +
				"/summarize — Summarize conversation\n" +
				"/session — Manage sessions (list, new, switch, delete)\n" +
				"/help — Show this help"
		}

		ag, sessionID := getAgent(chatKey)
		ag.SetChannelID(msg.ChannelID)

		result, err := ag.Run(context.Background(), msg.Text, nil)
		// Save history after every run (best-effort; ignore errors).
		_ = sm.SaveHistory(sessionID, ag.Messages())
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
		newAg.AskUserNotify = func(question string, options []string) {
			// Print the question; the spinner is stopped by ToolStartCallback.
			fmt.Println()
			fmt.Printf("  ❓ %s\n", question)
			for i, opt := range options {
				fmt.Printf("     %d. %s\n", i+1, opt)
			}
			fmt.Println("  (Type your answer, or /stop to cancel)")
		}
		// Pipeline node progress: print each status update on its own line.
		newAg.NotifyFunc = func(msg string) {
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

	// Read stdin in a dedicated goroutine so the main loop can remain
	// responsive (e.g. to /stop) while the agent is running.
	inputCh := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var accum strings.Builder
		inBlock := false
		for scanner.Scan() {
			raw := scanner.Text()
			if inBlock {
				if strings.TrimSpace(raw) == "```" {
					inBlock = false
					inputCh <- strings.TrimRight(accum.String(), "\n")
					accum.Reset()
				} else {
					accum.WriteString(raw + "\n")
					fmt.Print(stGray.Render("... ▸ "))
				}
				continue
			}
			if strings.TrimSpace(raw) == "```" {
				inBlock = true
				fmt.Print(stGray.Render("... ▸ "))
				continue
			}
			if strings.HasSuffix(raw, "\\") {
				accum.WriteString(strings.TrimSuffix(raw, "\\") + "\n")
				fmt.Print(stGray.Render("... ▸ "))
				continue
			}
			if accum.Len() > 0 {
				accum.WriteString(raw)
				inputCh <- accum.String()
				accum.Reset()
			} else {
				inputCh <- raw
			}
		}
		close(inputCh)
	}()

	// Ctrl+C: stop the running agent gracefully; if idle, exit.
	// The signal handler captures `ag` by variable reference so it always sees
	// the current agent even after a session switch.
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

			// ── /session commands ──────────────────────────────────────────────
			if strings.HasPrefix(strings.ToLower(input), "/session") {
				parts := strings.Fields(input)
				sub := ""
				if len(parts) >= 2 {
					sub = strings.ToLower(parts[1])
				}
				switch sub {
				case "": // /session — show current session info
					infos, _ := sm.ListWithPrefix("")
					ui.printInfo(fmt.Sprintf("Current session: %s", activeSessionID))
					if len(infos) > 1 {
						ui.printInfo("Use /session list to see all sessions.")
					}
				case "list":
					infos, err := sm.List()
					if err != nil || len(infos) == 0 {
						ui.printInfo("No sessions found.")
					} else {
						fmt.Println()
						for _, si := range infos {
							marker := ""
							if si.ID == activeSessionID {
								marker = "  ← current"
							}
							fmt.Printf("  • %-20s %d messages%s\n", si.ID, si.MessageCount, marker)
						}
						fmt.Println()
					}
				case "new":
					newName := ""
					if len(parts) >= 3 {
						newName = agent.SanitizeSessionID(strings.Join(parts[2:], "-"))
					} else {
						newName = fmt.Sprintf("session-%d", len(strings.Fields(activeSessionID)))
					}
					newAg, err := switchSession(newName)
					if err != nil {
						ui.printErr(fmt.Sprintf("Failed to create session: %s", err))
					} else {
						ag = newAg
						ui.printOK(fmt.Sprintf("Switched to new session '%s'.", activeSessionID))
					}
				case "switch":
					if len(parts) < 3 {
						ui.printWarn("Usage: /session switch <name>")
					} else {
						targetID := agent.SanitizeSessionID(strings.Join(parts[2:], "-"))
						if _, statErr := os.Stat(sm.SessionDir(targetID)); os.IsNotExist(statErr) {
							ui.printErr(fmt.Sprintf("Session '%s' does not exist.", targetID))
						} else {
							newAg, err := switchSession(targetID)
							if err != nil {
								ui.printErr(fmt.Sprintf("Failed to switch session: %s", err))
							} else {
								ag = newAg
								ui.printOK(fmt.Sprintf("Switched to session '%s'.", activeSessionID))
							}
						}
					}
				case "delete":
					if len(parts) < 3 {
						ui.printWarn("Usage: /session delete <name>")
					} else {
						delID := agent.SanitizeSessionID(strings.Join(parts[2:], "-"))
						if delID == activeSessionID {
							ui.printErr("Cannot delete the active session.")
						} else if err := sm.Delete(delID); err != nil {
							ui.printErr(fmt.Sprintf("Failed to delete session: %s", err))
						} else {
							ui.printOK(fmt.Sprintf("Deleted session '%s'.", delID))
						}
					}
				default:
					ui.printWarn("Usage: /session [list | new [name] | switch <name> | delete <name>]")
				}
				ui.printPrompt()
				continue
			}

			switch input {
			case "exit", "quit":
				_ = sm.SaveHistory(activeSessionID, ag.Messages())
				ui.printInfo("Goodbye!")
				return nil

			case "/clear":
				ag.ClearHistory()
				_ = sm.SaveHistory(activeSessionID, ag.Messages())
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
					_ = sm.SaveHistory(activeSessionID, ag.Messages())
				}
				ui.printPrompt()

			case "/think":
				if thinkFilter.LastThink == "" {
					ui.printInfo("No think-block captured yet.")
				} else {
					fmt.Println()
					fmt.Println(stGray.Render("┌── last thinking ") + stDim.Render(line(34)))
					for _, l := range strings.Split(strings.TrimSpace(thinkFilter.LastThink), "\n") {
						fmt.Println(stDim.Render("│ " + l))
					}
					fmt.Println(stGray.Render("└" + line(50)))
					fmt.Println()
				}
				ui.printPrompt()

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
				ui.printPrompt()

			case "/help":
				fmt.Println()
				helpLines := [][]string{
					{"/help", "Show this help"},
					{"/clear", "Clear conversation history"},
					{"/stop", "Interrupt a running task"},
					{"/think", "Print the last reasoning think-block"},
					{"/skills", "List available skills"},
					{"/summarize", "Compress conversation history"},
					{"/session", "Show current session"},
					{"/session list", "List all sessions"},
					{"/session new [name]", "Create and switch to a new session"},
					{"/session switch <name>", "Switch to an existing session"},
					{"/session delete <name>", "Delete a session"},
					{"exit / quit", "Exit AgeAge"},
					{"@/path/to/file", "Attach a file to your message"},
				}
				for _, row := range helpLines {
					fmt.Printf("  %s  %s\n",
						stBlue.Render(fmt.Sprintf("%-28s", row[0])),
						stGray.Render(row[1]),
					)
				}
				fmt.Println()
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
					case "ask_user":
						spinner.Stop() // AskUserNotify will print the question
					default:
						spinner.Update("Running " + name + "…")
					}
				}
				ag.ToolEndCallback = func(name string) {
					switch name {
					case "file_write", "file_edit":
						spinner.Start("Thinking…")
					default:
						spinner.Update("Thinking…")
					}
				}
				ag.ToolResultCallback = func(name, result string) {
					ui.printToolResult(name, result)
				}

				thinkFilter.Reset()
				agentCh = make(chan agentResult, 1)
				go func(text string, ps []llm.ContentPart, ch chan agentResult) {
					streamed := false
					// inner: the real output function, stops spinner on first token.
					thinkFilter.inner = func(token string) {
						if !streamed {
							spinner.Stop()
							streamed = true
						}
						fmt.Print(token)
					}
					result, err := ag.RunWithParts(context.Background(), text, ps, thinkFilter.Wrap())
					thinkFilter.Flush()
					spinner.Stop()
					ch <- agentResult{result, streamed, err}
				}(cleanText, parts, agentCh)
			}

		} else {
			select {
			case res := <-agentCh:
				agentCh = nil
				// Save history after every completed turn (best-effort).
				_ = sm.SaveHistory(activeSessionID, ag.Messages())
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
