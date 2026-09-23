package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPTool is a bridge to an external tool provided by an MCP server.
type MCPTool struct {
	Session *mcp.ClientSession
	Tool    *mcp.Tool
}

func (t *MCPTool) Name() string {
	return t.Tool.Name
}

func (t *MCPTool) Description() string {
	return t.Tool.Description
}

func (t *MCPTool) Parameters() map[string]interface{} {
	// MCP tool input schema is a JSON Schema object.
	var schema map[string]interface{}
	data, _ := json.Marshal(t.Tool.InputSchema)
	json.Unmarshal(data, &schema)
	return schema
}

// Metadata maps MCP's advisory annotations into AgeAge's scheduler metadata.
// A missing annotations object is deliberately conservative: the remote
// server is not assumed to be read-only, idempotent, or network-free.
func (t *MCPTool) Metadata() ToolMetadata {
	metadata := DefaultToolMetadata()
	metadata.DefaultTimeout = 0
	if t == nil || t.Tool == nil || t.Tool.Annotations == nil {
		return metadata
	}

	annotations := t.Tool.Annotations
	metadata.ReadOnly = annotations.ReadOnlyHint
	metadata.Idempotent = annotations.IdempotentHint || annotations.ReadOnlyHint
	if annotations.DestructiveHint != nil && *annotations.DestructiveHint {
		// Contradictory hints fail safe: a destructive operation must never be
		// scheduled as a parallel read-only tool.
		metadata.ReadOnly = false
		metadata.Risk = RiskHigh
	} else if annotations.ReadOnlyHint {
		metadata.Risk = RiskLow
	}
	if annotations.OpenWorldHint != nil {
		metadata.NetworkAccess = *annotations.OpenWorldHint
	}
	// MCP calls are remote operations.  Keep a finite default for annotated
	// tools while preserving the conservative no-timeout behavior for legacy
	// servers that supplied no annotations at all.
	metadata.DefaultTimeout = 2 * time.Minute
	return metadata
}

func (t *MCPTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var arguments map[string]any
	if err := json.Unmarshal(args, &arguments); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	result, err := t.Session.CallTool(ctx, &mcp.CallToolParams{
		Name:      t.Tool.Name,
		Arguments: arguments,
	})

	if err != nil {
		return "", fmt.Errorf("MCP call failed: %w", err)
	}

	var out string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			out += text.Text + "\n"
		} else {
			out += "[Non-text content]\n"
		}
	}

	if result.IsError {
		return out, fmt.Errorf("tool reported error")
	}

	return out, nil
}
