package server

import (
	"context"
	"encoding/json"
	"fmt"

	"ageage/agent"
	"ageage/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPServer wraps AgeAge as an MCP server.
type MCPServer struct {
	factory *agent.AgentFactory
}

// NewMCPServer creates a new AgeAge MCP server.
func NewMCPServer(factory *agent.AgentFactory) *MCPServer {
	return &MCPServer{
		factory: factory,
	}
}

// Start runs the MCP server over stdio.
func (s *MCPServer) Start() error {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "AgeAge",
			Version: "1.0.0",
		},
		nil,
	)

	// Create a temporary agent to get all tools for registration.
	ag := s.factory.CreateAgent(nil, "")
	internalTools := ag.GetRegistry().ListAll()

	for _, t := range internalTools {
		// Skip internal finish_task and external MCP tools to avoid loops.
		if t.Name() == "finish_task" {
			continue
		}
		if _, ok := t.(*tools.MCPTool); ok {
			continue
		}

		tool := t // Capture for closure
		mcpTool := &mcp.Tool{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: tool.Parameters(),
		}

		// Use a generic handler since AgeAge tools already handle JSON RawMessage.
		mcp.AddTool(server, mcpTool, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			// Create a fresh agent for each tool call (stateless server).
			callAg := s.factory.CreateAgent(nil, "")
			
			argBytes, _ := json.Marshal(args)
			result, err := callAg.GetRegistry().Execute(tool.Name(), argBytes)

			if err != nil {
				return &mcp.CallToolResult{
					Content: []mcp.Content{
						&mcp.TextContent{
							Text: fmt.Sprintf("Error: %v\nOutput: %s", err, result),
						},
					},
					IsError: true,
				}, nil, nil
			}

			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{
						Text: result,
					},
				},
			}, nil, nil
		})
	}

	// Run over stdio.
	return server.Run(context.Background(), &mcp.StdioTransport{})
}
