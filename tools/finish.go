package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// FinishTool is the tool that terminates the agent loop and returns a summary.
type FinishTool struct {
	// Finished is set to true when this tool is called.
	Finished bool
	// Summary holds the final summary text.
	Summary string
}

func (t *FinishTool) Name() string { return "finish_task" }

func (t *FinishTool) Description() string {
	return "Call this tool to end the current task and return the final summary to the user. You MUST call this tool when you have completed the task or have a final answer."
}

func (t *FinishTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"summary": map[string]interface{}{
				"type":        "string",
				"description": "The final summary or answer to present to the user.",
			},
		},
		"required": []string{"summary"},
	}
}

func (t *FinishTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var params struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	t.Finished = true
	t.Summary = params.Summary
	return params.Summary, nil
}

// Reset clears the finish state for a new agent loop.
func (t *FinishTool) Reset() {
	t.Finished = false
	t.Summary = ""
}
