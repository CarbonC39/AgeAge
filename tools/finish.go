package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// FinishTool is the tool that terminates the agent loop and returns a summary.
type FinishTool struct {
	// Finished is set to true when this tool is called successfully.
	Finished bool
	// Summary holds the final summary text.
	Summary string
	// Status is "success" or "failure" as reported by the agent.
	Status string
	// CheckTodos, when non-nil, is called on status="success" to verify all todos
	// are complete. Returns (allDone bool, pendingDescription string).
	CheckTodos func() (bool, string)
}

func (t *FinishTool) Name() string { return "finish_task" }

func (t *FinishTool) Description() string {
	return "Call this tool to deliver your final response to the user and end the task. " +
		"You MUST call this tool when you have a final answer or have completed all work. " +
		"Use status=\"success\" when done; status=\"failure\" to exit early."
}

func (t *FinishTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"status": map[string]interface{}{
				"type":        "string",
				"description": "\"success\" when all work is done; \"failure\" for early exit.",
				"enum":        []string{"success", "failure"},
			},
			"summary": map[string]interface{}{
				"type": "string",
				"description": "The exact message to send to the user. " +
					"Write it as you would write a reply — the actual content, not a description of what you said. " +
					"For example: write \"Here are the results: ...\" not \"Presented the results to the user.\"",
			},
		},
		"required": []string{"status", "summary"},
	}
}

func (t *FinishTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Default to success for backward compatibility with callers that omit status.
	if params.Status == "" {
		params.Status = "success"
	}

	// Validate todos before allowing a success completion.
	if params.Status == "success" && t.CheckTodos != nil {
		ok, pending := t.CheckTodos()
		if !ok {
			return fmt.Sprintf(
				"[Framework] Cannot finish with status=success: pending todos remain:\n%s\n\n"+
					"Complete them first, then call finish_task(status=\"success\"). "+
					"To abort early, call finish_task(status=\"failure\").", pending), nil
			// t.Finished stays false — the run loop continues.
		}
	}

	t.Status = params.Status
	t.Summary = params.Summary
	t.Finished = true
	return params.Summary, nil
}

// Reset clears the finish state for a new agent loop.
func (t *FinishTool) Reset() {
	t.Finished = false
	t.Summary = ""
	t.Status = ""
}
