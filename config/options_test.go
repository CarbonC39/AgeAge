package config

import (
	"reflect"
	"testing"
)

func assertUniqueChoices(t *testing.T, choices []Choice) {
	t.Helper()
	seen := make(map[string]bool, len(choices))
	for _, choice := range choices {
		if choice.Value == "" || choice.Label == "" || choice.Description == "" {
			t.Fatalf("incomplete choice: %#v", choice)
		}
		if seen[choice.Value] {
			t.Fatalf("duplicate choice value %q", choice.Value)
		}
		seen[choice.Value] = true
	}
}

func backendChoicesAsChoices(options []BackendChoice) []Choice {
	choices := make([]Choice, len(options))
	for i, option := range options {
		choices[i] = option.Choice
	}
	return choices
}

func TestOwnedConfigurationChoicesAreCompleteAndIndependent(t *testing.T) {
	for name, choices := range map[string][]Choice{
		"agent modes":             AgentModeChoices(),
		"channels":                ChannelTypeChoices(),
		"browser types":           BrowserTypeChoices(),
		"notification presets":    ProgressPresetChoices(),
		"notification categories": ProgressCategoryChoices(),
		"search backends":         backendChoicesAsChoices(WebSearchBackendChoices()),
		"fetch backends":          backendChoicesAsChoices(WebFetchBackendChoices()),
		"browser backends":        backendChoicesAsChoices(BrowserBackendChoices()),
	} {
		t.Run(name, func(t *testing.T) { assertUniqueChoices(t, choices) })
	}

	search := WebSearchBackendChoices()
	search[0].Label = "changed"
	if len(search[1].Fields) > 0 {
		search[1].Fields[0].Label = "changed"
	}
	again := WebSearchBackendChoices()
	if again[0].Label == "changed" || (len(again[1].Fields) > 0 && again[1].Fields[0].Label == "changed") {
		t.Fatal("backend choice provider exposed mutable backing data")
	}
}

func TestOwnedChoicesCoverRuntimeDefaultsAndSuggestions(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Agent.Mode != AgentModeSupervised || cfg.WebSearch.Backend != WebSearchBackendDuckDuckGo ||
		cfg.WebFetch.Backend != WebFetchBackendNative || cfg.Browser.Backend != BrowserBackendPlaywright ||
		cfg.Browser.BrowserType != BrowserTypeChromium {
		t.Fatalf("defaults do not use owned constants: %#v", cfg)
	}

	tests := []struct {
		url, base, model, strong string
	}{
		{"https://api.anthropic.com/v1", "base", "claude-haiku-4-5", "claude-opus-4-7"},
		{"https://api.deepseek.com/v1", "base", "deepseek-chat", "deepseek-reasoner"},
		{"https://generativelanguage.googleapis.com/v1beta/openai", "base", "gemini-3.5-flash", "gemini-3.1-pro"},
		{"http://localhost:11434/v1", "custom", "llama3.3", "custom"},
		{"https://custom.example/v1", "gpt-4o-mini-local", "gpt-4o-mini", "gpt-4o"},
	}
	for _, test := range tests {
		if got := SuggestModel(test.url); got != test.model {
			t.Errorf("SuggestModel(%q) = %q, want %q", test.url, got, test.model)
		}
		if got := SuggestStrongModel(test.url, test.base); got != test.strong {
			t.Errorf("SuggestStrongModel(%q, %q) = %q, want %q", test.url, test.base, got, test.strong)
		}
	}

	profiles := LLMProviderProfiles()
	if len(profiles) < 2 || profiles[0].BaseURL == "" || profiles[0].DefaultModel == "" {
		t.Fatalf("provider profiles = %#v", profiles)
	}
	profiles[0].APIKeyEnv[0] = "changed"
	if LLMProviderProfiles()[0].APIKeyEnv[0] == "changed" {
		t.Fatal("provider profiles exposed mutable backing data")
	}
}

func TestSetupProviderOrderingAndFieldsStayOwned(t *testing.T) {
	choiceValues := func(options []BackendChoice) []string {
		values := make([]string, len(options))
		for i, option := range options {
			values[i] = option.Value
			for _, field := range option.Fields {
				if field.ConfigKey == "" || field.Label == "" {
					t.Fatalf("%s has incomplete field metadata: %#v", option.Value, field)
				}
			}
		}
		return values
	}

	if got, want := choiceValues(WebSearchBackendChoices()), []string{
		WebSearchBackendDuckDuckGo, WebSearchBackendBrave, WebSearchBackendTavily, WebSearchBackendSearXNG,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("search provider order = %v, want %v", got, want)
	}
	if got, want := choiceValues(WebFetchBackendChoices()), []string{
		WebFetchBackendNative, WebFetchBackendJina, WebFetchBackendCrawl4AI,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fetch provider order = %v, want %v", got, want)
	}

	profiles := LLMProviderProfiles()
	if got, want := profiles[0].BaseURL, DefaultLLMBaseURL; got != want {
		t.Fatalf("default provider URL = %q, want %q", got, want)
	}
	if got, want := profiles[0].DefaultModel, DefaultLLMModel; got != want {
		t.Fatalf("default provider model = %q, want %q", got, want)
	}
	if got, want := APIKeyEnvironmentNames(), []string{
		"AGEAGE_API_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "DEEPSEEK_API_KEY", "GEMINI_API_KEY",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("API key environment precedence = %v, want %v", got, want)
	}
}
