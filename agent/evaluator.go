package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	if err != nil || ctx.Err() != nil {
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

	return fmt.Sprintf(`You are a skill quality evaluator AND optimizer. Your goals, in order:

0. PRESERVE the skill's original scope and generality. The skill description tells you its intended purpose — the skill must remain usable for ALL future tasks matching that description. Never rewrite it to solve only the specific request that triggered this evaluation. If the user asked to search for a particular topic and the pipeline failed, fix the SEARCH MECHANISM — do NOT hardcode the pipeline to always search that topic.
1. Fix deficiencies: wrong/missing tools in required_tools, broken prompt instructions, bad output handling.
2. Optimize workflow: reduce unnecessary agent turns, tighten tool lists, clarify node_complete expectations, simplify over-complex pipelines.
3. Report blockers the user must fix themselves (missing binary, missing API key, etc.).

Skill file: %s

%s

Use file_read to inspect the skill file. Use skill_patch to rewrite the file when fixing or optimizing.

When done, call finish_task with a JSON summary:
  {"verdict":"pass","fixed":false,"summary":"One sentence describing what was found.","report_to_user":""}
  {"verdict":"pass","fixed":true,"summary":"One sentence describing what was improved.","report_to_user":""}
  {"verdict":"fail","fixed":false,"summary":"One sentence.","report_to_user":"<explanation for the user>"}

Rules:
- Always populate "summary" with one sentence describing what you found or did.
- Do NOT patch for style preferences or correct behavior the user disliked.
- Do NOT patch for environment-specific problems beyond the skill's control.
- NEVER narrow the skill's scope or purpose to match only the task that triggered this evaluation. A general-purpose search pipeline must remain general-purpose.
- Preserve auto_generated: true and reset success_count: 0 when patching.

%s`, skillFilePath, fileTypeHint, pipelineSchemaHint)
}

func (e *Evaluator) buildUserMessage(snap EvalSnapshot) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Skill description: %s\n\n", snap.SkillDescription)
	fmt.Fprintf(&sb, "User request: %s\n\nAgent output:\n%s\n", snap.UserInput, snap.AgentOutput)
	if snap.OriginalError != "" {
		fmt.Fprintf(&sb, "\nOriginal error: %s\n", snap.OriginalError)
	}
	if snap.PipelineContext != "" {
		fmt.Fprintf(&sb, "\n%s\n", snap.PipelineContext)
	}
	if len(snap.ToolHistory) > 0 {
		sb.WriteString("\nTool calls:\n")
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
	sb.WriteString("\nEvaluate this execution and call finish_task with your verdict.")
	return sb.String()
}

// parseEvalVerdict extracts verdict fields from the evaluator agent's summary.
// The summary is expected to be a JSON object. Falls back to text-pattern detection
// when JSON parsing fails (LLMs frequently emit slightly malformed JSON with embedded
// control characters or stray markdown).
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
	if err := jsonutil.ParseToolArgs(jsonText, &v); err == nil {
		return v.Verdict, v.Fixed, v.ReportToUser, v.Summary
	}

	// JSON parsing failed. Fall back to keyword detection so the user is not pestered
	// with "format invalid" messages and the success counter still progresses sensibly.
	low := strings.ToLower(summary)
	switch {
	case strings.Contains(low, `"verdict":"fail"`) || strings.Contains(low, `"verdict": "fail"`):
		return "fail", false, "", ""
	case strings.Contains(low, `"verdict":"pass"`) || strings.Contains(low, `"verdict": "pass"`):
		return "pass", false, "", ""
	}
	// Unrecognised — return empty values; Run() will skip both the count update
	// and the user notification.
	return "", false, "", ""
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
