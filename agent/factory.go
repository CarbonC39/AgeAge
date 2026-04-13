package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ageage/config"
	"ageage/llm"
	"ageage/security"
	"ageage/skills"
	"ageage/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// AgentFactory handles the creation of Agent instances, caching common resources.
type AgentFactory struct {
	Config          *config.Config
	LLMClient       *llm.Client
	SecurityChecker *security.Checker
	Debug           bool
	CronStore       *tools.CronStore
	UserInputMgr    *tools.UserInputManager       // Shared ask_user pending-input state
	HasMemories     bool
	MCPSessions     map[string]*mcp.ClientSession // Active MCP sessions
	InjectSoul      bool                          // Passed to each created agent; true in serve/connect, false in CLI
	OnAlwaysAllow   func(operation string)        // Optional: called when user selects "a" (always-allow) in CLI confirm

	mcpMu    sync.RWMutex // Bug Fix 2: Protect MCPSessions
	skillsMu sync.RWMutex
	Skills   []skills.Skill
}

// GetSkills returns the current skill list (thread-safe; supports hot reload).
func (f *AgentFactory) GetSkills() []skills.Skill {
	f.skillsMu.RLock()
	defer f.skillsMu.RUnlock()
	return f.Skills
}

// WatchSkills polls the skills directory every 2 s and hot-reloads when any
// .md file changes. Run in a goroutine; returns when ctx is cancelled.
func (f *AgentFactory) WatchSkills(ctx context.Context) {
	dir := f.Config.SkillsDir()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lastMod := f.latestSkillMod(dir)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mod := f.latestSkillMod(dir)
			if mod.After(lastMod) {
				lastMod = mod
				newSkills, err := skills.LoadSkills(dir)
				if err != nil {
					fmt.Printf("⚠️  Warning: skill reload failed: %s\n", err)
					continue
				}
				f.skillsMu.Lock()
				f.Skills = newSkills
				f.skillsMu.Unlock()
				fmt.Printf("🔄 Skills reloaded (%d skills)\n", len(newSkills))
			}
		}
	}
}

// latestSkillMod returns the most recent modification time of any skill file
// (.md, .yaml, .yml) in dir.
func (f *AgentFactory) latestSkillMod(dir string) time.Time {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}
	}
	var latest time.Time
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if e.IsDir() || (ext != ".md" && ext != ".yaml" && ext != ".yml") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest
}

// NewFactory initializes a new AgentFactory.
func NewFactory(configPath string, debug bool) (*AgentFactory, error) {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		return nil, err
	}

	if cfg.LLM.APIKey == "" {
		cfg.LLM.APIKey = os.Getenv("AGEAGE_API_KEY")
		if cfg.LLM.APIKey == "" {
			cfg.LLM.APIKey = os.Getenv("OPENAI_API_KEY")
		}
	}

	if cfg.LLM.APIKey == "" {
		return nil, fmt.Errorf("LLM API key not configured. Set it in config.toml or AGEAGE_API_KEY / OPENAI_API_KEY environment variable")
	}

	// Resolve optional search API keys from environment variables.
	cfg.WebSearch.ResolveSearchAPIKeys()

	clientLLM := llm.NewClient(cfg.LLM.APIKey, cfg.LLM.BaseURL, cfg.LLM.Model, debug, cfg.LLM.MaxTokens)

	sec := security.NewChecker(
		cfg.Workspace,
		cfg.Security.BlockedCommands,
		cfg.Security.AllowedRoots,
		cfg.Security.ForbiddenRoots,
	)

	// Load skills once.
	loadedSkills, err := skills.LoadSkills(cfg.SkillsDir())
	if err != nil {
		fmt.Printf("⚠️  Warning: failed to load skills: %s\n", err)
		loadedSkills = nil
	}

	// Initialize cron store.
	cronPath := filepath.Join(cfg.Workspace, "data", "cron.json")
	cronStore := tools.NewCronStore(cronPath)

	// Check if memories exist (once at startup).
	hasMemories := false
	if data, err := os.ReadFile(cfg.MemoryPath()); err == nil {
		trimmed := strings.TrimSpace(string(data))
		hasMemories = len(trimmed) > 0
	}

	// Initialize MCP Sessions.
	mcpSessions := make(map[string]*mcp.ClientSession)
	if cfg.MCP.Enabled {
		for name, srv := range cfg.MCP.Servers {
			if debug {
				fmt.Printf("🔌 Connecting to MCP Server: %s (%s %s)...\n", name, srv.Command, strings.Join(srv.Args, " "))
			}

			client := mcp.NewClient(&mcp.Implementation{
				Name:    "AgeAge",
				Version: "1.0.0",
			}, nil)

			// Use CommandTransport for external servers.
			cmd := exec.Command(srv.Command, srv.Args...)
			cmd.Env = os.Environ()
			for k, v := range srv.Env {
				cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
			}

			transport := &mcp.CommandTransport{
				Command: cmd,
			}

			session, err := client.Connect(context.Background(), transport, nil)
			if err != nil {
				fmt.Printf("⚠️  Warning: failed to connect to MCP server %s: %s\n", name, err)
				continue
			}

			mcpSessions[name] = session
		}
	}

	return &AgentFactory{
		Config:          cfg,
		Skills:          loadedSkills,
		LLMClient:       clientLLM,
		SecurityChecker: sec,
		Debug:           debug,
		CronStore:       cronStore,
		UserInputMgr:    tools.NewUserInputManager(),
		HasMemories:     hasMemories,
		MCPSessions:     mcpSessions,
	}, nil
}

// ensureMCPSessions checks existing MCP sessions and attempts to reconnect
// to any servers that are missing or unresponsive.
func (f *AgentFactory) ensureMCPSessions() {
	if !f.Config.MCP.Enabled {
		return
	}

	if f.MCPSessions == nil {
		f.MCPSessions = make(map[string]*mcp.ClientSession)
	}

	for name, srv := range f.Config.MCP.Servers {
		existing, ok := f.MCPSessions[name]
		if ok && existing != nil {
			// Basic connectivity check: try to list tools with a very short timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_, err := existing.ListTools(ctx, nil)
			cancel()
			if err == nil {
				continue // Session is healthy.
			}
			if f.Debug {
				fmt.Printf("⚠️  MCP server %s unresponsive, reconnecting: %v\n", name, err)
			}
		}

		// Reconnect.
		if f.Debug {
			fmt.Printf("🔌 (Re)connecting to MCP Server: %s (%s %s)...\n", name, srv.Command, strings.Join(srv.Args, " "))
		}

		client := mcp.NewClient(&mcp.Implementation{
			Name:    "AgeAge",
			Version: "1.0.0",
		}, nil)

		cmd := exec.Command(srv.Command, srv.Args...)
		cmd.Env = os.Environ()
		for k, v := range srv.Env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}

		transport := &mcp.CommandTransport{Command: cmd}
		session, err := client.Connect(context.Background(), transport, nil)
		if err != nil {
			if f.Debug {
				fmt.Printf("⚠️  Failed to (re)connect to MCP server %s: %s\n", name, err)
			}
			continue
		}
		f.MCPSessions[name] = session
	}
}

// CreateAgent instantiates a new Agent with fresh tools.
func (f *AgentFactory) CreateAgent(confirmMgr *tools.ConfirmationManager, channelID string) *Agent {
	return f.CreateAgentFiltered(confirmMgr, channelID, nil)
}

// CreateAgentFiltered instantiates a new Agent with a subset of tools if allowedTools is provided.
func (f *AgentFactory) CreateAgentFiltered(confirmMgr *tools.ConfirmationManager, channelID string, allowedTools []string) *Agent {
	f.ensureMCPSessions() // Bug Fix 6: Reconnect dead MCP servers.

	registry := tools.NewRegistry()

	finishTool := &tools.FinishTool{}
	registry.Register(finishTool)

	isSupervised := f.Config.Agent.Mode == "supervised"
	autoAllowAll := false

	var currentAgent *Agent

	confirmFunc := func(operation string) bool {
		if autoAllowAll {
			return true
		}

		// If we have a confirmation manager and a channel ID, use async confirmation.
		if confirmMgr != nil && currentAgent != nil && currentAgent.GetChannelID() != "" {
			ag := currentAgent
			cid := ag.GetChannelID()

			prompt := fmt.Sprintf("*Agent Confirmation Required*\nOperation: `%s`\nReply with `y`, `n`, or `a` (always allow).", operation)
			if ag.NotifyFunc != nil {
				ag.NotifyFunc(prompt)
			} else {
				fmt.Printf("\n[IM Confirmation] %s\n", prompt)
			}

			_, resultCh := confirmMgr.RequestConfirmation(operation, cid, 10*time.Minute)
			allowed, ok := <-resultCh
			if !ok {
				return false // Timeout
			}
			return allowed
		}

		// Fallback to CLI confirmation.
		fmt.Printf("\n  ⚠  %-10s %s\n", "Confirm", operation)
		fmt.Print("  Allow? y / n / a (always) ▸ ")
		reader := bufio.NewReader(os.Stdin)
		input, _ := reader.ReadString('\n')
		choice := strings.TrimSpace(strings.ToLower(input))
		switch choice {
		case "y":
			return true
		case "a":
			autoAllowAll = true
			if f.OnAlwaysAllow != nil {
				f.OnAlwaysAllow(operation)
			}
			fmt.Println("  ✓  Auto-allow enabled for this session.")
			return true
		default:
			fmt.Println("  ⊘  Operation denied.")
			return false
		}
	}


	// Helper function to check if a tool should be registered
	shouldRegisterTool := func(toolName string) bool {
		if f.Config.ShouldExcludeTool(toolName) {
			return false
		}
		if allowedTools == nil {
			return true
		}
		for _, t := range allowedTools {
			if t == toolName {
				return true
			}
		}
		return false
	}

	bashTool := &tools.BashTool{
		Security:          f.SecurityChecker,
		Timeout:           30 * time.Second,
		Supervised:        isSupervised,
		AutoAllowCommands: f.Config.Bash.AutoAllowCommands,
		ConfirmFunc:       confirmFunc,
	}
	if shouldRegisterTool("bash") {
		registry.Register(bashTool)
	}

	if shouldRegisterTool("file_read") {
		registry.Register(&tools.FileReadTool{Security: f.SecurityChecker})
	}

	// fileConfirmFunc auto-approves writes/edits targeting any session's CONTEXT.md.
	// Operation string formats:
	//   FileWriteTool: "Write N bytes to `<abs-path>`"
	//   FileEditTool:  "Edit file `<abs-path>` (replace text)"
	// Both contain the absolute path as a distinct segment, so checking for the
	// canonical sub-path "/.ageage/sessions/<id>/CONTEXT.md" is sufficient.
	fileConfirmFunc := func(operation string) bool {
		slashed := filepath.ToSlash(operation)
		// Match any session CONTEXT.md: */.ageage/sessions/<id>/CONTEXT.md
		if strings.Contains(slashed, "/.ageage/sessions/") && strings.Contains(slashed, "/CONTEXT.md") {
			return true
		}
		return confirmFunc(operation)
	}

	if shouldRegisterTool("file_write") {
		registry.Register(&tools.FileWriteTool{
			Security:    f.SecurityChecker,
			Supervised:  isSupervised,
			ConfirmFunc: fileConfirmFunc,
		})
	}
	if shouldRegisterTool("file_edit") {
		registry.Register(&tools.FileEditTool{
			Security:    f.SecurityChecker,
			Supervised:  isSupervised,
			ConfirmFunc: fileConfirmFunc,
		})
	}

	// Memory tools.
	memoryPath := f.Config.MemoryPath()

	if shouldRegisterTool("memory_store") {
		registry.Register(&tools.MemoryStoreTool{
			MemoryPath:  memoryPath,
			Supervised:  isSupervised,
			ConfirmFunc: confirmFunc,
		})
	}

	if f.HasMemories {
		if shouldRegisterTool("memory_recall") {
			registry.Register(&tools.MemoryRecallTool{MemoryPath: memoryPath})
		}
		if shouldRegisterTool("memory_forget") {
			registry.Register(&tools.MemoryForgetTool{
				MemoryPath:  memoryPath,
				Supervised:  isSupervised,
				ConfirmFunc: confirmFunc,
			})
		}
	}

	// Web tools.
	if shouldRegisterTool("web_fetch") {
		registry.Register(&tools.WebFetchTool{Cfg: &f.Config.WebFetch})
	}
	if shouldRegisterTool("web_search") {
		registry.Register(&tools.WebSearchTool{Cfg: &f.Config.WebSearch})
	}

	// Cron tools.
	if shouldRegisterTool("cron_add") {
		registry.Register(&tools.CronAddTool{
			Store:       f.CronStore,
			Supervised:  isSupervised,
			ConfirmFunc: confirmFunc,
		})
	}
	if shouldRegisterTool("cron_remove") {
		registry.Register(&tools.CronRemoveTool{
			Store:       f.CronStore,
			Supervised:  isSupervised,
			ConfirmFunc: confirmFunc,
		})
	}
	if shouldRegisterTool("cron_list") {
		registry.Register(&tools.CronListTool{Store: f.CronStore})
	}

	// Delegation tool.
	if shouldRegisterTool("delegate") {
		registry.Register(&DelegateTool{factory: f, registry: registry})
	}

	// MCP Tools (external).
	for _, mcpSession := range f.MCPSessions {
		resp, err := mcpSession.ListTools(context.Background(), &mcp.ListToolsParams{})
		if err != nil {
			if f.Debug {
				fmt.Printf("⚠️  Warning: failed to list MCP tools: %s\n", err)
			}
			continue
		}

		for _, tInfo := range resp.Tools {
			if shouldRegisterTool(tInfo.Name) {
				registry.Register(&tools.MCPTool{
					Session: mcpSession,
					Tool:    tInfo,
				})
			}
		}
	}

	ag := NewAgent(f.Config, f.LLMClient, registry, finishTool, f.Skills, f.Debug)
	ag.factory = f
	ag.InjectSoul = f.InjectSoul
	ag.ConfirmationMgr = confirmMgr
	if channelID != "" {
		ag.SetChannelID(channelID)
	}

	// Router is only useful for the main agent. Sub-agents (allowedTools != nil)
	// always set IsSubAgent=true and the router is guarded by !IsSubAgent, so
	// creating it would waste the AGENT.md read and allocate memory for nothing.
	if allowedTools == nil && f.Config.Router.Enabled {
		ag.router = NewRouter(f.Config, f.LLMClient, f.GetSkills(), f.Debug)
		ag.router.InjectSoul = ag.InjectSoul
	}

	// If allowedTools is provided, also register any skill-only tools requested.
	// For the main agent (allowedTools=nil), these are injected turn-by-turn in Run().
	for _, name := range allowedTools {
		if mkTool, ok := skillOnlyToolFactories[name]; ok {
			if _, exists := registry.Get(name); !exists {
				registry.Register(mkTool(f, registry, ag))
			}
		}
	}

	currentAgent = ag
	return ag
}
