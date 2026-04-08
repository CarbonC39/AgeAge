package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ── Skill ────────────────────────────────────────────────────────────────────

// Skill represents a loaded skill. It is either a markdown skill (Prompt != "")
// or a YAML pipeline skill (Pipeline != nil). Both appear identically in the
// router catalog and respond to /skill-name commands.
type Skill struct {
	Name          string         `yaml:"name"`
	Version       string         `yaml:"version"`
	Description   string         `yaml:"description"`
	RequiredTools []string       `yaml:"required_tools"`
	// Complexity is an optional hint that bypasses the router LLM call.
	// Valid values: "simple", "medium", "complex".
	Complexity string         `yaml:"complexity"`
	Prompt     string         `yaml:"-"` // markdown body (regular skills)
	FilePath   string         `yaml:"-"`
	Pipeline   *PipelineSkill `yaml:"-"` // non-nil for pipeline skills
}

// IsPipeline reports whether this skill is a YAML pipeline skill.
func (s *Skill) IsPipeline() bool { return s.Pipeline != nil }

// CommandName returns the slash-command form: lowercase, spaces/underscores → hyphens.
// e.g. "Code Review" → "code-review", "deep_research" → "deep-research"
func (s *Skill) CommandName() string {
	n := strings.ToLower(s.Name)
	n = strings.ReplaceAll(n, " ", "-")
	n = strings.ReplaceAll(n, "_", "-")
	return n
}

// ── Pipeline types ────────────────────────────────────────────────────────────

// PipelineSkill is the parsed content of a .yaml pipeline skill file.
type PipelineSkill struct {
	// Vars declares pipeline variables and their default values.
	// $vars.input is always populated from the user's message.
	Vars     map[string]interface{} `yaml:"vars"`
	Pipeline []PipelineNode         `yaml:"pipeline"`
}

// PipelineNode is one step in a pipeline.
type PipelineNode struct {
	// ID is the unique node identifier, used for variable namespacing and todo display.
	ID string `yaml:"id"`

	// Type is "agent" (LLM-driven) or "auto" (direct tool call, no LLM).
	Type string `yaml:"type"`

	// ── auto-only ─────────────────────────────────────────────────────────────

	// Tool is the tool to call for type=auto nodes.
	Tool string `yaml:"tool"`

	// ── agent-only ────────────────────────────────────────────────────────────

	// Skill optionally activates a regular or pipeline skill inside this node.
	// Pipeline skills may be nested at most 1 level deep.
	Skill string `yaml:"skill"`

	// Tools is the tool allowlist for the node's agent.
	// If empty and no skill is specified, the node has access to all global tools.
	Tools []string `yaml:"tools"`

	// Complexity selects the LLM model: "simple", "medium", or "complex".
	Complexity string `yaml:"complexity"`

	// InjectSoul controls whether SOUL.md is included in the node's system prompt.
	InjectSoul bool `yaml:"inject_soul"`

	// OutputContext enables the optional "context" field in node_complete.
	// When true, the node's agent may include a context string in its finish
	// call that gets injected into subsequent agent nodes' prompts.
	OutputContext bool `yaml:"output_context"`

	// Prompt is the user-facing task description for the node's agent.
	// Supports template substitution: {{$vars.name}}, {{$foreach.current}}, {{$foreach.index}}.
	Prompt string `yaml:"prompt"`

	// ── both ──────────────────────────────────────────────────────────────────

	// Foreach specifies an array pipeline variable to iterate over.
	// e.g. "$vars.articles" runs this node once per element.
	// $foreach.current and $foreach.index are available inside the node.
	// Output variables are collected as arrays after all iterations complete.
	Foreach string `yaml:"foreach"`

	// Concurrency limits the number of parallel iterations for foreach nodes.
	// 0 or 1 = sequential; >1 = parallel. Default: 0.
	Concurrency int `yaml:"concurrency"`

	// Inputs maps tool/agent argument names to pipeline variable references.
	// Values may be $vars.name, $foreach.current, $foreach.index, or literals.
	Inputs map[string]string `yaml:"inputs"`

	// Outputs maps pipeline variable names to the node's output keys.
	// For auto nodes: the tool's return string is stored under the key "result".
	// For agent nodes: keys correspond to variable names declared in node_complete vars.
	Outputs map[string]string `yaml:"outputs"`

	// Validate specifies requirements for the node's input.
	// Currently supported: "not_empty" (fails if input prompt/vars are empty).
	Validate string `yaml:"validate"`
}

// ── Loaders ───────────────────────────────────────────────────────────────────

// LoadSkills scans dir for .md (regular) and .yaml (pipeline) skill files.
func LoadSkills(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read skills directory: %w", err)
	}

	var loaded []Skill
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		path := filepath.Join(dir, name)

		var skill *Skill
		var parseErr error

		switch {
		case strings.HasSuffix(name, ".md"):
			skill, parseErr = parseSkillFile(path)
		case strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml"):
			skill, parseErr = parsePipelineFile(path)
		default:
			continue
		}

		if parseErr != nil {
			return nil, fmt.Errorf("failed to parse skill %s: %w", name, parseErr)
		}
		if skill != nil {
			loaded = append(loaded, *skill)
		}
	}

	return loaded, nil
}

// parseSkillFile reads a Markdown file with optional YAML frontmatter.
func parseSkillFile(path string) (*Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)
	skill := &Skill{FilePath: path}

	if strings.HasPrefix(content, "---") {
		parts := strings.SplitN(content[3:], "---", 2)
		if len(parts) == 2 {
			if err := yaml.Unmarshal([]byte(strings.TrimSpace(parts[0])), skill); err != nil {
				return nil, fmt.Errorf("invalid frontmatter YAML: %w", err)
			}
			skill.Prompt = strings.TrimSpace(parts[1])
		} else {
			skill.Prompt = content
		}
	} else {
		skill.Prompt = content
	}

	if skill.Name == "" {
		skill.Name = strings.TrimSuffix(filepath.Base(path), ".md")
	}
	return skill, nil
}

// parsePipelineFile reads a standalone YAML pipeline definition.
// The file must contain top-level fields: name, description, vars, pipeline.
func parsePipelineFile(path string) (*Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// The top-level YAML structure combines Skill metadata with PipelineSkill data.
	var raw struct {
		Name        string                 `yaml:"name"`
		Version     string                 `yaml:"version"`
		Description string                 `yaml:"description"`
		Complexity  string                 `yaml:"complexity"`
		Vars        map[string]interface{} `yaml:"vars"`
		Pipeline    []PipelineNode         `yaml:"pipeline"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("invalid pipeline YAML: %w", err)
	}

	// A YAML file without a pipeline key is not a pipeline skill — skip it.
	if len(raw.Pipeline) == 0 {
		return nil, nil
	}

	name := raw.Name
	if name == "" {
		base := filepath.Base(path)
		name = strings.TrimSuffix(strings.TrimSuffix(base, ".yml"), ".yaml")
	}

	if raw.Vars == nil {
		raw.Vars = make(map[string]interface{})
	}
	// Ensure $vars.input always exists (populated from the user's message).
	if _, ok := raw.Vars["input"]; !ok {
		raw.Vars["input"] = ""
	}

	return &Skill{
		Name:        name,
		Version:     raw.Version,
		Description: raw.Description,
		Complexity:  raw.Complexity,
		FilePath:    path,
		Pipeline: &PipelineSkill{
			Vars:     raw.Vars,
			Pipeline: raw.Pipeline,
		},
	}, nil
}

// ── Utilities ─────────────────────────────────────────────────────────────────

// SkillsSummary returns a concise summary string of all skills for system prompts.
func SkillsSummary(all []Skill) string {
	if len(all) == 0 {
		return "No skills loaded."
	}

	var sb strings.Builder
	sb.WriteString("Available Skills:\n")
	for _, s := range all {
		fmt.Fprintf(&sb, "- **%s**", s.Name)
		if s.Description != "" {
			fmt.Fprintf(&sb, ": %s", s.Description)
		}
		if s.IsPipeline() {
			sb.WriteString(" [pipeline]")
		} else if len(s.RequiredTools) > 0 {
			fmt.Fprintf(&sb, " (tools: %s)", strings.Join(s.RequiredTools, ", "))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
