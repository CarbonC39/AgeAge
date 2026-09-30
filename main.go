package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
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
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "ageage",
		Short: "AgeAge - A mini Golang Agent framework",
		Long: "AgeAge is a lightweight, modular AI agent framework with token optimization and enterprise-grade security.\n\n" +
			"Workspace setup: ageage init [--plain] [--dir PATH]\n" +
			"Configuration:   ageage config [tools|edit|validate] (ageage tools is a compatibility alias)\n" +
			"Memory:          ageage memory [list|search|add|edit|remove|export]",
	}

	rootCmd.PersistentFlags().BoolVar(&debugFlag, "debug", false, "Enable debug output (print model raw output and tool flow)")

	// --- ageage init ---
	initCmd := initCommand()
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

	// --- ageage config ---
	rootCmd.AddCommand(configCommand())

	// --- ageage memory ---
	rootCmd.AddCommand(memoryCommand())

	// --- ageage tools (compatibility alias) ---
	toolsCmd := &cobra.Command{
		Use:   "tools",
		Short: "Edit the tool allowlist (compatibility alias for `ageage config tools`)",
		RunE:  runConfigTools,
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

	// --- ageage cron ---
	cronCmd := &cobra.Command{
		Use:   "cron",
		Short: "Manage scheduled tasks",
	}
	cronCmd.PersistentFlags().StringP("config", "c", "", "Path to config.toml")

	cronListCmd := &cobra.Command{
		Use:   "list",
		Short: "List scheduled tasks with next-run time and last status",
		RunE:  runCronList,
	}

	cronAddCmd := &cobra.Command{
		Use:   "add <schedule> <command>",
		Short: "Add a scheduled task",
		Long: "Add a scheduled task. <command> is free text, or 'skill:<name> [args]' to " +
			"run an existing skill/pipeline on schedule. Results are delivered to the " +
			"room named by --delivery (channelType:channelID[:t:threadID]) in connect/serve mode.",
		Args: cobra.ExactArgs(2),
		RunE: runCronAdd,
	}
	cronAddCmd.Flags().String("delivery", "", "IM delivery target: channelType:channelID or channelType:channelID:t:threadID")

	cronRemoveCmd := &cobra.Command{
		Use:     "remove <id>",
		Aliases: []string{"rm"},
		Short:   "Remove a scheduled task",
		Args:    cobra.ExactArgs(1),
		RunE:    runCronRemove,
	}

	cronRunCmd := &cobra.Command{
		Use:   "run <id>",
		Short: "Run a scheduled task immediately",
		Args:  cobra.ExactArgs(1),
		RunE:  runCronRun,
	}

	cronPauseCmd := &cobra.Command{
		Use:   "pause <id>",
		Short: "Pause a scheduled task without removing it",
		Args:  cobra.ExactArgs(1),
		RunE:  runCronPause,
	}

	cronResumeCmd := &cobra.Command{
		Use:   "resume <id>",
		Short: "Resume a paused scheduled task",
		Args:  cobra.ExactArgs(1),
		RunE:  runCronResume,
	}

	cronCmd.AddCommand(cronListCmd, cronAddCmd, cronRemoveCmd, cronRunCmd, cronPauseCmd, cronResumeCmd)
	rootCmd.AddCommand(cronCmd)

	return rootCmd
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

// findEnvAPIKey returns the first API key found in known environment variables.
func findEnvAPIKey() (key, name string) {
	for _, n := range config.APIKeyEnvironmentNames() {
		if v := os.Getenv(n); v != "" {
			return v, n
		}
	}
	return "", ""
}

// buildInitConfig builds the config.toml content using a strings.Builder.
func buildInitConfig(
	workspace, apiKey, baseURL, model, agentMode, toolsLine string,
	routerEnabled bool, routerClassifier, routerMedium, routerStrong string,
	evalEnabled bool, evalThreshold int,
	searchBackend, searxngURL, tavilyKey, braveKey string,
	fetchBackend, jinaKey, pythonCmd string,
	forbidRM, plannerEnabled, summarizeEnabled, keepRawToolCalls bool,
) string {
	if pythonCmd == "" {
		pythonCmd = config.DefaultCrawl4AICommand
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
	p("# [pipeline.models.base]\n# model = \"\"    # tier=base nodes (uses [llm] model by default)\n")
	p("# [pipeline.models.medium]\n# model = \"\"  # tier=medium nodes\n")
	p("# [pipeline.models.strong]\n# model = \"\"  # tier=strong nodes\n\n")

	p("[planner]\n")
	p("# Controls automatic skill creation for recurring workflows.\n")
	if plannerEnabled {
		p("enabled = true   # auto-create skills/pipelines for recurring workflows\n\n")
	} else {
		p("enabled = false  # never auto-create skills/pipelines (creation is manual only)\n\n")
	}

	if routerEnabled {
		p("[router]\n")
		p("# Routes requests to different model tiers by task complexity.\n")
		p("enabled     = true\n")
		p("max_history = 8\n\n")
		p("[router.classifier]\n")
		p("model    = %q  # lightweight model for intent classification\n", routerClassifier)
		p("# api_key  = \"\"  # override [llm].api_key for this tier (optional)\n")
		p("# base_url = \"\"  # override [llm].base_url for this tier (optional)\n\n")
		p("[router.medium]\n")
		p("model    = %q\n", routerMedium)
		p("# api_key  = \"\"\n")
		p("# base_url = \"\"\n\n")
		p("[router.strong]\n")
		p("model    = %q\n", routerStrong)
		p("# api_key  = \"\"\n")
		p("# base_url = \"\"\n\n")
	} else {
		p("[router]\n")
		p("# Routes requests to model tiers by task complexity.\n")
		p("# Set enabled = true and configure the sub-sections below to activate.\n")
		p("enabled     = false\n")
		p("# max_history = 8\n")
		p("# [router.classifier]\n# model    = \"gpt-4o-mini\"  # cheap intent classifier\n# api_key  = \"\"\n# base_url = \"\"\n")
		p("# [router.medium]\n# model    = %q\n# api_key  = \"\"\n# base_url = \"\"\n", model)
		p("# [router.strong]\n# model    = %q\n# api_key  = \"\"\n# base_url = \"\"\n\n", config.SuggestStrongModel(baseURL, model))
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
	if summarizeEnabled {
		p("enabled     = true\n")
	} else {
		p("enabled     = false\n")
	}
	p("# model     = \"\"  # defaults to [llm].model; use a cheaper model to save cost\n")
	p("threshold   = 10  # compress after this many message pairs\n")
	p("keep_recent = 4   # keep N most recent messages intact after compression\n\n")

	p("[history]\n")
	p("# In-place compression: collapses old tool-call turns into narrative text.\n")
	p("# Disable to keep raw tool-call JSON byte-identical across turns (KV-cache hits).\n")
	if keepRawToolCalls {
		p("compress_tool_turns = false  # keep raw tool-call JSON for KV-cache stability\n")
	} else {
		p("compress_tool_turns = true\n")
	}
	p("keep_recent_turns   = 2\n\n")

	p("[cron]\n")
	p("# Scheduled tasks: run unsupervised in full mode (hard security rules still apply).\n")
	p("catch_up            = false  # run the most recent missed trigger once after a restart\n")
	p("max_output          = 2000   # characters of the last run persisted for auditing\n")
	p("timeout             = 300    # maximum run duration in seconds\n")
	p("session_integration = false  # optionally continue the source logical session\n\n")

	p("[notifications]\n")
	p("preset      = \"balanced\"  # \"quiet\", \"balanced\", or \"verbose\"\n")
	p("include     = []\n")
	p("exclude     = []\n")
	p("throttle_ms = 750\n\n")
	p("# [notifications.channels.matrix]\n")
	p("# preset = \"quiet\"\n\n")

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
	p("# blocked_domains = []\n")
	p("# allow_private = false      # permit private search endpoints only in trusted environments\n")
	p("# allowed_domains = []       # optional host/domain allowlist\n\n")

	p("[web_fetch]\n")
	p("backend        = %q\n", fetchBackend)
	p("jina_api_key   = %q\n", jinaKey)
	p("crawl4ai_cmd   = %q\n", pythonCmd)
	p("max_characters = 15000\n\n")
	p("# Private/loopback/link-local targets are blocked by default.\n")
	p("allow_private  = false\n")
	p("# allowed_domains = []  # optional host/domain allowlist\n\n")

	p("[browser]\n")
	p("# Browser automation tools (browser_navigate, browser_click, etc.).\n")
	p("# backend = %q  # browser automation backend\n", config.BrowserBackendPlaywright)
	p("# headless    = true\n")
	p("# browser_type = %q  # Playwright browser engine\n", config.BrowserTypeChromium)
	p("# timeout     = 30           # seconds per browser action\n\n")
	p("# allow_private = false      # permit private targets only in trusted environments\n")
	p("# allowed_domains = []       # optional host/domain allowlist\n\n")

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
	p("forbidden_roots = []  # paths the agent can never access regardless of allowed_roots\n")
	p("forbid_rm       = %t  # true = block rm in bash entirely; agent must use system trash\n\n", forbidRM)

	p("[multimodal]\n")
	p("vision          = true        # false if your model does not support images\n")
	p("max_image_bytes = 10485760    # 10 MB\n")
	p("# [[multimodal.converters]]\n")
	p("# extensions = [\"pdf\"]\n")
	p("# command    = \"pdftotext {input} {output}\"\n\n")

	p("[server]\n")
	p("# HTTP API server (used by: ageage serve)\n")
	p("host = \"127.0.0.1\"\n")
	p("port = 8080\n")
	p("# api_key = \"\"              # optional Bearer token for /v1/*\n")
	p("# health_auth = false        # require the token for /health too\n")
	p("# cors_origins = []          # explicit browser origins; empty keeps local compatibility\n")
	p("# max_body_bytes = 4194304  # 4 MiB request limit\n")
	p("# max_concurrent = 8        # in-flight /v1 requests\n\n")

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

// startChannels registers and starts channel connectors based on config.
// Returns the manager (for shutdown) and count of registered channels.
// If no channels are enabled, returns nil manager and 0 count.
func startChannels(factory *agent.AgentFactory) (*channel.Manager, int, func(tools.CronEntry, string)) {
	cfg := factory.Config

	// Session manager — one per .ageage directory (shared across all chats).
	sm := agent.NewSessionManager(factory.Config.AgeAgeDirPath())
	// Persist active room/thread bindings separately from conversation history.
	// A malformed registry is never overwritten; the service falls back to the
	// deterministic legacy mapping for this process and reports the issue.
	sessionRegistry, registryErr := agent.OpenSessionRegistry(factory.Config.AgeAgeDirPath())
	if registryErr != nil {
		fmt.Printf("⚠️  Warning: session registry unavailable: %s\n", registryErr)
		sessionRegistry = nil
	}

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
	parseChatKey := func(chatKey string) (channelType, channelID, threadID string) {
		base := roomChatKey(chatKey)
		parts := strings.SplitN(base, ":", 2)
		if len(parts) == 2 {
			channelType, channelID = parts[0], parts[1]
		}
		if idx := strings.LastIndex(chatKey, ":t:"); idx >= 0 {
			threadID = chatKey[idx+3:]
		}
		return
	}
	bindChatSession := func(chatKey, sessionID string) error {
		if sessionRegistry == nil {
			return nil
		}
		channelType, channelID, threadID := parseChatKey(chatKey)
		kind := "room"
		if threadID != "" {
			kind = "thread"
		}
		if err := sessionRegistry.Bind(agent.SessionBinding{
			SessionID:   sessionID,
			ChannelType: channelType,
			ChannelID:   channelID,
			ThreadID:    threadID,
			Kind:        kind,
		}); err != nil {
			return err
		}
		return nil
	}

	// Restore durable room/thread attachments before handling messages. Stale
	// bindings whose session directory was removed are pruned, and duplicate
	// legacy bindings are reduced deterministically to one attachment.
	if sessionRegistry != nil {
		seenSessions := make(map[string]struct{})
		for _, binding := range sessionRegistry.List() {
			if binding.Kind == "cron" || strings.HasPrefix(binding.SessionID, "cron-") {
				// Older releases registered internal task-owned cron sessions here.
				// They have no external attachment and no cleanup lifecycle.
				_ = sessionRegistry.Unbind(binding.Key)
				continue
			}
			if binding.ChannelType == "" || binding.ChannelID == "" {
				_ = sessionRegistry.Unbind(binding.Key)
				continue
			}
			roomKey := binding.ChannelType + ":" + binding.ChannelID
			roomPrefix := agent.SanitizeSessionID(roomKey)
			if binding.SessionID != roomPrefix && !strings.HasPrefix(binding.SessionID, roomPrefix+"-") {
				_ = sessionRegistry.Unbind(binding.Key)
				continue
			}
			if _, err := os.Stat(sm.SessionDir(binding.SessionID)); os.IsNotExist(err) {
				_ = sessionRegistry.Unbind(binding.Key)
				continue
			}
			if _, duplicate := seenSessions[binding.SessionID]; duplicate {
				_ = sessionRegistry.Unbind(binding.Key)
				continue
			}
			chatKey := roomKey
			if binding.ThreadID != "" {
				chatKey += ":t:" + binding.ThreadID
			}
			activeSessions[chatKey] = binding.SessionID
			chatKeyBySessionID[binding.SessionID] = chatKey
			seenSessions[binding.SessionID] = struct{}{}
		}
	}

	// chatSessionID returns the active session ID for a chatKey, creating the
	// default session on first access. Must be called with agentMu held.
	// Thread chatKeys ("type:id:t:threadID") produce a session under the room's
	// prefix so all thread sessions are visible in the room's session list.
	chatSessionID := func(chatKey string) string {
		if id, ok := activeSessions[chatKey]; ok {
			return id
		}
		channelType, channelID, threadID := parseChatKey(chatKey)
		prefix := agent.SanitizeSessionID(roomChatKey(chatKey))
		id := prefix
		if threadID != "" {
			id += "-" + agent.SanitizeSessionID(threadID)
		}
		bindingKey := agent.BindingKey(channelType, channelID, threadID, "")
		if sessionRegistry != nil {
			if binding, ok := sessionRegistry.Get(bindingKey); ok &&
				(binding.SessionID == prefix || strings.HasPrefix(binding.SessionID, prefix+"-")) &&
				!strings.HasPrefix(binding.SessionID, "cron-") {
				// A persisted binding may be stale or point at another active
				// location. Do not create a second mutable Agent for it.
				ownedElsewhere := false
				for _, other := range sessionRegistry.GetForSession(binding.SessionID) {
					if other.Key != bindingKey {
						ownedElsewhere = true
						break
					}
				}
				if !ownedElsewhere {
					id = binding.SessionID
				}
			}
		}
		// Sanitization is not one-to-one, so two distinct external locations
		// can otherwise derive the same directory name. A stable key suffix
		// keeps those locations from ever sharing one mutable Agent instance.
		attachedElsewhere := false
		for otherKey, otherID := range activeSessions {
			if otherKey != chatKey && otherID == id {
				attachedElsewhere = true
				break
			}
		}
		if !attachedElsewhere && sessionRegistry != nil {
			for _, other := range sessionRegistry.GetForSession(id) {
				if other.Key != bindingKey {
					attachedElsewhere = true
					break
				}
			}
		}
		if attachedElsewhere {
			id += "-" + bindingKey[:12]
		}
		if err := sm.EnsureSession(id); err != nil {
			fmt.Printf("⚠️  Warning: could not initialise session %q: %s\n", id, err)
		}
		activeSessions[chatKey] = id
		chatKeyBySessionID[id] = chatKey
		if err := bindChatSession(chatKey, id); err != nil {
			fmt.Printf("⚠️  Warning: could not persist session binding: %s\n", err)
		}
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
		// Extract threadID (if any) so TodoSend / Notify can post inside the
		// originating thread instead of at the room's top level.
		threadID := ""
		if idx := strings.LastIndex(chatKey, ":t:"); idx >= 0 {
			threadID = chatKey[idx+3:]
		}

		ag := factory.CreateAgent(confirmMgr, "")
		ag.SetCommandPrefix(imCommandPrefix(channelType))
		ag.SetInteractionScope(tools.InteractionScope{
			ChannelType: channelType,
			ChannelID:   channelID,
			ThreadID:    threadID,
			SessionID:   sessionID,
		})
		ag.SessionDir = sm.SessionDir(sessionID)

		if ch, ok := channelsByType[channelType]; ok {
			if editable, ok := ch.(channel.Editable); ok {
				cID := channelID
				tID := threadID
				editChannelID := cID
				if channelType == config.ChannelDiscord && tID != "" {
					editChannelID = tID
				}
				threadEditable, hasThreadEditable := ch.(channel.ThreadEditable)
				ag.Callbacks.TodoSend = func(text string) string {
					var (
						msgID string
						err   error
					)
					if tID != "" && hasThreadEditable {
						msgID, err = threadEditable.SendMessageInThread(cID, tID, tID, text)
					} else {
						msgID, err = editable.SendMessage(cID, text)
					}
					if err != nil {
						fmt.Printf("[todo] send error: %s\n", err)
					}
					return msgID
				}
				ag.Callbacks.TodoEdit = func(msgID, text string) error {
					return editable.EditMessage(editChannelID, msgID, text)
				}
			}
		}
		ag.Callbacks.Notify = func(message string) {
			if managerPtr == nil || channelType == "" {
				return
			}
			// Inside a thread, keep notifications anchored to the thread.
			if threadID != "" {
				if ch, ok := channelsByType[channelType]; ok {
					if te, ok := ch.(channel.ThreadEditable); ok {
						_, _ = te.SendMessageInThread(channelID, threadID, threadID, message)
						return
					}
				}
			}
			managerPtr.Send(channelType, channelID, message)
		}
		ag.Callbacks.AskUser = func(question string, options []string) {
			if managerPtr != nil && channelType != "" {
				managerPtr.SendQuestion(channelType, channelID, question, options)
			}
		}
		ag.Callbacks.ToolStart = func(name, args string) {
			notifyFn := ag.Callbacks.Notify
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

	// handleSessionCmd processes channel-prefixed session sub-commands for a room.
	// roomKey is always "channelType:channelID" (no :t: suffix) — all sessions
	// in the room share this prefix. chatKey may include ":t:threadID" and is
	// used only to resolve the currently active session.
	sessionInUseLocked := func(sessionID, exceptChatKey string) bool {
		for key, activeID := range activeSessions {
			if key != exceptChatKey && activeID == sessionID {
				return true
			}
		}
		if sessionRegistry != nil {
			for _, binding := range sessionRegistry.GetForSession(sessionID) {
				channelType, channelID, threadID := parseChatKey(exceptChatKey)
				if binding.Key != agent.BindingKey(channelType, channelID, threadID, "") {
					return true
				}
			}
		}
		return false
	}

	handleSessionCmd := func(rKey, chatKey, rawInput, prefix string) string {
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
		case "": // <prefix>session — show current session
			infos, _ := sm.ListWithPrefix(defaultPrefix)
			cur := toDisplayName(currentSessionID)
			var sb strings.Builder
			fmt.Fprintf(&sb, "**Current session:** %s\n", cur)
			if len(infos) > 1 {
				fmt.Fprintf(&sb, "_Use %ssession list to see all sessions._", prefix)
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
			newFullID := ""
			if len(parts) >= 3 {
				name = strings.Join(parts[2:], "-")
				newFullID = toFullID(name)
				if err := sm.CreateSession(newFullID); err != nil {
					if _, statErr := os.Stat(sm.SessionDir(newFullID)); statErr == nil {
						return fmt.Sprintf("❌ Session '%s' already exists. Use %ssession switch %s.", name, prefix, name)
					}
					return fmt.Sprintf("❌ Failed to create session: %s", err)
				}
			} else {
				for n := 1; ; n++ {
					name = fmt.Sprintf("session-%d", n)
					newFullID = toFullID(name)
					if err := sm.CreateSession(newFullID); err == nil {
						break
					} else if _, statErr := os.Stat(sm.SessionDir(newFullID)); os.IsNotExist(statErr) {
						return fmt.Sprintf("❌ Failed to create session: %s", err)
					}
				}
			}

			agentMu.Lock()
			if err := bindChatSession(chatKey, newFullID); err != nil {
				agentMu.Unlock()
				_ = sm.Delete(newFullID)
				return fmt.Sprintf("❌ Failed to bind session: %s", err)
			}
			delete(agents, currentSessionID)
			delete(chatKeyBySessionID, currentSessionID)
			newAg := makeChatAgent(chatKey, newFullID)
			agents[newFullID] = newAg
			activeSessions[chatKey] = newFullID
			chatKeyBySessionID[newFullID] = chatKey
			agentMu.Unlock()
			return fmt.Sprintf("✅ Switched to new session **%s**.", toDisplayName(newFullID))

		case "switch":
			if len(parts) < 3 {
				return fmt.Sprintf("Usage: %ssession switch <name>", prefix)
			}
			name := strings.Join(parts[2:], "-")
			newFullID := toFullID(name)
			info, statErr := os.Stat(sm.SessionDir(newFullID))
			if statErr != nil || !info.IsDir() {
				if os.IsNotExist(statErr) || statErr == nil {
					return fmt.Sprintf("❌ Session '%s' does not exist. Use %ssession new %s to create it.", name, prefix, name)
				}
				return fmt.Sprintf("❌ Failed to inspect session '%s': %s", name, statErr)
			}
			agentMu.Lock()
			if sessionInUseLocked(newFullID, chatKey) {
				agentMu.Unlock()
				return fmt.Sprintf("❌ Session '%s' is active in another chat or thread; switch that session first.", name)
			}
			if err := bindChatSession(chatKey, newFullID); err != nil {
				agentMu.Unlock()
				return fmt.Sprintf("❌ Failed to bind session: %s", err)
			}
			_ = sm.EnsureSession(newFullID)
			delete(agents, currentSessionID)
			delete(agents, newFullID)
			delete(chatKeyBySessionID, currentSessionID)
			newAg := makeChatAgent(chatKey, newFullID)
			agents[newFullID] = newAg
			activeSessions[chatKey] = newFullID
			chatKeyBySessionID[newFullID] = chatKey
			agentMu.Unlock()
			return fmt.Sprintf("✅ Switched to session **%s**.", toDisplayName(newFullID))

		case "remove":
			if len(parts) < 3 {
				return fmt.Sprintf("Usage: %ssession remove <name>", prefix)
			}
			name := strings.Join(parts[2:], "-")
			delFullID := toFullID(name)
			if delFullID == currentSessionID {
				return "❌ Cannot remove the active session."
			}
			agentMu.Lock()
			if sessionInUseLocked(delFullID, "") {
				agentMu.Unlock()
				return "❌ Cannot remove a session active in another chat or thread."
			}
			delete(agents, delFullID)
			for key, activeID := range activeSessions {
				if activeID == delFullID {
					delete(activeSessions, key)
				}
			}
			delete(chatKeyBySessionID, delFullID)
			if sessionRegistry != nil {
				if err := sessionRegistry.RemoveSession(delFullID); err != nil {
					agentMu.Unlock()
					return fmt.Sprintf("❌ Failed to update session registry: %s", err)
				}
			}
			if err := sm.Trash(delFullID); err != nil {
				agentMu.Unlock()
				return fmt.Sprintf("❌ Failed to remove session: %s", err)
			}
			agentMu.Unlock()
			return fmt.Sprintf("🗑️ Removed session **%s**.", toDisplayName(delFullID))

		default:
			return fmt.Sprintf("Usage: %ssession [list|ls | new|n [name] | switch|sw <name> | remove|rm <name>]", prefix)
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
		cmd := parseIMCommand(msg.ChannelType, text)
		// Reuse the channel connector's inbound allowlist for confirmation
		// approval. The originating sender remains authorised even when the
		// allowlist is empty.
		var confirmationAllowlist []string
		switch msg.ChannelType {
		case config.ChannelTelegram:
			confirmationAllowlist = factory.Config.Channels.Telegram.AllowedUsers
		case config.ChannelDiscord:
			confirmationAllowlist = factory.Config.Channels.Discord.AllowedUsers
		case config.ChannelMatrix:
			confirmationAllowlist = factory.Config.Channels.Matrix.AllowedUsers
		}
		confirmMgr.SetAllowedRespondersForScope(tools.InteractionScope{
			ChannelType: msg.ChannelType,
			ChannelID:   msg.ChannelID,
		}, confirmationAllowlist...)

		// In group chats, only respond when the bot is @mentioned or replied to.
		// Confirmation replies and recognized channel commands are directed at the
		// bot by definition; unknown prefixed text still requires a mention.
		if msg.IsGroupChat && !msg.BotMentioned {
			if textLow != "y" && textLow != "n" && textLow != "a" && !isRecognizedIMCommand(cmd, factory.GetSkills()) {
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
			agentMu.Lock()
			pendingSession := activeSessions[chatKey]
			agentMu.Unlock()
			scope := tools.InteractionScope{
				ChannelType: msg.ChannelType,
				ChannelID:   msg.ChannelID,
				ThreadID:    msg.ThreadID,
				SessionID:   pendingSession,
				SenderID:    msg.SenderID,
			}
			if len(confirmMgr.GetPendingForScope(scope)) > 0 {
				allowed := (textLow == "y" || textLow == "a")
				if !confirmMgr.RespondForScope(scope, allowed) {
					return respond(msg, "❌ Confirmation could not be matched uniquely or you are not authorised.")
				}
				if !allowed {
					return respond(msg, "❌ Operation denied.")
				}
				return ""
			}
		}

		// stop and session abort bypass the per-chat mutex (which may be held by
		// the agent). Both are exact channel-aware command matches.
		if cmd.is("stop") && cmd.Args == "" || cmd.is("session") && strings.EqualFold(cmd.Args, "abort") {
			agentMu.Lock()
			if sessionID, ok := activeSessions[chatKey]; ok {
				if ag, ok := agents[sessionID]; ok {
					ag.Stop()
				}
			}
			react := activeReactions[chatKey]
			agentMu.Unlock()
			agentMu.Lock()
			pendingSession := activeSessions[chatKey]
			agentMu.Unlock()
			factory.UserInputMgr.CancelForScope(tools.InteractionScope{
				ChannelType: msg.ChannelType,
				ChannelID:   msg.ChannelID,
				ThreadID:    msg.ThreadID,
				SessionID:   pendingSession,
				SenderID:    msg.SenderID,
			})
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
		agentMu.Lock()
		pendingSession := activeSessions[chatKey]
		agentMu.Unlock()
		scope := tools.InteractionScope{
			ChannelType: msg.ChannelType,
			ChannelID:   msg.ChannelID,
			ThreadID:    msg.ThreadID,
			SessionID:   pendingSession,
			SenderID:    msg.SenderID,
		}
		if len(factory.UserInputMgr.GetPendingForScope(scope)) > 0 {
			if !factory.UserInputMgr.RespondForScope(scope, text) {
				return respond(msg, "❌ User input could not be matched uniquely or you are not authorised.")
			}
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

		// cred commands — never routed through the agent.
		// /cred set and /cred add are blocked in IM to prevent passwords appearing in chat logs.
		if cmd.is("cred") {
			return respond(msg, handleCredChanCmd(msg, factory.CredMgr, text, cmd.Prefix))
		}

		// sessions — list sessions for this room with matrix.to links.
		// Must come before the session command below.
		if cmd.is("sessions") && cmd.Args == "" {
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

		// session commands.
		if cmd.is("session") {
			parts := strings.Fields(text)
			sub := ""
			if len(parts) >= 2 {
				sub = strings.ToLower(parts[1])
			}
			// Normalize alias before any checks.
			if sub == "n" {
				sub = "new"
			}

			// No nesting: session new from within a thread is not allowed.
			if sub == "new" && msg.ThreadID != "" {
				return respond(msg, "❌ Cannot create a session from within a thread. Use the main chat window.")
			}

			// Matrix: session new in the main chat creates a thread-backed session.
			// The user's command event becomes the thread root; replies go inside it.
			// Session ID follows the room-prefix scheme: roomPrefix + "-" + sanitize(threadID).
			if sub == "new" && msg.ChannelType == config.ChannelMatrix && msg.ThreadID == "" {
				if msg.ReplyTo == "" {
					return respond(msg, "❌ Matrix did not provide a thread root for this command.")
				}
				threadChatKey := rKey + ":t:" + msg.ReplyTo
				roomPrefix := agent.SanitizeSessionID(rKey)
				newFullID := roomPrefix + "-" + agent.SanitizeSessionID(msg.ReplyTo)
				if err := sm.CreateSession(newFullID); err != nil {
					if _, statErr := os.Stat(sm.SessionDir(newFullID)); statErr == nil {
						return respond(msg, "❌ A session already exists for this thread.")
					}
					return respond(msg, fmt.Sprintf("❌ Failed to create session: %s", err))
				}
				if err := bindChatSession(threadChatKey, newFullID); err != nil {
					_ = sm.Delete(newFullID)
					return respond(msg, fmt.Sprintf("❌ Failed to bind session: %s", err))
				}
				agentMu.Lock()
				newAg := makeChatAgent(threadChatKey, newFullID)
				agents[newFullID] = newAg
				activeSessions[threadChatKey] = newFullID
				chatKeyBySessionID[newFullID] = threadChatKey
				agentMu.Unlock()

				if mx, ok := channelsByType[config.ChannelMatrix].(*channel.MatrixChannel); ok {
					_ = mx.SendInThread(msg.ChannelID, msg.ReplyTo, msg.ReplyTo, "✅ New session started. Continue in this thread.")
				}
				return ""
			}

			return respond(msg, handleSessionCmd(rKey, chatKey, text, cmd.Prefix))
		}

		// build [description] — create a skill or pipeline without entering the agent loop.
		// The planner runs isolated; the main conversation history is untouched.
		if cmd.is("build") {
			task := cmd.Args
			ch := channelsByType[msg.ChannelType]
			feedback := beginIMRunFeedback(ch, msg)
			buildFailed := true
			if feedback.reactionID != "" {
				agentMu.Lock()
				activeReactions[chatKey] = reactionInfo{msg.ChannelType, feedback.roomID, msg.ReplyTo, feedback.reactionID}
				agentMu.Unlock()
			}
			defer func() {
				feedback.Close(buildFailed)
				agentMu.Lock()
				delete(activeReactions, chatKey)
				agentMu.Unlock()
			}()
			_, sessionID := getAgent(chatKey)
			history, historyErr := sm.LoadHistory(sessionID)
			if historyErr != nil {
				return respond(msg, fmt.Sprintf("❌ Failed to load session history: %s", historyErr))
			}
			docsDir := filepath.Join(factory.Config.AgeAgeDirPath(), "docs")
			planner := agent.NewPlanner(factory, docsDir, factory.GetStandardToolNames())
			skill, buildErr := planner.CreateSkill(context.Background(), task, history)

			if buildErr != nil {
				return respond(msg, fmt.Sprintf("❌ Build failed: %s", buildErr))
			}
			buildFailed = false
			return respond(msg, fmt.Sprintf("✅ Built `%s` — use `%s%s` to activate.", skill.Name, cmd.Prefix, skill.CommandName()))
		}

		switch {
		case cmd.is("clear") && cmd.Args == "":
			ag, sessionID := getAgent(chatKey)
			if err := sm.WithSessionTransaction(sessionID, func(tx *agent.SessionTransaction) error {
				messages, err := tx.LoadHistory()
				if err != nil {
					return err
				}
				ag.SetMessages(messages)
				ag.ClearHistory()
				return tx.SaveHistory(ag.Messages())
			}); err != nil {
				return respond(msg, fmt.Sprintf("❌ Failed to clear history: %s", err))
			}
			return respond(msg, "🗑️ Conversation history cleared.")

		case cmd.is("summarize") && cmd.Args == "":
			ag, sessionID := getAgent(chatKey)
			var summary string
			err := sm.WithSessionTransaction(sessionID, func(tx *agent.SessionTransaction) error {
				messages, err := tx.LoadHistory()
				if err != nil {
					return err
				}
				ag.SetMessages(messages)
				summary, err = ag.ForceSummarize()
				if err != nil {
					return err
				}
				return tx.SaveHistory(ag.Messages())
			})
			if err != nil {
				return respond(msg, fmt.Sprintf("❌ %s", err))
			}
			return respond(msg, fmt.Sprintf("📋 Summary:\n%s", summary))

		case cmd.is("undo") && cmd.Args == "":
			ag, sessionID := getAgent(chatKey)
			n := 0
			if err := sm.WithSessionTransaction(sessionID, func(tx *agent.SessionTransaction) error {
				messages, err := tx.LoadHistory()
				if err != nil {
					return err
				}
				ag.SetMessages(messages)
				n = ag.RollbackLastTurn()
				if n == 0 {
					return nil
				}
				return tx.SaveHistory(ag.Messages())
			}); err != nil {
				return respond(msg, fmt.Sprintf("❌ Failed to undo: %s", err))
			}
			if n == 0 {
				return respond(msg, "Nothing to undo.")
			}
			return respond(msg, "↩️ Last turn undone.")

		case cmd.is("help") && cmd.Args == "":
			return respond(msg, "Available commands:\n"+
				fmt.Sprintf("%sclear — Clear conversation history (keeps session)\n", cmd.Prefix)+
				fmt.Sprintf("%sbuild [description] — Create a skill or pipeline (uses conversation context)\n", cmd.Prefix)+
				fmt.Sprintf("%sstop — Stop the current task\n", cmd.Prefix)+
				fmt.Sprintf("%ssummarize — Summarize conversation\n", cmd.Prefix)+
				fmt.Sprintf("%sundo — Remove the last turn from history\n", cmd.Prefix)+
				fmt.Sprintf("%sretry [text] — Re-run the last message (optionally modified)\n", cmd.Prefix)+
				fmt.Sprintf("%ssessions — List sessions for this room\n", cmd.Prefix)+
				fmt.Sprintf("%ssession list|ls — List sessions\n", cmd.Prefix)+
				fmt.Sprintf("%ssession new|n [name] — Start a new session\n", cmd.Prefix)+
				fmt.Sprintf("%ssession switch|sw <name> — Switch to a session\n", cmd.Prefix)+
				fmt.Sprintf("%ssession remove|rm <name> — Remove a session (moves to trash)\n", cmd.Prefix)+
				fmt.Sprintf("%scred list|ls — List stored credentials\n", cmd.Prefix)+
				fmt.Sprintf("%shelp — Show this help", cmd.Prefix))
		}

		// retry [modifier] — roll back the last turn and re-run with optional extra text.
		// Must NOT return here; fall through to the agent execution block below.
		retryRequested := cmd.is("retry")
		retryModifier := cmd.Args

		ch := channelsByType[msg.ChannelType]
		feedback := beginIMRunFeedback(ch, msg)
		runReporter := newIMProgressReporter(factory.Config.Notifications, ch, msg)
		runFailed := true
		if feedback.reactionID != "" {
			agentMu.Lock()
			activeReactions[chatKey] = reactionInfo{msg.ChannelType, feedback.roomID, msg.ReplyTo, feedback.reactionID}
			agentMu.Unlock()
		}
		defer func() {
			_ = runReporter.Close()
			feedback.Close(runFailed)
			agentMu.Lock()
			delete(activeReactions, chatKey)
			agentMu.Unlock()
		}()

		ag, sessionID := getAgent(chatKey)
		// Serialize the complete mutable Agent lifecycle per logical session.
		// The transaction keeps the lock through Agent.Run and history save;
		// use tx.SaveHistory below to avoid reacquiring the non-reentrant lock.
		tx, unlockSession := sm.BeginSessionTransaction(sessionID)
		defer unlockSession()
		persistedMessages, loadErr := tx.LoadHistory()
		if loadErr != nil {
			return respond(msg, fmt.Sprintf("Agent error: failed to load session history: %s", loadErr))
		}
		ag.SetMessages(persistedMessages)
		ag.SetInteractionScope(tools.InteractionScope{
			ChannelType: msg.ChannelType,
			ChannelID:   msg.ChannelID,
			ThreadID:    msg.ThreadID,
			SessionID:   sessionID,
			SenderID:    msg.SenderID,
		})

		askUser := func(question string, options []string) {
			var sb strings.Builder
			sb.WriteString("❓ ")
			sb.WriteString(question)
			for i, opt := range options {
				fmt.Fprintf(&sb, "\n%d. %s", i+1, opt)
			}
			if msg.Respond != nil {
				_ = msg.Respond(sb.String())
			} else if managerPtr != nil {
				managerPtr.SendQuestion(msg.ChannelType, msg.ChannelID, question, options)
			}
		}
		restoreCallbacks := installIMProgressCallbacks(ag, runReporter, askUser)
		defer restoreCallbacks()

		runText := msg.Text
		var retryParts []llm.ContentPart
		if retryRequested {
			lastMsg, ok := ag.LastTurnUserMessage()
			if !ok {
				runFailed = false
				return respond(msg, "Nothing to retry.")
			}
			runText = lastMsg.TextContent()
			retryParts = lastMsg.Parts
			if retryModifier != "" {
				runText += "\n\n" + retryModifier
			}
			ag.RollbackLastTurn()
		}
		// In group chats, prefix with a clean sender name so the agent can
		// distinguish participants. DMs are unlabelled (single user).
		if msg.IsGroupChat && msg.SenderID != "" && !retryRequested {
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
		if saveErr := tx.SaveHistory(ag.Messages()); saveErr != nil && err == nil {
			err = fmt.Errorf("save session history: %w", saveErr)
		}

		if err != nil {
			return respond(msg, fmt.Sprintf("Agent error: %s", err))
		}
		runFailed = false
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
			tg.AnswerCallback = func(channelID, threadID, senderID, answer string) {
				// Inline answers are bound to the active session in this room and
				// the callback sender; do not route by channel alone.
				agentMu.Lock()
				chatKey := config.ChannelTelegram + ":" + channelID
				if threadID != "" {
					chatKey += ":t:" + threadID
				}
				sessionID := activeSessions[chatKey]
				agentMu.Unlock()
				scope := tools.InteractionScope{
					ChannelType: config.ChannelTelegram,
					ChannelID:   channelID,
					ThreadID:    threadID,
					SessionID:   sessionID,
					SenderID:    senderID,
				}
				factory.UserInputMgr.RespondForScope(scope, answer)
			}
			manager.Register(tg)
			channelsByType[config.ChannelTelegram] = tg
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
			channelsByType[config.ChannelDiscord] = dc
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
			channelsByType[config.ChannelMatrix] = mx
			fmt.Println("  ✓  Matrix")
			registered++
		}
	}

	if registered == 0 {
		return nil, 0, nil
	}

	// cronDeliver posts cron outcomes to an Agent entry's structured owner
	// location, with the legacy flat delivery field retained for old/admin tasks.
	cronDeliver := func(e tools.CronEntry, summary string) {
		channelType, channelID, threadID, ok := cronDeliveryTarget(e)
		if !ok {
			if e.Delivery == "" && e.Owner.ChannelType == "" && e.Owner.ChannelID == "" {
				return
			}
			fmt.Printf("[cron] invalid delivery target %q\n", e.Delivery)
			return
		}
		if !cfg.Notifications.EffectiveForChannel(channelType).Allows(config.ProgressCron) {
			return
		}
		ch, ok := channelsByType[channelType]
		if !ok {
			fmt.Printf("[cron] no channel of type %q for delivery\n", channelType)
			return
		}
		msg := fmt.Sprintf("⏰ Cron **%s** ran:\n\n%s", e.ID, summary)
		if threadID != "" {
			if te, ok2 := ch.(channel.ThreadEditable); ok2 {
				if _, err := te.SendMessageInThread(channelID, threadID, threadID, msg); err != nil {
					fmt.Printf("[cron] delivery error: %s\n", err)
				}
				return
			}
		}
		ch.Send(channelID, msg)
	}

	return manager, registered, cronDeliver
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

	manager, registered, cronDeliver := startChannels(factory)
	if registered == 0 {
		return fmt.Errorf("no channels enabled. Enable at least one channel in [channels] config")
	}

	go factory.WatchSkills(watchCtx)
	go agent.NewCronScheduler(factory.CronStore, factory, cronDeliver).Run(watchCtx)

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

	// Start channel connectors in the background (also provides cron delivery).
	manager, chCount, cronDeliver := startChannels(factory)
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

	go factory.WatchSkills(watchCtx)
	go agent.NewCronScheduler(factory.CronStore, factory, cronDeliver).Run(watchCtx)

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
		factory.SecurityChecker.SetForbidRM(factory.Config.Security.ForbidRM)
		// Keep credentials protected even when the credential manager failed to
		// initialise or the CLI rebuilds the security checker for its workdir.
		factory.SecurityChecker.BlockFile(factory.Config.CredentialsPath())
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
	go agent.NewCronScheduler(factory.CronStore, factory, func(e tools.CronEntry, summary string) {
		fmt.Printf("\n  ⏰ Cron %s ran:\n%s\n\n", e.ID, summary)
	}).Run(watchCtx)

	// ── Session setup ─────────────────────────────────────────────────────────
	sm := agent.NewSessionManager(factory.Config.AgeAgeDirPath())
	sessionRegistry, registryErr := agent.OpenSessionRegistry(factory.Config.AgeAgeDirPath())
	if registryErr != nil {
		fmt.Fprintf(os.Stderr, "warning: session registry unavailable: %s\n", registryErr)
		sessionRegistry = nil
	}
	activeSessionID := "default"
	if err := sm.EnsureSession(activeSessionID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not initialise session: %s\n", err)
	}

	// createSessionAgent builds a fresh agent wired to a session directory.
	createSessionAgent := func(sessionID string) *agent.Agent {
		newAg := factory.CreateAgent(nil, "")
		newAg.SetInteractionScope(tools.InteractionScope{
			ChannelType: "cli",
			ChannelID:   "interactive",
			SessionID:   sessionID,
			SenderID:    "local",
		})
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

	// switchSession loads (or creates) the target and returns a fresh agent.
	// Mutations are already persisted under session transactions, so switching
	// must not write a potentially stale in-memory snapshot.
	switchSession := func(newID string) (*agent.Agent, error) {
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
	renameSession := func(oldID, newID string) error {
		if sessionRegistry != nil && len(sessionRegistry.GetForSession(oldID)) > 0 {
			return fmt.Errorf("session %q is active in an IM room or thread", oldID)
		}
		if factory.CronStore != nil {
			for _, entry := range factory.CronStore.List() {
				if entry.ContinueCurrentSession && entry.Owner.SessionID == oldID {
					return fmt.Errorf("session %q is used by scheduled task %q", oldID, entry.ID)
				}
			}
		}
		return sm.Rename(oldID, newID)
	}
	refreshCLIScope := func(ag *agent.Agent, sessionID string) {
		ag.SetInteractionScope(tools.InteractionScope{
			ChannelType: "cli",
			ChannelID:   "interactive",
			SessionID:   sessionID,
			SenderID:    "local",
		})
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
						if err := sm.CreateSession(newName); err != nil {
							ui.printErr(fmt.Sprintf("Failed to create session: %s", err))
							break
						}
					} else {
						for n := 1; ; n++ {
							newName = fmt.Sprintf("session-%d", n)
							if err := sm.CreateSession(newName); err == nil {
								break
							} else if _, statErr := os.Stat(sm.SessionDir(newName)); os.IsNotExist(statErr) {
								ui.printErr(fmt.Sprintf("Failed to create session: %s", err))
								newName = ""
								break
							}
						}
					}
					if newName == "" {
						break
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
						if err := renameSession(activeSessionID, newID); err != nil {
							ui.printErr(fmt.Sprintf("Rename failed: %s", err))
						} else {
							// Update the active session pointer and agent's session dir.
							activeSessionID = newID
							ag.SessionDir = sm.SessionDir(newID)
							refreshCLIScope(ag, newID)
							ui.printOK(fmt.Sprintf("Session renamed to '%s'.", newID))
						}
					default: // /session rename <old> <new>
						oldID := agent.SanitizeSessionID(parts[2])
						newID := agent.SanitizeSessionID(parts[3])
						if oldID == activeSessionID {
							// Renaming the active session: update pointer too.
							if err := renameSession(oldID, newID); err != nil {
								ui.printErr(fmt.Sprintf("Rename failed: %s", err))
							} else {
								activeSessionID = newID
								ag.SessionDir = sm.SessionDir(newID)
								refreshCLIScope(ag, newID)
								ui.printOK(fmt.Sprintf("Session renamed to '%s'.", newID))
							}
						} else {
							if err := renameSession(oldID, newID); err != nil {
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
						if sessionRegistry != nil && len(sessionRegistry.GetForSession(resolved)) > 0 {
							ui.printErr("Cannot delete a session active in an IM room or thread.")
							break
						}
						if factory.CronStore != nil {
							usedByCron := ""
							for _, entry := range factory.CronStore.List() {
								if entry.ContinueCurrentSession && entry.Owner.SessionID == resolved {
									usedByCron = entry.ID
									break
								}
							}
							if usedByCron != "" {
								ui.printErr(fmt.Sprintf("Cannot delete a session used by scheduled task %q.", usedByCron))
								break
							}
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
				n := 0
				err := sm.WithSessionTransaction(activeSessionID, func(tx *agent.SessionTransaction) error {
					messages, err := tx.LoadHistory()
					if err != nil {
						return err
					}
					ag.SetMessages(messages)
					n = ag.RollbackLastTurn()
					if n == 0 {
						return nil
					}
					return tx.SaveHistory(ag.Messages())
				})
				if err != nil {
					ui.printErr(fmt.Sprintf("Undo failed: %s", err))
					signalReady()
					continue
				}
				if n == 0 {
					ui.printWarn("Nothing to undo.")
				} else {
					ui.printOK("Last turn undone.")
				}
				signalReady()
				continue
			}

			// /build [description] — create a skill or pipeline without entering
			// the agent loop. The planner runs isolated; conversation history is
			// untouched.
			if lower == "/build" || strings.HasPrefix(lower, "/build ") {
				task := strings.TrimSpace(input[len("/build"):])
				ui.printStatus("Building skill/pipeline…")
				history, historyErr := sm.LoadHistory(activeSessionID)
				if historyErr != nil {
					ui.printErr(fmt.Sprintf("Build failed to load session history: %s", historyErr))
					signalReady()
					continue
				}
				docsDir := filepath.Join(factory.Config.AgeAgeDirPath(), "docs")
				planner := agent.NewPlanner(factory, docsDir, factory.GetStandardToolNames())
				skill, buildErr := planner.CreateSkill(context.Background(), task, history)
				if buildErr != nil {
					ui.printErr(fmt.Sprintf("Build failed: %s", buildErr))
				} else {
					ui.printOK(fmt.Sprintf("Built %s — use /%s to activate.", skill.Name, skill.CommandName()))
				}
				signalReady()
				continue
			}

			// /retry [modifier] — re-run last message, optionally with extra text.
			// directRun bypasses the switch so a retried message that happens to match
			// a slash command (e.g. user originally typed "/clear") is never intercepted.
			directRun := false
			retryRequested := false
			retryModifier := ""
			if lower == "/retry" || strings.HasPrefix(lower, "/retry ") {
				retryRequested = true
				retryModifier = strings.TrimSpace(input[len("/retry"):])
				directRun = true
			}

			shouldRun := directRun
			if !directRun {
				switch input {
				case "exit", "quit":
					_ = rl.SaveHistory(historyFile, 1000)
					ui.printInfo("Goodbye!")
					return nil

				case "/clear":
					err := sm.WithSessionTransaction(activeSessionID, func(tx *agent.SessionTransaction) error {
						messages, err := tx.LoadHistory()
						if err != nil {
							return err
						}
						ag.SetMessages(messages)
						ag.ClearHistory()
						return tx.SaveHistory(ag.Messages())
					})
					if err != nil {
						ui.printErr(fmt.Sprintf("Failed to clear history: %s", err))
					} else {
						ui.printOK("History cleared.")
					}
					signalReady()

				case "/stop":
					ui.printWarn("No task running.")
					signalReady()

				case "/summarize":
					ui.printStatus("Summarizing…")
					var summary string
					err := sm.WithSessionTransaction(activeSessionID, func(tx *agent.SessionTransaction) error {
						messages, err := tx.LoadHistory()
						if err != nil {
							return err
						}
						ag.SetMessages(messages)
						summary, err = ag.ForceSummarize()
						if err != nil {
							return err
						}
						return tx.SaveHistory(ag.Messages())
					})
					if err != nil {
						ui.printErr(err.Error())
					} else {
						fmt.Println()
						fmt.Println(summary)
						fmt.Println()
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
							{"/build [description]", "Create a skill or pipeline (uses conversation context)"},
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
				go func(text string, ps []llm.ContentPart, retry bool, modifier string, ch chan agentResult) {
					var buf strings.Builder
					thinkFilter.inner = func(token string) {
						buf.WriteString(token)
						approxTokens := buf.Len() / 4
						if approxTokens > 0 {
							spinner.Update(fmt.Sprintf("Writing… (~%d tokens)", approxTokens))
						}
					}
					tx, unlock := sm.BeginSessionTransaction(activeSessionID)
					messages, err := tx.LoadHistory()
					if err == nil {
						ag.SetMessages(messages)
						if retry {
							lastMsg, ok := ag.LastTurnUserMessage()
							if !ok {
								err = fmt.Errorf("nothing to retry")
							} else {
								text = lastMsg.TextContent()
								ps = lastMsg.Parts
								if modifier != "" {
									text += "\n\n" + modifier
								}
								ag.RollbackLastTurn()
							}
						}
					}
					result := ""
					if err == nil {
						result, err = ag.RunWithParts(context.Background(), text, ps, thinkFilter.Wrap())
						if saveErr := tx.SaveHistory(ag.Messages()); saveErr != nil && err == nil {
							err = fmt.Errorf("save session history: %w", saveErr)
						}
					}
					unlock()
					thinkFilter.Flush()
					spinner.Stop()
					ch <- agentResult{result, err}
				}(cleanText, parts, retryRequested, retryModifier, agentCh)
			}

		} else {
			select {
			case res := <-agentCh:
				agentCh = nil
				agentActive.Store(false)
				// Auto-rename auto-generated session names (session-N) to a slug
				// derived from the first user message so sessions are easy to identify.
				if isAutoSessionName(activeSessionID) && res.err == nil {
					if slug := firstMessageSlug(ag.Messages()); slug != "" && slug != activeSessionID {
						if err := renameSession(activeSessionID, slug); err == nil {
							ag.SessionDir = sm.SessionDir(slug)
							activeSessionID = slug
							refreshCLIScope(ag, slug)
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
					factory.UserInputMgr.CancelForScope(ag.GetInteractionScope())
					<-agentCh
					return nil
				}
				trimmed := strings.TrimSpace(input)
				if trimmed == "/stop" {
					ag.Stop()
					factory.UserInputMgr.CancelForScope(ag.GetInteractionScope())
					fmt.Println()
					ui.printWarn("Stop signal sent.")
				} else if len(factory.UserInputMgr.GetPendingForScope(ag.GetInteractionScope())) > 0 {
					// A pipeline node is waiting for user input — deliver the answer.
					factory.UserInputMgr.RespondForScope(ag.GetInteractionScope(), trimmed)
				}
			}
		}
	}

	_ = rl.SaveHistory(historyFile, 1000)
	return nil
}

// handleCredChanCmd processes channel-prefixed cred commands received from an
// IM channel. cred set and cred add are always rejected in IM to prevent passwords
// appearing in chat histories.
func handleCredChanCmd(_ channel.IncomingMessage, mgr *creds.Manager, rawInput string, prefixes ...string) string {
	prefix := "/"
	if len(prefixes) > 0 && prefixes[0] != "" {
		prefix = prefixes[0]
	}
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
			return fmt.Sprintf("Usage: %scred remove <name>", prefix)
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
		return fmt.Sprintf("Usage: %scred [list|ls | remove|rm <name> | reload]", prefix)

	default:
		if prefix != "/" {
			return fmt.Sprintf("Usage: %scred [list|ls | remove|rm <name> | reload]", prefix)
		}
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
	case config.ChannelMatrix:
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
		if isCronSessionID(exact.ID) {
			return "", fmt.Errorf("session %q is reserved for cron", query)
		}
		return exact.ID, nil
	}
	filtered := prefixMatches[:0]
	for _, match := range prefixMatches {
		if !isCronSessionID(match.ID) {
			filtered = append(filtered, match)
		}
	}
	prefixMatches = filtered
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

func isCronSessionID(id string) bool {
	return strings.HasPrefix(id, "cron-")
}

// printSessionList prints a formatted session list with dynamic column widths.
// activeID marks the currently active session with a ▶ indicator.
func printSessionList(infos []agent.SessionInfo, activeID string) {
	filtered := infos[:0]
	for _, info := range infos {
		if !isCronSessionID(info.ID) {
			filtered = append(filtered, info)
		}
	}
	infos = filtered
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
	go agent.NewCronScheduler(factory.CronStore, factory, nil).Run(watchCtx)

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

// --- ageage cron ---

// cronStoreFromCmd loads the config and opens the cron store for the CLI cron
// subcommands.
func cronServiceFromCmd(cmd *cobra.Command) (*tools.CronService, error) {
	configPath, _ := cmd.Flags().GetString("config")
	configPath = findConfigFile(configPath)
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, err
	}
	store := tools.NewCronStore(filepath.Join(cfg.ConfigDir(), "data", "cron.json"))
	service := tools.NewCronService(store, cfg.Cron.SessionIntegration)
	service.Timeout = time.Duration(cfg.Cron.Timeout) * time.Second
	service.MaxOutput = cfg.Cron.MaxOutput
	return service, nil
}

func runCronList(cmd *cobra.Command, args []string) error {
	service, err := cronServiceFromCmd(cmd)
	if err != nil {
		return err
	}
	entries := service.List(tools.AdminCronActor())
	if len(entries) == 0 {
		fmt.Println("No scheduled tasks.")
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Created < entries[j].Created })

	now := time.Now()
	for _, e := range entries {
		state := "enabled"
		if !e.Enabled {
			state = "paused"
		}
		status := e.LastStatus
		if status == "" {
			status = "never"
		}
		fmt.Printf("\n  ID:       %s  (%s)\n", e.ID, state)
		fmt.Printf("  Schedule: %s\n", e.Schedule)
		fmt.Printf("  Command:  %s\n", e.Command)
		fmt.Printf("  Status:   %s | runs: %d", status, e.RunCount)
		if e.LastRun != "" {
			fmt.Printf(" | last: %s", e.LastRun)
		}
		if next, ok := tools.NextRunTime(e.Schedule, now); ok {
			fmt.Printf(" | next: %s", next.Format(time.RFC3339))
		}
		fmt.Println()
		if e.Delivery != "" {
			fmt.Printf("  Delivery: %s\n", e.Delivery)
		}
		if e.LastError != "" {
			fmt.Printf("  Last err: %s\n", e.LastError)
		}
	}
	fmt.Println()
	return nil
}

func runCronAdd(cmd *cobra.Command, args []string) error {
	service, err := cronServiceFromCmd(cmd)
	if err != nil {
		return err
	}
	schedule, command := args[0], args[1]
	delivery, _ := cmd.Flags().GetString("delivery")
	if err := tools.ValidateCronExpr(schedule); err != nil {
		return err
	}
	entry, err := service.Add(tools.AdminCronActor(), schedule, command, delivery, true, false)
	if err != nil {
		return err
	}
	fmt.Printf("Added cron task:\n")
	fmt.Printf("  ID:       %s\n", entry.ID)
	fmt.Printf("  Schedule: %s\n", entry.Schedule)
	fmt.Printf("  Command:  %s\n", entry.Command)
	if next, ok := tools.NextRunTime(entry.Schedule, time.Now()); ok {
		fmt.Printf("  Next run: %s\n", next.Format(time.RFC3339))
	}
	if entry.Delivery != "" {
		fmt.Printf("  Delivery: %s\n", entry.Delivery)
	}
	return nil
}

func runCronRemove(cmd *cobra.Command, args []string) error {
	service, err := cronServiceFromCmd(cmd)
	if err != nil {
		return err
	}
	found, err := service.Remove(tools.AdminCronActor(), args[0])
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("cron task %s not found", args[0])
	}
	fmt.Printf("Removed cron task %s.\n", args[0])
	return nil
}

func runCronRun(cmd *cobra.Command, args []string) error {
	configPath, _ := cmd.Flags().GetString("config")
	configPath = findConfigFile(configPath)

	factory, err := agent.NewFactory(configPath, debugFlag)
	if err != nil {
		return err
	}
	if err := agent.EnsureAgeAgeDir(factory.Config.EffectiveWorkDir()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create .ageage directory: %s\n", err)
	}

	e, ok := factory.CronService.Get(tools.AdminCronActor(), args[0])
	if !ok {
		return fmt.Errorf("cron task %s not found", args[0])
	}

	result, err := factory.CronService.Run(context.Background(), tools.AdminCronActor(), e.ID)
	if err != nil {
		return fmt.Errorf("cron run failed: %w", err)
	}
	fmt.Println(result)
	return nil
}

func runCronPause(cmd *cobra.Command, args []string) error {
	return setCronEnabled(cmd, args[0], false, "paused")
}

func runCronResume(cmd *cobra.Command, args []string) error {
	return setCronEnabled(cmd, args[0], true, "resumed")
}

func setCronEnabled(cmd *cobra.Command, id string, enabled bool, verb string) error {
	service, err := cronServiceFromCmd(cmd)
	if err != nil {
		return err
	}
	found, err := service.SetEnabled(tools.AdminCronActor(), id, enabled)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("cron task %s not found", id)
	}
	fmt.Printf("Cron task %s %s.\n", id, verb)
	return nil
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
	info, err := os.Stat(configPath)
	if err != nil {
		return err
	}
	value := strings.TrimSpace(toolsLine)
	if before, after, ok := strings.Cut(value, "="); ok && strings.TrimSpace(before) == "tools" {
		value = strings.TrimSpace(after)
	} else if strings.HasPrefix(value, "#") {
		return fmt.Errorf("commented tool defaults cannot be written; use an empty allowlist")
	} else {
		return fmt.Errorf("invalid tools assignment")
	}
	change := configChange{section: "agent", key: "tools", value: value}
	replacement, err := applyTOMLChanges(data, []configChange{change})
	if err != nil {
		return err
	}
	if err := validateConfigBytes(replacement); err != nil {
		return err
	}
	return writeTOMLBatch(configPath, data, info.Mode().Perm(), replacement)
}

func runTools(cmd *cobra.Command, args []string) error {
	return runConfigTools(cmd, args)
}
