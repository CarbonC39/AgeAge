package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"ageage/tools"
)

// NodeResult is the structured output of a pipeline node's node_complete call.
type NodeResult struct {
	Status string                 // "success" or "failure"
	Vars   map[string]interface{} // output variables written back to the pipeline
	Reason string                 // human-readable failure reason
}

// NodeCompleteTool replaces finish_task inside pipeline node agents.
//
// When the agent calls this tool:
//   - The NodeResult is written to a buffered channel (read by PipelineExecutor).
//   - The shared FinishTool is marked done so the agent loop stops immediately.
//
// The buffered channel (capacity 1) ensures the write never blocks even when
// the executor hasn't yet drained it.
type NodeCompleteTool struct {
	resultCh   chan<- NodeResult
	finishTool *tools.FinishTool // shared with the node's agent; marks loop done
	finished   bool
}

func (t *NodeCompleteTool) Name() string { return "node_complete" }

func (t *NodeCompleteTool) Description() string {
	return "Signal that this pipeline node has finished. " +
		"Call with status=\"success\" and any output vars, " +
		"or status=\"failure\" and a reason (terminates the whole pipeline)."
}

func (t *NodeCompleteTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"status": map[string]interface{}{
				"type":        "string",
				"enum":        []string{"success", "failure"},
				"description": "Whether this node succeeded or failed.",
			},
			"vars": map[string]interface{}{
				"type":        "object",
				"description": "Output variables to write back to the pipeline. Keys should match the node's declared outputs.",
			},
			"reason": map[string]interface{}{
				"type":        "string",
				"description": "Human-readable explanation (required when status=failure).",
			},
		},
		"required": []string{"status"},
	}
}

func (t *NodeCompleteTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	if t.finished {
		return "", fmt.Errorf("node_complete already called for this node")
	}

	var p struct {
		Status string                 `json:"status"`
		Vars   map[string]interface{} `json:"vars"`
		Reason string                 `json:"reason"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if p.Status != "success" && p.Status != "failure" {
		p.Status = "success"
	}
	if p.Vars == nil {
		p.Vars = map[string]interface{}{}
	}

	t.finished = true

	// Write to buffered channel first (non-blocking) so the executor can read
	// the result immediately after Run() returns.
	t.resultCh <- NodeResult{
		Status: p.Status,
		Vars:   p.Vars,
		Reason: p.Reason,
	}

	// Mark the shared FinishTool done → the agent loop exits after this tool call.
	t.finishTool.Finished = true
	t.finishTool.Summary = "" // executor reads from channel; this value is discarded

	if p.Status == "failure" {
		return fmt.Sprintf("Node failed: %s. Pipeline will terminate.", p.Reason), nil
	}
	return "Node complete. Returning control to pipeline.", nil
}
