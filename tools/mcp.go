package tools

import (
	"context"
	"encoding/json"
	"fmt"

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

func (t *MCPTool) Execute(args json.RawMessage) (string, error) {
	var arguments map[string]any
	if err := json.Unmarshal(args, &arguments); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	result, err := t.Session.CallTool(context.Background(), &mcp.CallToolParams{
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
