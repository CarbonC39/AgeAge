package agent

import (
	"slices"
	"time"

	"ageage/tools"
)

// ToolAvailability describes how the Agent makes a tool available. It belongs
// to the Agent layer because it is lifecycle policy, not an execution property
// of the tool itself.
type ToolAvailability string

const (
	ToolDefault   ToolAvailability = "default"
	ToolSkillOnly ToolAvailability = "skill_only"
	ToolInternal  ToolAvailability = "internal"
	ToolExternal  ToolAvailability = "external"
)

// ToolOption is the owner-provided catalog entry consumed by validation,
// planning, and administrative UIs.
type ToolOption struct {
	Descriptor   tools.ToolDescriptor
	Availability ToolAvailability
	Configurable bool
}

type toolCatalogSpec struct {
	prototype    func() tools.Tool
	runtime      func(toolBuildContext) tools.Tool
	availability ToolAvailability
	configurable bool
}

type toolBuildContext struct {
	factory      *AgentFactory
	registry     *tools.Registry
	supervised   bool
	confirm      func(string) bool
	confirmFile  func(string) bool
	currentAgent func() *Agent
}

var coreToolCatalog = []toolCatalogSpec{
	{
		prototype: func() tools.Tool { return &tools.BashTool{} },
		runtime: func(c toolBuildContext) tools.Tool {
			return &tools.BashTool{
				Security:           c.factory.SecurityChecker,
				Timeout:            30 * time.Second,
				Supervised:         c.supervised,
				AutoAllowCommands:  c.factory.Config.Bash.AutoAllowCommands,
				MaxOutputBytes:     c.factory.Config.Bash.MaxOutputBytes,
				WorkDir:            c.factory.Config.EffectiveWorkDir(),
				PassthroughEnvVars: c.factory.Config.Bash.PassthroughEnvVars,
				ConfirmFunc:        c.confirm,
				RedactFunc: func(text string) string {
					if c.factory.CredMgr == nil {
						return text
					}
					return c.factory.CredMgr.Scrub(text)
				},
			}
		},
		availability: ToolDefault, configurable: true,
	},
	{prototype: func() tools.Tool { return &tools.FileReadTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.FileReadTool{Security: c.factory.SecurityChecker, DocsDir: c.factory.DocsDir}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.FileWriteTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.FileWriteTool{Security: c.factory.SecurityChecker, Supervised: c.supervised, ConfirmFunc: c.confirmFile}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.FileEditTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.FileEditTool{Security: c.factory.SecurityChecker, Supervised: c.supervised, ConfirmFunc: c.confirmFile}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.MemoryStoreTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.MemoryStoreTool{MemoryPath: c.factory.Config.MemoryPath(), Supervised: c.supervised, ConfirmFunc: c.confirm}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.MemoryRecallTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.MemoryRecallTool{MemoryPath: c.factory.Config.MemoryPath()}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.MemoryForgetTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.MemoryForgetTool{MemoryPath: c.factory.Config.MemoryPath(), Supervised: c.supervised, ConfirmFunc: c.confirm}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.WebFetchTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.WebFetchTool{Cfg: &c.factory.Config.WebFetch}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.WebSearchTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.WebSearchTool{Cfg: &c.factory.Config.WebSearch}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.CronAddTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.CronAddTool{Store: c.factory.CronStore, Service: c.factory.CronService,
			ScopeFunc:          func() tools.InteractionScope { return c.currentAgent().GetInteractionScope() },
			SessionIntegration: c.factory.Config.Cron.SessionIntegration, Supervised: c.supervised, ConfirmFunc: c.confirm}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.CronRemoveTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.CronRemoveTool{Store: c.factory.CronStore, Service: c.factory.CronService,
			ScopeFunc:  func() tools.InteractionScope { return c.currentAgent().GetInteractionScope() },
			Supervised: c.supervised, ConfirmFunc: c.confirm}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.CronListTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.CronListTool{Store: c.factory.CronStore, Service: c.factory.CronService,
			ScopeFunc: func() tools.InteractionScope { return c.currentAgent().GetInteractionScope() }}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.CronRunTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.CronRunTool{Store: c.factory.CronStore, Service: c.factory.CronService,
			ScopeFunc: func() tools.InteractionScope { return c.currentAgent().GetInteractionScope() }}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.CronPauseTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.CronPauseTool{Store: c.factory.CronStore, Service: c.factory.CronService,
			ScopeFunc: func() tools.InteractionScope { return c.currentAgent().GetInteractionScope() }}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.CronResumeTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &tools.CronResumeTool{Store: c.factory.CronStore, Service: c.factory.CronService,
			ScopeFunc: func() tools.InteractionScope { return c.currentAgent().GetInteractionScope() }}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &DelegateTool{} }, runtime: func(c toolBuildContext) tools.Tool {
		return &DelegateTool{factory: c.factory, registry: c.registry}
	}, availability: ToolDefault, configurable: true},
	{prototype: func() tools.Tool { return &tools.FinishTool{} }, availability: ToolInternal},
	{prototype: func() tools.Tool { return &NodeCompleteTool{} }, availability: ToolInternal},
}

func descriptorFromPrototype(prototype func() tools.Tool) tools.ToolDescriptor {
	t := prototype()
	metadata := tools.DefaultToolMetadata()
	if provider, ok := t.(tools.MetadataProvider); ok {
		metadata = provider.Metadata()
		if metadata.Risk == "" {
			metadata.Risk = tools.RiskMedium
		}
	}
	return tools.ToolDescriptor{
		Name:        t.Name(),
		Description: t.Description(),
		Metadata:    metadata,
	}
}

func optionFromSpec(spec toolCatalogSpec) ToolOption {
	return ToolOption{
		Descriptor:   descriptorFromPrototype(spec.prototype),
		Availability: spec.availability,
		Configurable: spec.configurable,
	}
}

// ToolCatalog returns a deterministic copy of every Agent-owned tool option.
// Prototypes are side-effect free: rendering this catalog never initializes an
// LLM, browser, MCP connection, scheduler, or backing file.
func ToolCatalog() []ToolOption {
	options := make([]ToolOption, 0, len(coreToolCatalog)+len(skillOnlyToolSpecs))
	for _, spec := range coreToolCatalog {
		options = append(options, optionFromSpec(spec))
	}
	for _, spec := range skillOnlyToolSpecs {
		options = append(options, optionFromSpec(spec.catalog))
	}
	return options
}

// ConfigurableToolOptions returns only tools that may appear in agent.tools.
func ConfigurableToolOptions() []ToolOption {
	all := ToolCatalog()
	options := make([]ToolOption, 0, len(all))
	for _, option := range all {
		if option.Configurable {
			options = append(options, option)
		}
	}
	return options
}

// ConfigurableToolOptionsWithConfigured preserves names from an existing
// allowlist that are not currently available in the built-in catalog (for
// example an MCP tool whose server is offline). This is a pure catalog merge;
// it never connects to an external provider.
func ConfigurableToolOptionsWithConfigured(configured []string) []ToolOption {
	options := ConfigurableToolOptions()
	known := make([]string, 0, len(options))
	for _, option := range options {
		known = append(known, option.Descriptor.Name)
	}
	for _, name := range configured {
		if name == "" || slices.Contains(known, name) {
			continue
		}
		options = append(options, ToolOption{
			Descriptor: tools.ToolDescriptor{
				Name:        name,
				Description: "Configured external or currently unavailable tool",
				Metadata:    tools.DefaultToolMetadata(),
			},
			Availability: ToolExternal,
			Configurable: true,
		})
		known = append(known, name)
	}
	return options
}

func knownToolNames() []string {
	options := ToolCatalog()
	names := make([]string, 0, len(options))
	for _, option := range options {
		names = append(names, option.Descriptor.Name)
	}
	return names
}
