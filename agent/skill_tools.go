package agent

// skillOnlyToolFactories maps tool names to factory functions for tools that
// require special initialization. These tools are registered in three cases:
//  1. Per-turn injection when a matched skill declares them in required_tools.
//  2. At factory time when listed in agent.tools config (global allowlist).
//  3. At factory time when listed in the sub-agent allowedTools parameter.
//
// Factory signature: func(factory *AgentFactory, registry *tools.Registry) tools.Tool
// Both parameters may be used by tools that need sub-agent creation or config access.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ageage/config"
	"ageage/llm"
	"ageage/tools"
)

// skillOnlyToolFactories is the registry of all skill-only tools.
// Factory signature: func(factory, registry, agent) Tool
// The agent pointer is provided so tools can read/write per-run agent state
// (e.g. todoStore) and access NotifyFunc.
var skillOnlyToolFactories = map[string]func(*AgentFactory, *tools.Registry, *Agent) tools.Tool{
	"grep": func(f *AgentFactory, _ *tools.Registry, _ *Agent) tools.Tool {
		return &tools.GrepTool{Security: f.SecurityChecker}
	},
	"glob": func(f *AgentFactory, _ *tools.Registry, _ *Agent) tools.Tool {
		return &tools.GlobTool{Security: f.SecurityChecker, Workspace: f.Config.EffectiveWorkDir()}
	},
	"tree": func(f *AgentFactory, _ *tools.Registry, _ *Agent) tools.Tool {
		return &tools.TreeTool{WorkDir: f.Config.EffectiveWorkDir()}
	},
	"update_todos": func(f *AgentFactory, _ *tools.Registry, a *Agent) tools.Tool {
		store := &tools.TodoStore{}
		if a.TodoSendFunc != nil {
			store.SendFunc = a.TodoSendFunc
			store.EditFunc = a.TodoEditFunc
		} else {
			store.NotifyFunc = a.NotifyFunc
		}
		a.todoStore = store
		return &tools.UpdateTodosTool{Store: store}
	},
	"ask_user": func(f *AgentFactory, _ *tools.Registry, a *Agent) tools.Tool {
		return &tools.AskUserTool{
			ChannelID:     a.GetChannelID(),
			Manager:       f.UserInputMgr,
			NotifyFuncPtr: &a.AskUserNotify,
		}
	},
	"escalate": func(f *AgentFactory, r *tools.Registry, _ *Agent) tools.Tool {
		return &EscalateTool{factory: f, registry: r}
	},
	"browser_navigate": func(f *AgentFactory, _ *tools.Registry, a *Agent) tools.Tool {
		if a.browserSess == nil {
			a.browserSess = tools.NewBrowserSession(&f.Config.Browser)
		}
		return &tools.BrowserNavigateTool{Session: a.browserSess}
	},
	"browser_action": func(f *AgentFactory, _ *tools.Registry, a *Agent) tools.Tool {
		if a.browserSess == nil {
			a.browserSess = tools.NewBrowserSession(&f.Config.Browser)
		}
		return &tools.BrowserActionTool{Session: a.browserSess}
	},
	"browser_content": func(f *AgentFactory, _ *tools.Registry, a *Agent) tools.Tool {
		if a.browserSess == nil {
			a.browserSess = tools.NewBrowserSession(&f.Config.Browser)
		}
		return &tools.BrowserContentTool{Session: a.browserSess}
	},
}

// delegateToolParams returns the shared OpenAI-compatible parameter schema
// used by both DelegateFastTool and DelegateExpertTool.
func delegateToolParams() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"task": map[string]interface{}{
				"type":        "string",
				"description": "The specific goal or task for the sub-agent to achieve.",
			},
			"tools": map[string]interface{}{
				"type":  "array",
				"items": map[string]interface{}{"type": "string"},
				"description": "Tool names the sub-agent may use. " +
					"Delegation tools (delegate, escalate) are always excluded.",
			},
			"pre_tool": map[string]interface{}{
				"type":        "string",
				"description": "Optional: a tool to execute before the sub-agent starts, to gather context.",
			},
			"pre_tool_args": map[string]interface{}{
				"type":        "object",
				"description": "Optional: arguments for the pre_tool.",
			},
		},
		"required": []string{"task", "tools"},
	}
}

// delegateBlacklist is the set of delegation tools that must never be passed
// to a sub-agent to prevent infinite recursion.
var delegateBlacklist = map[string]bool{
	"delegate": true,
	"escalate": true,
}

// runSkillDelegate creates and runs a sub-agent with the given model config.
// It always strips delegation tools from the allowed-tools list.
func runSkillDelegate(
	ctx context.Context,
	factory *AgentFactory,
	registry *tools.Registry,
	a SubAgentArgs,
	modelCfg config.ModelConfig,
	label string,
) (string, error) {
	// Filter out delegation tools.
	var safeTools []string
	for _, t := range a.Tools {
		if !delegateBlacklist[t] {
			safeTools = append(safeTools, t)
		}
	}

	subAgent := factory.CreateAgentFiltered(nil, "", UniqueStrings(safeTools))
	subAgent.InjectSoul = false   // sub-agents never get personality injection
	subAgent.InjectContext = false // delegate sub-agents don't need workspace context
	subAgent.IsSubAgent = true
	subAgent.MaxIterations = factory.Config.SubAgent.MaxIterations

	// Resolve and apply model (falls back to base LLM if model config is empty).
	modelName, apiKey, baseURL := modelCfg.Resolve(
		factory.Config.LLM.Model,
		factory.LLMClient.APIKey(),
		factory.LLMClient.BaseURL(),
	)
	subAgent.SetLLMClient(llm.NewClient(apiKey, baseURL, modelName, factory.Debug, factory.Config.LLM.MaxTokens))

	if factory.Debug {
		fmt.Printf("  ⤷  %-10s %s  [%s]\n", label, a.Task, modelName)
	}

	// Execute optional pre-tool.
	if a.PreTool != "" {
		if factory.Debug {
			fmt.Printf("  ⤷  %-10s pre-tool: %s\n", label, a.PreTool)
		}
		preResult, err := registry.Execute(ctx, a.PreTool, a.PreToolArgs)
		if err != nil {
			return "", fmt.Errorf("pre_tool %q failed: %w", a.PreTool, err)
		}
		subAgent.AddHistory(
			fmt.Sprintf("[PRE-EXECUTION DATA]\nTool %q result:\n---\n%s\n---", a.PreTool, preResult),
			"Acknowledged. I will use this data.",
		)
	}

	// Cap with SubAgent.Timeout if configured; inherit parent ctx so /stop propagates.
	execCtx := ctx
	if factory.Config.SubAgent.Timeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, time.Duration(factory.Config.SubAgent.Timeout)*time.Second)
		defer cancel()
	}

	result, err := subAgent.Run(execCtx, a.Task, nil)
	if err != nil {
		if strings.Contains(err.Error(), "maximum iterations") {
			return "", fmt.Errorf("%s sub-agent hit iteration limit: %w", label, err)
		}
		return "", fmt.Errorf("%s sub-agent failed: %w", label, err)
	}

	if factory.Debug {
		fmt.Printf("  ⤷  %-10s done\n", label)
	}
	return result, nil
}

// ── EscalateTool ────────────────────────────────────────────────────────────

// EscalateTool delegates a sub-task to a powerful sub-agent using the
// configured strong model. Skill-only; use when the task demands the highest
// quality reasoning that the base agent model cannot provide.
type EscalateTool struct {
	factory  *AgentFactory
	registry *tools.Registry
}

func (t *EscalateTool) Name() string { return "escalate" }

func (t *EscalateTool) Description() string {
	return "Escalates a complex sub-task to an expert sub-agent using the strongest available model. " +
		"Use for deep reasoning, multi-step analysis, or tasks requiring the highest output quality. " +
		"Delegation tools are automatically excluded from the sub-agent."
}

func (t *EscalateTool) Parameters() map[string]interface{} { return delegateToolParams() }

func (t *EscalateTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a SubAgentArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	return runSkillDelegate(ctx, t.factory, t.registry, a, t.factory.Config.Router.StrongModel, "Escalate")
}
