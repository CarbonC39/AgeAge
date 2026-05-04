package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration for AgeAge.
type Config struct {
	Workspace string          `toml:"workspace"` // Working content directory (where the agent reads/writes files)
	WorkDir   string          `toml:"-"`          // Effective working dir for file ops; defaults to Workspace, overridden in CLI mode
	configDir string          // Dir containing config.toml; AgeAge data (AGENT.md, memories, skills) lives here
	LLM       LLMConfig       `toml:"llm"`
	Agent     AgentConfig     `toml:"agent"`
	SubAgent  SubAgentConfig  `toml:"subagent"`
	Pipeline  PipelineConfig  `toml:"pipeline"`
	Router    RouterConfig    `toml:"router"`
	Summarize SummarizeConfig `toml:"summarize"`
	Security  SecurityConfig  `toml:"security"`
	Bash      BashConfig      `toml:"bash"`
	WebSearch WebSearchConfig `toml:"web_search"`
	WebFetch  WebFetchConfig  `toml:"web_fetch"`
	Browser     BrowserConfig     `toml:"browser"`
	Multimodal  MultimodalConfig  `toml:"multimodal"`
	MCP         MCPConfig         `toml:"mcp"`
	Channels  ChannelConfig   `toml:"channels"`
	Server    ServerConfig    `toml:"server"`
	Eval      EvalConfig      `toml:"eval"`
}

// PipelineModels maps pipeline node model tiers to specific model configs.
// Takes precedence over [router] model settings when set.
type PipelineModels struct {
	Base   ModelConfig `toml:"base"`   // Model for tier=base nodes
	Medium ModelConfig `toml:"medium"` // Model for tier=medium nodes
	Strong ModelConfig `toml:"strong"` // Model for tier=strong nodes
}

// PipelineConfig holds settings for pipeline execution.
type PipelineConfig struct {
	ForeachConcurrency int            `toml:"foreach_concurrency"` // Max parallel foreach iterations; 0 or 1 = sequential
	Models             PipelineModels `toml:"models"`              // Per-complexity model overrides
}

// SubAgentConfig holds settings for sub-agents.
type SubAgentConfig struct {
	MaxIterations int         `toml:"max_iterations"` // Maximum iterations for sub-agent
	Timeout       int         `toml:"timeout"`        // Timeout in seconds for sub-agent
	Model         ModelConfig `toml:"model"`          // Optional independent model for sub-agent
}

// EvalConfig holds settings for the Evaluator quality-check system.
type EvalConfig struct {
	// SuccessThreshold is the number of consecutive evaluator passes before an
	// auto-generated skill graduates and evaluation stops. 0 means always evaluate.
	SuccessThreshold int         `toml:"success_threshold"`
	// Model is used for evaluation when success_count >= 1 (cheaper tier).
	// Defaults to [router.medium] when empty.
	Model            ModelConfig `toml:"model"`
}

// MCPConfig holds MCP client/server settings.
type MCPConfig struct {
	Enabled bool               `toml:"enabled"`
	Servers map[string]MCPServer `toml:"servers"` // External MCP servers to connect to
}

// MCPServer defines an external MCP server connection.
type MCPServer struct {
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
}

// LLMConfig holds settings for the LLM provider.
type LLMConfig struct {
	APIKey      string  `toml:"api_key"`
	BaseURL     string  `toml:"base_url"`
	Model       string  `toml:"model"`
	Temperature float64 `toml:"temperature"`
	MaxTokens   int     `toml:"max_tokens"` // Maximum tokens in the response; -1 = no limit
}

// AgentConfig holds agent behavior settings.
type AgentConfig struct {
	MaxIterations    int      `toml:"max_iterations"`
	Mode             string   `toml:"mode"`               // "full" or "supervised"
	NonIncludeTools  []string `toml:"non_include_tools"`  // Tools that should never be injected
	MaxParallelTools int      `toml:"max_parallel_tools"` // 0 or 1 = sequential; >1 = parallel tool execution
	Tools            []string `toml:"tools"`              // If non-empty, only these tools are registered by default (positive allowlist)
}

// ModelConfig holds specific settings for a model (possibly from a different provider).
type ModelConfig struct {
	Model   string `toml:"model"`
	APIKey  string `toml:"api_key"`
	BaseURL string `toml:"base_url"`
}

// RouterConfig holds settings for the intent router.
type RouterConfig struct {
	Enabled          bool        `toml:"enabled"`
	ClassifierModel  ModelConfig `toml:"classifier"` // Lightweight model for intent classification
	MediumModel      ModelConfig `toml:"medium"`      // Medium model for linear tasks
	StrongModel      ModelConfig `toml:"strong"`      // Strong model for complex tasks
	MaxHistory       int         `toml:"max_history"`
}

// Resolve returns the model, API key and BaseURL, falling back to defaults if empty.
func (m *ModelConfig) Resolve(defaultModel, defaultKey, defaultURL string) (string, string, string) {
	model := m.Model
	if model == "" {
		model = defaultModel
	}
	key := m.APIKey
	if key == "" {
		key = defaultKey
	}
	url := m.BaseURL
	if url == "" {
		url = defaultURL
	}
	return model, key, url
}

// SummarizeConfig holds settings for context summarization.
type SummarizeConfig struct {
	Enabled    bool   `toml:"enabled"`
	Model      string `toml:"model"`       // Model to use for summarization
	Threshold  int    `toml:"threshold"`   // Number of message pairs before summarizing
	KeepRecent int    `toml:"keep_recent"` // Number of recent messages to keep intact
}

// SecurityConfig holds security-related settings.
type SecurityConfig struct {
	BlockedCommands []string `toml:"blocked_commands"`
	AllowedRoots    []string `toml:"allowed_roots"`
	ForbiddenRoots  []string `toml:"forbidden_roots"`
}

// BashConfig holds bash tool settings.
type BashConfig struct {
	AutoAllowCommands    []string `toml:"auto_allow_commands"`     // Commands that skip supervised confirmation
	MaxOutputBytes       int      `toml:"max_output_bytes"`        // Cap on stdout+stderr combined (default 4 MB); prevents OOM on large outputs
	PassthroughEnvVars   []string `toml:"passthrough_env_vars"`    // Additional env var names/prefixes forwarded to subprocesses
}

// WebSearchConfig holds web search tool settings.
type WebSearchConfig struct {
	Backend          string   `toml:"backend"`         // "duckduckgo", "searxng", "tavily", or "brave"
	SearXNGURL       string   `toml:"searxng_url"`     // SearXNG instance URL
	TavilyAPIKey     string   `toml:"tavily_api_key"`  // Tavily Search API key; falls back to TAVILY_API_KEY env var
	BraveAPIKey      string   `toml:"brave_api_key"`   // Brave Search API key; falls back to BRAVE_API_KEY env var
	MaxSearchResults int      `toml:"max_results"`     // Maximum number of search results to return
	BlockedDomains   []string `toml:"blocked_domains"` // List of domains to exclude from search results
}

// ResolveSearchAPIKeys fills empty API key fields from environment variables.
// Called after LoadConfig so config-file values always take precedence.
func (c *WebSearchConfig) ResolveSearchAPIKeys() {
	if c.TavilyAPIKey == "" {
		c.TavilyAPIKey = os.Getenv("TAVILY_API_KEY")
	}
	if c.BraveAPIKey == "" {
		c.BraveAPIKey = os.Getenv("BRAVE_API_KEY")
	}
}

// WebFetchConfig holds web fetch tool settings.
type WebFetchConfig struct {
	Backend       string `toml:"backend"`        // "native", "jina", or "crawl4ai"
	JinaAPIKey    string `toml:"jina_api_key"`   // Jina Reader API key (optional)
	Crawl4AICmd   string `toml:"crawl4ai_cmd"`   // Python command for Crawl4AI (e.g., "python" or "python3")
	MaxCharacters int    `toml:"max_characters"` // Maximum characters to return for native backend
}

// ConverterConfig defines a command-line tool that converts a file format to plain text.
type ConverterConfig struct {
	Extensions []string `toml:"extensions"` // file extensions this converter handles, e.g. ["pdf","docx"]
	Command    string   `toml:"command"`     // command template: {input} → input file, {output} → output .md file
}

// MultimodalConfig holds settings for file attachment processing.
type MultimodalConfig struct {
	Vision        bool              `toml:"vision"`          // true = LLM accepts image parts; false = extract text instead
	MaxImageBytes int64             `toml:"max_image_bytes"` // max size for image attachments (default 10 MB)
	Converters    []ConverterConfig `toml:"converters"`      // converters for non-image document formats
}

// FindConverter returns the first ConverterConfig that handles the given extension (without dot, lowercase).
// Returns a pointer to a copy so callers cannot mutate the config slice element.
func (c *Config) FindConverter(ext string) *ConverterConfig {
	for i := range c.Multimodal.Converters {
		for _, e := range c.Multimodal.Converters[i].Extensions {
			if strings.EqualFold(e, ext) {
				conv := c.Multimodal.Converters[i]
				return &conv
			}
		}
	}
	return nil
}

// BrowserConfig holds browser automation tool settings.
type BrowserConfig struct {
	Backend     string `toml:"backend"`      // "playwright" or "agent-browser"
	Headless    bool   `toml:"headless"`     // Run browser in headless mode (default true)
	BrowserType string `toml:"browser_type"` // "chromium", "firefox", or "webkit" (playwright only)
	AgentBin    string `toml:"agent_bin"`    // Path to agent-browser binary (default "agent-browser")
	Timeout     int    `toml:"timeout"`      // Seconds per browser action (default 30)
}

// ChannelConfig holds settings for IM channel connectors.
type ChannelConfig struct {
	Parallel bool           `toml:"parallel"` // Process messages concurrently
	Telegram TelegramConfig `toml:"telegram"`
	Discord  DiscordConfig  `toml:"discord"`
	Matrix   MatrixConfig   `toml:"matrix"`
}

// TelegramConfig holds Telegram Bot settings.
type TelegramConfig struct {
	Enabled      bool     `toml:"enabled"`
	BotToken     string   `toml:"bot_token"`
	AllowedUsers []string `toml:"allowed_users"` // Telegram user IDs (as strings) that may use the bot; empty = allow all
}

// DiscordConfig holds Discord Bot settings.
type DiscordConfig struct {
	Enabled      bool     `toml:"enabled"`
	BotToken     string   `toml:"bot_token"`
	ChannelIDs   []string `toml:"channel_ids"`   // Discord channel IDs to monitor (required for REST polling)
	AllowedUsers []string `toml:"allowed_users"` // Discord user IDs that may use the bot; empty = allow all
}

// MatrixConfig holds Matrix client settings.
type MatrixConfig struct {
	Enabled      bool     `toml:"enabled"`
	Homeserver   string   `toml:"homeserver"`    // e.g., "https://matrix.org"
	UserID       string   `toml:"user_id"`       // e.g., "@bot:matrix.org"
	AccessToken  string   `toml:"access_token"`
	RoomIDs      []string `toml:"room_ids"`      // Rooms to monitor; empty = all joined rooms
	AllowedUsers []string `toml:"allowed_users"` // Matrix user IDs (e.g. "@alice:matrix.org"); empty = allow all
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host string `toml:"host"`
	Port int    `toml:"port"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Workspace: ".",
		LLM: LLMConfig{
			BaseURL:     "https://api.openai.com/v1",
			Model:       "gpt-4o-mini",
			Temperature: 0.7,
			MaxTokens:   8192,
		},
		Agent: AgentConfig{
			MaxIterations:   20,
			Mode:            "supervised",
			NonIncludeTools: []string{},
		},
		SubAgent: SubAgentConfig{
			MaxIterations: 10,
			Timeout:       300,
		},
		Security: SecurityConfig{
			BlockedCommands: []string{
				"rm -rf /",
				"rm -rf /*",
				"mkfs",
				"dd if=",
				":(){ :|:& };:",
				"> /dev/sda",
				"chmod -R 777 /",
			},
		},
		Router: RouterConfig{
			Enabled:         false,
			ClassifierModel: ModelConfig{}, // inherits base LLM model
			MediumModel:     ModelConfig{}, // inherits base LLM model
			StrongModel:     ModelConfig{}, // inherits base LLM model
			MaxHistory:      8,
		},
		Summarize: SummarizeConfig{
			Enabled:    false,
			Model:      "", // inherits base LLM model
			Threshold:  10,
			KeepRecent: 4,
		},
		Bash: BashConfig{
			AutoAllowCommands: []string{},
			MaxOutputBytes:    4 * 1024 * 1024, // 4 MB
		},
		WebSearch: WebSearchConfig{
			Backend:          "duckduckgo",
			MaxSearchResults: 10,
			BlockedDomains:   []string{},
		},
		WebFetch: WebFetchConfig{
			Backend:       "native",
			Crawl4AICmd:   "python",
			MaxCharacters: 15000,
		},
		Browser: BrowserConfig{
			Backend:     "playwright",
			Headless:    true,
			BrowserType: "chromium",
			AgentBin:    "agent-browser",
			Timeout:     30,
		},
		Multimodal: MultimodalConfig{
			Vision:        true,
			MaxImageBytes: 10 * 1024 * 1024, // 10 MB
			Converters:    []ConverterConfig{},
		},
		Server: ServerConfig{
			Host: "127.0.0.1",
			Port: 8080,
		},
		Eval: EvalConfig{
			SuccessThreshold: 3,
		},
	}
}

// LoadConfig reads a TOML config file and merges it with defaults.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}

	// configDir = directory containing config.toml.
	// All AgeAge data (AGENT.md, memories, skills) lives here.
	absPath, _ := filepath.Abs(path)
	cfg.configDir = filepath.Dir(absPath)

	// Resolve workspace (file-ops working dir) relative to configDir.
	if !filepath.IsAbs(cfg.Workspace) {
		cfg.Workspace = filepath.Join(cfg.configDir, cfg.Workspace)
	}
	cfg.Workspace, _ = filepath.Abs(cfg.Workspace)
	cfg.WorkDir = cfg.Workspace // default; CLI mode overrides this to cwd

	return cfg, nil
}

// ConfigDir returns the directory containing config.toml.
// AgeAge data (AGENT.md, SOUL.md, memories, skills) all live here.
func (c *Config) ConfigDir() string {
	return c.configDir
}

// EnsureDirs creates runtime directories under configDir.
// Call after LoadConfig, not inside it, to keep the loader side-effect-free.
func (c *Config) EnsureDirs() error {
	dataDir := filepath.Join(c.configDir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("failed to create data directory: %w", err)
	}
	return nil
}

// SOULPath returns the path to the SOUL.md file.
func (c *Config) SOULPath() string {
	return filepath.Join(c.configDir, "data", "SOUL.md")
}

// AgentPath returns the path to the AGENT.md file.
func (c *Config) AgentPath() string {
	return filepath.Join(c.configDir, "data", "AGENT.md")
}

// EffectiveWorkDir returns the working directory for file operations.
// In CLI mode this is the launch directory; in serve/connect it is Workspace.
func (c *Config) EffectiveWorkDir() string {
	if c.WorkDir != "" {
		return c.WorkDir
	}
	return c.Workspace
}

// SkillsDir returns the path to the skills directory.
func (c *Config) SkillsDir() string {
	return filepath.Join(c.configDir, "skills")
}

// MemoryPath returns the path to the MEMORY.jsonl file.
func (c *Config) MemoryPath() string {
	return filepath.Join(c.configDir, "data", "MEMORY.jsonl")
}

// AgeAgeDirPath returns the .ageage directory path within the effective work dir.
func (c *Config) AgeAgeDirPath() string {
	return filepath.Join(c.EffectiveWorkDir(), ".ageage")
}

// ContextMDPath returns the path to .ageage/CONTEXT.md.
func (c *Config) ContextMDPath() string {
	return filepath.Join(c.AgeAgeDirPath(), "CONTEXT.md")
}

// CredentialsPath returns the path to credentials.toml, stored alongside config.toml.
func (c *Config) CredentialsPath() string {
	return filepath.Join(c.configDir, "credentials.toml")
}

// WorkspaceSettingsPath returns the path to .ageage/settings.json.
func (c *Config) WorkspaceSettingsPath() string {
	return filepath.Join(c.AgeAgeDirPath(), "settings.json")
}

// ShouldExcludeTool checks if a tool should be excluded based on non_include_tools config.
func (c *Config) ShouldExcludeTool(toolName string) bool {
	toolNameLower := strings.ToLower(toolName)
	for _, excludePatterns := range c.Agent.NonIncludeTools {
		patternLower := strings.ToLower(strings.TrimSpace(excludePatterns))
		if patternLower == "" {
			continue
		}
		// Exact match
		if toolNameLower == patternLower {
			return true
		}
		// Prefix match (e.g., "memory_" excludes all memory tools)
		if strings.HasPrefix(toolNameLower, patternLower) {
			return true
		}
	}
	return false
}
