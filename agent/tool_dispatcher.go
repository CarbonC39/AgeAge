package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"ageage/creds"
	"ageage/tools"
)

// ToolDispatchHooks contains optional presentation callbacks. Hooks receive
// redacted arguments and results; execution always receives substituted values.
type ToolDispatchHooks struct {
	Start func(name, args string)
	End   func(name string)
}

// ToolDispatcher is the single policy boundary for invoking registered tools.
// It protects the credential store path, substitutes credential placeholders,
// redacts presentation data, executes the tool, and scrubs its output.
type ToolDispatcher struct {
	Registry *tools.Registry
	CredMgr  *creds.Manager
}

func NewToolDispatcher(registry *tools.Registry, credMgr *creds.Manager) *ToolDispatcher {
	return &ToolDispatcher{Registry: registry, CredMgr: credMgr}
}

// Execute invokes a tool through the shared dispatch policy.
func (d *ToolDispatcher) Execute(
	ctx context.Context,
	name string,
	args json.RawMessage,
	hooks ToolDispatchHooks,
) (string, error) {
	if d == nil || d.Registry == nil {
		return "", fmt.Errorf("tool dispatcher has no registry")
	}

	execArgs := args
	displayArgs := string(args)
	if d.CredMgr != nil {
		if d.CredMgr.ContainsCredPath(displayArgs) {
			return "", fmt.Errorf("direct access to the credentials file is system-protected and not permitted")
		}
		substituted, err := d.CredMgr.SubstituteJSON(execArgs)
		if err != nil {
			return "", fmt.Errorf("credential substitution failed: %w", err)
		}
		execArgs = substituted
		// Presentation keeps the original {{cred:name}} placeholders. Scrubbing a
		// re-encoded JSON document is insufficient for secrets containing escaped
		// characters (for example, a newline becomes "\\n" and no longer matches
		// the in-memory value byte-for-byte).
		displayArgs = d.CredMgr.Scrub(displayArgs)
	}

	if hooks.Start != nil {
		hooks.Start(name, displayArgs)
	}
	result, err := d.Registry.Execute(ctx, name, execArgs)
	if hooks.End != nil {
		hooks.End(name)
	}

	if d.CredMgr != nil {
		result = d.CredMgr.Scrub(result)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			redacted := d.CredMgr.Scrub(err.Error())
			if redacted != err.Error() {
				err = errors.New(redacted)
			}
		}
	}
	return result, err
}

// ExecuteTool exposes policy-compliant tool execution for external adapters
// such as the MCP server.
func (a *Agent) ExecuteTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	return NewToolDispatcher(a.registry, a.CredMgr).Execute(ctx, name, args, ToolDispatchHooks{})
}
