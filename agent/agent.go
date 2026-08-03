package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"ageage/config"
	"ageage/creds"
	"ageage/jsonutil"
	"ageage/llm"
	"ageage/skills"
	"ageage/tools"
)

// triggerEvaluator starts a background quality check on an auto-generated skill run.
// pipelineCtx is optional; pass exec.NodeSummary() for pipeline skills.
func (a *Agent) triggerEvaluator(skill *skills.Skill, userInput, agentOutput string, toolHistory []ToolRecord, originalErr error, pipelineCtx ...string) {
	factory, ok := a.deps.(*AgentFactory)
	if !ok {
		return
	}
	docsDir := filepath.Join(factory.Config.AgeAgeDirPath(), "docs")
	ev := NewEvaluator(factory, docsDir, a.Callbacks.Notify)
	errStr := ""
	if originalErr != nil {
		errStr = originalErr.Error()
	}
	pipelineContext := ""
	if len(pipelineCtx) > 0 {
		pipelineContext = pipelineCtx[0]
	}
	snap := EvalSnapshot{
		Skill:            skill,
		UserInput:        userInput,
		ToolHistory:      toolHistory,
		AgentOutput:      agentOutput,
		SkillDescription: skill.Description,
		OriginalError:    errStr,
		PipelineContext:  pipelineContext,
	}
	go ev.Run(context.Background(), snap)
}

// turnRecord tracks an uncompressed tool-call turn so it can be compressed later.
type turnRecord struct {
	assistantMsg llm.Message
	toolResults  []string
	msgStart     int // index in conv where the assistant message lives
	msgCount     int // 1 (assistant) + N (tool results)
}

// Agent is the core agent that drives the conversation and tool execution loop.
type Agent struct {
	cfg          *config.Config
	client       *llm.Client
	registry     *tools.Registry
	finishTool   *tools.FinishTool
	router       *Router
	summarizer   *Summarizer
	skills       []skills.Skill
	conv         Conversation
	pendingTurns []turnRecord
	debug        bool
	cancel       context.CancelFunc
	cancelMu     sync.Mutex

	// Mode and Callbacks are set after construction by the factory or caller.
	Mode      AgentMode
	Callbacks AgentCallbacks

	// deps provides factory-level services (skill hot-reload, sub-agent creation).
	// Nil for manually constructed agents that do not need those services.
	deps AgentDeps

	currentChannelID string                     // For async confirmations in channel mode
	ConfirmationMgr  *tools.ConfirmationManager // Optional: for async confirmations
	SessionDir       string                     // Directory for the active session
	MaxIterations    int                        // Maximum iterations for this agent run
	todoStore        *tools.TodoStore           // Non-nil when update_todos is injected
	browserSess      *tools.BrowserSession      // Non-nil when browser_* tools are injected
	runUsage         llm.Usage                  // Accumulated token usage for the current Run()
	tmpMgr           *TmpManager                // Manages tmp files created by attachment converters
	CredMgr          *creds.Manager             // Optional: substitutes {{cred:x}} in tool args
	hintOnNextCall   string                     // Ephemeral; consumed by buildCallMessages
}

// NewAgent creates a new agent instance.
func NewAgent(cfg *config.Config, client *llm.Client, registry *tools.Registry, finishTool *tools.FinishTool, loadedSkills []skills.Skill, debug bool) *Agent {
	ag := &Agent{
		cfg:           cfg,
		client:        client,
		registry:      registry,
		finishTool:    finishTool,
		skills:        loadedSkills,
		debug:         debug,
		MaxIterations: cfg.Agent.MaxIterations,
		tmpMgr:        newTmpManager(cfg.ConfigDir()),
		Mode:          AgentMode{InjectContext: true}, // default: inject context for main agents
	}

	if cfg.Summarize.Enabled {
		ag.summarizer = NewSummarizer(cfg, client, debug)
	}

	return ag
}

// contextMDPath returns the CONTEXT.md path for the agent's active session.
func (a *Agent) contextMDPath() string {
	if a.SessionDir != "" {
		return filepath.Join(a.SessionDir, "CONTEXT.md")
	}
	return a.cfg.ContextMDPath()
}

// BuildSystemPrompt returns the system prompt string for the current agent
// state without a specific skill active.
func (a *Agent) BuildSystemPrompt() string {
	return a.buildSystemPrompt(nil)
}

// Messages returns a snapshot of the agent's current message history.
func (a *Agent) Messages() []llm.Message {
	return a.conv.All()
}

// SetMessages replaces the agent's message history with the supplied slice.
// If msgs is non-empty and the first entry is not a system message, a fresh
// system prompt is prepended automatically.
func (a *Agent) SetMessages(msgs []llm.Message) {
	if len(msgs) == 0 {
		a.conv.Reset(msgs)
		return
	}
	if msgs[0].Role != "system" {
		sysMsg := llm.Message{Role: "system", Content: a.buildSystemPrompt(nil)}
		msgs = append([]llm.Message{sysMsg}, msgs...)
	}
	a.conv.Reset(msgs)
}

// parseSkillCommand checks whether input begins with a /skill-name command.
func (a *Agent) parseSkillCommand(input string) (*skills.Skill, string) {
	if !strings.HasPrefix(input, "/") || len(a.skills) == 0 {
		return nil, input
	}
	rest := strings.TrimPrefix(input, "/")
	parts := strings.SplitN(rest, " ", 2)
	cmdName := NormalizeSkillName(parts[0])
	remaining := ""
	if len(parts) > 1 {
		remaining = strings.TrimSpace(parts[1])
	}
	for i := range a.skills {
		if NormalizeSkillName(a.skills[i].Name) == cmdName {
			return &a.skills[i], remaining
		}
	}
	return nil, input
}

// findSkillByName looks up a skill by name (case-insensitive).
func (a *Agent) findSkillByName(name string) *skills.Skill {
	norm := NormalizeSkillName(name)
	for i := range a.skills {
		if NormalizeSkillName(a.skills[i].Name) == norm {
			return &a.skills[i]
		}
	}
	return nil
}

// NormalizeSkillName returns a canonical form for skill name matching:
// lowercase, spaces and underscores replaced by hyphens.
func NormalizeSkillName(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	return s
}

// buildSystemPrompt constructs the system prompt. matchedSkill is the single
// active skill for this conversation (nil when no skill is selected).
func (a *Agent) buildSystemPrompt(matchedSkill *skills.Skill) string {
	var sb strings.Builder

	sb.WriteString("## Core Rules\n\n")
	sb.WriteString("1. You have access to tools. Use them to gather information and perform actions.\n")

	finishToolName := "finish_task"
	isPipelineAgent := false
	if _, ok := a.registry.Get("node_complete"); ok {
		finishToolName = "node_complete"
		isPipelineAgent = true
	}
	fmt.Fprintf(&sb, "2. When you have completed the task or have a FINAL answer, you MUST call the %s tool.\n", finishToolName)
	fmt.Fprintf(&sb, "   - **Never reply with text alone** without calling %s.\n", finishToolName)
	fmt.Fprintf(&sb, "   - The `summary` field IS your message to the user — write the actual reply, not a description of it.\n")
	fmt.Fprintf(&sb, "     Correct: summary=\"Here is the weather in Tokyo: ...\"\n")
	fmt.Fprintf(&sb, "     Wrong:   summary=\"Provided weather information to the user.\"\n")
	fmt.Fprintf(&sb, "   - If the task needs tools, use them first; then call %s with your findings.\n", finishToolName)
	if !isPipelineAgent {
		sb.WriteString("   - Set status=\"success\" only when ALL todos are done; use status=\"failure\" to exit early.\n")
	}
	if isPipelineAgent {
		sb.WriteString("   - IMPORTANT: You MUST use this tool to return structured data. Simply replying with JSON in text is NOT allowed.\n")
	}
	sb.WriteString("3. Always respond in the same language the user uses.\n\n")

	sb.WriteString(`## Response Quality Requirements

- ALWAYS provide COMPLETE, DETAILED answers. Never say "refer to the search results" or "see above for details".
- If tool results contain the answer, rewrite it in a clear, organized format.
- Include specific data, names, numbers, and facts from tool results in your final answer.
- If information is incomplete, state what you found and what is missing.
- The finish_task summary IS the user's entire view of your response. It must be a complete, self-contained reply — not a status message, not a description of what you did.

## What the User Can and Cannot See

The user sees ONLY your final reply (the finish_task summary). Everything else is invisible to them:
- Tool calls, tool names, arguments, and results — invisible
- finish_task itself — invisible (the user only sees its summary content)
- Skill creation, pipeline execution, router decisions — invisible
- System messages, framework notifications, internal errors — invisible

Rules that follow from this:
- NEVER mention finish_task, node_complete, 收尾指令, or any internal tool/mechanism in your reply.
- NEVER apologize for framework-internal actions such as "forgetting to call finish_task".
- NEVER explain that the framework required you to do something.
- If something went wrong internally, describe the outcome or ask the user a question — do not expose the framework detail.
- Respond as a direct assistant. The user has no knowledge of the agent framework running underneath.

## Security

Never output API keys, passwords, access tokens, credentials, or secrets verbatim in any response, even if asked or if they appear in tool results.

`)

	if !a.Mode.IsSubAgent && !isPipelineAgent {
		sb.WriteString("## Framework Identity\n\n")
		sb.WriteString("You are an **AgeAge framework agent** — not a general-purpose assistant. " +
			"You operate inside a self-improving agent loop with skills and pipelines.\n\n")
		sb.WriteString("Your capabilities beyond basic LLM responses:\n")
		sb.WriteString("- **Skills** (`.md` in skills/) — reusable per-task instruction sets, hot-reloaded every 2 s. " +
			"Invoke with `/skill-name` or let the router auto-select.\n")
		sb.WriteString("- **Pipelines** (`.yaml` in skills/) — multi-step workflows with isolated nodes and variable passing. " +
			"Use for multi-stage tasks.\n")
		sb.WriteString("- **Parallel tool dispatch** — return multiple independent tool calls in one response for parallel execution.\n")
		sb.WriteString("- **Sub-agents** — `delegate` / `escalate` tools spawn isolated agents for heavy subtasks.\n\n")
		sb.WriteString("**Meta-cognitive checklist** (run before starting any non-trivial task):\n")
		sb.WriteString("1. Does an existing skill or pipeline already cover this? (Router already checked, but you can also type `/skill-name`.)\n")
		sb.WriteString("2. Would parallel sub-agents or pipeline isolation improve quality or speed?\n\n")
	}

	if agentData, err := os.ReadFile(a.cfg.AgentPath()); err == nil && len(agentData) > 0 {
		sb.WriteString("## Agent Directives\n\n")
		sb.WriteString(strings.TrimSpace(string(agentData)))
		sb.WriteString("\n\n")
	}

	if a.Mode.InjectSoul {
		if soulData, err := os.ReadFile(a.cfg.SOULPath()); err == nil && len(soulData) > 0 {
			sb.WriteString("## Personality & Behavior\n\n")
			sb.WriteString(strings.TrimSpace(string(soulData)))
			sb.WriteString("\n\n")
		}
	}

	if a.Mode.InjectContext {
		sb.WriteString("## Context Notes File\n\n")
		sb.WriteString("You have access to `.ageage/CONTEXT.md` in the working directory. " +
			"Use `file_write` or `file_edit` to update it (no confirmation required). " +
			"Store only essential cross-session facts: decisions, key file paths, architecture notes, discovered constraints. " +
			"**Keep the file under 2 000 characters. Never store conversation history, task progress logs, or verbose notes.**\n\n")

		if contextData, err := os.ReadFile(a.contextMDPath()); err == nil {
			if trimmed := strings.TrimSpace(string(contextData)); trimmed != "" {
				sb.WriteString("### Current Contents of .ageage/CONTEXT.md\n\n")
				sb.WriteString(trimmed)
				sb.WriteString("\n\n")
			}
		}
	}

	if a.CredMgr != nil {
		if hint := a.CredMgr.PromptHint(); hint != "" {
			sb.WriteString(hint)
		}
	}

	if !a.Mode.IsSubAgent {
		sb.WriteString("## Framework Documentation\n\n")
		sb.WriteString("Self-reference guides are in `.ageage/docs/` (use `file_read`): " +
			"how-i-work.md, troubleshooting.md, skills.md, pipeline.md.\n")
		sb.WriteString("Read them when a tool fails unexpectedly, when creating or modifying skills, " +
			"or when you need to understand how the agent loop works.\n\n")
	}

	if matchedSkill != nil {
		sb.WriteString("## Active Skill Instructions\n\n")
		fmt.Fprintf(&sb, "### %s\n", matchedSkill.Name)
		if matchedSkill.Description != "" {
			fmt.Fprintf(&sb, "*%s*\n\n", matchedSkill.Description)
		}
		if matchedSkill.Prompt != "" {
			sb.WriteString(matchedSkill.Prompt)
			sb.WriteString("\n\n")
		}
	}

	return sb.String()
}

// Stop aborts the currently running agent loop by cancelling its context.
func (a *Agent) Stop() {
	a.cancelMu.Lock()
	defer a.cancelMu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
}

// ForceSummarize manually triggers conversation summarization.
func (a *Agent) ForceSummarize() (string, error) {
	if a.summarizer == nil {
		return "", fmt.Errorf("summarization is not enabled in config")
	}
	if a.conv.Len() <= 2 {
		return "", fmt.Errorf("not enough conversation history to summarize")
	}

	oldCount := a.conv.Len()
	newMessages, err := a.summarizer.Summarize(context.Background(), a.conv.All())
	if err != nil {
		return "", err
	}

	var summaryText string
	for _, m := range newMessages {
		if m.Role == "system" && strings.HasPrefix(m.Content, "[Previous conversation summary]") {
			summaryText = strings.TrimPrefix(m.Content, "[Previous conversation summary]\n")
			break
		}
	}

	a.conv.Reset(newMessages)
	a.debugLog("Summarize", "compressed %d → %d messages", oldCount, len(newMessages))
	return summaryText, nil
}

// LastRunUsage returns the accumulated token usage from the most recent Run() call.
func (a *Agent) LastRunUsage() llm.Usage { return a.runUsage }

// Run executes a single user turn through the agent loop (text-only shorthand).
func (a *Agent) Run(ctx context.Context, userInput string, streamCb llm.StreamCallback) (string, error) {
	return a.RunWithParts(ctx, userInput, nil, streamCb)
}

// RunWithParts executes a single user turn. If parts is non-nil the user message
// is sent as a multimodal content array; otherwise userInput is used as plain text.
func (a *Agent) RunWithParts(ctx context.Context, userInput string, parts []llm.ContentPart, streamCb llm.StreamCallback) (string, error) {
	a.finishTool.Reset()
	a.runUsage = llm.Usage{}

	// Hot-reload: refresh skill list from deps on every run.
	// This is a cheap pointer swap; WatchSkills goroutine does the actual I/O.
	if a.deps != nil {
		current := a.deps.GetSkills()
		a.skills = current
		if a.router != nil {
			a.router.skills = current
		}
	}

	// Create a cancellable context for this run.
	ctx, cancel := context.WithCancel(ctx)
	a.cancelMu.Lock()
	a.cancel = cancel
	a.cancelMu.Unlock()
	defer cancel()

	// --- Skill selection ---
	isFirstTurn := a.conv.Len() == 0

	var matchedSkill *skills.Skill
	actualInput := userInput

	if !a.Mode.IsSubAgent {
		matchedSkill, actualInput = a.parseSkillCommand(userInput)
		if matchedSkill != nil {
			a.debugLog("Skill", "explicit command: %s (remaining: %q)", matchedSkill.Name, actualInput)
			if actualInput == "" {
				actualInput = "Please proceed based on the skill instructions."
			}
		}
	}

	// Add user message. Use actualInput so the /skill-name prefix is stripped.
	userMsg := llm.Message{Role: "user", Content: actualInput}
	if len(parts) > 0 {
		userMsg.Parts = parts
	}
	if isFirstTurn {
		a.conv.Reset([]llm.Message{userMsg})
	} else {
		msgs := a.conv.All()
		if len(msgs) > 0 {
			lastMsg := msgs[len(msgs)-1]
			if lastMsg.Role == "user" && lastMsg.Content == actualInput && len(lastMsg.Parts) == 0 && len(userMsg.Parts) == 0 {
				a.conv.Splice(len(msgs)-1, len(msgs), userMsg)
			} else {
				a.conv.Append(userMsg)
			}
		} else {
			a.conv.Append(userMsg)
		}
	}

	// Proactively summarize if history is getting long (before LLM call).
	a.trySummarize(ctx)

	// --- Router phase ---
	// Runs before system-prompt finalisation so skill selection can influence
	// the first-turn system prompt.
	var routerResult *RouterResult

	if a.router != nil && !a.Mode.IsSubAgent {
		if matchedSkill != nil {
			tc := normalizeTier(matchedSkill.Tier)
			if tc == "" {
				if matchedSkill.IsPipeline() {
					tc = TierStrong
				} else {
					tc = TierMedium
				}
			}
			routerResult = &RouterResult{
				Tier:          tc,
				RequiredTools: matchedSkill.RequiredTools,
				Reasoning:     "explicit skill selection",
			}
			a.debugLog("Router", "skipped (explicit skill %q) tier=%s", matchedSkill.Name, tc)
		} else {
			toolsForRouter := a.registry.ListExcept("finish_task", "memory_store")
			var err error
			routerResult, err = a.router.Route(ctx, actualInput, toolsForRouter, a.conv.All())
			if err != nil {
				a.debugLog("Router", "failed, using all tools: %s", err)
			} else {
				a.debugLog("Router", "tier=%s skill=%q tools=%v",
					routerResult.Tier, routerResult.Skill, routerResult.RequiredTools)
				if routerResult.Skill != "" {
					matchedSkill = a.findSkillByName(routerResult.Skill)
					if matchedSkill != nil {
						a.debugLog("Skill", "router selected: %s", matchedSkill.Name)
						if matchedSkill.Tier != "" {
							if tc := normalizeTier(matchedSkill.Tier); tc != "" {
								routerResult.Tier = tc
								a.debugLog("Router", "skill tier override → %s", tc)
							}
						}
					}
				}
			}
		}
	}

	// --- Planner: create a skill only for explicitly recurring workflows with no match ---
	if a.router != nil && !a.Mode.IsSubAgent &&
		routerResult != nil && routerResult.Tier == TierStrong &&
		routerResult.Skill == "" && matchedSkill == nil &&
		routerResult.Checks.IsRecurringWorkflow {
		if factory, ok := a.deps.(*AgentFactory); ok {
			docsDir := filepath.Join(factory.Config.AgeAgeDirPath(), "docs")
			planner := NewPlanner(factory, docsDir, factory.GetStandardToolNames())
			if skill, err := planner.CreateSkill(ctx, actualInput, nil); err == nil && skill != nil {
				matchedSkill = skill
				routerResult.Skill = skill.Name
				if skill.Tier != "" {
					if tc := normalizeTier(skill.Tier); tc != "" {
						routerResult.Tier = tc
					}
				}
				a.debugLog("Planner", "created skill %q", skill.Name)
				if a.Callbacks.Notify != nil {
					desc := skill.Description
					if desc == "" {
						desc = "(no description)"
					}
					a.Callbacks.Notify(fmt.Sprintf(
						"🔧 Created skill **%s** — %s\nInvoke later with `/%s` or describe a matching task.",
						skill.CommandName(), desc, skill.CommandName()))
				}
			} else if err != nil {
				a.debugLog("Planner", "skill creation failed (%s) — proceeding without skill", err)
				if a.Callbacks.Notify != nil {
					a.Callbacks.Notify(fmt.Sprintf(
						"⚠️ Could not create a reusable skill for this task: %s — running it as a one-off.", err))
				}
			}
		}
	}

	// --- System prompt initialization/refresh ---
	// Sub-agents have their system prompt pre-built by the caller and must not be overwritten.
	if a.conv.HasSystem() {
		if !a.Mode.IsSubAgent {
			a.conv.SetSystemContent(a.buildSystemPrompt(matchedSkill))
		}
	} else {
		a.conv.PrependSystem(llm.Message{
			Role:    "system",
			Content: a.buildSystemPrompt(matchedSkill),
		})
	}

	// --- Pipeline skill handling ---
	if matchedSkill != nil && matchedSkill.IsPipeline() {
		result, nodeSummary, err := a.runPipelineSkill(ctx, matchedSkill, actualInput)
		if err != nil {
			if ctx.Err() != nil {
				return "(Task stopped by user)", nil
			}
			if matchedSkill.AutoGenerated {
				a.triggerEvaluator(matchedSkill, actualInput, "Agent error: "+err.Error(), nil, err)
			}
			return "", err
		}
		a.conv.Append(llm.Message{Role: "assistant", Content: result})
		a.trySummarize(ctx)
		a.gcTmp()
		if streamCb != nil {
			streamCb(result)
		}
		if matchedSkill.AutoGenerated {
			a.triggerEvaluator(matchedSkill, actualInput, result, nil, nil, nodeSummary)
		}
		return result, nil
	}

	// Phase 1: inject skill-only tools for the duration of this run.
	cleanup := a.injectSkillTools(matchedSkill)
	defer cleanup()

	// Phase 2: select tool set and LLM client.
	toolDefs, activeClient, upgradeUsed := a.buildExecPlan(routerResult, matchedSkill)

	// Phase 3: execute the main loop.
	defer a.gcTmp()
	result, err := a.runLoop(ctx, streamCb, toolDefs, activeClient, upgradeUsed)
	if matchedSkill != nil && matchedSkill.AutoGenerated && ctx.Err() == nil {
		agentOutput := result
		if err != nil {
			agentOutput = "Agent error: " + err.Error()
		}
		a.triggerEvaluator(matchedSkill, actualInput, agentOutput, a.conv.ToolHistory(), err)
	}
	return result, err
}

// injectSkillTools registers skill-only tools for the duration of one Run call.
// Returns a cleanup function that removes those tools and closes any browser session.
func (a *Agent) injectSkillTools(skill *skills.Skill) func() {
	var injected []string
	if !a.Mode.IsSubAgent && a.deps != nil && skill != nil {
		for _, toolName := range skill.RequiredTools {
			if mkTool, ok := skillOnlyToolFactories[toolName]; ok {
				if _, exists := a.registry.Get(toolName); !exists {
					if t := mkTool(a.deps, a.registry, a); t != nil {
						a.registry.Register(t)
						injected = append(injected, toolName)
						a.debugLog("Skill", "injected %s", toolName)
					}
				}
			}
		}
	}
	return func() {
		for _, name := range injected {
			a.registry.Unregister(name)
		}
		if a.browserSess != nil {
			a.browserSess.Close()
		}
	}
}

// buildExecPlan selects the tool set and LLM client for a run based on the
// router result and active skill.
// Returns toolDefs, the initial LLM client, and whether an upgraded model was used.
func (a *Agent) buildExecPlan(rr *RouterResult, skill *skills.Skill) (toolDefs []llm.ToolDef, activeClient *llm.Client, upgradeUsed bool) {
	activeClient = a.client

	// Collect the tool list for this run.
	var neededTools []string
	switch {
	case rr != nil && rr.Tier == TierBase && len(rr.RequiredTools) == 0:
		// Pure knowledge Q&A: only finish_task and optionally memory_recall.
		if t, ok := a.registry.Get("memory_recall"); ok {
			recall, isRecall := t.(*tools.MemoryRecallTool)
			if !isRecall || recall.HasMemories() {
				neededTools = []string{"memory_recall"}
			}
		}
	case rr != nil && len(rr.RequiredTools) > 0:
		// Router specified tools as a starting set (hint, not hard limit).
		neededTools = append(neededTools, rr.RequiredTools...)
	default:
		// No router, or router ran without restricting tools.
		neededTools = a.registry.List()
	}

	if skill != nil {
		neededTools = append(neededTools, skill.RequiredTools...)
	}

	neededTools = ensureFinishTask(neededTools)

	// Delegation tool injection (main agent only; sub-agents must not recurse).
	if !a.Mode.IsSubAgent {
		if a.router != nil {
			if rr != nil && (rr.Tier == TierMedium || rr.Tier == TierStrong) {
				neededTools = append(neededTools, "delegate")
			}
		} else {
			neededTools = append(neededTools, "delegate")
		}
	}

	// web_search implies web_fetch: the agent needs to open pages it finds.
	for _, t := range neededTools {
		if t == "web_search" {
			neededTools = append(neededTools, "web_fetch")
			break
		}
	}

	neededTools = UniqueStrings(neededTools)
	toolDefs = a.registry.ToOpenAIToolsFiltered(neededTools)

	// Model upgrade based on router tier.
	if rr != nil {
		var targetModel config.ModelConfig
		if rr.Tier == TierStrong && a.cfg.Router.StrongModel.Model != "" {
			targetModel = a.cfg.Router.StrongModel
		} else if rr.Tier == TierMedium && a.cfg.Router.MediumModel.Model != "" {
			targetModel = a.cfg.Router.MediumModel
		}
		if targetModel.Model != "" {
			modelName, apiKey, baseURL := targetModel.Resolve(a.cfg.LLM.Model, a.client.APIKey(), a.client.BaseURL())
			activeClient = llm.NewClient(apiKey, baseURL, modelName, a.debug, a.cfg.LLM.MaxTokens)
			upgradeUsed = true
			a.debugLog("Router", "upgrade → %s", modelName)
		}
	}

	return
}

// runLoop executes the main LLM ↔ tool iteration loop.
func (a *Agent) runLoop(ctx context.Context, streamCb llm.StreamCallback, toolDefs []llm.ToolDef, activeClient *llm.Client, upgradeUsed bool) (string, error) {
	fallbackUsed := false
	textOnlyStreak := 0 // consecutive responses with no tool calls and no finish_task

	// Determine the finish tool name for hint messages.
	hintFinishName := "finish_task"
	if _, ok := a.registry.Get("node_complete"); ok {
		hintFinishName = "node_complete"
	}

	for i := 0; i < a.MaxIterations; i++ {
		if ctx.Err() != nil {
			return "(Task stopped by user)", nil
		}

		a.debugSeparator(i + 1)

		var assistantMsg *llm.Message
		var err error

		callMessages := a.buildCallMessages()

		type toolExecResult struct {
			result  string
			execErr error
		}
		inflightResults := make(map[int]chan toolExecResult)

		maxPar := a.cfg.Agent.MaxParallelTools
		if maxPar <= 0 {
			maxPar = 1
		}
		sem := make(chan struct{}, maxPar)

		// dispatchTool executes a single tool call.
		dispatchTool := func(tc llm.ToolCall) (string, error) {
			var rawArgs json.RawMessage
			if err2 := jsonutil.ParseToolArgs(tc.Function.Arguments, &rawArgs); err2 != nil {
				rawArgs = json.RawMessage(tc.Function.Arguments)
			}

			a.debugLog("Tool▷", "%s  %s", tc.Function.Name, briefActionSummary(tc.Function.Name, tc.Function.Arguments))
			dispatcher := NewToolDispatcher(a.registry, a.CredMgr)
			res, execErr := dispatcher.Execute(ctx, tc.Function.Name, rawArgs, ToolDispatchHooks{
				Start: a.Callbacks.ToolStart,
				End:   a.Callbacks.ToolEnd,
			})
			if a.Callbacks.ToolResult != nil {
				a.Callbacks.ToolResult(tc.Function.Name, res)
			}
			a.debugLog("Tool◁", "%s  %s", tc.Function.Name, truncateStr(res, 600))
			a.debugBlankLine()
			return res, execErr
		}

		// For streaming calls, start read-only tools progressively.
		var toolCallStreamCb llm.ToolCallStreamCb
		if streamCb != nil && maxPar > 1 {
			toolCallStreamCb = func(idx int, call llm.ToolCall) {
				if !isReadOnlyTool(call.Function.Name) {
					return
				}
				ch := make(chan toolExecResult, 1)
				inflightResults[idx] = ch
				go func() {
					sem <- struct{}{}
					defer func() { <-sem }()
					res, execErr := dispatchTool(call)
					ch <- toolExecResult{res, execErr}
				}()
			}
		}

		if streamCb != nil {
			var u *llm.Usage
			assistantMsg, u, err = activeClient.ChatCompletionStream(
				ctx, callMessages, toolDefs, a.cfg.LLM.Temperature, streamCb, toolCallStreamCb,
			)
			if u != nil {
				a.runUsage.PromptTokens += u.PromptTokens
				a.runUsage.CompletionTokens += u.CompletionTokens
				a.runUsage.TotalTokens += u.TotalTokens
			}
		} else {
			resp, rerr := activeClient.ChatCompletion(
				ctx, callMessages, toolDefs, a.cfg.LLM.Temperature,
			)
			if rerr != nil {
				err = rerr
			} else if len(resp.Choices) > 0 {
				assistantMsg = &resp.Choices[0].Message
				if resp.Usage != nil {
					a.runUsage.PromptTokens += resp.Usage.PromptTokens
					a.runUsage.CompletionTokens += resp.Usage.CompletionTokens
					a.runUsage.TotalTokens += resp.Usage.TotalTokens
				}
			} else {
				err = fmt.Errorf("empty response from LLM")
			}
		}

		if err != nil {
			if ctx.Err() != nil {
				return "(Task stopped by user)", nil
			}
			if upgradeUsed && !fallbackUsed {
				a.debugLog("Router", "fallback → %s", a.cfg.LLM.Model)
				activeClient = a.client
				upgradeUsed = false
				fallbackUsed = true
				i--
				continue
			}
			return "", fmt.Errorf("LLM call failed at iteration %d: %w", i+1, err)
		}

		// Serialize the entire batch if any tool has side effects.
		hasMutation := false
		for _, tc := range assistantMsg.ToolCalls {
			if !isReadOnlyTool(tc.Function.Name) {
				hasMutation = true
				break
			}
		}
		if hasMutation {
			maxPar = 1
			sem = make(chan struct{}, maxPar)
			// Wait for all pre-started read-only goroutines before dispatching
			// any mutation tool, so mutations never run in parallel with them.
			for _, ch := range inflightResults {
				res := <-ch
				ch <- res // put result back for the collection loop
			}
		}

		turnStart := a.conv.Len()
		cleanedMsg := *assistantMsg
		cleanedMsg.Content = sanitizeOutput(cleanedMsg.Content)
		a.conv.Append(cleanedMsg)

		if len(cleanedMsg.ToolCalls) == 0 {
			textOnlyStreak++
			if textOnlyStreak == 1 {
				// First bare-text response with no tool calls: nudge the agent.
				a.hintOnNextCall = "[Framework] You replied with text but did not call " +
					hintFinishName + " or use any tool. " +
					"If this is your final answer, call " + hintFinishName +
					" and put your ACTUAL reply text in the summary field. " +
					"The summary MUST contain the COMPLETE answer with all data and content. " +
					"A summary saying only 'completed' / 'task done' is NOT acceptable " +
					"— the user needs the actual information. " +
					"If the task requires tool use, invoke the appropriate tools first."
				continue
			}
			// Second consecutive bare-text response: accept as final to prevent looping.
			a.trySummarize(ctx)
			if cleanedMsg.Content != "" {
				return cleanedMsg.Content, nil
			}
			return "(Agent returned empty response)", nil
		}
		textOnlyStreak = 0

		// Dispatch tool calls not yet started progressively.
		for idx, tc := range cleanedMsg.ToolCalls {
			if _, alreadyStarted := inflightResults[idx]; alreadyStarted {
				continue
			}
			tc := tc
			ch := make(chan toolExecResult, 1)
			inflightResults[idx] = ch
			if maxPar > 1 {
				go func() {
					sem <- struct{}{}
					defer func() { <-sem }()
					res, execErr := dispatchTool(tc)
					ch <- toolExecResult{res, execErr}
				}()
			} else {
				res, execErr := dispatchTool(tc)
				ch <- toolExecResult{res, execErr}
			}
		}

		var currentTurnResults []string
		anyToolError := false
		for idx, tc := range cleanedMsg.ToolCalls {
			res := <-inflightResults[idx]

			toolResult := res.result
			if res.execErr != nil {
				anyToolError = true
				toolResult = fmt.Sprintf("Error: %s", res.execErr.Error())
				if res.result != "" {
					toolResult = fmt.Sprintf("Error: %s\nPartial output: %s", res.execErr.Error(), res.result)
				}
			}

			historyResult := compactToolResult(toolResult)
			toolCallID := tc.ID
			if toolCallID == "" {
				toolCallID = fmt.Sprintf("%s_%d", tc.Function.Name, idx)
			}
			a.conv.Append(llm.Message{
				Role:       "tool",
				Content:    historyResult,
				ToolCallID: toolCallID,
			})
			currentTurnResults = append(currentTurnResults, toolResult)
		}

		if anyToolError && !a.Mode.IsSubAgent {
			a.hintOnNextCall = `A tool call above returned an error. ` +
				`Read .ageage/docs/troubleshooting.md to diagnose common failure causes before retrying.`
		}

		if a.finishTool.Finished {
			a.conv.Append(llm.Message{
				Role:    "assistant",
				Content: a.finishTool.Summary,
			})
			a.trySummarize(ctx)
			if a.todoStore != nil && a.todoStore.IsComplete() {
				a.todoStore.Clear()
			}
			return a.finishTool.Summary, nil
		}

		if ctx.Err() != nil {
			return "(Task stopped by user)", nil
		}

		if a.cfg.History.CompressToolTurns {
			a.pendingTurns = append(a.pendingTurns, turnRecord{
				assistantMsg: cleanedMsg,
				toolResults:  currentTurnResults,
				msgStart:     turnStart,
				msgCount:     1 + len(cleanedMsg.ToolCalls),
			})
			keepRecentTurns := a.cfg.History.KeepRecentTurns
			if keepRecentTurns <= 0 {
				keepRecentTurns = 2
			}
			for len(a.pendingTurns) > keepRecentTurns {
				a.compressOldestTurn()
			}
		}
	}

	return "", fmt.Errorf("agent reached maximum iterations (%d) without completing the task", a.MaxIterations)
}

// runPipelineSkill executes a YAML-based pipeline skill.
// Requires deps to be an *AgentFactory because pipeline nodes need LLM client
// and debug flag access that are not part of the AgentDeps interface.
// Returns the result, node summary (for the evaluator), and any error.
func (a *Agent) runPipelineSkill(ctx context.Context, skill *skills.Skill, input string) (result, nodeSummary string, err error) {
	factory, ok := a.deps.(*AgentFactory)
	if !ok {
		return "", "", fmt.Errorf("pipeline skills require an AgentFactory")
	}
	exec := NewPipelineExecutor(
		skill.Pipeline,
		skill,
		factory,
		input,
		a.SessionDir,
		0,
		a.Callbacks.TodoSend,
		a.Callbacks.TodoEdit,
		a.Callbacks.Notify,
		a.Callbacks.AskUser,
		a.ConfirmationMgr,
		a.currentChannelID,
		a.registry,
	)
	result, err = exec.Run(ctx)
	if err != nil {
		return "", "", err
	}
	return result, exec.NodeSummary(), nil
}

// gcTmp removes managed tmp files no longer referenced in the message history.
func (a *Agent) gcTmp() {
	if a.tmpMgr != nil {
		a.tmpMgr.GC(a.conv.All())
	}
}

// --- Output sanitization ---

var thinkTagPairs = [][2]string{
	{"<think>", "</think>"},
	{"<thought>", "</thought>"},
}

func sanitizeOutput(s string) string {
	s = stripLeadingThinkBlocks(s)
	s = convertLatex(s)
	return strings.TrimSpace(s)
}

func stripLeadingThinkBlocks(s string) string {
	for {
		t := strings.TrimLeft(s, " \t\n\r")
		found := false
		for _, pair := range thinkTagPairs {
			if !strings.HasPrefix(t, pair[0]) {
				continue
			}
			closeIdx := strings.Index(t, pair[1])
			if closeIdx == -1 {
				return ""
			}
			s = t[closeIdx+len(pair[1]):]
			found = true
			break
		}
		if !found {
			break
		}
	}
	return s
}

// readOnlyTools is the set of tool names guaranteed to have no side effects.
var readOnlyTools = map[string]bool{
	"file_read":     true,
	"web_fetch":     true,
	"web_search":    true,
	"memory_recall": true,
	"glob":          true,
	"grep":          true,
	"cron_list":     true,
}

func isReadOnlyTool(name string) bool { return readOnlyTools[name] }

// --- Debug helpers ---

var debugIcons = map[string]string{
	"Router":    "◆",
	"Tool▷":     "▷",
	"Tool◁":     "◁",
	"Summarize": "⟳",
	"History":   "⊞",
	"Delegate":  "⤷",
	"Skill":     "◈",
}

func (a *Agent) debugSeparator(iteration int) {
	if !a.debug {
		return
	}
	label := fmt.Sprintf(" Turn %d ", iteration)
	pad := 52 - len(label)
	if pad < 4 {
		pad = 4
	}
	fmt.Printf("\n── %s%s\n", label, strings.Repeat("─", pad))
}

const debugIndent = "                "

func (a *Agent) debugLog(category, format string, args ...interface{}) {
	if !a.debug {
		return
	}
	msg := fmt.Sprintf(format, args...)
	icon, ok := debugIcons[category]
	if !ok {
		icon = "·"
	}
	msg = strings.ReplaceAll(msg, "\n", "\n"+debugIndent)
	fmt.Printf("  %s  %-10s %s\n", icon, category, msg)
}

func (a *Agent) debugBlankLine() {
	if a.debug {
		fmt.Println()
	}
}

// --- Token optimization ---

const maxToolResultInHistory = 4000

func compactToolResult(result string) string {
	runes := []rune(result)
	if len(runes) <= maxToolResultInHistory {
		return result
	}
	return string(runes[:maxToolResultInHistory]) + "\n\n[... output truncated for token efficiency. Key information is above.]"
}

func ensureFinishTask(toolNames []string) []string {
	for _, n := range toolNames {
		if n == "finish_task" {
			return toolNames
		}
	}
	return append(toolNames, "finish_task")
}

// compressOldestTurn replaces the oldest pending turn's messages with a single
// narrative assistant message, updating pending-turn indices accordingly.
func (a *Agent) compressOldestTurn() {
	if len(a.pendingTurns) == 0 {
		return
	}
	oldest := a.pendingTurns[0]
	a.pendingTurns = a.pendingTurns[1:]

	start := oldest.msgStart
	end := start + oldest.msgCount
	if end > a.conv.Len() {
		return
	}

	narrative := buildTurnNarrative(oldest.assistantMsg, oldest.toolResults)
	compressed := llm.Message{Role: "assistant", Content: narrative}

	removed := a.conv.Splice(start, end, compressed)
	for i := range a.pendingTurns {
		a.pendingTurns[i].msgStart -= removed
	}

	a.debugLog("History", "%d messages → 1 narrative", oldest.msgCount)
}

func buildTurnNarrative(assistantMsg llm.Message, toolResults []string) string {
	var sb strings.Builder
	if assistantMsg.Content != "" {
		sb.WriteString(assistantMsg.Content)
		sb.WriteString("\n\n")
	}
	for i, tc := range assistantMsg.ToolCalls {
		result := toolResults[i]
		const maxResultLen = 1500
		if runes := []rune(result); len(runes) > maxResultLen {
			result = string(runes[:maxResultLen]) + "… (truncated)"
		}
		action := briefActionSummary(tc.Function.Name, tc.Function.Arguments)
		fmt.Fprintf(&sb, "%d. %s\n   → %s\n", i+1, action, result)
	}
	return sb.String()
}

func briefActionSummary(toolName, rawArgs string) string {
	param := extractPrimaryArg(rawArgs)

	switch toolName {
	case "bash":
		if param != "" {
			return "Ran: " + param
		}
	case "file_read":
		if param != "" {
			return "Read: " + param
		}
	case "file_write":
		if param != "" {
			return "Wrote: " + param
		}
	case "file_edit":
		if param != "" {
			return "Edited: " + param
		}
	case "web_search":
		if param != "" {
			return "Searched: " + param
		}
	case "web_fetch":
		if param != "" {
			return "Fetched: " + param
		}
	case "memory_store":
		if param != "" {
			return "Stored: " + param
		}
	case "memory_recall":
		if param != "" {
			return "Recalled: " + param
		}
	case "file_list":
		if param != "" {
			return "Listed: " + param
		}
	case "glob":
		if param != "" {
			return "Globbed: " + param
		}
	case "grep":
		if param != "" {
			return "Grepped: " + param
		}
	case "finish_task", "node_complete":
		if param != "" {
			return "Finished: " + param
		}
		return "Finished task"
	}
	if param != "" {
		return toolName + ": " + param
	}
	return toolName
}

func extractPrimaryArg(rawArgs string) string {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(rawArgs), &m); err != nil {
		if len(rawArgs) > 120 {
			return rawArgs[:120] + "…"
		}
		return rawArgs
	}

	priority := []string{"command", "path", "query", "url", "task", "key", "content", "expression", "name"}
	for _, key := range priority {
		if v, ok := m[key]; ok {
			s := fmt.Sprintf("%v", v)
			if len(s) > 120 {
				s = s[:120] + "…"
			}
			return s
		}
	}

	for _, v := range m {
		if s, ok := v.(string); ok {
			if len(s) > 120 {
				s = s[:120] + "…"
			}
			return s
		}
	}
	return ""
}

// UniqueStrings returns a slice with duplicate strings removed, preserving order.
func UniqueStrings(input []string) []string {
	seen := make(map[string]struct{}, len(input))
	out := make([]string, 0, len(input))
	for _, s := range input {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

func truncateStr(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// buildCallMessages returns the message slice to send to the LLM this turn.
// Appends a temporary context message (time, workspace, todos) at the end
// without touching the stored conversation — this keeps prior messages
// byte-identical across turns, maximising KV cache reuse.
func (a *Agent) buildCallMessages() []llm.Message {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<context>Time: %s | Workspace: %s | OS: %s | Arch: %s</context>",
		time.Now().Format("2006-01-02 15:04:05 MST (Monday)"),
		a.cfg.EffectiveWorkDir(),
		runtime.GOOS,
		runtime.GOARCH,
	)
	if a.todoStore != nil {
		if todos := a.todoStore.Format(); todos != "" {
			fmt.Fprintf(&sb, "\n<todos>\n%s</todos>", todos)
		}
	}

	if a.hintOnNextCall != "" {
		fmt.Fprintf(&sb, "\n[Framework] %s", a.hintOnNextCall)
		a.hintOnNextCall = ""
	}

	ctxStr := sb.String()
	out := a.conv.Snapshot()

	if len(out) > 0 && out[len(out)-1].Role == "user" {
		lastIdx := len(out) - 1
		if len(out[lastIdx].Parts) > 0 {
			out[lastIdx].Parts = append(append([]llm.ContentPart{}, out[lastIdx].Parts...), llm.ContentPart{
				Type: "text",
				Text: "\n\n" + ctxStr,
			})
		} else {
			out[lastIdx].Content += "\n\n" + ctxStr
		}
	} else {
		out = append(out, llm.Message{Role: "user", Content: ctxStr})
	}

	return out
}

// trySummarize attempts to compress conversation history if threshold is exceeded.
func (a *Agent) trySummarize(ctx context.Context) {
	if a.summarizer == nil {
		return
	}
	if !a.summarizer.ShouldSummarize(a.conv.All()) {
		return
	}

	oldCount := a.conv.Len()
	newMessages, err := a.summarizer.Summarize(ctx, a.conv.All())
	if err != nil {
		a.debugLog("Summarize", "failed: %s", err)
		return
	}

	a.conv.Reset(newMessages)
	// pendingTurns hold indices into the old message slice; clear them so
	// compressOldestTurn doesn't iterate over invalid records.
	a.pendingTurns = nil
	a.debugLog("Summarize", "compressed %d → %d messages", oldCount, len(newMessages))
}

// GetRegistry returns the agent's tool registry.
func (a *Agent) GetRegistry() *tools.Registry {
	return a.registry
}

// TmpManager returns the agent's tmp file manager (for CLI attachment processing).
func (a *Agent) TmpManager() *TmpManager { return a.tmpMgr }

// LastTurnUserMessage returns the last user message in the conversation history.
func (a *Agent) LastTurnUserMessage() (llm.Message, bool) {
	msgs := a.conv.All()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i], true
		}
	}
	return llm.Message{}, false
}

// RollbackLastTurn removes the last complete user→assistant exchange from history.
func (a *Agent) RollbackLastTurn() int {
	msgs := a.conv.All()
	lastUserIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return 0
	}

	removed := len(msgs) - lastUserIdx
	a.conv.TruncateTo(lastUserIdx)

	kept := a.pendingTurns[:0:0]
	for _, pt := range a.pendingTurns {
		if pt.msgStart < lastUserIdx {
			kept = append(kept, pt)
		}
	}
	a.pendingTurns = kept

	return removed
}

// ClearHistory resets the conversation history, pending turn queue, and todo list.
func (a *Agent) ClearHistory() {
	a.conv.Reset(nil)
	a.pendingTurns = nil
	a.hintOnNextCall = ""
	a.finishTool.CheckTodos = nil
	if a.todoStore != nil {
		a.todoStore.Clear()
		a.todoStore = nil
	}
	if a.tmpMgr != nil {
		a.tmpMgr.ClearAll()
	}
}

// SetChannelID sets the current channel ID for async confirmations.
func (a *Agent) SetChannelID(channelID string) {
	a.currentChannelID = channelID
}

// SetLLMClient overrides the LLM client for this agent.
func (a *Agent) SetLLMClient(client *llm.Client) {
	a.client = client
	if a.summarizer != nil {
		a.summarizer.SetClient(client)
	}
}

// GetChannelID returns the current channel ID.
func (a *Agent) GetChannelID() string {
	return a.currentChannelID
}

// AddHistory adds historical messages to the conversation context without
// triggering agent execution.
func (a *Agent) AddHistory(userInput, assistantReply string) {
	if a.conv.Len() == 0 {
		a.conv.Append(llm.Message{
			Role:    "system",
			Content: a.buildSystemPrompt(nil),
		})
	}

	if userInput != "" {
		a.conv.Append(llm.Message{Role: "user", Content: userInput})
	}

	if assistantReply != "" {
		a.conv.Append(llm.Message{Role: "assistant", Content: sanitizeOutput(assistantReply)})
	}

	a.trySummarize(context.Background())
}
