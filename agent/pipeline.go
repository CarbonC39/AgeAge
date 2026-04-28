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
	skill     *skills.Skill   // wrapping Skill metadata (name, description)
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

		// Each node execution writes outputs into a local map.
		// Merging into e.vars happens here (after the node completes) so that
		// node execution functions never write directly to shared state —
		// this is what makes parallel foreach safe.
		out := make(map[string]interface{})
		var err error

		if node.Foreach != "" {
			err = e.execForeach(ctx, node)
			// execForeach merges its own outputs directly; out is unused.
		} else {
			err = e.execNode(ctx, node, nil, -1, out)
			if err == nil {
				e.mergeOutputs(out)
			}
		}

		if err != nil {
			if e.nestDepth == 0 {
				e.nodeStatus[node.ID] = nodeStatusFailed
				// Mark all remaining nodes as skipped.
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
			return "", fmt.Errorf("pipeline node %q failed: %w", node.ID, err)
		}

		if e.nestDepth == 0 {
			e.nodeStatus[node.ID] = nodeStatusDone
			e.updateTodos()
		}
	}

	// Return the first common output variable found, or a fallback message.
	for _, name := range []string{"result", "output", "answer"} {
		if v, ok := e.vars[name]; ok {
			return fmt.Sprintf("%v", v), nil
		}
	}
	return "Pipeline completed successfully.", nil
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
	default:
		return fmt.Errorf("foreach variable %q must be an array (got %T)", node.Foreach, arrayVal)
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

		// Collect all results; cancel on first error.
		results := make([]iterResult, len(items))
		var firstErr error
		for i := 0; i < len(items); i++ {
			r := <-resCh
			if r.err != nil && firstErr == nil {
				firstErr = fmt.Errorf("foreach iteration %d: %w", r.idx, r.err)
				subCancel() // signal remaining goroutines to stop
			}
			results[r.idx] = r
		}
		if firstErr != nil {
			return firstErr
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

	result, err := e.sharedReg.Execute(ctx, node.Tool, argsJSON)
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

	// Build tool allowlist: skill's required tools ∪ node's explicit tools.
	// A nil allowlist means all global tools are available.
	var filteredTools []string
	if nodeSkill != nil && len(nodeSkill.RequiredTools) > 0 || len(node.Tools) > 0 {
		var toolList []string
		if nodeSkill != nil {
			toolList = append(toolList, nodeSkill.RequiredTools...)
		}
		toolList = append(toolList, node.Tools...)
		filteredTools = UniqueStrings(toolList)
	}

	// Build the task prompt once; it is the same for all attempts.
	taskPrompt := e.buildNodePrompt(node, foreachItem, foreachIdx)
	if len(node.Outputs) > 0 {
		var keys []string
		for _, k := range node.Outputs {
			keys = append(keys, k)
		}
		taskPrompt += fmt.Sprintf(
			"\n\n---\nIMPORTANT: Call node_complete when done. Expected keys in the 'vars' argument: %s.",
			strings.Join(keys, ", "),
		)
	}

	// Attempt execution, falling back one tier on transient LLM errors.
	complexities := e.complexityFallbacks(node.Complexity)

	var lastErr error
	for attempt, complexity := range complexities {
		if attempt > 0 {
			e.debugf("Fallback", "%s  attempt %d failed (%s) → retrying with complexity=%s", node.ID, attempt, lastErr, complexity)
		}
		nodeOut, err := e.runAgentNodeAttempt(ctx, node, nodeSkill, filteredTools, taskPrompt, complexity)
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
// using the given complexity (which may differ from node.Complexity on retry).
func (e *PipelineExecutor) runAgentNodeAttempt(
	ctx context.Context,
	node skills.PipelineNode,
	nodeSkill *skills.Skill,
	filteredTools []string,
	taskPrompt string,
	complexity string,
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

	// Apply model from complexity (node or fallback).
	e.applyNodeModel(subAgent, complexity)

	// Pre-inject the system prompt. RunWithParts checks len(messages)==0 to
	// determine the first turn; setting it here tells RunWithParts that the
	// system prompt is already built and should not be overwritten.
	sysprompt := subAgent.buildSystemPrompt(nodeSkill)
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
		// Agent returned without calling node_complete (direct answer or max
		// iterations reached). Fall back: treat text as "result".
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

	// Surface LLM errors that node_complete didn't mask.
	if runErr != nil {
		return nil, fmt.Errorf("agent node %q: %w", node.ID, runErr)
	}

	e.debugf("Agent◁", "%s  vars=%v", node.ID, nodeResult.Vars)

	out := make(map[string]interface{}, len(node.Outputs))
	for pipelineVar, nodeOutputKey := range node.Outputs {
		if v, ok := nodeResult.Vars[nodeOutputKey]; ok {
			out[pipelineVar] = v
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
		val := fmt.Sprintf("%v", e.resolveValue(varRef, foreachItem, foreachIdx))
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

// complexityFallbacks returns the ordered list of complexity tiers to attempt.
// On a transient LLM error the engine retries once with the next lower tier.
func (e *PipelineExecutor) complexityFallbacks(primary string) []string {
	tiers := []string{primary}
	switch normalizeComplexity(primary) {
	case TaskWorkflow:
		tiers = append(tiers, string(TaskAtomic))
	case TaskAtomic:
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

// applyNodeModel sets the sub-agent's LLM client based on node complexity.
// Checks [pipeline.models] first; falls back to [router] model settings.
// No-ops if complexity is empty or no matching model is configured.
func (e *PipelineExecutor) applyNodeModel(subAgent *Agent, complexity string) {
	if complexity == "" {
		return
	}
	pm := e.factory.Config.Pipeline.Models
	var targetModel config.ModelConfig
	switch normalizeComplexity(complexity) {
	case TaskWorkflow:
		if pm.Complex.Model != "" {
			targetModel = pm.Complex
		} else {
			targetModel = e.factory.Config.Router.StrongModel
		}
	case TaskAtomic:
		if pm.Medium.Model != "" {
			targetModel = pm.Medium
		} else {
			targetModel = e.factory.Config.Router.MediumModel
		}
	case TaskDirect:
		if pm.Simple.Model != "" {
			targetModel = pm.Simple
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
	e.debugf("Model", "complexity=%s → %s", complexity, modelName)
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
			if m, ok := foreachItem.(map[string]interface{}); ok {
				return m[field]
			}
			return nil
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
		foreachStr := fmt.Sprintf("%v", foreachItem)
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
			return fmt.Sprintf("%v", v)
		}
		return match
	})

	return result
}

// ── Todo display ──────────────────────────────────────────────────────────────

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
	} else if e.notifyFn != nil && e.todoMsgID == "" {
		// Plain notify: send once at the start; incremental updates not possible.
		e.notifyFn(text)
		e.todoMsgID = "notified"
	}
}

func (e *PipelineExecutor) debugf(category, format string, args ...interface{}) {
	if !e.debug {
		return
	}
	fmt.Printf("  ◈  %-10s %s\n", category, fmt.Sprintf(format, args...))
}
