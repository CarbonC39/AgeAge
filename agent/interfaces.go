package agent

import (
	"ageage/config"
	"ageage/security"
	"ageage/skills"
	"ageage/tools"
)

// AgentDeps is the set of factory-level services an Agent needs at runtime.
// Implemented by AgentFactory; the interface lets tests supply lightweight fakes
// without constructing a full factory.
type AgentDeps interface {
	// GetSkills returns the current skill list (supports hot-reload).
	GetSkills() []skills.Skill
	// CreateAgentFiltered creates an isolated sub-agent with a filtered tool set.
	CreateAgentFiltered(confirmMgr *tools.ConfirmationManager, channelID string, allowedTools []string) *Agent
	// GetConfig exposes configuration consumed by skill-only tool factories.
	GetConfig() *config.Config
	// GetSecurity exposes the security checker used by sandboxed tools.
	GetSecurity() *security.Checker
	// GetUserInputMgr exposes the shared ask_user pending-input state.
	GetUserInputMgr() *tools.UserInputManager
}

// AgentCallbacks groups all optional outbound notification callbacks.
// The zero value is valid — all nil callbacks are silently skipped.
type AgentCallbacks struct {
	Notify     func(message string)
	AskUser    func(question string, options []string)
	TodoSend   func(text string) string
	TodoEdit   func(msgID, text string) error
	ToolStart  func(name, args string)
	ToolEnd    func(name string)
	ToolResult func(name, result string)
}

// AgentMode groups boolean configuration flags for an Agent.
// The zero value represents a non-soul-injected, non-sub-agent without
// context injection. Callers that want context injection (the default for
// main agents) must set InjectContext = true explicitly.
type AgentMode struct {
	IsSubAgent    bool
	InjectSoul    bool
	InjectContext bool
}
