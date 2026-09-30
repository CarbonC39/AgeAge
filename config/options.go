package config

import (
	"slices"
	"strings"
)

const (
	DefaultLLMBaseURL          = "https://api.openai.com/v1"
	DefaultLLMModel            = "gpt-4o-mini"
	DefaultCrawl4AICommand     = "python"
	DefaultAgentBrowserCommand = "agent-browser"

	AgentModeSupervised = "supervised"
	AgentModeFull       = "full"

	ChannelTelegram = "telegram"
	ChannelDiscord  = "discord"
	ChannelMatrix   = "matrix"

	WebSearchBackendDuckDuckGo = "duckduckgo"
	WebSearchBackendBrave      = "brave"
	WebSearchBackendTavily     = "tavily"
	WebSearchBackendSearXNG    = "searxng"

	WebFetchBackendNative   = "native"
	WebFetchBackendJina     = "jina"
	WebFetchBackendCrawl4AI = "crawl4ai"

	BrowserBackendPlaywright   = "playwright"
	BrowserBackendAgentBrowser = "agent-browser"

	BrowserTypeChromium = "chromium"
	BrowserTypeFirefox  = "firefox"
	BrowserTypeWebKit   = "webkit"

	ConfigKeySearXNGURL   = "searxng_url"
	ConfigKeyTavilyAPIKey = "tavily_api_key"
	ConfigKeyBraveAPIKey  = "brave_api_key"
	ConfigKeyJinaAPIKey   = "jina_api_key"
	ConfigKeyCrawl4AICmd  = "crawl4ai_cmd"
	ConfigKeyAgentBin     = "agent_bin"
)

// Choice is display metadata for a stable configuration value. Callers render
// these values but the owning config package defines their meaning.
type Choice struct {
	Value       string
	Label       string
	Description string
}

// BackendField describes a backend-specific follow-up value without exposing
// a general form schema.
type BackendField struct {
	ConfigKey    string
	Label        string
	DefaultValue string
	Secret       bool
	Optional     bool
}

// BackendChoice augments a stable backend choice with its owned follow-up
// fields. Returned slices are copies and are safe for callers to modify.
type BackendChoice struct {
	Choice
	Fields []BackendField
}

// LLMProviderProfile owns setup examples and model suggestions for a known
// OpenAI-compatible provider. Custom endpoints remain valid and use fallback
// suggestions.
type LLMProviderProfile struct {
	Name         string
	BaseURL      string
	Note         string
	DefaultModel string
	StrongModel  string
	APIKeyEnv    []string
	URLMarkers   []string
}

var agentModeChoices = []Choice{
	{Value: AgentModeSupervised, Label: "supervised", Description: "pause for confirmation before every tool call"},
	{Value: AgentModeFull, Label: "full", Description: "fully autonomous, no confirmation prompts"},
}

var channelTypeChoices = []Choice{
	{Value: ChannelTelegram, Label: "Telegram", Description: "Telegram Bot API channel."},
	{Value: ChannelDiscord, Label: "Discord", Description: "Discord channel connector."},
	{Value: ChannelMatrix, Label: "Matrix", Description: "Matrix room connector."},
}

var webSearchBackendChoices = []BackendChoice{
	{Choice: Choice{Value: WebSearchBackendDuckDuckGo, Label: "DuckDuckGo", Description: "no API key, works immediately"}},
	{Choice: Choice{Value: WebSearchBackendBrave, Label: "Brave", Description: "higher quality results (Brave Search API key required)"}, Fields: []BackendField{{ConfigKey: ConfigKeyBraveAPIKey, Label: "Brave Search API Key", Secret: true}}},
	{Choice: Choice{Value: WebSearchBackendTavily, Label: "Tavily", Description: "optimized for LLM agents (Tavily API key required)"}, Fields: []BackendField{{ConfigKey: ConfigKeyTavilyAPIKey, Label: "Tavily API Key", Secret: true}}},
	{Choice: Choice{Value: WebSearchBackendSearXNG, Label: "SearXNG", Description: "self-hosted, privacy-friendly"}, Fields: []BackendField{{ConfigKey: ConfigKeySearXNGURL, Label: "SearXNG instance URL", DefaultValue: "http://localhost:8888"}}},
}

var webFetchBackendChoices = []BackendChoice{
	{Choice: Choice{Value: WebFetchBackendNative, Label: "Native", Description: "built-in Go HTTP client, no setup"}},
	{Choice: Choice{Value: WebFetchBackendJina, Label: "Jina", Description: "cleaner extraction; optional API key for higher rate limits"}, Fields: []BackendField{{ConfigKey: ConfigKeyJinaAPIKey, Label: "Jina API Key", Secret: true, Optional: true}}},
	{Choice: Choice{Value: WebFetchBackendCrawl4AI, Label: "Crawl4AI", Description: "best content quality; requires Python + crawl4ai package"}, Fields: []BackendField{{ConfigKey: ConfigKeyCrawl4AICmd, Label: "Python command", DefaultValue: DefaultCrawl4AICommand}}},
}

var browserBackendChoices = []BackendChoice{
	{Choice: Choice{Value: BrowserBackendPlaywright, Label: "Playwright", Description: "Built-in Playwright browser automation."}},
	{Choice: Choice{Value: BrowserBackendAgentBrowser, Label: "Agent Browser", Description: "Use the external agent-browser CLI."}, Fields: []BackendField{{ConfigKey: ConfigKeyAgentBin, Label: "Agent Browser command", DefaultValue: DefaultAgentBrowserCommand}}},
}

var browserTypeChoices = []Choice{
	{Value: BrowserTypeChromium, Label: "Chromium", Description: "Chromium browser engine."},
	{Value: BrowserTypeFirefox, Label: "Firefox", Description: "Firefox browser engine."},
	{Value: BrowserTypeWebKit, Label: "WebKit", Description: "WebKit browser engine."},
}

var llmProviderProfiles = []LLMProviderProfile{
	{Name: "OpenAI", BaseURL: DefaultLLMBaseURL, DefaultModel: DefaultLLMModel, StrongModel: "gpt-4o", APIKeyEnv: []string{"OPENAI_API_KEY"}, URLMarkers: []string{"openai.com"}},
	{Name: "Anthropic", BaseURL: "https://api.anthropic.com/v1", DefaultModel: "claude-haiku-4-5", StrongModel: "claude-opus-4-7", APIKeyEnv: []string{"ANTHROPIC_API_KEY"}, URLMarkers: []string{"anthropic"}},
	{Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1", DefaultModel: "deepseek-chat", StrongModel: "deepseek-reasoner", APIKeyEnv: []string{"DEEPSEEK_API_KEY"}, URLMarkers: []string{"deepseek"}},
	{Name: "Gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", DefaultModel: "gemini-3.5-flash", StrongModel: "gemini-3.1-pro", APIKeyEnv: []string{"GEMINI_API_KEY"}, URLMarkers: []string{"generativelanguage", "gemini"}},
	{Name: "Mistral", BaseURL: "https://api.mistral.ai/v1", DefaultModel: "mistral-small-latest", StrongModel: "mistral-large-latest", URLMarkers: []string{"mistral"}},
	{Name: "Ollama", BaseURL: "http://localhost:11434/v1", Note: "no API key needed", DefaultModel: "llama3.3", URLMarkers: []string{"11434"}},
}

func cloneChoices(source []Choice) []Choice { return append([]Choice(nil), source...) }

func cloneBackendChoices(source []BackendChoice) []BackendChoice {
	result := make([]BackendChoice, len(source))
	for i, option := range source {
		result[i] = option
		result[i].Fields = append([]BackendField(nil), option.Fields...)
	}
	return result
}

func AgentModeChoices() []Choice               { return cloneChoices(agentModeChoices) }
func ChannelTypeChoices() []Choice             { return cloneChoices(channelTypeChoices) }
func WebSearchBackendChoices() []BackendChoice { return cloneBackendChoices(webSearchBackendChoices) }
func WebFetchBackendChoices() []BackendChoice  { return cloneBackendChoices(webFetchBackendChoices) }
func BrowserBackendChoices() []BackendChoice   { return cloneBackendChoices(browserBackendChoices) }
func BrowserTypeChoices() []Choice             { return cloneChoices(browserTypeChoices) }

func LLMProviderProfiles() []LLMProviderProfile {
	result := make([]LLMProviderProfile, len(llmProviderProfiles))
	for i, profile := range llmProviderProfiles {
		result[i] = profile
		result[i].APIKeyEnv = append([]string(nil), profile.APIKeyEnv...)
		result[i].URLMarkers = append([]string(nil), profile.URLMarkers...)
	}
	return result
}

// APIKeyEnvironmentNames returns the precedence order used by setup when it
// discovers a provider key in the environment.
func APIKeyEnvironmentNames() []string {
	names := []string{"AGEAGE_API_KEY"}
	for _, profile := range llmProviderProfiles {
		for _, name := range profile.APIKeyEnv {
			if name != "" && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

func profileForURL(baseURL string) (LLMProviderProfile, bool) {
	baseURL = strings.ToLower(baseURL)
	for _, profile := range llmProviderProfiles[1:] {
		for _, marker := range profile.URLMarkers {
			if strings.Contains(baseURL, marker) {
				return profile, true
			}
		}
	}
	return LLMProviderProfile{}, false
}

func SuggestModel(baseURL string) string {
	if profile, ok := profileForURL(baseURL); ok {
		return profile.DefaultModel
	}
	return llmProviderProfiles[0].DefaultModel
}

func SuggestStrongModel(baseURL, baseModel string) string {
	if profile, ok := profileForURL(baseURL); ok && profile.StrongModel != "" {
		return profile.StrongModel
	}
	if strings.Contains(strings.ToLower(baseModel), DefaultLLMModel) {
		return llmProviderProfiles[0].StrongModel
	}
	return baseModel
}
