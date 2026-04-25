package agent

import (
	"context"
	"fmt"
	"os"
	"strings"

	"ageage/config"
	"ageage/jsonutil"
	"ageage/llm"
	"ageage/skills"
)

// TaskComplexity represents the classified complexity of a user request.
type TaskComplexity string

const (
	TaskSimple  TaskComplexity = "simple"
	TaskMedium  TaskComplexity = "medium"
	TaskComplex TaskComplexity = "complex"
)

// RouterResult is the output of the intent router.
type RouterResult struct {
	Complexity    TaskComplexity `json:"complexity"`
	Skill         string         `json:"skill"`          // Name of the skill the router selected (empty = none)
	RequiredTools []string       `json:"required_tools"`
	Reasoning     string         `json:"reasoning"`
	DirectAnswer  string         `json:"direct_answer,omitempty"`
}

// Router classifies user intent and selects appropriate tools/model.
type Router struct {
	cfg          *config.Config
	client       *llm.Client
	skills       []skills.Skill
	debug        bool
	agentContent string // Content of AGENT.md for routing context
	soulContent  string // Content of SOUL.md (personality)
	InjectSoul   bool   // Whether to inject soulContent into the prompt
}

// NewRouter creates a new Router.
func NewRouter(cfg *config.Config, client *llm.Client, loadedSkills []skills.Skill, debug bool) *Router {
	// Read AGENT.md for routing context (behavioral rules).
	agentContent := ""
	if data, err := os.ReadFile(cfg.AgentPath()); err == nil {
		agentContent = strings.TrimSpace(string(data))
	}

	// Read SOUL.md for persona.
	soulContent := ""
	if data, err := os.ReadFile(cfg.SOULPath()); err == nil {
		soulContent = strings.TrimSpace(string(data))
	}

	return &Router{
		cfg:          cfg,
		client:       client,
		skills:       loadedSkills,
		debug:        debug,
		agentContent: agentContent,
		soulContent:  soulContent,
	}
}

// buildRouterPrompt constructs the router system prompt.
//
// Cache layout (ordered from most-stable to least-stable):
//   1. Fixed instruction header                — never changes
//   2. Skill catalog (all skills, name+desc)   — changes only on hot-reload
//   3. Tool list                               — changes per turn
//   4. Levels + JSON schema                    — never changes
//
// Placing the stable skill catalog BEFORE the variable tool list means the
// model can cache the entire prefix up to the tool list, giving a cache hit
// on every turn that has the same skill set.
func (r *Router) buildRouterPrompt(availableTools []string) string {
	var sb strings.Builder

	// ── 1. Fixed header ───────────────────────────────────────────────────────
	sb.WriteString("[SYSTEM: TASK EVALUATION PROTOCOL]\n")
	sb.WriteString("Analyze the request and return ONLY the JSON object specified below.\n")
	sb.WriteString("Rules: no markdown fences, no tool calls, no preamble, no text before or after the JSON.\n")
	sb.WriteString("Self-reference docs in .ageage/docs/ (file_read): how-i-work.md, skills.md, pipeline.md, troubleshooting.md.\n\n")

	// ── 2. Skill catalog (stable prefix) ─────────────────────────────────────
	// Always show ALL skills so the router can select one. Users can also
	// invoke a skill explicitly with /skill-name, bypassing this selection.
	if len(r.skills) > 0 {
		sb.WriteString("Available skills (choose at most 1 by its exact name, or leave \"skill\" empty):\n")
		for _, s := range r.skills {
			desc := s.Description
			if desc == "" {
				desc = "(no description)"
			}
			typeSuffix := ""
			if s.IsPipeline() {
				typeSuffix = " [pipeline - always use \"complex\" for these]"
			}
			fmt.Fprintf(&sb, "- %s: %s%s\n", s.Name, desc, typeSuffix)
		}
		sb.WriteString("\n")
	}

	// ── 3. Tool list (variable per turn) ─────────────────────────────────────
	sb.WriteString("Tools: ")
	sb.WriteString(strings.Join(availableTools, ", "))
	sb.WriteString("\n\n")

	// ── 4. Levels + JSON schema (fixed) ──────────────────────────────────────
	sb.WriteString(`Levels:
- "simple": Pure conversation — no tools needed. Populate "direct_answer" with your reply.
- "medium": Single-step or straightforward tool use (fetch a URL, run a command, look up a fact). No planning required; no matching skill.
- "complex": Use this level for ALL of the following cases:
    (a) A skill or pipeline from the list above clearly matches — fill "skill" with its exact name.
    (b) The task requires multi-step planning, complex reasoning, or is worth capturing as a reusable workflow — leave "skill" empty (the system will create one automatically).
  Always choose "complex" for pipeline skills.

Output exactly this JSON and nothing else:
{
  "complexity": "simple|medium|complex",
  "skill": "exact skill name or empty string",
  "required_tools": ["tool1", ...],
  "reasoning": "brief",
  "direct_answer": "..."
}
DO NOT wrap the JSON in code fences. DO NOT call any tools or functions. DO NOT add any text outside the JSON object.
`)

	return sb.String()
}

// Route classifies the user input and returns routing decisions.
func (r *Router) Route(ctx context.Context, userInput string, availableTools []string, history []llm.Message) (*RouterResult, error) {
	routerPrompt := r.buildRouterPrompt(availableTools)

	var sb strings.Builder
	if r.InjectSoul && r.soulContent != "" {
		sb.WriteString("## Personality & Behavior\n\n")
		sb.WriteString(r.soulContent)
		sb.WriteString("\n\n")
	}
	if r.agentContent != "" {
		sb.WriteString("## Agent Directives\n\n")
		sb.WriteString(r.agentContent)
		sb.WriteString("\n\n")
	}
	sb.WriteString(routerPrompt)

	messages := []llm.Message{
		{Role: "system", Content: sb.String()},
	}
	messages = append(messages, r.filterHistoryForRouter(history)...)

	// Use the router (lightweight) model, falling back to the base LLM on failure.
	modelName, apiKey, baseURL := r.cfg.Router.ClassifierModel.Resolve(r.cfg.LLM.Model, r.client.APIKey(), r.client.BaseURL())
	routerClient := llm.NewClient(apiKey, baseURL, modelName, r.debug, 0)

	resp, err := routerClient.ChatCompletionJSON(ctx, messages, r.cfg.LLM.Temperature)
	if err != nil {
		// If a dedicated router model was configured and failed, retry with the base LLM.
		baseModel, baseKey, baseURL2 := r.cfg.LLM.Model, r.client.APIKey(), r.client.BaseURL()
		if modelName != baseModel || apiKey != baseKey || baseURL != baseURL2 {
			if r.debug {
				fmt.Printf("  ◆  %-10s router model failed (%v), retrying with base LLM\n", "Router", err)
			}
			fallbackClient := llm.NewClient(baseKey, baseURL2, baseModel, r.debug, 0)
			resp, err = fallbackClient.ChatCompletionJSON(ctx, messages, r.cfg.LLM.Temperature)
		}
	}
	if err != nil {
		if r.debug {
			fmt.Printf("  ◆  %-10s router call failed, falling back to medium: %v\n", "Router", err)
		}
		return &RouterResult{
			Complexity:    TaskMedium,
			RequiredTools: availableTools,
			Reasoning:     "router LLM call failed",
		}, nil
	}

	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
		if r.debug {
			fmt.Printf("  ◆  %-10s router returned empty response, falling back to medium\n", "Router")
		}
		return &RouterResult{
			Complexity:    TaskMedium,
			RequiredTools: availableTools,
			Reasoning:     "router returned empty response",
		}, nil
	}

	content := resp.Choices[0].Message.Content

	if r.debug {
		fmt.Printf("  ◆  %-10s %s\n", "Router", strings.ReplaceAll(content, "\n", " "))
	}

	result, err := r.parseRouterResponse(content)
	if err != nil {
		// Fallback: treat as medium complexity, use all tools.
		if r.debug {
			fmt.Printf("  ◆  %-10s parse failed, using all tools\n", "Router")
		}
		return &RouterResult{
			Complexity:    TaskMedium,
			RequiredTools: availableTools,
			Reasoning:     "Router parse failed",
		}, nil
	}

	return result, nil
}

// filterHistoryForRouter filters conversation history for router context.
func (r *Router) filterHistoryForRouter(history []llm.Message) []llm.Message {
	var filtered []llm.Message
	maxHistoryForRouter := r.cfg.Router.MaxHistory
	if maxHistoryForRouter <= 0 {
		maxHistoryForRouter = 8
	}
	if maxHistoryForRouter > 32 {
		maxHistoryForRouter = 32
	}

	for i := len(history) - 1; i >= 0 && len(filtered) < maxHistoryForRouter; i-- {
		msg := history[i]
		if msg.Role == "user" || msg.Role == "assistant" {
			if msg.TextContent() != "" {
				filtered = append([]llm.Message{msg}, filtered...)
			}
		}
	}

	return filtered
}

// parseRouterResponse parses and validates the router's JSON response.
func (r *Router) parseRouterResponse(content string) (*RouterResult, error) {
	var result RouterResult
	if err := jsonutil.ParseToolArgs(content, &result); err != nil {
		return nil, fmt.Errorf("failed to parse router JSON: %w", err)
	}

	// Validate complexity.
	switch result.Complexity {
	case TaskSimple, TaskMedium, TaskComplex:
		// Valid.
	default:
		result.Complexity = TaskMedium
	}

	// Validate skill.
	if result.Skill != "" {
		found := false
		for _, s := range r.skills {
			if strings.EqualFold(s.Name, result.Skill) {
				result.Skill = s.Name
				found = true
				break
			}
		}
		if !found {
			if r.debug {
				fmt.Printf("  ◆  %-10s router selected unknown skill %q, ignoring\n", "Router", result.Skill)
			}
			result.Skill = ""
		}
	}

	return &result, nil
}
