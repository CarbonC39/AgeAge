package agent

import (
	"slices"
	"testing"

	"ageage/config"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolCatalogIsCompleteUniqueAndSideEffectFree(t *testing.T) {
	options := ToolCatalog()
	if len(options) == 0 {
		t.Fatal("tool catalog is empty")
	}
	seen := make(map[string]bool, len(options))
	for _, option := range options {
		name := option.Descriptor.Name
		if name == "" || option.Descriptor.Description == "" || option.Descriptor.Metadata.Risk == "" {
			t.Fatalf("incomplete tool option: %#v", option)
		}
		if seen[name] {
			t.Fatalf("duplicate catalog tool %q", name)
		}
		seen[name] = true
		if option.Availability == ToolInternal && option.Configurable {
			t.Fatalf("internal tool %q is configurable", name)
		}
	}
	for _, required := range []string{"finish_task", "node_complete", "next_step"} {
		if !seen[required] {
			t.Fatalf("internal tool %q missing from catalog", required)
		}
	}
}

func TestSkillOnlyFactoriesAndCatalogCannotDrift(t *testing.T) {
	want := make(map[string]bool)
	for _, option := range ToolCatalog() {
		if option.Availability == ToolSkillOnly || option.Descriptor.Name == "next_step" {
			want[option.Descriptor.Name] = true
		}
	}
	if len(want) != len(skillOnlyToolFactories) {
		t.Fatalf("catalog skill tools=%v factories=%v", want, skillOnlyToolFactories)
	}
	for name := range skillOnlyToolFactories {
		if !want[name] {
			t.Fatalf("factory %q has no matching catalog entry", name)
		}
	}
}

func TestDefaultRuntimeRegistrationMatchesCatalog(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	factory := &AgentFactory{Config: cfg}
	ag := factory.CreateAgent(nil, "")

	actual := ag.registry.List()
	expected := []string{"finish_task"}
	for _, option := range ToolCatalog() {
		name := option.Descriptor.Name
		switch option.Availability {
		case ToolDefault:
			expected = append(expected, name)
			if !slices.Contains(actual, name) {
				t.Errorf("default catalog tool %q was not registered", name)
			}
		case ToolSkillOnly:
			if slices.Contains(actual, name) {
				t.Errorf("skill-only tool %q was registered by default", name)
			}
		}
	}
	if !slices.Contains(actual, "finish_task") {
		t.Fatal("mandatory finish_task was not registered")
	}
	if !slices.Equal(actual, expected) {
		t.Fatalf("runtime registration drifted from catalog\nactual:   %#v\nexpected: %#v", actual, expected)
	}
}

func TestCatalogDrivesConfiguredAndPlannerToolLists(t *testing.T) {
	configured := ConfigurableToolOptionsWithConfigured([]string{"grep", "offline_mcp_tool"})
	last := configured[len(configured)-1]
	if last.Descriptor.Name != "offline_mcp_tool" || last.Availability != ToolExternal || !last.Configurable {
		t.Fatalf("configured external option = %#v", last)
	}

	cfg := config.DefaultConfig()
	cfg.Agent.Tools = []string{"grep", "bash"}
	cfg.Agent.NonIncludeTools = []string{"bash"}
	factory := &AgentFactory{Config: cfg}
	if got := factory.GetStandardToolNames(); !slices.Equal(got, []string{"grep"}) {
		t.Fatalf("configured standard tools = %#v", got)
	}
}

func TestMCPDiscoveryOrderingIsDeterministic(t *testing.T) {
	servers := map[string]config.MCPServer{
		"zeta":  {},
		"alpha": {},
		"mid":   {},
	}
	if got, want := sortedMCPServerNames(servers), []string{"alpha", "mid", "zeta"}; !slices.Equal(got, want) {
		t.Fatalf("server order = %v, want %v", got, want)
	}

	sessions := map[string]*mcp.ClientSession{
		"zeta":  new(mcp.ClientSession),
		"alpha": new(mcp.ClientSession),
		"mid":   new(mcp.ClientSession),
	}
	orderedSessions := sortedMCPSessions(sessions)
	gotNames := make([]string, len(orderedSessions))
	for i, entry := range orderedSessions {
		gotNames[i] = entry.name
	}
	if want := []string{"alpha", "mid", "zeta"}; !slices.Equal(gotNames, want) {
		t.Fatalf("session order = %v, want %v", gotNames, want)
	}

	input := []*mcp.Tool{{Name: "zeta"}, {Name: "alpha"}, {Name: "mid"}}
	orderedTools := sortedMCPTools(input)
	gotNames = gotNames[:0]
	for _, tool := range orderedTools {
		gotNames = append(gotNames, tool.Name)
	}
	if want := []string{"alpha", "mid", "zeta"}; !slices.Equal(gotNames, want) {
		t.Fatalf("tool order = %v, want %v", gotNames, want)
	}
	if input[0].Name != "zeta" {
		t.Fatal("tool sorting mutated provider response")
	}
}
