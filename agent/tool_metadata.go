package agent

import "ageage/tools"

// Metadata for agent-owned tools is kept alongside the registry policy
// boundary.  These tools change orchestration state, so they are not marked
// read-only even when they do not mutate files or the network.

func (t *DelegateTool) Metadata() tools.ToolMetadata {
	return tools.ToolMetadata{Risk: tools.RiskHigh, ConcurrencyKey: "agent"}
}

func (t *NodeCompleteTool) Metadata() tools.ToolMetadata {
	return tools.ToolMetadata{Risk: tools.RiskLow, ConcurrencyKey: "pipeline"}
}

func (t *NextStepTool) Metadata() tools.ToolMetadata {
	return tools.ToolMetadata{Risk: tools.RiskMedium, ConcurrencyKey: "agent"}
}

func (t *EscalateTool) Metadata() tools.ToolMetadata {
	return tools.ToolMetadata{Risk: tools.RiskHigh, ConcurrencyKey: "interaction"}
}

func (t *skillPatchTool) Metadata() tools.ToolMetadata {
	return tools.ToolMetadata{Risk: tools.RiskHigh, ConcurrencyKey: "filesystem"}
}
