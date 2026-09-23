package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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

// RiskLevel describes the potential impact of invoking a tool.  Metadata is a
// policy hint, not a security boundary: tools supplied by an untrusted
// extension should still be treated as capable of doing what their
// implementation permits.
type RiskLevel string

const (
	RiskLow      RiskLevel = "low"
	RiskMedium   RiskLevel = "medium"
	RiskHigh     RiskLevel = "high"
	RiskCritical RiskLevel = "critical"
)

// ToolMetadata describes execution characteristics used by the agent's
// scheduler and policy/audit layers.  It is deliberately separate from Tool
// so existing third-party tools continue to satisfy the small Tool interface.
type ToolMetadata struct {
	ReadOnly       bool          `json:"read_only"`
	Idempotent     bool          `json:"idempotent"`
	Risk           RiskLevel     `json:"risk"`
	DefaultTimeout time.Duration `json:"default_timeout"`
	NetworkAccess  bool          `json:"network_access"`
	ConcurrencyKey string        `json:"concurrency_key"`
}

// MetadataProvider is an optional interface implemented by tools that can
// describe their own execution characteristics.
type MetadataProvider interface {
	Metadata() ToolMetadata
}

// DefaultToolMetadata is intentionally conservative.  A tool that does not
// implement MetadataProvider is assumed to have side effects, to be
// non-idempotent, and to carry medium risk.
func DefaultToolMetadata() ToolMetadata {
	return ToolMetadata{Risk: RiskMedium}
}

// Registry manages all registered tools.
// Tools are stored in a map for O(1) lookup, with a parallel insertion-ordered
// name slice. All listing functions iterate the slice so that tool order is
// deterministic (Go map iteration order is randomized) — a stable, repeatable
// tool list keeps prompts byte-identical across requests, which matters for
// KV-cache reuse and reproducible model behavior.
type Registry struct {
	tools    map[string]Tool
	order    []string                // tool names in registration order
	metadata map[string]ToolMetadata // explicit descriptors, when provided
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{
		tools:    make(map[string]Tool),
		metadata: make(map[string]ToolMetadata),
	}
}

// Register adds a tool to the registry. Re-registering an existing name updates
// the tool in place without changing its position in the order.
func (r *Registry) Register(t Tool) {
	name := t.Name()
	if _, ok := r.tools[name]; ok {
		r.tools[name] = t
		// A plain replacement must not inherit an explicit descriptor from the
		// previous implementation. Falling back to the replacement's provider
		// (or conservative defaults) is safer than retaining stale policy data.
		delete(r.metadata, name)
		return
	}
	r.tools[name] = t
	r.order = append(r.order, name)
}

// RegisterWithMetadata registers a tool and an explicit descriptor.  This is
// useful for adapters (such as MCP) whose metadata is supplied separately
// from the Tool implementation.
func (r *Registry) RegisterWithMetadata(t Tool, metadata ToolMetadata) {
	if t == nil {
		return
	}
	r.Register(t)
	if r.metadata == nil {
		r.metadata = make(map[string]ToolMetadata)
	}
	r.metadata[t.Name()] = metadata
}

// Metadata returns the descriptor for a registered tool.  Missing metadata is
// never treated as safe: callers receive conservative defaults.  The boolean
// reports whether a tool with that name is registered.
func (r *Registry) Metadata(name string) (ToolMetadata, bool) {
	t, ok := r.tools[name]
	if !ok {
		return DefaultToolMetadata(), false
	}
	if metadata, exists := r.metadata[name]; exists {
		return metadata, true
	}
	if provider, ok := t.(MetadataProvider); ok {
		metadata := provider.Metadata()
		if metadata.Risk == "" {
			metadata.Risk = RiskMedium
		}
		return metadata, true
	}
	return DefaultToolMetadata(), true
}

// GetMetadata is an alias for Metadata for callers that prefer Get-style
// registry APIs.
func (r *Registry) GetMetadata(name string) (ToolMetadata, bool) {
	return r.Metadata(name)
}

// Unregister removes a tool from the registry by name. No-op if not present.
func (r *Registry) Unregister(name string) {
	if _, ok := r.tools[name]; !ok {
		return
	}
	delete(r.tools, name)
	delete(r.metadata, name)
	for i, n := range r.order {
		if n == name {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// Get retrieves a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// namesInOrder returns the currently registered tool names in registration order.
func (r *Registry) namesInOrder() []string {
	names := make([]string, 0, len(r.order))
	for _, name := range r.order {
		if _, ok := r.tools[name]; ok {
			names = append(names, name)
		}
	}
	return names
}

// List returns all registered tool names in registration order.
func (r *Registry) List() []string {
	return r.namesInOrder()
}

// ListExcept returns all registered tool names except those specified, in
// registration order.
func (r *Registry) ListExcept(exclude ...string) []string {
	excludeSet := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excludeSet[e] = true
	}
	names := make([]string, 0, len(r.order))
	for _, name := range r.order {
		if _, ok := r.tools[name]; ok && !excludeSet[name] {
			names = append(names, name)
		}
	}
	return names
}

// ToOpenAITools converts all registered tools to OpenAI tool definitions, in
// registration order.
func (r *Registry) ToOpenAITools() []llm.ToolDef {
	defs := make([]llm.ToolDef, 0, len(r.order))
	for _, name := range r.order {
		t, ok := r.tools[name]
		if !ok {
			continue
		}
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

// ToOpenAIToolsFiltered converts only the named tools to OpenAI tool
// definitions. The output follows the registry's registration order (the input
// slice is treated as a set), so the definitions are stable regardless of how
// the caller ordered its name list.
func (r *Registry) ToOpenAIToolsFiltered(names []string) []llm.ToolDef {
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}

	var defs []llm.ToolDef
	for _, name := range r.order {
		t, ok := r.tools[name]
		if !ok || !nameSet[name] {
			continue
		}
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

// ListAll returns all registered tools in registration order.
func (r *Registry) ListAll() []Tool {
	list := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		if t, ok := r.tools[name]; ok {
			list = append(list, t)
		}
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
