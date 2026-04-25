package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ageage/llm"
	"ageage/security"
	"ageage/skills"
	"ageage/tools"
)

const maxPlannerRetries = 3

// Planner creates new skill files for complex tasks that have no matching skill.
// It runs an isolated strong-model agent, validates the generated file with
// ValidateSkillFile, and retries up to maxPlannerRetries times on failure.
type Planner struct {
	factory   *AgentFactory
	docsDir   string
	skillsDir string
}

// NewPlanner returns a Planner scoped to the given docs directory.
func NewPlanner(factory *AgentFactory, docsDir string) *Planner {
	return &Planner{
		factory:   factory,
		docsDir:   docsDir,
		skillsDir: factory.Config.SkillsDir(),
	}
}

// CreateSkill asks a sandboxed agent to author a new skill file, validates it,
// and returns the loaded Skill. It retries up to maxPlannerRetries times when
// the generated file fails validation. Returns an error if all attempts fail.
func (p *Planner) CreateSkill(ctx context.Context, userTask string) (*skills.Skill, error) {
	if err := os.MkdirAll(p.skillsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create skills dir: %w", err)
	}

	ag := p.makeAgent()

	// Snapshot existing skill files so we can detect the newly created one.
	preFiles := fileNameSet(p.skillsDir)

	var (
		prompt      = p.buildPrompt(userTask)
		trackedPath string // the file created in the first successful attempt
	)

	for attempt := 0; attempt < maxPlannerRetries; attempt++ {
		ag.finishTool.Reset()

		if _, err := ag.Run(ctx, prompt, nil); err != nil {
			return nil, fmt.Errorf("planner agent failed: %w", err)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// On the first attempt, find the newly created file.
		if trackedPath == "" {
			trackedPath = firstNewSkillFile(p.skillsDir, preFiles)
			if trackedPath == "" {
				prompt = fmt.Sprintf(
					"You did not create any skill file. Write a .md or .yaml file to %s "+
						"and then call finish_task.", p.skillsDir)
				preFiles = fileNameSet(p.skillsDir)
				continue
			}
		}

		errs := ValidateSkillFile(trackedPath, nil)
		if len(errs) == 0 {
			skill, err := skills.LoadSkillByPath(trackedPath)
			if err != nil || skill == nil {
				return nil, fmt.Errorf("load generated skill %s: %v", trackedPath, err)
			}
			p.reloadFactorySkills()
			return skill, nil
		}

		// Build feedback for the next retry.
		var sb strings.Builder
		fmt.Fprintf(&sb, "File %s has validation errors — fix them and call finish_task again:\n",
			filepath.Base(trackedPath))
		for i, e := range errs {
			fmt.Fprintf(&sb, "%d. %s\n", i+1, e.Error())
		}
		prompt = sb.String()
	}

	return nil, fmt.Errorf("skill creation failed after %d attempts (validation errors remain)", maxPlannerRetries)
}

// makeAgent builds an isolated agent for the planner run.
// It uses the strong model, a sandboxed security checker scoped to the skills
// and docs directories, and only registers file_read, file_write, finish_task.
func (p *Planner) makeAgent() *Agent {
	cfg := p.factory.Config

	sec := security.NewChecker(
		p.skillsDir,
		[]string{},          // no bash commands
		[]string{p.docsDir}, // also allow reading the docs directory
		[]string{},
	)

	modelName, apiKey, baseURL := cfg.Router.StrongModel.Resolve(
		cfg.LLM.Model, p.factory.LLMClient.APIKey(), p.factory.LLMClient.BaseURL(),
	)
	client := llm.NewClient(apiKey, baseURL, modelName, p.factory.Debug, cfg.LLM.MaxTokens)

	registry := tools.NewRegistry()
	finishTool := &tools.FinishTool{}
	registry.Register(finishTool)
	registry.Register(&tools.FileReadTool{Security: sec})
	registry.Register(&tools.FileWriteTool{
		Security:    sec,
		Supervised:  false,
		ConfirmFunc: func(string) bool { return true },
	})

	ag := NewAgent(cfg, client, registry, finishTool, nil, p.factory.Debug)
	ag.Mode = AgentMode{IsSubAgent: true}
	ag.MaxIterations = 15

	// Pre-populate the system prompt so the agent keeps it on repeated Run() calls.
	ag.conv.Reset([]llm.Message{{Role: "system", Content: p.systemPrompt()}})

	return ag
}

func (p *Planner) systemPrompt() string {
	return fmt.Sprintf(`You are a skill architect. Your only job is to create a single reusable skill file.

Skills directory: %s
Framework docs (read these first): %s

Rules:
- Write exactly ONE file to the skills directory.
- Set "auto_generated: true" and "success_count: 0" in the file.
- For agent skills (.md): use YAML frontmatter with name, description, complexity, auto_generated, success_count.
- For pipeline skills (.yaml): follow the schema in %s/pipeline.md.
- Valid complexity values: simple, medium, complex.
- Always call finish_task when done.`, p.skillsDir, p.docsDir, p.docsDir)
}

func (p *Planner) buildPrompt(userTask string) string {
	return fmt.Sprintf("Create a reusable skill or pipeline for the following task:\n\n%s", userTask)
}

// reloadFactorySkills forces a hot-reload of the factory's skill list so the
// newly created skill is visible to subsequent router calls.
func (p *Planner) reloadFactorySkills() {
	newSkills, err := skills.LoadSkills(p.skillsDir)
	if err != nil {
		return
	}
	p.factory.skillsMu.Lock()
	p.factory.Skills = newSkills
	p.factory.skillsMu.Unlock()
}

// fileNameSet returns the set of filenames (base names only) currently in dir.
func fileNameSet(dir string) map[string]bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	set := make(map[string]bool, len(entries))
	for _, e := range entries {
		set[e.Name()] = true
	}
	return set
}

// firstNewSkillFile returns the absolute path of the first .md/.yaml/.yml file
// in dir that does not appear in existing.
func firstNewSkillFile(dir string, existing map[string]bool) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || existing[e.Name()] {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".md" || ext == ".yaml" || ext == ".yml" {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}
