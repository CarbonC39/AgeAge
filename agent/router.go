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
	TaskDirect   TaskComplexity = "direct"   // pure conversation, no tools
	TaskAtomic   TaskComplexity = "atomic"   // single tool call, fixed target
	TaskWorkflow TaskComplexity = "workflow"  // multi-step, planning, skill/pipeline

	// Deprecated aliases kept for backward compatibility with existing skill files.
	TaskSimple  = TaskDirect
	TaskMedium  = TaskAtomic
	TaskComplex = TaskWorkflow
)

// normalizeComplexity maps both the current names ("direct", "atomic", "workflow")
// and the legacy names ("simple", "medium", "complex") to a canonical TaskComplexity.
// Returns "" for unrecognised values.
func normalizeComplexity(s string) TaskComplexity {
	switch strings.ToLower(s) {
	case "direct", "simple":
		return TaskDirect
	case "atomic", "medium":
		return TaskAtomic
	case "workflow", "complex":
		return TaskWorkflow
	default:
		return ""
	}
}

// RouterChecks is the factual checklist the router answers about the user request.
// The engine derives TaskComplexity from these fields rather than asking the LLM
// to label it directly (removes semantic bias).
type RouterChecks struct {
	NeedsTools         bool `json:"needs_tools"`
	NeedsMultipleCalls bool `json:"needs_multiple_calls"`
	NeedsSynthesis     bool `json:"needs_synthesis"`
}

// RouterResult is the output of the intent router.
type RouterResult struct {
	Complexity    TaskComplexity `json:"-"`              // derived by deriveComplexity(), not parsed
	Checks        RouterChecks   `json:"checks"`
	Skill         string         `json:"skill"`          // Name of the skill the router selected (empty = none)
	RequiredTools []string       `json:"required_tools"`
	Reasoning     string         `json:"reasoning"`
}

// deriveComplexity computes Complexity from Checks and RequiredTools.
// Called after JSON parsing; fallback results set Complexity directly.
func (r *RouterResult) deriveComplexity() {
	multiTool := len(r.RequiredTools) >= 2
	c := r.Checks
	switch {
	case c.NeedsMultipleCalls || c.NeedsSynthesis || multiTool:
		r.Complexity = TaskWorkflow
	case c.NeedsTools || len(r.RequiredTools) > 0:
		r.Complexity = TaskAtomic
	default:
		r.Complexity = TaskDirect
	}
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
//   1. Fixed: checklist questions, rules, calibration examples — never changes
//   2. Skill catalog (all skills, name+desc)                   — changes on hot-reload
//   3. Tool list                                               — changes per turn
//   4. Fixed: JSON schema                                      — never changes
func (r *Router) buildRouterPrompt(availableTools []string) string {
	var sb strings.Builder

	// ── 1. Fixed: checklist questions + rules + examples ─────────────────────
	sb.WriteString(`[SYSTEM: TASK CLASSIFICATION PROTOCOL]
You are a one-shot classifier. Output ONLY the JSON object at the end of this prompt.
No markdown fences, no tool calls, no preamble, no text outside the JSON.

STEP 1 — Skill match (check this first)
  Does a skill from the list below CLEARLY and SPECIFICALLY match this request?
  → YES: set skill="<exact name>", continue to STEP 2.
  → NO:  leave skill="" and continue to STEP 2.

STEP 2 — Answer these three factual checks about the request:

  needs_tools: Does answering this completely require ANY external tool call?
    false — answer comes entirely from training data: math, greetings, well-known concepts
    true  — task involves files, web content, system state, code, APIs, real-time data
    (When uncertain → true. Tool call beats hallucination.)

  needs_multiple_calls: Are 2 or more separate tool calls required?
    true  — web search + read results; multiple files; multi-stage operation
    false — exactly one named file; exactly one known command
    ANY web/internet search → always true (fetch is call 1; reading results is call 2+)

  needs_synthesis: Does the answer require combining/analyzing results from multiple sources?
    true  — comparison, research, "explain how our X works" (read files + analyze)
    false — single-source retrieval; run one command and return raw output

Calibration:
  "hello"                           needs_tools=false, needs_multiple_calls=false, needs_synthesis=false
  "what is recursion"               needs_tools=false, needs_multiple_calls=false, needs_synthesis=false
  "read /etc/hosts"                 needs_tools=true,  needs_multiple_calls=false, needs_synthesis=false, tools=[file_read]
  "run: git status"                 needs_tools=true,  needs_multiple_calls=false, needs_synthesis=false, tools=[bash]
  "fetch https://api.example.com/x" needs_tools=true,  needs_multiple_calls=false, needs_synthesis=false, tools=[web_fetch]
  "search for ActivityPub libs"     needs_tools=true,  needs_multiple_calls=true,  needs_synthesis=true,  tools=[web_search]
  "what's the weather in Tokyo"     needs_tools=true,  needs_multiple_calls=true,  needs_synthesis=true,  tools=[web_search]
  "explain our auth flow"           needs_tools=true,  needs_multiple_calls=true,  needs_synthesis=true,  tools=[file_read,grep]

`)

	// ── 2. Skill catalog (changes on hot-reload) ──────────────────────────────
	if len(r.skills) > 0 {
		sb.WriteString("Skills (pick at most one by its exact name, or leave \"skill\" empty):\n")
		for _, s := range r.skills {
			desc := s.Description
			if desc == "" {
				desc = "(no description)"
			}
			typeSuffix := ""
			if s.IsPipeline() {
				typeSuffix = " [pipeline]"
			}
			fmt.Fprintf(&sb, "- %s: %s%s\n", s.Name, desc, typeSuffix)
		}
		sb.WriteString("\n")
	}

	// ── 3. Tool list (variable per turn) ─────────────────────────────────────
	sb.WriteString("Available tools: ")
	sb.WriteString(strings.Join(availableTools, ", "))
	sb.WriteString("\n\n")

	// ── 4. Fixed: JSON schema ─────────────────────────────────────────────────
	sb.WriteString(`Output exactly this JSON and nothing else:
{
  "skill": "exact skill name or empty string",
  "required_tools": ["tool1", ...],
  "reasoning": "one sentence",
  "checks": {
    "needs_tools": false,
    "needs_multiple_calls": false,
    "needs_synthesis": false
  }
}
DO NOT wrap in code fences. DO NOT call any tools. DO NOT add any text outside the JSON object.
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
			fmt.Printf("  ◆  %-10s router call failed, falling back to atomic: %v\n", "Router", err)
		}
		return &RouterResult{
			Complexity:    TaskAtomic,
			RequiredTools: availableTools,
			Reasoning:     "router LLM call failed",
		}, nil
	}

	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
		if r.debug {
			fmt.Printf("  ◆  %-10s router returned empty response, falling back to atomic\n", "Router")
		}
		return &RouterResult{
			Complexity:    TaskAtomic,
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
		if r.debug {
			fmt.Printf("  ◆  %-10s parse failed, using all tools\n", "Router")
		}
		return &RouterResult{
			Complexity:    TaskAtomic,
			RequiredTools: availableTools,
			Reasoning:     "router parse failed",
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

	// Derive complexity from the checklist and required_tools count.
	result.deriveComplexity()

	return &result, nil
}
