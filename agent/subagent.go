package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ageage/llm"
	"ageage/tools"
)

// DelegateTool allows the agent to delegate tasks to a sub-agent.
type DelegateTool struct {
	factory  *AgentFactory
	registry *tools.Registry
}

func (t *DelegateTool) Name() string {
	return "delegate"
}

func (t *DelegateTool) Description() string {
	return "Delegates a sub-task to an isolated sub-agent with its own tool set and iteration budget. " +
		"Use this to parallelize work or isolate complex operations. " +
		"You MUST specify which tools the sub-agent needs. " +
		"Optionally provide a 'pre_tool' to gather initial context; its output is injected into the sub-agent automatically."
}

func (t *DelegateTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"task": map[string]interface{}{
				"type":        "string",
				"description": "The specific goal or task for the sub-agent to achieve.",
			},
			"tools": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "string",
				},
				"description": "List of tool names the sub-agent is allowed to use. Only tools you currently have access to can be delegated.",
			},
			"pre_tool": map[string]interface{}{
				"type":        "string",
				"description": "Optional: A tool to execute BEFORE the sub-agent starts, to gather context or perform a setup action.",
			},
			"pre_tool_args": map[string]interface{}{
				"type":        "object",
				"description": "Optional: The arguments for the pre_tool.",
			},
		},
		"required": []string{"task", "tools"},
	}
}

type SubAgentArgs struct {
	Task        string          `json:"task"`
	Tools       []string        `json:"tools"`
	PreTool     string          `json:"pre_tool"`
	PreToolArgs json.RawMessage `json:"pre_tool_args"`
}

func (t *DelegateTool) Execute(args json.RawMessage) (string, error) {
	var a SubAgentArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("failed to parse delegation arguments: %w", err)
	}

	if t.factory.Debug {
		fmt.Printf("  ⤷  %-10s %s\n", "Delegate", a.Task)
	}

	// Prevent infinite recursion: strip all delegation tools from the sub-agent's tool list.
	var safeTools []string
	for _, toolName := range a.Tools {
		if !delegateBlacklist[toolName] {
			safeTools = append(safeTools, toolName)
		}
	}

	// Create the sub-agent with dynamically filtered tools.
	subAgent := t.factory.CreateAgentFiltered(nil, "", UniqueStrings(safeTools))
	subAgent.InjectSoul = false   // sub-agents never get personality injection
	subAgent.InjectContext = false // delegate sub-agents don't need workspace context
	subAgent.IsSubAgent = true
	subAgent.MaxIterations = t.factory.Config.SubAgent.MaxIterations

	// Set sub-agent specific model if configured.
	if t.factory.Config.SubAgent.Model.Model != "" {
		modelName, apiKey, baseURL := t.factory.Config.SubAgent.Model.Resolve(t.factory.Config.LLM.Model, t.factory.LLMClient.APIKey(), t.factory.LLMClient.BaseURL())
		subAgent.SetLLMClient(llm.NewClient(apiKey, baseURL, modelName, t.factory.Debug, t.factory.Config.LLM.MaxTokens))
		if t.factory.Debug {
			fmt.Printf("  ⤷  %-10s model: %s\n", "Delegate", modelName)
		}
	}

	// 1. Execute Pre-tool if requested with robust error handling.
	if a.PreTool != "" {
		if t.factory.Debug {
			fmt.Printf("  ⤷  %-10s pre-tool: %s\n", "Delegate", a.PreTool)
		}

		preResult, err := t.registry.Execute(a.PreTool, a.PreToolArgs)
		if err != nil {
			// If pre-tool fails, we report it back to the main agent.
			// The main agent can then decide to retry or skip.
			return "", fmt.Errorf("pre_tool '%s' failed, aborting sub-task: %w", a.PreTool, err)
		}

		// Inject pre-tool result as foundational context.
		// We use a System message injected via AddHistory to ensure it's at the very beginning.
		subAgent.AddHistory(
			fmt.Sprintf("[SYSTEM: PRE-EXECUTION DATA]\nThe tool '%s' was executed to gather initial context. Result:\n---\n%s\n---", a.PreTool, preResult),
			"Acknowledged. I will incorporate this pre-execution data into my strategy.",
		)
	}

	// 2. Run the sub-agent with timeout and iteration monitoring.
	ctx := context.Background()
	if t.factory.Config.SubAgent.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(t.factory.Config.SubAgent.Timeout)*time.Second)
		defer cancel()
	}

	result, err := subAgent.Run(ctx, a.Task, nil)
	if err != nil {
		// FALLBACK LOGIC: If independent model fails, retry with default agent model.
		if t.factory.Config.SubAgent.Model.Model != "" {
			if t.factory.Debug {
				fmt.Printf("  ⤷  %-10s model failed (%v) — retrying with default\n", "Delegate", err)
			}
			// Reset sub-agent for fallback retry: clear the failed conversation history
			// so the retry starts fresh without the failed attempt confusing the LLM.
			subAgent.ClearHistory()
			subAgent.SetLLMClient(t.factory.LLMClient)
			result, err = subAgent.Run(ctx, a.Task, nil)
			if err == nil {
				return result, nil
			}
		}

		// Final failure reporting
		if strings.Contains(err.Error(), "maximum iterations") {
			return "", fmt.Errorf("sub-agent hit iteration limit. Task may be too complex or stuck in a loop. Error: %w", err)
		}
		return "", fmt.Errorf("sub-agent failed during execution: %w", err)
	}

	if t.factory.Debug {
		fmt.Printf("  ⤷  %-10s done\n", "Delegate")
	}

	return result, nil
}
