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

// turnRecord tracks an uncompressed tool-call turn so it can be compressed later.
type turnRecord struct {
	assistantMsg llm.Message
	toolResults  []string
	msgStart     int // index in a.messages where the assistant message lives
	msgCount     int // 1 (assistant) + N (tool results)
}

// Agent is the core agent that drives the conversation and tool execution loop.
type Agent struct {
	cfg              *config.Config
	client           *llm.Client
	registry         *tools.Registry
	finishTool       *tools.FinishTool
	router           *Router
	summarizer       *Summarizer
	skills           []skills.Skill
	messages         []llm.Message
	pendingTurns     []turnRecord // Recent uncompressed tool-call turns
	debug            bool
	cancel           context.CancelFunc
	cancelMu         sync.Mutex
	currentChannelID string                         // For async confirmations in channel mode
	ConfirmationMgr  *tools.ConfirmationManager     // Optional: for async confirmations
	NotifyFunc       func(message string)                    // Optional: for sending notifications to the channel
	AskUserNotify    func(question string, options []string) // Optional: send ask_user question to the user
	TodoSendFunc     func(text string) string                // Optional: send a todo notification, returns message ID
	TodoEditFunc     func(msgID, text string) error          // Optional: edit a previously sent todo notification
	InjectSoul          bool                           // If true, SOUL.md personality is injected (serve/connect default true; CLI default false)
	IsSubAgent          bool                           // If true, this is a sub-agent (disables pre-execution and skill-only tool injection)
	InjectContext       bool                           // If true, .ageage/CONTEXT.md is injected into the system prompt (default true for main agent and pipeline nodes)
	SessionDir          string                         // Directory for the active session (e.g. .ageage/sessions/default); CONTEXT.md is read from here
	MaxIterations       int                            // Maximum iterations for this agent run
	factory             *AgentFactory                  // Back-reference used for per-turn skill tool injection; nil for manually created agents
	todoStore           *tools.TodoStore               // Non-nil when update_todos is injected; cleared after finish_task
	browserSess         *tools.BrowserSession          // Non-nil when browser_* tools are injected; closed after Run
	runUsage            llm.Usage                      // Accumulated token usage for the current Run(); reset each call
	tmpMgr              *TmpManager                    // Manages tmp files created by attachment converters
	ToolStartCallback   func(name, args string)         // Optional: called just before each tool executes (CLI spinner/diff display)
	ToolEndCallback     func(name string)               // Optional: called just after each tool completes
	ToolResultCallback  func(name, result string)       // Optional: called with the tool's output after execution
	CredMgr             *creds.Manager                  // Optional: substitutes {{cred:x}} in tool args, scrubs results
	hintOnNextCall      string                          // Ephemeral; consumed by buildCallMessages, not stored in history
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
		messages:      make([]llm.Message, 0, 64),
		tmpMgr:        newTmpManager(cfg.ConfigDir()),
		InjectContext: true, // on by default; explicitly disabled for delegate sub-agents
	}

	if cfg.Summarize.Enabled {
		ag.summarizer = NewSummarizer(cfg, client, debug)
	}

	return ag
}

// contextMDPath returns the CONTEXT.md path for the agent's active session.
// When SessionDir is set (session-aware usage) it points to the session's own
// CONTEXT.md; otherwise it falls back to the workspace-level path.
func (a *Agent) contextMDPath() string {
	if a.SessionDir != "" {
		return filepath.Join(a.SessionDir, "CONTEXT.md")
	}
	return a.cfg.ContextMDPath()
}

// BuildSystemPrompt returns the system prompt string for the current agent
// state without a specific skill active. Used when restoring history so the
// fresh context (CONTEXT.md) is embedded in a new system message.
func (a *Agent) BuildSystemPrompt() string {
	return a.buildSystemPrompt(nil)
}

// Messages returns a snapshot of the agent's current message history.
func (a *Agent) Messages() []llm.Message {
	return a.messages
}

// SetMessages replaces the agent's message history with the supplied slice.
// If msgs is non-empty and the first entry is not a system message, a fresh
// system prompt is prepended automatically. This is the correct way to restore
// conversation history when resuming a session.
func (a *Agent) SetMessages(msgs []llm.Message) {
	if len(msgs) == 0 {
		a.messages = msgs
		return
	}
	if msgs[0].Role != "system" {
		sysMsg := llm.Message{Role: "system", Content: a.buildSystemPrompt(nil)}
		msgs = append([]llm.Message{sysMsg}, msgs...)
	}
	a.messages = msgs
}

// parseSkillCommand checks whether input begins with a /skill-name command.
// If a matching skill is found, it returns a pointer to it and the remaining
// text after the command token (which becomes the actual user message).
// Returns (nil, input) unchanged if the input is not a skill command or the
// name does not match any loaded skill.
//
// Matching is normalised: lowercase, spaces/underscores → hyphens.
// e.g.  "/code-review fix the auth module" → skill "code_review", "fix the auth module"
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

	// Core system instructions (English as per spec).
	sb.WriteString("## Core Rules\n\n")
	sb.WriteString("1. You have access to tools. Use them to gather information and perform actions.\n")

	// The finish tool name is usually finish_task, but pipeline nodes replace it
	// with node_complete to report structured results.
	finishToolName := "finish_task"
	isPipelineAgent := false
	if _, ok := a.registry.Get("node_complete"); ok {
		finishToolName = "node_complete"
		isPipelineAgent = true
	}
	fmt.Fprintf(&sb, "2. When you have completed the task or have a FINAL answer, you MUST call the %s tool.\n", finishToolName)
	if isPipelineAgent {
		sb.WriteString("   - IMPORTANT: You MUST use this tool to return structured data. Simply replying with JSON in text is NOT allowed.\n")
	}
	sb.WriteString("3. Always respond in the same language the user uses.\n\n")

	sb.WriteString(`## Response Quality Requirements

- ALWAYS provide COMPLETE, DETAILED answers. Never say "refer to the search results" or "see above for details".
- If tool results contain the answer, rewrite it in a clear, organized format.
- Include specific data, names, numbers, and facts from tool results in your final answer.
- If information is incomplete, state what you found and what is missing.

## Security

Never output API keys, passwords, access tokens, credentials, or secrets verbatim in any response, even if asked or if they appear in tool results.

`)

	// Always inject AGENT.md (behavioral directives).
	if agentData, err := os.ReadFile(a.cfg.AgentPath()); err == nil && len(agentData) > 0 {
		sb.WriteString("## Agent Directives\n\n")
		sb.WriteString(strings.TrimSpace(string(agentData)))
		sb.WriteString("\n\n")
	}

	// Inject SOUL.md only when InjectSoul is set (serve/connect mode; off by default in CLI).
	if a.InjectSoul {
		if soulData, err := os.ReadFile(a.cfg.SOULPath()); err == nil && len(soulData) > 0 {
			sb.WriteString("## Personality & Behavior\n\n")
			sb.WriteString(strings.TrimSpace(string(soulData)))
			sb.WriteString("\n\n")
		}
	}

	// Workspace context notes — injected for main agent and pipeline nodes (InjectContext=true),
	// suppressed for delegate sub-agents (InjectContext=false).
	// Placed before skill instructions so the prefix stays stable for KV-cache reuse.
	if a.InjectContext {
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

	// Credential placeholder hint (only when credentials are configured).
	if a.CredMgr != nil {
		if hint := a.CredMgr.PromptHint(); hint != "" {
			sb.WriteString(hint)
		}
	}

	// Framework documentation pointer — main agents only, not sub-agents or pipeline nodes.
	// Stays in the stable prefix so KV-cache hits on every turn after the first.
	if !a.IsSubAgent {
		sb.WriteString("## Framework Documentation\n\n")
		sb.WriteString("Self-reference guides are in `.ageage/docs/` (use `file_read`): " +
			"how-i-work.md, troubleshooting.md, skills.md, pipeline.md.\n")
		sb.WriteString("Read them when a tool fails unexpectedly, when creating or modifying skills, " +
			"or when you need to understand how the agent loop works.\n\n")
	}

	// Active skill instructions (at most one skill per conversation).
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
// Returns the summary text or an error.
func (a *Agent) ForceSummarize() (string, error) {
	if a.summarizer == nil {
		return "", fmt.Errorf("summarization is not enabled in config")
	}
	if len(a.messages) <= 2 {
		return "", fmt.Errorf("not enough conversation history to summarize")
	}

	oldCount := len(a.messages)
	newMessages, err := a.summarizer.Summarize(context.Background(), a.messages)
	if err != nil {
		return "", err
	}

	// Extract the summary text from the new messages.
	var summaryText string
	for _, m := range newMessages {
		if m.Role == "system" && strings.HasPrefix(m.Content, "[Previous conversation summary]") {
			summaryText = strings.TrimPrefix(m.Content, "[Previous conversation summary]\n")
			break
		}
	}

	a.messages = newMessages
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

	// Hot-reload: refresh skill list from factory on every run.
	// This is a cheap pointer swap; the factory's WatchSkills goroutine does the actual I/O.
	if a.factory != nil {
		current := a.factory.GetSkills()
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
	// Skills are triggered in one of two ways:
	//   1. Explicit /skill-name command in user input (highest priority).
	//   2. Router LLM selects a skill from the full catalog (fallback).
	// Skill selection happens BEFORE the system prompt is built so that the
	// active skill's instructions can be included on the first turn.
	isFirstTurn := len(a.messages) == 0

	var matchedSkill *skills.Skill
	actualInput := userInput

	if !a.IsSubAgent {
		matchedSkill, actualInput = a.parseSkillCommand(userInput)
		if matchedSkill != nil {
			a.debugLog("Skill", "explicit command: %s (remaining: %q)", matchedSkill.Name, actualInput)
		}
	}

	// Add user message (plain text or multimodal). Use actualInput so the
	// /skill-name prefix is stripped from the stored message.
	userMsg := llm.Message{Role: "user", Content: actualInput}
	if len(parts) > 0 {
		userMsg.Parts = parts
	}

	// On the first turn we cannot build the system prompt yet (we may still
	// need the router to select a skill). Temporarily store only the user
	// message; filterHistoryForRouter ignores system messages anyway.
	if isFirstTurn {
		a.messages = []llm.Message{userMsg}
	} else {
		a.messages = append(a.messages, userMsg)
	}

	// Proactively summarize if history is getting long (before LLM call).
	a.trySummarize(ctx)

	// --- Router phase ---
	// Runs before system-prompt finalisation so the router's skill selection
	// can influence the first-turn system prompt.
	var routerResult *RouterResult

	if a.router != nil && !a.IsSubAgent {
		// If an explicit skill is active, skip the router entirely.
		// The skill overrides both skill selection and complexity.
		if matchedSkill != nil {
			tc := TaskComplexity(strings.ToLower(matchedSkill.Complexity))
			if tc == "" {
				if matchedSkill.IsPipeline() {
					tc = TaskComplex
				} else {
					tc = TaskMedium
				}
			}

			switch tc {
			case TaskSimple, TaskMedium, TaskComplex:
				// valid
			default:
				a.debugLog("Skill", "unknown complexity %q in skill %q, falling back to medium", matchedSkill.Complexity, matchedSkill.Name)
				tc = TaskMedium
			}
			routerResult = &RouterResult{
				Complexity:    tc,
				RequiredTools: matchedSkill.RequiredTools,
				Reasoning:     "explicit skill selection",
			}
			a.debugLog("Router", "skipped (explicit skill %q) complexity=%s", matchedSkill.Name, tc)
		} else {
			// Call the router. If no explicit skill was given, the router may
			// select one from its full skill catalog.
			toolsForRouter := a.registry.ListExcept("finish_task", "memory_store")
			var err error
			routerResult, err = a.router.Route(ctx, actualInput, toolsForRouter, a.messages)
			if err != nil {
				a.debugLog("Router", "failed, using all tools: %s", err)
			} else {
				a.debugLog("Router", "complexity=%s skill=%q tools=%v",
					routerResult.Complexity, routerResult.Skill, routerResult.RequiredTools)
				// Router-selected skill (only when the user did not already give one).
				if routerResult.Skill != "" {
					matchedSkill = a.findSkillByName(routerResult.Skill)
					if matchedSkill != nil {
						a.debugLog("Skill", "router selected: %s", matchedSkill.Name)
						// If the router-selected skill declares a complexity, honour it
						// as an override over the router's own complexity assessment.
						if matchedSkill.Complexity != "" {
							tc := TaskComplexity(strings.ToLower(matchedSkill.Complexity))
							switch tc {
							case TaskSimple, TaskMedium, TaskComplex:
							default:
								tc = TaskMedium
							}
							routerResult.Complexity = tc
							a.debugLog("Router", "skill complexity override → %s", tc)
						}
					}
				}
			}
		}
	}

	// --- System prompt initialization/refresh ---
	// Three cases:
	//   (a) System message already at index 0 — update in place (skill may change turn to turn).
	//       Exception: sub-agents have their system prompt pre-built by the caller with the
	//       correct nodeSkill/instructions. matchedSkill is always nil for sub-agents (skill
	//       parsing is skipped via !IsSubAgent), so overwriting would erase nodeSkill content.
	//   (b) History loaded from disk without a system message (SaveHistory never persists it) — prepend.
	//   (c) First turn, messages is empty — prepend.
	// Cases (b) and (c) are identical: both need a prepend.
	if len(a.messages) > 0 && a.messages[0].Role == "system" {
		if !a.IsSubAgent {
			a.messages[0].Content = a.buildSystemPrompt(matchedSkill)
		}
		// Sub-agent: keep the pre-built system prompt from the caller.
	} else {
		sysMsg := llm.Message{
			Role:    "system",
			Content: a.buildSystemPrompt(matchedSkill),
		}
		a.messages = append([]llm.Message{sysMsg}, a.messages...)
	}

	// --- Pipeline skill handling ---
	// Pipeline skills run entirely outside the normal agent loop: each node
	// spawns an isolated sub-agent. Return early after the pipeline completes.
	if matchedSkill != nil && matchedSkill.IsPipeline() {
		if a.factory == nil {
			return "", fmt.Errorf("pipeline skills require an AgentFactory")
		}
		result, err := a.runPipelineSkill(ctx, matchedSkill, actualInput)
		if err != nil {
			return "", err
		}
		a.messages = append(a.messages, llm.Message{
			Role:    "assistant",
			Content: result,
		})
		a.trySummarize(ctx)
		a.gcTmp()
		if streamCb != nil {
			streamCb(result)
		}
		return result, nil
	}

	// Handle simple tasks (direct answer from router, no tool loop needed).
	if routerResult != nil && routerResult.Complexity == TaskSimple && routerResult.DirectAnswer != "" {
		a.messages = append(a.messages, llm.Message{
			Role:    "assistant",
			Content: routerResult.DirectAnswer,
		})
		if streamCb != nil {
			streamCb(routerResult.DirectAnswer)
		}
		return routerResult.DirectAnswer, nil
	}

	// --- Skill-only tool injection ---
	// Instantiate and register tools that live outside the global registry.
	// These are removed again via defer at the end of this Run call.
	var injectedTools []string
	if !a.IsSubAgent && a.factory != nil && matchedSkill != nil {
		for _, toolName := range matchedSkill.RequiredTools {
			if mkTool, ok := skillOnlyToolFactories[toolName]; ok {
				if _, exists := a.registry.Get(toolName); !exists {
					a.registry.Register(mkTool(a.factory, a.registry, a))
					injectedTools = append(injectedTools, toolName)
					a.debugLog("Skill", "injected %s", toolName)
				}
			}
		}
	}
	defer func() {
		for _, name := range injectedTools {
			a.registry.Unregister(name)
		}
		if a.browserSess != nil {
			a.browserSess.Close()
			a.browserSess = nil
		}
	}()

	// Select the model and tools based on router result and active skill.
	activeClient := a.client

	// Collect the tool list for this turn.
	var neededTools []string
	if routerResult != nil && len(routerResult.RequiredTools) > 0 {
		neededTools = append(neededTools, routerResult.RequiredTools...)
	} else if routerResult != nil && matchedSkill == nil {
		// Router ran but imposed no tool restriction — use all available tools.
		neededTools = a.registry.List()
	} else if routerResult == nil && matchedSkill == nil {
		// No router, no skill: use all available tools.
		neededTools = a.registry.List()
	}

	// Always include tools declared by the active skill.
	if matchedSkill != nil {
		neededTools = append(neededTools, matchedSkill.RequiredTools...)
	}

	// ALWAYS include finish_task.
	neededTools = ensureFinishTask(neededTools)

	// Delegation tool injection (main agent only; sub-agents must not recurse).
	if !a.IsSubAgent {
		if a.router != nil {
			// With a router: only inject for medium/complex tasks.
			if routerResult != nil && (routerResult.Complexity == TaskMedium || routerResult.Complexity == TaskComplex) {
				neededTools = append(neededTools, "delegate")
			}
		} else {
			// No router: always available.
			neededTools = append(neededTools, "delegate")
		}
	}

	// web_search implies web_fetch: the agent needs to be able to open pages it finds.
	for _, t := range neededTools {
		if t == "web_search" {
			neededTools = append(neededTools, "web_fetch")
			break
		}
	}

	// De-duplicate and filter.
	neededTools = UniqueStrings(neededTools)
	toolDefs := a.registry.ToOpenAIToolsFiltered(neededTools)

	upgradeUsed := false
	if routerResult != nil {
		// Use medium/strong model if configured.
		var targetModel config.ModelConfig
		if routerResult.Complexity == TaskComplex && a.cfg.Router.StrongModel.Model != "" {
			targetModel = a.cfg.Router.StrongModel
		} else if routerResult.Complexity == TaskMedium && a.cfg.Router.MediumModel.Model != "" {
			targetModel = a.cfg.Router.MediumModel
		}

		if targetModel.Model != "" {
			modelName, apiKey, baseURL := targetModel.Resolve(a.cfg.LLM.Model, a.client.APIKey(), a.client.BaseURL())
			activeClient = llm.NewClient(apiKey, baseURL, modelName, a.debug, a.cfg.LLM.MaxTokens)
			upgradeUsed = true
			a.debugLog("Router", "upgrade → %s", modelName)
		}
	}

	// --- Execution loop ---
	fallbackUsed := false
	defer a.gcTmp() // Ensure cleanup on any exit path.

	for i := 0; i < a.MaxIterations; i++ {
		// Check for cancellation.
		if ctx.Err() != nil {
			return "(Task stopped by user)", nil
		}

		a.debugSeparator(i + 1)

		var assistantMsg *llm.Message
		var err error

		// Inject context (time, workspace, OS, arch) and current todos into the
		// last user message. This keeps the system prompt byte-identical across
		// calls, enabling KV cache hits on the entire prior conversation.
		callMessages := a.buildCallMessages()

		// inflightResults[idx] receives the tool result for tool call at position idx.
		type toolExecResult struct {
			result  string
			execErr error
		}
		inflightResults := make(map[int]chan toolExecResult)

		// Determine effective parallelism.
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

			// Credential security: block direct file access and substitute placeholders.
			if a.CredMgr != nil {
				argsStr := string(rawArgs)
				// Block access to credentials file (defense-in-depth: security checker
				// already blocks file tools; this catches bash and other tools).
				if a.CredMgr.ContainsCredPath(argsStr) {
					return "error: direct access to the credentials file is system-protected and not permitted", nil
				}
				// Replace {{cred:name}} placeholders with actual values.
				if substituted := a.CredMgr.Substitute(argsStr); substituted != argsStr {
					rawArgs = json.RawMessage(substituted)
				}
			}

			a.debugLog("Tool▷", "%s  %s", tc.Function.Name, briefActionSummary(tc.Function.Name, tc.Function.Arguments))
			if a.ToolStartCallback != nil {
				a.ToolStartCallback(tc.Function.Name, string(rawArgs))
			}
			res, execErr := a.registry.Execute(ctx, tc.Function.Name, rawArgs)

			// Scrub any credential values that leaked into the tool result before
			// they are stored in conversation history.
			if a.CredMgr != nil {
				res = a.CredMgr.Scrub(res)
			}

			if a.ToolEndCallback != nil {
				a.ToolEndCallback(tc.Function.Name)
			}
			if a.ToolResultCallback != nil {
				a.ToolResultCallback(tc.Function.Name, res)
			}
			a.debugLog("Tool◁", "%s  %s", tc.Function.Name, truncateStr(res, 600))
			a.debugBlankLine()
			return res, execErr
		}

		// For streaming calls, wire a progressive callback.
		// Bug Fix 1: Sequential dependency in streaming.
		// If we are in parallel mode (maxPar > 1), we check for mutations.
		// To be safe, if we detect multiple tool calls in a stream, and we suspect
		// dependencies, we can either serialize them or defer them.
		var toolCallStreamCb llm.ToolCallStreamCb
		if streamCb != nil && maxPar > 1 {
			toolCallStreamCb = func(idx int, call llm.ToolCall) {
				// We don't know the FULL list of tools yet in a stream.
				// If the model is known to emit dependent tools (like file_write followed by bash),
				// progressive execution is risky.
				// For now, we allow progressive execution only for non-mutation tools.
				// Conservatively treat any non-read-only tool (including unknown
				// MCP/custom tools) as a mutation; defer it to post-stream serialisation.
				if !isReadOnlyTool(call.Function.Name) {
					// Don't start mutation tools progressively; let the main loop
					// handle them after the stream ends (where they might be serialized).
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
			// If context was cancelled (user stop), return cleanly.
			if ctx.Err() != nil {
				return "(Task stopped by user)", nil
			}

			// If upgraded model failed and we haven't fallen back yet, try router model.
			if upgradeUsed && !fallbackUsed && a.cfg.Router.ClassifierModel.Model != "" {
				modelName, apiKey, baseURL := a.cfg.Router.ClassifierModel.Resolve(a.cfg.LLM.Model, a.client.APIKey(), a.client.BaseURL())
				a.debugLog("Router", "fallback → %s", modelName)
				activeClient = llm.NewClient(apiKey, baseURL, modelName, a.debug, a.cfg.LLM.MaxTokens)
				upgradeUsed = false
				fallbackUsed = true
				i-- // Retry this iteration with the fallback model
				continue
			}

			return "", fmt.Errorf("LLM call failed at iteration %d: %w", i+1, err)
		}

		// Dependency detection: if any tool in this turn may have side effects,
		// serialize the entire batch to prevent race conditions.
		// Unknown tools (MCP, custom) are conservatively treated as mutations.
		hasMutation := false
		for _, tc := range assistantMsg.ToolCalls {
			if !isReadOnlyTool(tc.Function.Name) {
				hasMutation = true
				break
			}
		}
		if hasMutation {
			maxPar = 1 // Serialize this specific turn.
			// Re-create sem with new capacity.
			sem = make(chan struct{}, maxPar)
		}

		// Strip <think> blocks and store assistant message.
		turnStart := len(a.messages)
		cleanedMsg := *assistantMsg
		cleanedMsg.Content = sanitizeOutput(cleanedMsg.Content)
		a.messages = append(a.messages, cleanedMsg)

		// No tool calls — treat as final response.
		if len(cleanedMsg.ToolCalls) == 0 {
			a.trySummarize(ctx)
			a.gcTmp()
			if cleanedMsg.Content != "" {
				return cleanedMsg.Content, nil
			}
			return "(Agent returned empty response)", nil
		}

		// Dispatch any tool calls not yet started progressively (sequential path
		// or parallel path for tool calls whose JSON was only complete after stream end).
		for idx, tc := range cleanedMsg.ToolCalls {
			if _, alreadyStarted := inflightResults[idx]; alreadyStarted {
				continue
			}
			tc := tc // capture
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
				// Sequential: run inline (block until done, then write to channel).
				res, execErr := dispatchTool(tc)
				ch <- toolExecResult{res, execErr}
			}
		}

		// Collect results in call order and append to message history.
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
			a.messages = append(a.messages, llm.Message{
				Role:       "tool",
				Content:    historyResult,
				ToolCallID: tc.ID,
			})
			currentTurnResults = append(currentTurnResults, toolResult)
		}

		// When a tool fails, set an ephemeral hint for the next LLM call.
		// The hint is consumed by buildCallMessages and never stored in history.
		// Suppressed for sub-agents and pipeline nodes (they have narrower scopes).
		if anyToolError && !a.IsSubAgent {
			a.hintOnNextCall = `A tool call above returned an error. ` +
				`Read .ageage/docs/troubleshooting.md to diagnose common failure causes before retrying.`
		}

		// Check finish after all tools have been collected.
		if a.finishTool.Finished {
			a.messages = append(a.messages, llm.Message{
				Role:    "assistant",
				Content: a.finishTool.Summary,
			})
			a.trySummarize(ctx)
			if a.todoStore != nil && a.todoStore.IsComplete() {
				a.todoStore.Clear()
			}
			a.gcTmp()
			return a.finishTool.Summary, nil
		}

		// Check for cancellation after tool batch.
		if ctx.Err() != nil {
			return "(Task stopped by user)", nil
		}

		// Record this turn. Compress any turns older than the 2 most recent ones,
		// preserving the last 2 full rounds intact for LLM context continuity.
		a.pendingTurns = append(a.pendingTurns, turnRecord{
			assistantMsg: cleanedMsg,
			toolResults:  currentTurnResults,
			msgStart:     turnStart,
			msgCount:     1 + len(cleanedMsg.ToolCalls),
		})
		const keepRecentTurns = 2
		for len(a.pendingTurns) > keepRecentTurns {
			a.compressOldestTurn()
		}
	}

	return "", fmt.Errorf("agent reached maximum iterations (%d) without completing the task", a.MaxIterations)
}

// runPipelineSkill executes a YAML-based pipeline skill.
func (a *Agent) runPipelineSkill(ctx context.Context, skill *skills.Skill, input string) (string, error) {
	exec := NewPipelineExecutor(
		skill.Pipeline,
		skill,
		a.factory,
		input,
		a.SessionDir, // Pass session dir (Bug Fix 1)
		0,
		a.TodoSendFunc,
		a.TodoEditFunc,
		a.NotifyFunc,
		a.AskUserNotify,
		a.ConfirmationMgr,
		a.currentChannelID,
		a.registry,
	)
	return exec.Run(ctx)
}

// gcTmp removes managed tmp files no longer referenced in the message history.
func (a *Agent) gcTmp() {
	if a.tmpMgr != nil {
		a.tmpMgr.GC(a.messages)
	}
}

// --- Output sanitization ---

// thinkTagPairs lists the open/close pairs for all known thinking-block tag families.
var thinkTagPairs = [][2]string{
	{"<think>", "</think>"},
	{"<thought>", "</thought>"},
}

// sanitizeOutput post-processes LLM text output before it is returned to the
// caller or stored in history. It:
//  1. Strips think/thought blocks anchored to the start of the response.
//     Blocks that appear after real content are left intact — they are body
//     text (e.g. the model discussing the tags), not internal reasoning.
//  2. Converts common LaTeX math expressions to their Unicode equivalents so
//     that IM platforms receive readable text instead of raw LaTeX.
func sanitizeOutput(s string) string {
	s = stripLeadingThinkBlocks(s)
	s = convertLatex(s)
	return strings.TrimSpace(s)
}

// stripLeadingThinkBlocks removes consecutive think/thought blocks from the
// very start of a response. Blocks embedded after real content are left as-is.
// An unclosed block at the start (truncated response) is discarded entirely.
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
				// Unclosed block at the start: response was truncated mid-thought.
				// Return empty — there is no visible answer to show.
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

// readOnlyTools is the set of tool names that are guaranteed to have no side
// effects and are therefore safe to run in parallel with other tools.
// Every tool NOT in this set — including all MCP tools and unknown custom tools
// — is conservatively treated as a mutation and causes the entire batch to be
// serialised, preventing concurrent-write races.
var readOnlyTools = map[string]bool{
	"file_read":     true,
	"web_fetch":     true,
	"web_search":    true,
	"memory_recall": true,
	"glob":          true,
	"grep":          true,
	"cron_list":     true,
}

// isReadOnlyTool reports whether name is a known side-effect-free tool.
func isReadOnlyTool(name string) bool { return readOnlyTools[name] }


// --- Debug helpers ---

// debugIcons maps log categories to visual symbols.
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

// debugIndent is the column offset of message content in debugLog output.
// Format: "  X  CATEGORY   " = 2 + 1(icon) + 2 + 10(category) + 1 = 16 visual cols.
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
	// Indent continuation lines so they align with the column.
	msg = strings.ReplaceAll(msg, "\n", "\n"+debugIndent)
	fmt.Printf("  %s  %-10s %s\n", icon, category, msg)
}

func (a *Agent) debugBlankLine() {
	if a.debug {
		fmt.Println()
	}
}

// --- Token optimization ---

// compactToolResult truncates tool output stored in message history to save tokens.
// The full result is already used by the current iteration; the history only needs
// enough context for subsequent iterations.
const maxToolResultInHistory = 4000

func compactToolResult(result string) string {
	runes := []rune(result)
	if len(runes) <= maxToolResultInHistory {
		return result
	}
	// Keep the first portion and a note about truncation.
	return string(runes[:maxToolResultInHistory]) + "\n\n[... output truncated for token efficiency. Key information is above.]"
}

// ensureFinishTask makes sure finish_task is always in the tool list.
func ensureFinishTask(toolNames []string) []string {
	for _, n := range toolNames {
		if n == "finish_task" {
			return toolNames
		}
	}
	return append(toolNames, "finish_task")
}

// compressOldestTurn compresses the oldest pending turn in-place, replacing its
// assistant + tool-result messages with a single narrative assistant message.
// Indices of remaining pending turns are updated to reflect the splice.
func (a *Agent) compressOldestTurn() {
	if len(a.pendingTurns) == 0 {
		return
	}
	oldest := a.pendingTurns[0]
	a.pendingTurns = a.pendingTurns[1:]

	start := oldest.msgStart
	end := start + oldest.msgCount
	if end > len(a.messages) {
		return // sanity guard
	}

	narrative := buildTurnNarrative(oldest.assistantMsg, oldest.toolResults)
	compressed := llm.Message{Role: "assistant", Content: narrative}

	// Splice [start, end) → [compressed].
	removed := oldest.msgCount - 1
	newMsgs := make([]llm.Message, 0, len(a.messages)-removed)
	newMsgs = append(newMsgs, a.messages[:start]...)
	newMsgs = append(newMsgs, compressed)
	newMsgs = append(newMsgs, a.messages[end:]...)
	a.messages = newMsgs

	// Shift indices of remaining (newer) pending turns.
	for i := range a.pendingTurns {
		a.pendingTurns[i].msgStart -= removed
	}

	a.debugLog("History", "%d messages → 1 narrative", oldest.msgCount)
}

// buildTurnNarrative creates a factual work-log string for a completed tool-call turn.
// It avoids "called tool / arguments" framing so the LLM reads it as memory,
// not as a prompt to repeat tool invocations.
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

// briefActionSummary converts a tool name + raw JSON args into a short human-readable
// action description, without exposing "tool call" framing to the LLM.
func briefActionSummary(toolName, rawArgs string) string {
	// Try to extract the single most meaningful field from JSON args.
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
			return "Stored memory: " + param
		}
	case "memory_recall":
		if param != "" {
			return "Recalled memory: " + param
		}
	case "memory_forget":
		if param != "" {
			return "Removed memory: " + param
		}
	case "cron_add":
		if param != "" {
			return "Scheduled: " + param
		}
	case "cron_remove":
		return "Removed scheduled task"
	case "cron_list":
		return "Listed scheduled tasks"
	case "finish_task":
		return "Completed task"
	case "delegate", "escalate":
		if param != "" {
			return "Delegated: " + param
		}
	}

	// Generic fallback: toolName + abbreviated param.
	if param != "" {
		return toolName + ": " + param
	}
	return toolName
}

// extractPrimaryArg parses JSON args and returns the value of the most meaningful field.
// Priority order covers the most common tool parameter names.
func extractPrimaryArg(rawArgs string) string {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(rawArgs), &m); err != nil {
		// Not JSON — truncate and return raw.
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

	// Fall back to the first string-valued field found.
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

// truncateStr truncates a string for debug display.
func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// buildCallMessages returns the message slice to send to the LLM this turn.
// It appends a temporary "user" message containing the current time, workspace,
// and any active todos at the very end — never touching the stored a.messages.
// Keeping all prior messages byte-identical maximises KV cache reuse.
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

	// Consume the ephemeral error hint (set when a tool failed last iteration).
	// Appended after the context block so it gets the agent's attention without
	// polluting persistent history or the stable system-prompt prefix.
	if a.hintOnNextCall != "" {
		fmt.Fprintf(&sb, "\n[Framework] %s", a.hintOnNextCall)
		a.hintOnNextCall = ""
	}

	ctxStr := sb.String()
	// Create a copy of the message history to avoid mutating a.messages directly.
	out := make([]llm.Message, len(a.messages))
	copy(out, a.messages)

	// To keep the API happy (alternating roles) and the context effective:
	// 1. If the last message is from User, append the context to it.
	// 2. Otherwise, append a new User message with the context.
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

	if !a.summarizer.ShouldSummarize(a.messages) {
		return
	}

	oldCount := len(a.messages)
	newMessages, err := a.summarizer.Summarize(ctx, a.messages)
	if err != nil {
		a.debugLog("Summarize", "failed: %s", err)
		return
	}

	a.messages = newMessages
	// pendingTurns hold indices into the old message slice; they become stale
	// after summarization replaces the entire history. Drop them so
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

// LastTurnUserMessage returns the last user message in the conversation history
// and whether one was found. Tool-result messages are skipped.
func (a *Agent) LastTurnUserMessage() (llm.Message, bool) {
	for i := len(a.messages) - 1; i >= 0; i-- {
		if a.messages[i].Role == "user" {
			return a.messages[i], true
		}
	}
	return llm.Message{}, false
}

// RollbackLastTurn removes the last complete user→assistant exchange from
// history (one user message plus all subsequent assistant/tool messages that
// followed it). It also trims pendingTurns accordingly.
// Returns the number of messages removed, or 0 if there was nothing to roll back.
func (a *Agent) RollbackLastTurn() int {
	// Find the last user message index.
	lastUserIdx := -1
	for i := len(a.messages) - 1; i >= 0; i-- {
		if a.messages[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return 0
	}

	removed := len(a.messages) - lastUserIdx
	// Use 3-index slice to break backing-array reference, preventing memory leak.
	a.messages = a.messages[:lastUserIdx:lastUserIdx]

	// Trim pendingTurns: drop any whose msgStart is at or after lastUserIdx.
	kept := a.pendingTurns[:0:0] // empty, own backing array
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
	a.messages = nil
	a.pendingTurns = nil
	if a.todoStore != nil {
		a.todoStore.Clear()
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
// Also updates the summarizer's client so it uses matching credentials
// if the new client connects to a different API provider.
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

// AddHistory adds historical messages to the conversation context without triggering agent execution.
// This is used when process_history is enabled to load pre-existing messages into context.
func (a *Agent) AddHistory(userInput, assistantReply string) {
	if len(a.messages) == 0 {
		// AddHistory is used to pre-load prior conversation; no skill is active.
		a.messages = append(a.messages, llm.Message{
			Role:    "system",
			Content: a.buildSystemPrompt(nil),
		})
	}

	if userInput != "" {
		a.messages = append(a.messages, llm.Message{
			Role:    "user",
			Content: userInput,
		})
	}

	if assistantReply != "" {
		a.messages = append(a.messages, llm.Message{
			Role:    "assistant",
			Content: sanitizeOutput(assistantReply),
		})
	}

	a.trySummarize(context.Background())
}
