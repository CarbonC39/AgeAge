package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ageage/internal/agentdocs"
	"ageage/llm"
	"ageage/security"
	"ageage/skills"
	"ageage/tools"
)

const maxPlannerRetries = 3

// pipelineSchemaHint is a compact pipeline YAML reference injected into
// planner and evaluator prompts to prevent hallucinations about field names
// and outputs semantics.
const pipelineSchemaHint = `## Pipeline YAML — Compact Schema

` + "```" + `yaml
name: my-skill
description: "..."
tier: base|medium|strong      # optional; skips router when set
returns: answer               # which pipeline var to return to user; defaults to result/output/answer
vars:                         # typed YAML defaults (string/number/bool/list/map)
  my_var: ""

pipeline:
  - id: step1                 # required, unique
    type: agent               # default — uses LLM; OR type: auto — direct tool call (no LLM)

    # agent-only
    prompt: |                 # {{my_var}} {{$vars.x}} {{$foreach.current}} {{$foreach.index}}
      Do something with {{input}}
    tools: [file_read, bash]  # optional allowlist for this node's sub-agent
    tier: medium              # base | medium | strong

    # auto-only
    tool: web_fetch           # required for type:auto
    inputs:                   # key → literal or $vars.x or $foreach.current
      url: $vars.input

    # both
    outputs: my_var           # SCALAR  → reads "result" key from node_complete → stored as my_var
    # outputs: [a, b]         # LIST    → a reads key "a", b reads key "b" (each var = own key name)
    # outputs: {x: result}    # MAP     → pipeline var x reads the node_complete key named "result"
    foreach: my_list          # iterate over array var
    validate: not_empty       # fail node if any resolved input is empty
` + "```" + `

**node_complete rules (agent nodes):**
- Call: node_complete(status="success"|"failure", vars={...}, reason="...", context="...")
- Scalar outputs: put the value under key "result"
- List outputs: put each value under its own name (e.g. vars={"a": ..., "b": ...})
- Map outputs: put each value under the right-hand key declared in the map
- NEVER invent key names — use exactly what the schema declares
- CRITICAL: The end user CANNOT see the agent's internal work. You MUST include
  COMPLETE results in the vars — never signal just "done". The vars content IS
  the only output the user receives.`

// Planner creates new skill files for complex tasks that have no matching skill.
// It runs an isolated strong-model agent, validates the generated file with
// ValidateSkillFile, and retries up to maxPlannerRetries times on failure.
type Planner struct {
	factory   *AgentFactory
	docsDir   string
	skillsDir string
	toolNames []string
}

// NewPlanner returns a Planner scoped to the given docs directory.
// toolNames is the list of tools available to the main agent (not the planner itself).
func NewPlanner(factory *AgentFactory, docsDir string, toolNames []string) *Planner {
	return &Planner{
		factory:   factory,
		docsDir:   docsDir,
		skillsDir: factory.Config.SkillsDir(),
		toolNames: toolNames,
	}
}

// CreateSkill asks a sandboxed agent to author a new skill file, validates it,
// and returns the loaded Skill. It retries up to maxPlannerRetries times when
// the generated file fails validation. Returns an error if all attempts fail.
//
// history is an optional snapshot of the caller's conversation (used by /build
// to give the planner context). Pass nil for the automatic workflow invocation.
func (p *Planner) CreateSkill(ctx context.Context, userTask string, history []llm.Message) (*skills.Skill, error) {
	if err := os.MkdirAll(p.skillsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create skills dir: %w", err)
	}

	ag := p.makeAgent()

	// Snapshot existing skill files so we can detect the newly created one.
	preFiles := fileNameSet(p.skillsDir)

	var (
		prompt      = p.buildPrompt(userTask, history)
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
			// Extract trackedPath from finish_task summary
			trackedPath = strings.TrimSpace(ag.finishTool.Summary)
			if trackedPath == "" || !strings.HasPrefix(trackedPath, p.skillsDir) {
				trackedPath = firstNewSkillFile(p.skillsDir, preFiles)
			}
			if trackedPath == "" {
				prompt = fmt.Sprintf(
					"You did not create any skill file or failed to provide its path. Write a .md or .yaml file to %s "+
						"and then call finish_task with its absolute path as the summary.", p.skillsDir)
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
			// Normalise the filename so it matches skill.CommandName(). Prevents
			// snake_case-vs-kebab-case mismatches between the on-disk name and
			// the YAML's `name:` field (which break /command lookups and produce
			// duplicate files on subsequent agent runs).
			canonical := filepath.Join(filepath.Dir(trackedPath), skill.CommandName()+filepath.Ext(trackedPath))
			if canonical != trackedPath {
				if rErr := os.Rename(trackedPath, canonical); rErr == nil {
					trackedPath = canonical
					skill.FilePath = canonical
				}
			}
			// Remove any other skill files the agent created besides the tracked one.
			cleanupExtraSkillFiles(p.skillsDir, preFiles, filepath.Base(trackedPath))
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
	registry.Register(&tools.FileReadTool{Security: sec, DocsDir: p.docsDir})
	registry.Register(&tools.FileWriteTool{
		Security:    sec,
		Supervised:  false,
		ConfirmFunc: func(string) bool { return true },
	})

	ag := NewAgent(cfg, client, registry, finishTool, nil, p.factory.Debug)
	ag.Mode = AgentMode{IsSubAgent: true}
	ag.MaxIterations = 15

	// Embed doc content directly so models that skip file_read still get the schema.
	guide, _ := agentdocs.Read("planner-guide.md")
	pipeline, _ := agentdocs.Read("pipeline.md")
	ag.conv.Reset([]llm.Message{{Role: "system", Content: p.buildSystemPrompt(guide, pipeline)}})

	return ag
}

func (p *Planner) buildSystemPrompt(guide, pipeline string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "You are a skill architect. Your only job is to create a single reusable skill file.\n\n")
	fmt.Fprintf(&sb, "Skills directory: %s\n\n", p.skillsDir)

	if len(p.toolNames) > 0 {
		sb.WriteString("## Available Tools\n\n")
		sb.WriteString("Use ONLY these tool names in required_tools, node `tools:`, and `tool:` fields:\n")
		sb.WriteString(strings.Join(p.toolNames, ", "))
		sb.WriteString("\n\nReferencing a tool not in this list will cause runtime failures.\n\n")
	}

	if guide != "" {
		sb.WriteString("## Framework Reference\n\n")
		sb.WriteString(guide)
		sb.WriteString("\n\n")
	}
	if pipeline != "" {
		sb.WriteString("## Pipeline Schema\n\n")
		sb.WriteString(pipeline)
		sb.WriteString("\n\n")
	}

	sb.WriteString("## Rules\n" +
		"- Write exactly ONE file to the skills directory (.md for agent skills, .yaml for pipelines).\n" +
		"- Set auto_generated: true and success_count: 0 in the file.\n" +
		"- Use the `tier:` field (not `complexity:`). Valid values: base, medium, strong.\n" +
		"- For agent skills (.md): always set required_tools to the minimal list the skill needs.\n" +
		"- For pipeline .yaml files: if the YAML fails validation, use file_read to read the ENTIRE\n" +
		"  file back first, then rewrite it completely from scratch — never patch individual lines.\n" +
		"- For pipeline agent nodes: the end user CANNOT see the node's internal tool calls or " +
		"  intermediate steps. The ONLY way to deliver results is through node_complete vars. " +
		"  Always include a prompt instruction telling the agent to put complete findings in vars.\n" +
		"- Call finish_task(status=\"success\", summary=<absolute path of file>) when done.\n\n" +
		"## Node type selection\n\n" +
		"Choose the MOST specific type — prefer deterministic over LLM-driven:\n\n" +
		"  type: auto  — deterministic action (tool + args fully known at authoring time).\n" +
		"               Zero LLM cost. Use for: file reads, bash commands, web fetches\n" +
		"               of known URLs, API calls with known params.\n\n" +
		"  delegate    — use the delegate tool inside a type:agent node when you know the\n" +
		"               sub-task and can supply a pre_tool. Better than a bare agent node.\n\n" +
		"  type: agent — ONLY when genuine LLM reasoning is required (analysis, synthesis,\n" +
		"               decision-making). Do NOT use for deterministic actions.\n\n" +
		"Priority: auto > delegate > agent.\n\n")

	sb.WriteString("## Skill Format Selection\n\n" +
		"Default to .md (agent skill). Only use .yaml (pipeline) when ALL of the following are true:\n" +
		"1. The task has genuinely parallel independent steps with no data dependencies between them,\n" +
		"   OR different stages need completely isolated and incompatible tool sets.\n" +
		"2. Step count is ≥ 3.\n\n" +
		"A single agent in a .md skill calling multiple tools is simpler and less error-prone than a\n" +
		"multi-node pipeline. Two-step flows like 'search then summarize' or 'read file then process'\n" +
		"→ use .md, not a pipeline.\n\n")

	sb.WriteString("## Pipeline Hard Rules (apply when writing .yaml)\n\n" +
		"1. The FIRST pipeline node MUST be `type: agent`. The user's raw natural-language\n" +
		"   message arrives in `{{input}}` / `$vars.input` — a `type: auto` first node would\n" +
		"   feed natural language into a tool's typed schema and crash. Use the first agent\n" +
		"   node to parse/extract structured values for later auto nodes.\n" +
		"2. The LAST pipeline node MUST produce the variable named in top-level `returns:`\n" +
		"   (or one of `result`/`output`/`answer`). Its prompt MUST explicitly instruct the\n" +
		"   sub-agent: \"the user sees ONLY this value — put the COMPLETE final answer here.\"\n" +
		"   A pipeline that ends without producing a returnable var shows the user nothing.\n" +
		"3. Every agent node's `prompt:` MUST include:\n" +
		"   (a) one-sentence goal;\n" +
		"   (b) brief meaning of each `{{var}}` referenced;\n" +
		"   (c) the required output format (prose / JSON / markdown);\n" +
		"   (d) explicit note: \"the user cannot see intermediate steps — put the complete\n" +
		"       result into node_complete vars.\"\n" +
		"4. Every agent node's prompt MUST tell the agent what to do on partial failure\n" +
		"   (empty/error/missing data from previous step) — never assume happy path only.\n\n")

	sb.WriteString("## Tier Selection (the skill's `tier:` field — NOT the current router rating)\n\n" +
		"`tier:` reflects how complex this skill will be on FUTURE calls, not how the router\n" +
		"rated the request that prompted creation.\n\n" +
		"- `base`   — single step, ≤1 tool call, no synthesis (e.g. fetch+summarize, read+reply)\n" +
		"- `medium` — multiple tools / 2+ sources / moderate reasoning\n" +
		"- `strong` — cross-system workflows, decision trees, parallel sub-tasks\n\n" +
		"Examples:\n" +
		"  Analyze one URL (fetch + summarize) → `tier: base`\n" +
		"  Compare 3 sources & write report     → `tier: medium`\n" +
		"  Multi-file refactor across services  → `tier: strong`\n\n" +
		"Never copy the router's tier; pick based on the skill's intrinsic complexity.\n\n")

	sb.WriteString("## Worked Pipeline Example (study the structure)\n\n" +
		"```yaml\n" +
		"name: analyze-link\n" +
		"description: \"Fetch a URL and summarize it for the user.\"\n" +
		"tier: base\n" +
		"auto_generated: true\n" +
		"success_count: 0\n" +
		"vars:\n" +
		"  url: \"\"\n" +
		"  page: \"\"\n" +
		"returns: answer\n" +
		"pipeline:\n" +
		"  - id: extract\n" +
		"    type: agent          # rule 1: first node is agent\n" +
		"    tier: base\n" +
		"    prompt: |\n" +
		"      Goal: extract the URL the user wants analyzed.\n" +
		"      {{input}} is the user's raw message.\n" +
		"      Output format: call node_complete with vars={\"result\":\"<url>\"}.\n" +
		"      The user cannot see this node — put the URL string into vars.result.\n" +
		"      If no URL is present, set vars.result to an empty string.\n" +
		"    outputs: url\n" +
		"  - id: fetch\n" +
		"    type: auto\n" +
		"    tool: web_fetch\n" +
		"    inputs: { url: $vars.url }\n" +
		"    outputs: page\n" +
		"  - id: report\n" +
		"    type: agent          # rule 2: last node produces the returnable var\n" +
		"    tier: base\n" +
		"    prompt: |\n" +
		"      Goal: write the user-facing summary.\n" +
		"      {{page}} is the fetched page text (may be empty if fetch failed).\n" +
		"      If {{page}} is empty or contains an error message, explain the failure\n" +
		"      and suggest the user check the URL.\n" +
		"      Otherwise produce a 3-paragraph summary: topic, key claims, credibility.\n" +
		"      Output format: prose markdown.\n" +
		"      IMPORTANT: the user sees ONLY node_complete vars.result — put the\n" +
		"      COMPLETE summary there.\n" +
		"    outputs: { answer: result }\n" +
		"```\n\n")

	sb.WriteString(pipelineSchemaHint)
	return sb.String()
}

func (p *Planner) buildPrompt(userTask string, history []llm.Message) string {
	var sb strings.Builder
	if len(history) > 0 {
		sb.WriteString("## Recent Conversation Context\n\n")
		start := len(history) - 8
		if start < 0 {
			start = 0
		}
		for _, m := range history[start:] {
			text := m.TextContent()
			if text == "" || (m.Role != "user" && m.Role != "assistant") {
				continue
			}
			label := "User"
			if m.Role == "assistant" {
				label = "Assistant"
			}
			fmt.Fprintf(&sb, "[%s]: %s\n\n", label, text)
		}
	}
	if userTask != "" {
		fmt.Fprintf(&sb, "## Task\n\nCreate a reusable skill or pipeline for:\n\n%s", userTask)
	} else {
		sb.WriteString("## Task\n\nBased on the conversation above, create a reusable skill or pipeline that captures this recurring workflow.")
	}
	return sb.String()
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

// cleanupExtraSkillFiles removes any .md/.yaml/.yml files in dir that are NOT in
// the existing set and are not the keepBase. Used to drop stray duplicates the
// planner agent may have written under a non-canonical name.
func cleanupExtraSkillFiles(dir string, existing map[string]bool, keepBase string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || existing[e.Name()] || e.Name() == keepBase {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".md" && ext != ".yaml" && ext != ".yml" {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
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
