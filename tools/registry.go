package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"ageage/llm"
)

// Tool is the interface that all tools must implement.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]interface{}
	// Execute runs the tool. ctx is the agent's run context and must be forwarded
	// to any blocking or cancellable operations (HTTP calls, sub-agents, etc.).
	Execute(ctx context.Context, args json.RawMessage) (string, error)
}

// Registry manages all registered tools.
type Registry struct {
	tools map[string]Tool
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Register adds a tool to the registry.
func (r *Registry) Register(t Tool) {
	r.tools[t.Name()] = t
}

// Unregister removes a tool from the registry by name. No-op if not present.
func (r *Registry) Unregister(name string) {
	delete(r.tools, name)
}

// Get retrieves a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// List returns all registered tool names.
func (r *Registry) List() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	return names
}

// ListExcept returns all registered tool names except those specified.
func (r *Registry) ListExcept(exclude ...string) []string {
	excludeSet := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excludeSet[e] = true
	}
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		if !excludeSet[name] {
			names = append(names, name)
		}
	}
	return names
}

// ToOpenAITools converts all registered tools to OpenAI tool definitions.
func (r *Registry) ToOpenAITools() []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, llm.ToolDef{
			Type: "function",
			Function: llm.FunctionDef{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(),
			},
		})
	}
	return defs
}

// ToOpenAIToolsFiltered converts only the named tools to OpenAI tool definitions.
func (r *Registry) ToOpenAIToolsFiltered(names []string) []llm.ToolDef {
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}

	var defs []llm.ToolDef
	for _, t := range r.tools {
		if nameSet[t.Name()] {
			defs = append(defs, llm.ToolDef{
				Type: "function",
				Function: llm.FunctionDef{
					Name:        t.Name(),
					Description: t.Description(),
					Parameters:  t.Parameters(),
				},
			})
		}
	}
	return defs
}

// ListAll returns all registered tools.
func (r *Registry) ListAll() []Tool {
	var list []Tool
	for _, t := range r.tools {
		list = append(list, t)
	}
	return list
}

// Execute executes a tool by name with the given arguments.
func (r *Registry) Execute(ctx context.Context, name string, args json.RawMessage) (string, error) {
	t, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return t.Execute(ctx, args)
}
