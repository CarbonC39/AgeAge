package agent

import (
	"fmt"
	"regexp"
	"strings"

	"ageage/skills"
)

// ValidationError describes a single validation failure found in a skill file.
type ValidationError struct {
	Field   string
	Message string
}

func (e ValidationError) Error() string {
	if e.Field != "" {
		return fmt.Sprintf("%s: %s", e.Field, e.Message)
	}
	return e.Message
}

// varsRefRE matches $vars.<identifier> inside prompt text.
var varsRefRE = regexp.MustCompile(`\$vars\.([a-zA-Z_][a-zA-Z0-9_]*)`)

// standardToolNames is the baseline set of tool names available to agents.
// Used by ValidateSkillFile to detect unknown tool references in pipeline nodes.
var standardToolNames = []string{
	"bash", "file_read", "file_write", "file_edit",
	"glob", "grep", "tree",
	"web_fetch", "web_search",
	"memory_store", "memory_recall", "memory_forget",
	"cron_add", "cron_remove", "cron_list",
	"delegate", "ask_user", "update_todos",
	"finish_task", "node_complete",
}

// ValidateSkillFile loads a skill file at path and checks it for common errors.
// knownTools overrides the baseline tool list for pipeline tool-name checks;
// pass nil to use standardToolNames.
// Returns a (possibly empty) slice of ValidationError.
func ValidateSkillFile(path string, knownTools []string) []ValidationError {
	skill, err := skills.LoadSkillByPath(path)
	if err != nil {
		return []ValidationError{{Message: "parse error: " + err.Error()}}
	}
	if skill == nil {
		return []ValidationError{{Message: "file did not produce a valid skill"}}
	}

	var errs []ValidationError

	// Required fields present on every skill type.
	if skill.Name == "" {
		errs = append(errs, ValidationError{Field: "name", Message: "required field missing"})
	}
	if skill.Description == "" {
		errs = append(errs, ValidationError{Field: "description", Message: "required field missing"})
	}
	switch skill.Complexity {
	case "direct", "atomic", "workflow", "simple", "medium", "complex":
		// valid (new names + legacy aliases)
	case "":
		errs = append(errs, ValidationError{Field: "complexity", Message: "required field missing"})
	default:
		errs = append(errs, ValidationError{
			Field:   "complexity",
			Message: fmt.Sprintf("invalid value %q — must be direct, atomic, or workflow", skill.Complexity),
		})
	}

	if !skill.IsPipeline() {
		return errs
	}

	// Build effective known-tool set.
	toolList := knownTools
	if len(toolList) == 0 {
		toolList = standardToolNames
	}
	knownSet := make(map[string]bool, len(toolList)+2)
	for _, t := range toolList {
		knownSet[t] = true
	}
	knownSet["finish_task"] = true
	knownSet["node_complete"] = true

	pl := skill.Pipeline

	// Collect vars declared in the top-level vars: block.
	declared := make(map[string]bool, len(pl.Vars))
	for k := range pl.Vars {
		declared[k] = true
	}

	// producedVars accumulates variable names added by previous nodes' outputs.
	producedVars := make(map[string]bool)

	for _, node := range pl.Pipeline {
		// Build available-var set for this node.
		available := make(map[string]bool, len(declared)+len(producedVars))
		for k := range declared {
			available[k] = true
		}
		for k := range producedVars {
			available[k] = true
		}
		// Vars bound through this node's inputs: are also reachable in its prompt.
		for _, raw := range node.Inputs {
			if sv, ok := raw.(string); ok && strings.HasPrefix(sv, "$vars.") {
				available[strings.TrimPrefix(sv, "$vars.")] = true
			}
		}

		// Check tool allowlist names.
		for _, t := range node.Tools {
			if !knownSet[t] {
				errs = append(errs, ValidationError{
					Field:   "node[" + node.ID + "].tools",
					Message: fmt.Sprintf("unknown tool %q", t),
				})
			}
		}
		if node.Type == "auto" && node.Tool != "" && !knownSet[node.Tool] {
			errs = append(errs, ValidationError{
				Field:   "node[" + node.ID + "].tool",
				Message: fmt.Sprintf("unknown tool %q", node.Tool),
			})
		}

		// Check $vars references in the node prompt.
		for _, m := range varsRefRE.FindAllStringSubmatch(node.Prompt, -1) {
			varName := m[1]
			if !available[varName] {
				errs = append(errs, ValidationError{
					Field:   "node[" + node.ID + "].prompt",
					Message: fmt.Sprintf("references undeclared variable $vars.%s", varName),
				})
			}
		}

		// Register this node's outputs so later nodes can reference them.
		for outVar := range node.Outputs {
			producedVars[outVar] = true
		}
	}

	return errs
}
