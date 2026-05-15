package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"ageage/config"
	"ageage/jsonutil"
	"ageage/llm"
	"ageage/security"
	"ageage/skills"
	"ageage/tools"
)

// EvalSnapshot captures all context needed to evaluate a skill execution.
type EvalSnapshot struct {
	Skill            *skills.Skill
	UserInput        string
	ToolHistory      []ToolRecord
	AgentOutput      string
	SkillDescription string
	OriginalError    string
	PipelineContext  string // node output summary from exec.NodeSummary(); empty for agent skills
}

// Evaluator performs background quality checks on auto-generated skill runs.
// It runs as a goroutine so the user receives their answer before evaluation starts.
type Evaluator struct {
	factory  *AgentFactory
	docsDir  string
	notifyFn func(string)
}

// NewEvaluator creates an Evaluator. notifyFn is called after every evaluation
// to surface the result (pass/improved/fail) to the user; it may be nil.
func NewEvaluator(factory *AgentFactory, docsDir string, notifyFn func(string)) *Evaluator {
	return &Evaluator{
		factory:  factory,
		docsDir:  docsDir,
		notifyFn: notifyFn,
	}
}

// Run evaluates the skill execution described by snap.
// Call as a goroutine — it blocks until evaluation completes then updates the
// skill's success_count on disk.
func (e *Evaluator) Run(ctx context.Context, snap EvalSnapshot) {
	cfg := e.factory.Config

	// Skip evaluation once the skill has graduated.
	threshold := cfg.Eval.SuccessThreshold
	if threshold > 0 && snap.Skill.SuccessCount >= threshold {
		return
	}

	// Model selection: strong on first run, cheaper on subsequent passes.
	var modelCfg config.ModelConfig
	if snap.Skill.SuccessCount == 0 {
		modelCfg = cfg.Router.StrongModel
	} else {
		if cfg.Eval.Model.Model != "" {
			modelCfg = cfg.Eval.Model
		} else {
			modelCfg = cfg.Router.MediumModel
		}
	}

	ag := e.makeAgent(snap.Skill.FilePath, modelCfg)

	userMsg := e.buildUserMessage(snap)
	result, err := ag.Run(ctx, userMsg, nil)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		fmt.Printf("[Evaluator] run failed for skill %s: %s\n", snap.Skill.Name, err)
		if e.notifyFn != nil {
			e.notifyFn(fmt.Sprintf("⚠️ Skill **%s** evaluation crashed: %s", snap.Skill.CommandName(), err))
		}
		return
	}

	verdict, fixed, reportToUser, evalSummary := parseEvalVerdict(result)

	switch {
	case verdict == "pass" && !fixed:
		_ = skills.UpdateSkillSuccessCount(snap.Skill.FilePath, true)
	case verdict == "pass" && fixed:
		_ = skills.UpdateSkillSuccessCount(snap.Skill.FilePath, false)
	case verdict == "fail":
		_ = skills.UpdateSkillSuccessCount(snap.Skill.FilePath, false)
	}
	if msg := formatEvalMessage(verdict, fixed, evalSummary, reportToUser); msg != "" && e.notifyFn != nil {
		e.notifyFn(msg)
	}
}

func formatEvalMessage(verdict string, fixed bool, summary, report string) string {
	switch {
	case verdict == "fail" && report != "":
		return "❌ **Evaluator:** " + report
	case verdict == "unknown":
		preview := report
		if preview == "" {
			preview = summary
		}
		if preview == "" {
			preview = "(no parseable verdict)"
		}
		return "⚠️ **Evaluator returned malformed verdict:** " + preview
	case fixed:
		return "🔧 **Evaluator improved skill:** " + summary
	}
	// Plain pass: no notification — the user already received their answer.
	return ""
}

// makeAgent creates a sandboxed agent for evaluation. The agent has access to
// file_read (skill dir + docs), the skill_patch tool (locked to the skill file),
// and finish_task.
func (e *Evaluator) makeAgent(skillFilePath string, modelCfg config.ModelConfig) *Agent {
	cfg := e.factory.Config
	skillDir := filepath.Dir(skillFilePath)

	sec := security.NewChecker(
		skillDir,
		[]string{},
		[]string{e.docsDir},
		[]string{},
	)

	modelName, apiKey, baseURL := modelCfg.Resolve(
		cfg.LLM.Model, e.factory.LLMClient.APIKey(), e.factory.LLMClient.BaseURL(),
	)
	client := llm.NewClient(apiKey, baseURL, modelName, e.factory.Debug, cfg.LLM.MaxTokens)

	registry := tools.NewRegistry()
	finishTool := &tools.FinishTool{}
	registry.Register(finishTool)
	registry.Register(&tools.FileReadTool{Security: sec})
	registry.Register(&skillPatchTool{skillFilePath: skillFilePath})

	ag := NewAgent(cfg, client, registry, finishTool, nil, e.factory.Debug)
	ag.Mode = AgentMode{IsSubAgent: true}
	ag.MaxIterations = 8

	ag.conv.Reset([]llm.Message{{Role: "system", Content: e.systemPrompt(skillFilePath)}})
	return ag
}

func (e *Evaluator) systemPrompt(skillFilePath string) string {
	var fileTypeHint string
	ext := strings.ToLower(filepath.Ext(skillFilePath))
	if strings.HasSuffix(ext, ".yaml") || strings.HasSuffix(ext, ".yml") {
		fileTypeHint = "This is a PIPELINE (.yaml) skill. It must remain a pipeline. Do NOT convert it to a .md agent skill."
	} else {
		fileTypeHint = "This is an AGENT (.md) skill. It must remain an agent skill with YAML frontmatter."
	}

	return fmt.Sprintf(`You are a skill quality evaluator AND optimizer.

Skill file: %s

%s

Use file_read to inspect the skill file. Use skill_patch to rewrite the file when fixing or optimizing.

## Priority 0 — PRESERVE GENERALITY (highest)

The skill's description is its general contract. It must keep working for ALL future
inputs that match the description, not just the one input that triggered this review.

NEVER bake the specific user input, URL, topic, file path, or example into the skill's
prompts, tools, or instructions. The execution you are reviewing is ONE data point —
treat it like a test case, not like a specification.

❌ BAD (narrowing — do not do this):
   Original prompt:  "Read {{input}} and summarize its main topic"
   Bad patched:      "Read the Wikipedia article about Go and summarize it for the user.
                      The user wants Go's history and syntax."

✅ GOOD (generalizing — fix the mechanism, not the example):
   Original prompt:  "Read {{input}} and summarize its main topic"
   Good patched:     "Read the URL given in {{input}}. If the page returns an error or
                      is empty, explain the failure and suggest a retry or alternative.
                      Otherwise write a 3-paragraph summary covering: main topic,
                      key claims, source credibility."

Self-check before patching: substitute {{input}} with a completely different but
description-matching value. Does the patched prompt still make sense? If not, you are
over-specializing — revert.

## Priority 1 — DIAGNOSTIC CHECKLIST

Walk through these in order. Most evaluations need to fix item (a) or (b); do not jump
to cosmetic tweaks before checking these:

  (a) Output quality. Did the last node produce a complete, user-useful result?
      If output was empty / just "done" / wrong format (JSON when prose was needed) /
      missing key content — this is the #1 thing to fix. Look at the LAST agent node's
      prompt: does it explicitly say "put the COMPLETE final answer into node_complete
      vars.<key>" and "the user sees ONLY this"? If not, add it.
  (b) Fault tolerance. When a step fails (network error, empty content, refusal, missing
      data) does the downstream prompt know how to handle it, or does the pipeline just
      crash / produce garbage? Add explicit "if X is empty/missing, do Y" instructions.
  (c) Prompt clarity. Each agent node's prompt MUST state: goal, meaning of each {{var}},
      output format, and "user cannot see intermediate work."
  (d) Tool fit. Is required_tools (or node-level tools:) minimal and sufficient?
  (e) Variable plumbing. Every referenced {{var}} produced by a prior node? outputs
      key names correct?
  (f) Tier appropriateness. Single fetch+summarize → base. Multi-tool reasoning →
      medium. Cross-system workflows → strong. Don't inflate.
  (g) Structural sanity. First node type:agent? Last node produces returns: target
      (or result/output/answer)?

## Priority 2 — DECIDE

After the checklist:
- Issues exist AND fixable in the skill → call skill_patch with the corrected file,
  then finish_task with verdict="pass", fixed=true, summary="<what you changed and why>".
- Issues exist but they are environment problems (missing API key, broken external
  service, missing binary, user permission) → finish_task with verdict="fail",
  fixed=false, report_to_user="<plain-language explanation for the user>".
- No real issues → finish_task with verdict="pass", fixed=false, summary="<one sentence>".

## finish_task format

  {"verdict":"pass","fixed":false,"summary":"One sentence.","report_to_user":""}
  {"verdict":"pass","fixed":true,"summary":"One sentence about the fix.","report_to_user":""}
  {"verdict":"fail","fixed":false,"summary":"One sentence.","report_to_user":"<for user>"}

Other rules:
- Always populate "summary" with one sentence describing what you found or did.
- Do NOT patch for style preferences or correct behavior the user disliked.
- Preserve auto_generated: true and reset success_count: 0 when patching.

%s`, skillFilePath, fileTypeHint, pipelineSchemaHint)
}

func (e *Evaluator) buildUserMessage(snap EvalSnapshot) string {
	var sb strings.Builder
	sb.WriteString("# Skill Under Review — the GENERAL contract (preserve this)\n\n")
	fmt.Fprintf(&sb, "Description: %s\n\n", snap.SkillDescription)
	sb.WriteString("# This Specific Execution — ONE data point (do NOT bake into the skill)\n\n")
	sb.WriteString("Treat the inputs below as a test case. Fix the mechanism so any\n")
	sb.WriteString("description-matching input would work — never hardcode this example.\n\n")
	fmt.Fprintf(&sb, "User input (single example): %s\n\n", snap.UserInput)
	fmt.Fprintf(&sb, "Agent output:\n%s\n", snap.AgentOutput)
	if snap.OriginalError != "" {
		fmt.Fprintf(&sb, "\nOriginal error: %s\n", snap.OriginalError)
	}
	if snap.PipelineContext != "" {
		fmt.Fprintf(&sb, "\n%s\n", snap.PipelineContext)
	}
	if len(snap.ToolHistory) > 0 {
		sb.WriteString("\nTool calls (this run only):\n")
		for i, tr := range snap.ToolHistory {
			args := tr.Args
			if len(args) > 1000 {
				args = args[:1000] + "…"
			}
			result := tr.Result
			if len(result) > 1500 {
				result = result[:1500] + "…"
			}
			fmt.Fprintf(&sb, "%d. %s(%s) → %s\n", i+1, tr.Name, args, result)
		}
	}
	sb.WriteString("\n# Your Task\n\n")
	sb.WriteString("Walk the diagnostic checklist in the system prompt. If you patch, patch\n")
	sb.WriteString("for ALL future inputs matching the description — not for this one.\n")
	sb.WriteString("Then call finish_task with the JSON verdict.")
	return sb.String()
}

// evalFieldRE extracts a string-valued field from a possibly-malformed JSON
// blob. Captures only single-line content (no escaped quote handling) which is
// the common case for evaluator output.
var evalFieldRE = regexp.MustCompile(`"(verdict|report_to_user|summary)"\s*:\s*"([^"]*)"`)

// parseEvalVerdict extracts verdict fields from the evaluator agent's summary.
// The summary is expected to be a JSON object. Falls back to regex extraction
// when JSON parsing fails (LLMs frequently emit slightly malformed JSON with
// embedded control characters or stray markdown). When even the regex finds
// nothing, returns verdict="unknown" with the raw summary as report so the
// user is notified instead of silently swallowing the result.
func parseEvalVerdict(summary string) (verdict string, fixed bool, reportToUser string, evalSummary string) {
	// Try to extract JSON from the summary (agent may include surrounding text).
	jsonText := summary
	start := strings.Index(jsonText, "{")
	end := strings.LastIndex(jsonText, "}")
	if start >= 0 && end > start {
		jsonText = jsonText[start : end+1]
	}

	var v struct {
		Verdict      string `json:"verdict"`
		Fixed        bool   `json:"fixed"`
		ReportToUser string `json:"report_to_user"`
		Summary      string `json:"summary"`
	}
	if err := jsonutil.ParseToolArgs(jsonText, &v); err == nil && v.Verdict != "" {
		return v.Verdict, v.Fixed, v.ReportToUser, v.Summary
	}

	// JSON parsing failed or yielded no verdict. Try to pull the named fields
	// out with a regex so the user still gets the substance of the message.
	fields := map[string]string{}
	for _, m := range evalFieldRE.FindAllStringSubmatch(summary, -1) {
		fields[m[1]] = m[2]
	}
	if fields["verdict"] != "" {
		return fields["verdict"], false, fields["report_to_user"], fields["summary"]
	}

	// Last resort: surface the raw text under verdict="unknown" so the user
	// learns the evaluator returned a malformed verdict instead of nothing.
	preview := strings.TrimSpace(summary)
	if len(preview) > 300 {
		preview = preview[:300] + "…"
	}
	return "unknown", false, preview, ""
}

// ── skillPatchTool ─────────────────────────────────────────────────────────────

// skillPatchTool is an evaluator-only tool that overwrites a single locked skill
// file. It cannot be used to write to any other path.
type skillPatchTool struct {
	skillFilePath string
}

func (t *skillPatchTool) Name() string { return "skill_patch" }

func (t *skillPatchTool) Description() string {
	return "Replace the entire content of the active skill file with the provided content. " +
		"Use this to fix deficiencies in the skill's prompt or tool configuration."
}

func (t *skillPatchTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"content": map[string]interface{}{
				"type":        "string",
				"description": "The complete new content for the skill file.",
			},
		},
		"required": []string{"content"},
	}
}

func (t *skillPatchTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if err := os.WriteFile(t.skillFilePath, []byte(params.Content), 0o644); err != nil {
		return "", fmt.Errorf("write skill file: %w", err)
	}
	if errs := ValidateSkillFile(t.skillFilePath, nil); len(errs) > 0 {
		var msgs []string
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return "", fmt.Errorf("patched file failed validation: %s", strings.Join(msgs, "; "))
	}
	return "Skill file updated and validated.", nil
}
