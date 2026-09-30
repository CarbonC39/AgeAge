package agent

// skillOnlyToolFactories maps tool names to factory functions for tools that
// require special initialization. These tools are registered in three cases:
//  1. Per-turn injection when a matched skill declares them in required_tools.
//  2. At factory time when listed in agent.tools config (global allowlist).
//  3. At factory time when listed in the sub-agent allowedTools parameter.

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

type skillOnlyToolSpec struct {
	catalog toolCatalogSpec
	factory func(AgentDeps, *tools.Registry, *Agent) tools.Tool
}

// skillOnlyToolSpecs keeps lifecycle metadata beside each runtime constructor.
// Its slice order is the stable catalog and prompt order.
var skillOnlyToolSpecs = []skillOnlyToolSpec{
	{catalog: toolCatalogSpec{prototype: func() tools.Tool { return &NextStepTool{} }, availability: ToolInternal}, factory: func(_ AgentDeps, _ *tools.Registry, a *Agent) tools.Tool {
		return &NextStepTool{agent: a}
	}},
	{catalog: toolCatalogSpec{prototype: func() tools.Tool { return &tools.GrepTool{} }, availability: ToolSkillOnly, configurable: true}, factory: func(f AgentDeps, _ *tools.Registry, _ *Agent) tools.Tool {
		return &tools.GrepTool{Security: f.GetSecurity()}
	}},
	{catalog: toolCatalogSpec{prototype: func() tools.Tool { return &tools.GlobTool{} }, availability: ToolSkillOnly, configurable: true}, factory: func(f AgentDeps, _ *tools.Registry, _ *Agent) tools.Tool {
		return &tools.GlobTool{Security: f.GetSecurity(), Workspace: f.GetConfig().EffectiveWorkDir()}
	}},
	{catalog: toolCatalogSpec{prototype: func() tools.Tool { return &tools.TreeTool{} }, availability: ToolSkillOnly, configurable: true}, factory: func(f AgentDeps, _ *tools.Registry, _ *Agent) tools.Tool {
		return &tools.TreeTool{
			WorkDir:  f.GetConfig().EffectiveWorkDir(),
			Security: f.GetSecurity(),
		}
	}},
	{catalog: toolCatalogSpec{prototype: func() tools.Tool { return &tools.UpdateTodosTool{} }, availability: ToolSkillOnly, configurable: true}, factory: func(_ AgentDeps, _ *tools.Registry, a *Agent) tools.Tool {
		store := &tools.TodoStore{}
		if a.Callbacks.TodoSend != nil {
			store.SendFunc = a.Callbacks.TodoSend
			store.EditFunc = a.Callbacks.TodoEdit
		} else {
			store.NotifyFunc = a.Callbacks.Notify
		}
		a.todoStore = store
		a.finishTool.CheckTodos = func() (bool, string) {
			return store.IsComplete(), store.PendingList()
		}
		return &tools.UpdateTodosTool{Store: store}
	}},
	{catalog: toolCatalogSpec{prototype: func() tools.Tool { return &tools.AskUserTool{} }, availability: ToolSkillOnly, configurable: true}, factory: func(f AgentDeps, _ *tools.Registry, a *Agent) tools.Tool {
		return &tools.AskUserTool{
			ChannelID:     a.GetChannelID(),
			Scope:         a.GetInteractionScope(),
			Manager:       f.GetUserInputMgr(),
			NotifyFuncPtr: &a.Callbacks.AskUser,
		}
	}},
	{catalog: toolCatalogSpec{prototype: func() tools.Tool { return &EscalateTool{} }, availability: ToolSkillOnly, configurable: true}, factory: func(f AgentDeps, r *tools.Registry, _ *Agent) tools.Tool {
		// EscalateTool needs LLM client and debug flag, which require a full factory.
		factory, ok := f.(*AgentFactory)
		if !ok {
			return nil
		}
		return &EscalateTool{factory: factory, registry: r}
	}},
	{catalog: toolCatalogSpec{prototype: func() tools.Tool { return &tools.BrowserNavigateTool{} }, availability: ToolSkillOnly, configurable: true}, factory: func(f AgentDeps, _ *tools.Registry, a *Agent) tools.Tool {
		if a.browserSess == nil {
			a.browserSess = tools.NewBrowserSession(&f.GetConfig().Browser)
		}
		return &tools.BrowserNavigateTool{Session: a.browserSess}
	}},
	{catalog: toolCatalogSpec{prototype: func() tools.Tool { return &tools.BrowserActionTool{} }, availability: ToolSkillOnly, configurable: true}, factory: func(f AgentDeps, _ *tools.Registry, a *Agent) tools.Tool {
		if a.browserSess == nil {
			a.browserSess = tools.NewBrowserSession(&f.GetConfig().Browser)
		}
		return &tools.BrowserActionTool{Session: a.browserSess}
	}},
	{catalog: toolCatalogSpec{prototype: func() tools.Tool { return &tools.BrowserContentTool{} }, availability: ToolSkillOnly, configurable: true}, factory: func(f AgentDeps, _ *tools.Registry, a *Agent) tools.Tool {
		if a.browserSess == nil {
			a.browserSess = tools.NewBrowserSession(&f.GetConfig().Browser)
		}
		return &tools.BrowserContentTool{Session: a.browserSess}
	}},
}

var skillOnlyToolFactories = func() map[string]func(AgentDeps, *tools.Registry, *Agent) tools.Tool {
	factories := make(map[string]func(AgentDeps, *tools.Registry, *Agent) tools.Tool, len(skillOnlyToolSpecs))
	for _, spec := range skillOnlyToolSpecs {
		factories[descriptorFromPrototype(spec.catalog.prototype).Name] = spec.factory
	}
	return factories
}()

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
	var safeTools []string
	for _, t := range a.Tools {
		if !delegateBlacklist[t] {
			safeTools = append(safeTools, t)
		}
	}

	subAgent := factory.CreateAgentFiltered(nil, "", UniqueStrings(safeTools))
	subAgent.Mode.InjectSoul = false
	subAgent.Mode.InjectContext = false
	subAgent.Mode.IsSubAgent = true
	subAgent.MaxIterations = factory.Config.SubAgent.MaxIterations

	modelName, apiKey, baseURL := modelCfg.Resolve(
		factory.Config.LLM.Model,
		factory.LLMClient.APIKey(),
		factory.LLMClient.BaseURL(),
	)
	subAgent.SetLLMClient(llm.NewClient(apiKey, baseURL, modelName, factory.Debug, factory.Config.LLM.MaxTokens))

	if factory.Debug {
		fmt.Printf("  ⤷  %-10s %s  [%s]\n", label, a.Task, modelName)
	}

	if a.PreTool != "" {
		if factory.Debug {
			fmt.Printf("  ⤷  %-10s pre-tool: %s\n", label, a.PreTool)
		}
		preResult, err := NewToolDispatcher(registry, factory.CredMgr).Execute(
			ctx, a.PreTool, a.PreToolArgs, ToolDispatchHooks{},
		)
		if err != nil {
			return "", fmt.Errorf("pre_tool %q failed: %w", a.PreTool, err)
		}
		subAgent.AddHistory(
			fmt.Sprintf("[PRE-EXECUTION DATA]\nTool %q result:\n---\n%s\n---", a.PreTool, preResult),
			"Acknowledged. I will use this data.",
		)
	}

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

// ── NextStepTool ─────────────────────────────────────────────────────────────

// NextStepTool advances a segmented skill to its next segment. It is injected
// automatically whenever a segmented skill is active and removed at turn end.
// Advancing rebuilds the system prompt with the new segment's instructions, so
// the very next LLM call sees the new guidance.
type NextStepTool struct {
	agent *Agent
}

func (t *NextStepTool) Name() string { return "next_step" }

func (t *NextStepTool) Description() string {
	return "Advance to the next segment of the currently active segmented skill. " +
		"Call this tool after completing the work described in the current segment. " +
		"The next segment's instructions replace the current ones."
}

func (t *NextStepTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

func (t *NextStepTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	a := t.agent
	sk := a.activeSkill
	if sk == nil || !sk.Segmented || len(sk.Segments) == 0 {
		return "", fmt.Errorf("next_step called but no segmented skill is active")
	}
	if a.segIdx >= len(sk.Segments)-1 {
		return "", fmt.Errorf("already on the final segment; call finish_task to complete the task")
	}

	a.segIdx++
	a.conv.SetSystemContent(a.buildSystemPrompt(sk))
	return fmt.Sprintf("Advanced to segment %d of %d of skill %q. "+
		"Continue working according to the new instructions.",
		a.segIdx+1, len(sk.Segments), sk.Name), nil
}

// ── EscalateTool ────────────────────────────────────────────────────────────

// EscalateTool delegates a sub-task to a powerful sub-agent using the
// configured strong model.
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
