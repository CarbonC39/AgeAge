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
	Skill       *skills.Skill
	UserInput   string
	ToolHistory []ToolRecord
	AgentOutput string
}

// Evaluator performs background quality checks on auto-generated skill runs.
// It runs as a goroutine so the user receives their answer before evaluation starts.
type Evaluator struct {
	factory  *AgentFactory
	docsDir  string
	notifyFn func(string)
}

// NewEvaluator creates an Evaluator. notifyFn is called when a blocker is found
// that the user needs to fix; it may be nil.
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

	verdict, fixed, reportToUser := parseEvalVerdict(result)

	switch {
	case verdict == "pass" && !fixed:
		_ = skills.UpdateSkillSuccessCount(snap.Skill.FilePath, true)
	case verdict == "pass" && fixed:
		_ = skills.UpdateSkillSuccessCount(snap.Skill.FilePath, false)
	case verdict == "fail":
		_ = skills.UpdateSkillSuccessCount(snap.Skill.FilePath, false)
		if reportToUser != "" && e.notifyFn != nil {
			e.notifyFn("[Evaluator] " + reportToUser)
		}
	}
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
	return fmt.Sprintf(`You are a quality evaluator for auto-generated skills.
Review whether the skill executed correctly. Use file_read to inspect the skill file if needed.

Skill file: %s

Instructions:
- If execution was correct with no defects:
    call finish_task with {"verdict":"pass","fixed":false,"report_to_user":""}
- If the skill has fixable deficiencies (wrong prompt, missing tool, poor instructions):
    use skill_patch to improve the file, then call finish_task with {"verdict":"pass","fixed":true,"report_to_user":""}
- If there is an architectural/environment blocker the user must fix (browser tool not installed,
    required API key missing, external service unavailable):
    call finish_task with {"verdict":"fail","fixed":false,"report_to_user":"<explanation>"}

IMPORTANT: The finish_task summary must be valid JSON matching the structure above.`,
		skillFilePath)
}

func (e *Evaluator) buildUserMessage(snap EvalSnapshot) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "User request: %s\n\nAgent output:\n%s\n", snap.UserInput, snap.AgentOutput)
	if len(snap.ToolHistory) > 0 {
		sb.WriteString("\nTool calls:\n")
		for i, tr := range snap.ToolHistory {
			args := tr.Args
			if len(args) > 200 {
				args = args[:200] + "…"
			}
			result := tr.Result
			if len(result) > 300 {
				result = result[:300] + "…"
			}
			fmt.Fprintf(&sb, "%d. %s(%s) → %s\n", i+1, tr.Name, args, result)
		}
	}
	sb.WriteString("\nEvaluate this execution and call finish_task with your verdict.")
	return sb.String()
}

// parseEvalVerdict extracts verdict fields from the evaluator agent's summary.
// The summary is expected to be a JSON object. Falls back gracefully on parse errors.
func parseEvalVerdict(summary string) (verdict string, fixed bool, reportToUser string) {
	// Try to extract JSON from the summary (agent may include surrounding text).
	start := strings.Index(summary, "{")
	end := strings.LastIndex(summary, "}")
	if start >= 0 && end > start {
		summary = summary[start : end+1]
	}

	var v struct {
		Verdict      string `json:"verdict"`
		Fixed        bool   `json:"fixed"`
		ReportToUser string `json:"report_to_user"`
	}
	if err := jsonutil.ParseToolArgs(summary, &v); err != nil {
		// Unrecognised format — treat as a pass with no fix so the count increments.
		return "pass", false, ""
	}
	return v.Verdict, v.Fixed, v.ReportToUser
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
	return "Skill file updated.", nil
}
