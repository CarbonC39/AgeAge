package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"ageage/config"
	"ageage/llm"
	"ageage/skills"
	"ageage/tools"
)

// varInterpolateRe matches {{$vars.name}}, $vars.name, and {{name}} tokens in prompts.
var varInterpolateRe = regexp.MustCompile(`\{\{\$vars\.([a-zA-Z_][a-zA-Z0-9_]*)\}\}|\$vars\.([a-zA-Z_][a-zA-Z0-9_]*)|\{\{([a-zA-Z_][a-zA-Z0-9_]*)\}\}`)

// Node status values for todo display.
const (
	nodeStatusPending = "pending"
	nodeStatusRunning = "running"
	nodeStatusDone    = "done"
	nodeStatusFailed  = "failed"
	nodeStatusSkipped = "skipped"
)

// PipelineExecutor orchestrates execution of a YAML pipeline skill.
//
// Nodes run sequentially in isolation (each agent node gets a fresh sub-agent
// with no shared conversation history). Variables flow between nodes via a
// shared vars map.
type PipelineExecutor struct {
	ps        *skills.PipelineSkill
	skill     *skills.Skill // wrapping Skill metadata (name, description)
	factory   *AgentFactory
	sharedReg *tools.Registry // parent registry; used by auto nodes for direct tool calls

	vars       map[string]interface{} // live pipeline variable state
	sessionDir string                 // directory for the active session
	nestDepth  int                    // 0 = top-level; nested pipeline skills run at depth 1
	debug      bool

	// Soul injection: index of the last agent node in the pipeline.
	// SOUL.md is injected only in that node (if factory.InjectSoul is true).
	lastAgentNodeIdx int // -1 = no agent nodes
	currentNodeIdx   int // updated each iteration in Run()

	// Todo notification callbacks — mirror the parent Agent fields.
	sendFn        func(text string) string
	editFn        func(msgID, text string) error
	notifyFn      func(message string)
	askUserNotify func(string, []string) // Propagated to pipeline sub-agents for ask_user tool

	// Passed through to sub-agents for supervised-mode confirmations.
	confirmMgr *tools.ConfirmationManager
	channelID  string

	nodeStatus map[string]string // node ID → nodeStatus* value
	todoMsgID  string
}

// NewPipelineExecutor creates a new executor for the given pipeline skill.
// nestDepth=0 for a top-level pipeline; nested pipeline skills run at depth 1.
func NewPipelineExecutor(
	ps *skills.PipelineSkill,
	skill *skills.Skill,
	factory *AgentFactory,
	input string,
	sessionDir string,
	nestDepth int,
	sendFn func(text string) string,
	editFn func(msgID, text string) error,
	notifyFn func(message string),
	askUserNotify func(string, []string),
	confirmMgr *tools.ConfirmationManager,
	channelID string,
	sharedReg *tools.Registry,
) *PipelineExecutor {
	// Initialise vars from declared defaults, then overlay with the user's input.
	// Explicit loop instead of maps.Copy because ps.Vars is map[string]string
	// while vars is map[string]interface{} — the value types differ.
	vars := make(map[string]interface{}, len(ps.Vars)+1)
	for k, v := range ps.Vars {
		vars[k] = v
	}
	vars["input"] = input

	status := make(map[string]string, len(ps.Pipeline))
	for _, node := range ps.Pipeline {
		status[node.ID] = nodeStatusPending
	}

	return &PipelineExecutor{
		ps:            ps,
		skill:         skill,
		factory:       factory,
		sharedReg:     sharedReg,
		vars:          vars,
		sessionDir:    sessionDir,
		nestDepth:     nestDepth,
		debug:         factory.Debug,
		sendFn:        sendFn,
		editFn:        editFn,
		notifyFn:      notifyFn,
		askUserNotify: askUserNotify,
		confirmMgr:    confirmMgr,
		channelID:     channelID,
		nodeStatus:    status,
	}
}

// Run executes all pipeline nodes in sequence and returns the final result.
func (e *PipelineExecutor) Run(ctx context.Context) (string, error) {
	// Runtime sanity check that complements skill_validator: the first node
	// must be type:agent because $vars.input contains the user's raw
	// natural-language text. An auto first node would feed that text into a
	// tool's typed schema and fail.
	if len(e.ps.Pipeline) > 0 && strings.ToLower(e.ps.Pipeline[0].Type) == "auto" {
		skillName := ""
		if e.skill != nil {
			skillName = e.skill.Name
		}
		return "", fmt.Errorf("pipeline %q: first node must be type:agent (auto cannot parse the user's natural-language input)", skillName)
	}

	// Only the top-level executor manages todo display.
	if e.nestDepth == 0 {
		e.updateTodos()
	}

	// Compute last agent node index once; only that node gets SOUL injection.
	e.lastAgentNodeIdx = -1
	for i, node := range e.ps.Pipeline {
		if strings.ToLower(node.Type) != "auto" {
			e.lastAgentNodeIdx = i
		}
	}

	for i, node := range e.ps.Pipeline {
		e.currentNodeIdx = i
		if ctx.Err() != nil {
			return "", ctx.Err()
		}

		if e.nestDepth == 0 {
			e.nodeStatus[node.ID] = nodeStatusRunning
			e.updateTodos()
		}

		out := make(map[string]interface{})
		var err error

		if node.Foreach != "" {
			err = e.execForeach(ctx, node)
		} else {
			err = e.execNode(ctx, node, nil, -1, out)
			if err == nil {
				e.mergeOutputs(out)
			}
		}

		if err != nil {
			if e.nestDepth == 0 {
				e.nodeStatus[node.ID] = nodeStatusFailed
				found := false
				for _, n := range e.ps.Pipeline {
					if found {
						e.nodeStatus[n.ID] = nodeStatusSkipped
					}
					if n.ID == node.ID {
						found = true
					}
				}
				e.updateTodos()
			}
			if i > 0 {
				prev := e.ps.Pipeline[i-1]
				return "", fmt.Errorf("pipeline node %q failed after %q succeeded: %w", node.ID, prev.ID, err)
			}
			return "", fmt.Errorf("pipeline node %q failed: %w", node.ID, err)
		}

		if e.nestDepth == 0 {
			e.nodeStatus[node.ID] = nodeStatusDone
			e.updateTodos()
		}
	}

	// Return the declared output variable, or fall back to common names, or a default.
	if e.ps.Returns != "" {
		if v, ok := e.vars[e.ps.Returns]; ok {
			return fmtVar(v), nil
		}
		return "", fmt.Errorf("pipeline declares returns: %q but variable not produced by any node", e.ps.Returns)
	}
	for _, name := range []string{"result", "output", "answer"} {
		if v, ok := e.vars[name]; ok {
			return fmtVar(v), nil
		}
	}
	return "", fmt.Errorf("pipeline produced no returnable output — declare `returns:` or have a node output `result`/`output`/`answer` (skill defect)")
}

// mergeOutputs writes a node's out map into e.vars.
// Called by Run() for non-foreach nodes; execForeach manages its own merging.
func (e *PipelineExecutor) mergeOutputs(out map[string]interface{}) {
	for pipelineVar, v := range out {
		e.vars[pipelineVar] = v
	}
}

// ── Foreach ───────────────────────────────────────────────────────────────────

// execForeach iterates a node over an array pipeline variable.
// After all iterations, per-output-key values are collected as slices and
// written to e.vars. Parallel execution is used when Config.Pipeline.ForeachConcurrency > 1.
func (e *PipelineExecutor) execForeach(ctx context.Context, node skills.PipelineNode) error {
	// Normalize foreach var: bare names (no $ prefix) resolve as $vars.name.
	foreachRef := node.Foreach
	if foreachRef != "" && !strings.HasPrefix(foreachRef, "$") {
		foreachRef = "$vars." + foreachRef
	}
	arrayVal := e.resolveValue(foreachRef, nil, -1)
	if arrayVal == nil {
		return fmt.Errorf("foreach variable %q is nil or not found", node.Foreach)
	}

	var items []interface{}
	switch v := arrayVal.(type) {
	case []interface{}:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	case []int:
		for _, n := range v {
			items = append(items, float64(n))
		}
	case []float64:
		for _, f := range v {
			items = append(items, f)
		}
	case []bool:
		for _, b := range v {
			items = append(items, b)
		}
	case []map[string]interface{}:
		for _, m := range v {
			items = append(items, m)
		}
	default:
		rv := reflect.ValueOf(arrayVal)
		if rv.Kind() == reflect.Slice {
			items = make([]interface{}, rv.Len())
			for i := 0; i < rv.Len(); i++ {
				items[i] = rv.Index(i).Interface()
			}
		} else {
			return fmt.Errorf("foreach variable %q must be an array (got %T)", node.Foreach, arrayVal)
		}
	}

	if len(items) == 0 {
		return nil
	}

	outputAccum := make(map[string][]interface{}, len(node.Outputs))

	concurrency := e.factory.Config.Pipeline.ForeachConcurrency
	if concurrency <= 1 {
		// ── Sequential path ───────────────────────────────────────────────
		for idx, item := range items {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			out := make(map[string]interface{})
			if err := e.execNode(ctx, node, item, idx, out); err != nil {
				return fmt.Errorf("foreach iteration %d: %w", idx, err)
			}
			// Collect outputs per pipeline var (will become slices).
			for pipelineVar := range node.Outputs {
				outputAccum[pipelineVar] = append(outputAccum[pipelineVar], out[pipelineVar])
			}
		}
	} else {
		// ── Parallel path ─────────────────────────────────────────────────
		// Each goroutine writes to its own `out` map — no shared writes to
		// e.vars. A cancellable sub-context lets the first error stop the
		// remaining goroutines.
		type iterResult struct {
			idx int
			out map[string]interface{}
			err error
		}

		subCtx, subCancel := context.WithCancel(ctx)
		defer subCancel()

		resCh := make(chan iterResult, len(items))
		sem := make(chan struct{}, concurrency)

		for i, it := range items {
			go func(idx int, item interface{}) {
				sem <- struct{}{}
				defer func() { <-sem }()

				var result iterResult
				defer func() {
					if r := recover(); r != nil {
						result = iterResult{idx: idx, out: nil, err: fmt.Errorf("panic: %v", r)}
					}
					resCh <- result
				}()

				out := make(map[string]interface{})
				err := e.execNode(subCtx, node, item, idx, out)
				result = iterResult{idx: idx, out: out, err: err}
			}(i, it)
		}

		// Collect all results; cancel on first error but keep draining to avoid goroutine leak.
		results := make([]iterResult, len(items))
		var failedIdxs []int
		var firstErr error
		for i := 0; i < len(items); i++ {
			r := <-resCh
			if r.err != nil {
				failedIdxs = append(failedIdxs, r.idx)
				if firstErr == nil {
					firstErr = r.err
					subCancel() // signal remaining goroutines to stop
				}
			}
			results[r.idx] = r
		}
		if firstErr != nil {
			if len(failedIdxs) == 1 {
				return fmt.Errorf("foreach iteration %d: %w", failedIdxs[0], firstErr)
			}
			var msgs []string
			for _, idx := range failedIdxs {
				msgs = append(msgs, fmt.Sprintf("iteration %d: %s", idx, results[idx].err))
			}
			return fmt.Errorf("foreach: %d of %d iterations failed:\n%s", len(failedIdxs), len(items), strings.Join(msgs, "\n"))
		}

		// Merge results in index order for deterministic output.
		for _, r := range results {
			for pipelineVar := range node.Outputs {
				outputAccum[pipelineVar] = append(outputAccum[pipelineVar], r.out[pipelineVar])
			}
		}
	}

	// Write collected slices back to e.vars.
	for pipelineVar, collected := range outputAccum {
		e.vars[pipelineVar] = collected
	}
	return nil
}

// ── Node dispatch ─────────────────────────────────────────────────────────────

// execNode validates and dispatches to the correct executor (agent or auto).
// Outputs are written to `out` (keyed by PIPELINE VAR NAME from node.Outputs).
func (e *PipelineExecutor) execNode(ctx context.Context, node skills.PipelineNode, foreachItem interface{}, foreachIdx int, out map[string]interface{}) error {
	if node.Validate == "not_empty" {
		for argName, varRef := range node.Inputs {
			val := e.resolveValue(varRef, foreachItem, foreachIdx)
			isEmpty := val == nil || val == ""
			if s, ok := val.(string); ok && strings.TrimSpace(s) == "" {
				isEmpty = true
			} else if val != nil {
				v := reflect.ValueOf(val)
				switch v.Kind() {
				case reflect.Slice, reflect.Array, reflect.Map:
					if v.Len() == 0 {
						isEmpty = true
					}
				}
			}
			if isEmpty {
				return fmt.Errorf("node %q validation failed: input %q is empty", node.ID, argName)
			}
		}
	}

	switch node.Type {
	case "agent", "":
		return e.execAgentNode(ctx, node, foreachItem, foreachIdx, out)
	case "auto":
		return e.execAutoNode(ctx, node, foreachItem, foreachIdx, out)
	default:
		return fmt.Errorf("unknown node type %q (valid: agent, auto)", node.Type)
	}
}

// ── Auto node ─────────────────────────────────────────────────────────────────

// execAutoNode calls a tool directly without involving an LLM.
// Resolved inputs are marshalled to JSON and passed to the tool.
// Outputs are written to `out` keyed by pipeline var name.
func (e *PipelineExecutor) execAutoNode(ctx context.Context, node skills.PipelineNode, foreachItem interface{}, foreachIdx int, out map[string]interface{}) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if node.Tool == "" {
		return fmt.Errorf("auto node %q must specify 'tool'", node.ID)
	}
	if e.sharedReg == nil {
		return fmt.Errorf("auto node %q: no shared registry available for tool calls", node.ID)
	}

	// Resolve and marshal inputs.
	inputMap := make(map[string]interface{}, len(node.Inputs))
	for argName, varRef := range node.Inputs {
		val := e.resolveValue(varRef, foreachItem, foreachIdx)
		// If the resolved value is a plain string, it may contain {{…}} templates.
		if s, ok := val.(string); ok {
			val = e.interpolatePrompt(s, foreachItem, foreachIdx)
		}
		inputMap[argName] = val
	}
	argsJSON, err := json.Marshal(inputMap)
	if err != nil {
		return fmt.Errorf("auto node %q: failed to marshal inputs: %w", node.ID, err)
	}

	e.debugf("Auto▷", "%s  tool=%s", node.ID, node.Tool)

	result, err := NewToolDispatcher(e.sharedReg, e.factory.CredMgr).Execute(
		ctx, node.Tool, argsJSON, ToolDispatchHooks{},
	)
	if err != nil {
		return fmt.Errorf("auto node %q: tool %q failed: %w", node.ID, node.Tool, err)
	}

	e.debugf("Auto◁", "%s  %s", node.ID, truncateStr(result, 300))

	// Try to parse result as JSON so structured values can be used as foreach arrays, etc.
	var resultObj interface{}
	if json.Unmarshal([]byte(result), &resultObj) == nil {
		switch resultObj.(type) {
		case map[string]interface{}, []interface{}:
			// keep parsed value
		default:
			resultObj = result // fall back to string for JSON primitives
		}
	} else {
		resultObj = result
	}

	// Build the tool's output namespace: always includes "result".
	// If the result is a JSON object, its keys are also available individually.
	toolOutputs := map[string]interface{}{"result": resultObj}
	if m, ok := resultObj.(map[string]interface{}); ok {
		for k, v := range m {
			toolOutputs[k] = v
		}
	}

	for pipelineVar, nodeOutputKey := range node.Outputs {
		if v, ok := toolOutputs[nodeOutputKey]; ok {
			out[pipelineVar] = v
		}
	}
	return nil
}

// ── Agent node ────────────────────────────────────────────────────────────────

// execAgentNode creates an isolated sub-agent and runs it for a pipeline node.
// The standard finish_task tool is replaced with node_complete so that the
// node can report structured outputs back to the executor.
// Outputs are written to `out` keyed by pipeline var name.
// On transient LLM errors the engine automatically retries with the next lower
// complexity tier (complex→medium, medium→base).
func (e *PipelineExecutor) execAgentNode(ctx context.Context, node skills.PipelineNode, foreachItem interface{}, foreachIdx int, out map[string]interface{}) error {
	// Resolve the node's Skill (if specified).
	var nodeSkill *skills.Skill
	if node.Skill != "" {
		nodeSkill = e.lookupSkill(node.Skill)
		if nodeSkill == nil {
			return fmt.Errorf("agent node %q: skill %q not found", node.ID, node.Skill)
		}
		if nodeSkill.IsPipeline() {
			return e.execNestedPipeline(ctx, node, nodeSkill, foreachItem, foreachIdx, out)
		}
	}

	// Build tool allowlist for this node's sub-agent.
	// node.Tools is per-node and takes priority.
	// skill.RequiredTools is router metadata (what the pipeline as a whole uses) and
	// must NOT bleed into individual node sub-agents.
	// A nil allowlist means all global tools are available.
	var filteredTools []string
	if len(node.Tools) > 0 {
		filteredTools = node.Tools
	} else if nodeSkill != nil && len(nodeSkill.RequiredTools) > 0 {
		filteredTools = nodeSkill.RequiredTools
	}

	// Build the task prompt once; it is the same for all attempts.
	taskPrompt := e.buildNodePrompt(node, foreachItem, foreachIdx)

	// Inject only the pipeline variables referenced by this node.
	// Injecting everything causes large foreach-accumulated arrays to pollute unrelated nodes.
	referencedVars := make(map[string]bool)
	for _, varRef := range node.Inputs {
		if s, ok := varRef.(string); ok {
			if key := extractVarsKey(s); key != "" {
				referencedVars[key] = true
			}
		}
	}
	for _, m := range varInterpolateRe.FindAllStringSubmatch(node.Prompt, -1) {
		for _, cap := range m[1:] {
			if cap != "" {
				referencedVars[cap] = true
				break
			}
		}
	}
	referencedVars["input"] = true // always expose the pipeline entry-point variable

	var injectedVars strings.Builder
	for k := range referencedVars {
		if v, ok := e.vars[k]; ok {
			val := truncateStr(fmtVar(v), 300)
			fmt.Fprintf(&injectedVars, "- %s = %s\n", k, val)
		}
	}
	if injectedVars.Len() > 0 {
		taskPrompt += "\n\n---\n**Pipeline variables for this node:**\n" + injectedVars.String()
	}

	if len(node.Outputs) > 0 {
		var pairs []string
		for pipelineVar, nodeKey := range node.Outputs {
			pairs = append(pairs, fmt.Sprintf("%q: <your %s>", nodeKey, pipelineVar))
		}
		taskPrompt += fmt.Sprintf(
			"\n\n---\n**When done, call exactly:**\n"+
				"node_complete(status=\"success\", vars={%s})\n"+
				"Use these exact key names — do NOT rename them.",
			strings.Join(pairs, ", "),
		)
	}

	// Attempt execution, falling back one tier on transient LLM errors.
	tiers := e.tierFallbacks(node.Tier)

	var lastErr error
	for attempt, tier := range tiers {
		if attempt > 0 {
			e.debugf("Fallback", "%s  attempt %d failed (%s) → retrying with tier=%s", node.ID, attempt, lastErr, tier)
		}
		nodeOut, err := e.runAgentNodeAttempt(ctx, node, nodeSkill, filteredTools, taskPrompt, tier)
		if err == nil {
			for k, v := range nodeOut {
				out[k] = v
			}
			return nil
		}
		// Do not retry on context cancellation — the user or timeout killed the run.
		if ctx.Err() != nil {
			return err
		}
		lastErr = err
	}
	return lastErr
}

// runAgentNodeAttempt performs a single execution attempt for an agent node
// using the given tier (which may differ from node.Tier on retry).
func (e *PipelineExecutor) runAgentNodeAttempt(
	ctx context.Context,
	node skills.PipelineNode,
	nodeSkill *skills.Skill,
	filteredTools []string,
	taskPrompt string,
	tier string,
) (map[string]interface{}, error) {
	// Create an isolated sub-agent. Sub-agents in pipelines:
	//   - Do NOT get pipeline todo callbacks (would conflict with pipeline's own display).
	//   - Do NOT load skills (IsSubAgent=true skips skill-only tool injection).
	//   - Do NOT run the router.
	subAgent := e.factory.CreateAgentFiltered(e.confirmMgr, e.channelID, filteredTools)
	subAgent.Mode.IsSubAgent = true
	subAgent.SessionDir = e.sessionDir
	// SOUL is injected only in the last agent node; context is always injected.
	subAgent.Mode.InjectSoul = e.factory.InjectSoul && (e.currentNodeIdx == e.lastAgentNodeIdx)
	subAgent.Mode.InjectContext = true
	subAgent.Callbacks.AskUser = e.askUserNotify
	if e.factory.Config.SubAgent.MaxIterations > 0 {
		subAgent.MaxIterations = e.factory.Config.SubAgent.MaxIterations
	}

	// Apply model from tier (node or fallback).
	e.applyNodeModel(subAgent, tier)

	// Pre-inject the system prompt. RunWithParts checks len(messages)==0 to
	// determine the first turn; setting it here tells RunWithParts that the
	// system prompt is already built and should not be overwritten.
	sysprompt := subAgent.buildSystemPrompt(nodeSkill) +
		"\n\n**CRITICAL — User visibility:** Your internal reasoning, tool calls, and " +
		"intermediate steps are NOT visible to the end user. The ONLY thing the user " +
		"sees is the final pipeline result. Therefore you MUST include your COMPLETE " +
		"findings, analysis, and answer in the node_complete vars. Never call " +
		"node_complete with a placeholder like 'done' or a one-word result — " +
		"the vars content IS your only channel to deliver results.\n\n" +
		"**Pipeline rule:** When calling node_complete, use the EXACT key names " +
		"specified in the task instruction. The default key is always \"result\" — " +
		"never substitute \"output\", \"answer\", \"text\", or any other name unless " +
		"explicitly told to use a different name."
	subAgent.conv.Reset([]llm.Message{{Role: "system", Content: sysprompt}})

	// Swap finish_task → node_complete.
	resultCh := make(chan NodeResult, 1)
	subAgent.registry.Unregister("finish_task")
	subAgent.registry.Register(&NodeCompleteTool{
		resultCh:   resultCh,
		finishTool: subAgent.finishTool,
	})

	e.debugf("Agent▷", "%s  %q", node.ID, truncateStr(taskPrompt, 200))

	runResult, runErr := subAgent.Run(ctx, taskPrompt, nil)

	// Read the structured result. Channel has capacity 1 so the write in
	// NodeCompleteTool.Execute is never blocking; read is always non-blocking.
	var nodeResult NodeResult
	select {
	case nodeResult = <-resultCh:
		// node_complete was called — use structured output.
	default:
		// Agent returned without calling node_complete.
		// Propagate LLM or iteration errors immediately instead of masking them.
		if runErr != nil {
			return nil, fmt.Errorf("agent node %q: %w", node.ID, runErr)
		}
		e.debugf("Warn", "%s  node_complete not called — falling back to last text output", node.ID)
		fallback := runResult
		if fallback == "" {
			msgs := subAgent.conv.All()
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Role == "assistant" && msgs[i].Content != "" {
					fallback = msgs[i].Content
					break
				}
			}
		}
		nodeResult = NodeResult{
			Status: "success",
			Vars:   map[string]interface{}{"result": fallback},
		}
	}

	if nodeResult.Status == "failure" {
		reason := nodeResult.Reason
		if reason == "" {
			reason = "node reported failure without a reason"
		}
		// node_complete(failure) is a deliberate agent decision — do not retry.
		return nil, fmt.Errorf("%s", reason)
	}

	// Surface LLM errors not already handled above (node_complete was called but run also errored).
	if runErr != nil {
		return nil, fmt.Errorf("agent node %q: %w", node.ID, runErr)
	}

	e.debugf("Agent◁", "%s  vars=%v", node.ID, nodeResult.Vars)

	// First pass: collect all outputs that matched by exact key.
	out := make(map[string]interface{}, len(node.Outputs))
	var missingPipelineVar string // pipeline-var name of the one missing entry (if any)
	var missingNodeKey string     // expected node_complete key that was absent
	missingCount := 0
	for pipelineVar, nodeOutputKey := range node.Outputs {
		if v, ok := nodeResult.Vars[nodeOutputKey]; ok {
			out[pipelineVar] = v
		} else {
			missingPipelineVar = pipelineVar
			missingNodeKey = nodeOutputKey
			missingCount++
		}
	}
	// Brute-force recovery: exactly 1 output was missing AND agent returned exactly
	// 1 var (with the wrong key name). This is the canonical "agent wrote 'answer'
	// instead of 'result'" pattern. Safe to steal because there is no ambiguity.
	switch {
	case missingCount == 1 && len(nodeResult.Vars) == 1:
		for wrongKey, v := range nodeResult.Vars {
			out[missingPipelineVar] = v
			e.debugf("Warn", "node %s: expected key %q, got %q — using fallback value", node.ID, missingNodeKey, wrongKey)
			if e.notifyFn != nil {
				e.notifyFn(fmt.Sprintf("⚠️ Node **%s** returned key `%s` instead of `%s` — value recovered but skill may need updating.", node.ID, wrongKey, missingNodeKey))
			}
		}
	case missingCount > 0:
		e.debugf("Warn", "node %s: missing output key %q (got vars: %v)", node.ID, missingNodeKey, nodeResult.Vars)
		if e.notifyFn != nil {
			e.notifyFn(fmt.Sprintf("Warning: node %q did not provide expected output key %q", node.ID, missingNodeKey))
		}
	}
	return out, nil
}

// execNestedPipeline runs a pipeline skill referenced from a node's Skill field.
// Nesting is limited to 1 level deep to prevent runaway recursion.
func (e *PipelineExecutor) execNestedPipeline(ctx context.Context, node skills.PipelineNode, nestedSkill *skills.Skill, foreachItem interface{}, foreachIdx int, out map[string]interface{}) error {
	if e.nestDepth >= 1 {
		return fmt.Errorf("agent node %q: pipeline skills may not be nested more than 1 level deep", node.ID)
	}

	// Resolve ALL node.Inputs as overrides for the nested pipeline's vars.
	// "input" is the canonical entry point; other keys map to named nested vars.
	initialInput := ""
	extraVars := make(map[string]interface{}, len(node.Inputs))
	for argName, varRef := range node.Inputs {
		val := fmtVar(e.resolveValue(varRef, foreachItem, foreachIdx))
		if argName == "input" {
			initialInput = val
		}
		extraVars[argName] = val
	}

	nestedExec := NewPipelineExecutor(
		nestedSkill.Pipeline,
		nestedSkill,
		e.factory,
		initialInput,
		e.sessionDir,
		e.nestDepth+1,
		nil, nil, nil, nil, // no independent todo display for nested pipelines
		e.confirmMgr,
		e.channelID,
		e.sharedReg,
	)
	nestedExec.askUserNotify = e.askUserNotify
	// Apply all resolved inputs as nested vars (including non-"input" keys).
	for k, v := range extraVars {
		nestedExec.vars[k] = v
	}

	result, err := nestedExec.Run(ctx)
	if err != nil {
		return fmt.Errorf("nested pipeline %q: %w", nestedSkill.Name, err)
	}

	// Expose nested outputs: individual vars and the final result string.
	nestedOutputs := make(map[string]interface{}, len(nestedExec.vars)+1)
	for k, v := range nestedExec.vars {
		nestedOutputs[k] = v
	}
	nestedOutputs["result"] = result

	for pipelineVar, nodeOutputKey := range node.Outputs {
		if v, ok := nestedOutputs[nodeOutputKey]; ok {
			out[pipelineVar] = v
		}
	}
	return nil
}

// tierFallbacks returns the ordered list of model tiers to attempt.
// On a transient LLM error the engine retries once with the next lower tier.
func (e *PipelineExecutor) tierFallbacks(primary string) []string {
	tiers := []string{primary}
	switch normalizeTier(primary) {
	case TierStrong:
		tiers = append(tiers, string(TierMedium))
	case TierMedium:
		tiers = append(tiers, "") // empty = base model (no override)
	}
	return tiers
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// lookupSkill finds a skill by name (case-insensitive) from the factory's list.
func (e *PipelineExecutor) lookupSkill(name string) *skills.Skill {
	norm := NormalizeSkillName(name)
	all := e.factory.GetSkills()
	for i := range all {
		if NormalizeSkillName(all[i].Name) == norm {
			return &all[i]
		}
	}
	return nil
}

// applyNodeModel sets the sub-agent's LLM client based on node tier.
// Checks [pipeline.models] first; falls back to [router] model settings.
// No-ops if tier is empty or no matching model is configured.
func (e *PipelineExecutor) applyNodeModel(subAgent *Agent, tier string) {
	if tier == "" {
		return
	}
	pm := e.factory.Config.Pipeline.Models
	var targetModel config.ModelConfig
	switch normalizeTier(tier) {
	case TierStrong:
		if pm.Strong.Model != "" {
			targetModel = pm.Strong
		} else {
			targetModel = e.factory.Config.Router.StrongModel
		}
	case TierMedium:
		if pm.Medium.Model != "" {
			targetModel = pm.Medium
		} else {
			targetModel = e.factory.Config.Router.MediumModel
		}
	case TierBase:
		if pm.Base.Model != "" {
			targetModel = pm.Base
		}
	}
	if targetModel.Model == "" {
		return
	}
	modelName, apiKey, baseURL := targetModel.Resolve(
		e.factory.Config.LLM.Model,
		e.factory.LLMClient.APIKey(),
		e.factory.LLMClient.BaseURL(),
	)
	subAgent.SetLLMClient(llm.NewClient(apiKey, baseURL, modelName, e.factory.Debug, e.factory.Config.LLM.MaxTokens))
	e.debugf("Model", "tier=%s → %s", tier, modelName)
}

// buildNodePrompt constructs the task string passed to a node's sub-agent.
func (e *PipelineExecutor) buildNodePrompt(node skills.PipelineNode, foreachItem interface{}, foreachIdx int) string {
	if node.Prompt == "" {
		return "Complete the assigned task."
	}
	return e.interpolatePrompt(node.Prompt, foreachItem, foreachIdx)
}

// resolveValue resolves a pipeline value to its runtime counterpart.
//
// String values are treated as variable references:
//
//	$foreach.current        → foreachItem
//	$foreach.current.field  → field of foreachItem (foreachItem must be a map)
//	$foreach.index          → foreachIdx (int)
//	$vars.name              → e.vars["name"]
//	<anything else>         → literal string (unchanged)
//
// Map and slice values are resolved recursively: string elements are treated
// as variable references while non-string elements (numbers, booleans, nil)
// are passed through unchanged. This lets inputs such as:
//
//	tools: []
//	pre_tool_args:
//	  path: $vars.file_path
//	  start_line: $foreach.current.start_line
//
// be expressed naturally in YAML and resolved at runtime.
func (e *PipelineExecutor) resolveValue(val interface{}, foreachItem interface{}, foreachIdx int) interface{} {
	switch v := val.(type) {
	case string:
		switch {
		case v == "$foreach.current":
			return foreachItem
		case v == "$foreach.index":
			return foreachIdx
		case strings.HasPrefix(v, "$foreach.current."):
			field := strings.TrimPrefix(v, "$foreach.current.")
			switch m := foreachItem.(type) {
			case map[string]interface{}:
				return m[field]
			case map[string]string:
				return m[field]
			default:
				rv := reflect.ValueOf(foreachItem)
				if rv.Kind() == reflect.Map && rv.Type().Key().Kind() == reflect.String {
					fv := rv.MapIndex(reflect.ValueOf(field))
					if fv.IsValid() {
						return fv.Interface()
					}
				}
				return nil
			}
		case strings.HasPrefix(v, "$vars."):
			key := strings.TrimPrefix(v, "$vars.")
			return e.vars[key]
		case strings.HasPrefix(v, "$") &&
			!strings.HasPrefix(v, "$foreach.") &&
			!strings.HasPrefix(v, "$config."):
			// $name shorthand for $vars.name
			return e.vars[strings.TrimPrefix(v, "$")]
		default:
			// Expand any {{$vars.x}} / {{$foreach.*}} / {{name}} tokens embedded in literals.
			return e.interpolatePrompt(v, foreachItem, foreachIdx)
		}
	case map[string]interface{}:
		resolved := make(map[string]interface{}, len(v))
		for k, mv := range v {
			resolved[k] = e.resolveValue(mv, foreachItem, foreachIdx)
		}
		return resolved
	case []interface{}:
		resolved := make([]interface{}, len(v))
		for i, ev := range v {
			resolved[i] = e.resolveValue(ev, foreachItem, foreachIdx)
		}
		return resolved
	default:
		return val // int, float64, bool, nil — pass through unchanged
	}
}

// interpolatePrompt replaces template tokens in a string.
// Supported forms (both are equivalent):
//
//	{{$vars.x}}  or  $vars.x
//	{{$foreach.current}}  or  $foreach.current
//	{{$foreach.index}}    or  $foreach.index
//	{{$config.workspace}}
func (e *PipelineExecutor) interpolatePrompt(tmpl string, foreachItem interface{}, foreachIdx int) string {
	result := tmpl
	if foreachIdx >= 0 {
		foreachStr := fmtVar(foreachItem)
		foreachIdxStr := fmt.Sprintf("%d", foreachIdx)
		result = strings.ReplaceAll(result, "{{$foreach.current}}", foreachStr)
		result = strings.ReplaceAll(result, "$foreach.current", foreachStr)
		result = strings.ReplaceAll(result, "{{$foreach.index}}", foreachIdxStr)
		result = strings.ReplaceAll(result, "$foreach.index", foreachIdxStr)
	}
	if e.factory != nil {
		workDir := e.factory.Config.EffectiveWorkDir()
		result = strings.ReplaceAll(result, "{{$config.workspace}}", workDir)
		result = strings.ReplaceAll(result, "$config.workspace", workDir)
	}

	result = varInterpolateRe.ReplaceAllStringFunc(result, func(match string) string {
		m := varInterpolateRe.FindStringSubmatch(match)
		var key string
		if m[1] != "" {
			key = m[1]
		} else if m[2] != "" {
			key = m[2]
		} else if m[3] != "" {
			key = m[3]
		}
		if v, ok := e.vars[key]; ok {
			return fmtVar(v)
		}
		return match
	})

	return result
}

// NodeSummary returns a brief human-readable summary of all node outputs
// for inclusion in the main conversation history so the agent can explain
// how it arrived at a pipeline result.
func (e *PipelineExecutor) NodeSummary() string {
	var sb strings.Builder
	sb.WriteString("Pipeline node outputs:\n")
	for _, node := range e.ps.Pipeline {
		for pvar := range node.Outputs {
			if v, ok := e.vars[pvar]; ok {
				fmt.Fprintf(&sb, "- %s → %s: %s\n", node.ID, pvar, truncateStr(fmtVar(v), 500))
			}
		}
	}
	return sb.String()
}

// formatTodos returns a formatted pipeline status string for display.
func (e *PipelineExecutor) formatTodos() string {
	icons := map[string]string{
		nodeStatusPending: "⬜",
		nodeStatusRunning: "🔄",
		nodeStatusDone:    "✅",
		nodeStatusFailed:  "❌",
		nodeStatusSkipped: "⏭️",
	}
	var sb strings.Builder
	name := "Pipeline"
	if e.skill != nil {
		name = e.skill.Name
	}
	fmt.Fprintf(&sb, "**%s**\n", name)
	for _, node := range e.ps.Pipeline {
		status := e.nodeStatus[node.ID]
		icon := icons[status]
		if icon == "" {
			icon = "⬜"
		}
		fmt.Fprintf(&sb, "%s %s\n", icon, node.ID)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// updateTodos sends or edits the todo notification message.
func (e *PipelineExecutor) updateTodos() {
	text := e.formatTodos()
	if e.sendFn != nil {
		if e.todoMsgID == "" {
			e.todoMsgID = e.sendFn(text)
		} else if e.editFn != nil {
			_ = e.editFn(e.todoMsgID, text)
		}
	} else if e.notifyFn != nil {
		e.notifyFn(text)
	}
}

// fmtVar converts a pipeline variable value to a string suitable for prompt injection.
// Strings are returned as-is. Complex values (slices, maps) are JSON-encoded so
// downstream models can parse them. Nil becomes empty string.
func fmtVar(v interface{}) string {
	switch val := v.(type) {
	case string:
		return val
	case nil:
		return ""
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}
}

func (e *PipelineExecutor) debugf(category, format string, args ...interface{}) {
	if !e.debug {
		return
	}
	fmt.Printf("  ◈  %-10s %s\n", category, fmt.Sprintf(format, args...))
}

// extractVarsKey returns the pipeline variable name from a single input reference string.
// Handles: $vars.name, $name (shorthand for $vars.name).
// Returns "" for non-variable references ($foreach.*, $config.*, or plain literals).
func extractVarsKey(ref string) string {
	if strings.HasPrefix(ref, "$vars.") {
		return strings.TrimPrefix(ref, "$vars.")
	}
	if strings.HasPrefix(ref, "$") &&
		!strings.HasPrefix(ref, "$foreach.") &&
		!strings.HasPrefix(ref, "$config.") {
		return strings.TrimPrefix(ref, "$")
	}
	return ""
}
