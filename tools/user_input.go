package tools

import (
	"fmt"
	"sync"
	"time"

	"ageage/input"
)

// PendingUserInput is a scoped ask_user request. ID is internal today but
// allows future UIs to present an explicit request selector.
type PendingUserInput struct {
	ID        string
	Scope     InteractionScope
	CreatedAt time.Time
	ResultCh  chan string
	InputCh   chan input.UserInput
}

// UserInputManager tracks pending ask_user requests. Legacy channel-only
// methods remain available for CLI callers; channel integrations should use
// scoped methods so a user cannot answer another session's question.
type UserInputManager struct {
	mu      sync.Mutex
	pending map[string]*PendingUserInput
	seq     uint64
}

func NewUserInputManager() *UserInputManager {
	return &UserInputManager{pending: make(map[string]*PendingUserInput)}
}

// RequestInput preserves the original channel-only API.
func (m *UserInputManager) RequestInput(channelID string) <-chan string {
	_, ch := m.RequestInputScoped(LegacyInteractionScope(channelID))
	return ch
}

// RequestInputScoped registers a request. An exact duplicate scope replaces
// the older request, while different sessions/threads/users may wait together.
func (m *UserInputManager) RequestInputScoped(scope InteractionScope) (string, <-chan string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		m.pending = make(map[string]*PendingUserInput)
	}
	for id, pending := range m.pending {
		if pending.Scope.equal(scope) {
			closePending(pending)
			delete(m.pending, id)
		}
	}
	m.seq++
	id := fmt.Sprintf("input_%d", m.seq)
	ch := make(chan string, 1)
	m.pending[id] = &PendingUserInput{ID: id, Scope: scope, CreatedAt: time.Now(), ResultCh: ch}
	return id, ch
}

// RequestUserInputScoped registers a typed request for text and multimodal input.
func (m *UserInputManager) RequestUserInputScoped(scope InteractionScope) (string, <-chan input.UserInput) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		m.pending = make(map[string]*PendingUserInput)
	}
	for id, pending := range m.pending {
		if pending.Scope.equal(scope) {
			closePending(pending)
			delete(m.pending, id)
		}
	}
	m.seq++
	id := fmt.Sprintf("input_%d", m.seq)
	ch := make(chan input.UserInput, 1)
	m.pending[id] = &PendingUserInput{ID: id, Scope: scope, CreatedAt: time.Now(), InputCh: ch}
	return id, ch
}

// Respond preserves the original channel-only API and rejects ambiguity.
func (m *UserInputManager) Respond(channelID, answer string) bool {
	return m.RespondForScope(LegacyInteractionScope(channelID), answer)
}

// RespondForScope delivers an answer only when exactly one request matches.
func (m *UserInputManager) RespondForScope(scope InteractionScope, answer string) bool {
	return m.RespondInputForScope(scope, input.UserInput{Text: answer})
}

// RespondInputForScope delivers a typed answer only to an exact matching scope.
// A legacy string-only request rejects answers carrying parts rather than
// silently discarding attachment data.
func (m *UserInputManager) RespondInputForScope(scope InteractionScope, answer input.UserInput) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	var match *PendingUserInput
	var matchID string
	for id, pending := range m.pending {
		if pending.Scope.equal(scope) {
			if match != nil {
				return false
			}
			match, matchID = pending, id
		}
	}
	if match == nil {
		return false
	}
	if !deliverUserInput(match, answer) {
		return false
	}
	delete(m.pending, matchID)
	return true
}

// RespondByID delivers an answer after validating the complete scope.
func (m *UserInputManager) RespondByID(id string, scope InteractionScope, answer string) bool {
	return m.RespondInputByID(id, scope, input.UserInput{Text: answer})
}

// RespondInputByID delivers a typed answer only to the exact request ID and
// owner scope. Legacy string requests reject parts rather than dropping them.
func (m *UserInputManager) RespondInputByID(id string, scope InteractionScope, answer input.UserInput) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending, ok := m.pending[id]
	if !ok || !pending.Scope.equal(scope) {
		return false
	}
	if !deliverUserInput(pending, answer) {
		return false
	}
	delete(m.pending, id)
	return true
}

func deliverUserInput(pending *PendingUserInput, answer input.UserInput) bool {
	if pending.InputCh != nil {
		pending.InputCh <- answer
		return true
	}
	if len(answer.Parts) > 0 {
		return false
	}
	pending.ResultCh <- answer.Text
	return true
}

func (m *UserInputManager) Cancel(channelID string) bool {
	return m.CancelForScope(LegacyInteractionScope(channelID))
}

// CancelForScope cancels exactly one matching request and rejects ambiguity.
func (m *UserInputManager) CancelForScope(scope InteractionScope) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	var match *PendingUserInput
	var matchID string
	for id, pending := range m.pending {
		if pending.Scope.equal(scope) {
			if match != nil {
				return false
			}
			match, matchID = pending, id
		}
	}
	if match == nil {
		return false
	}
	closePending(match)
	delete(m.pending, matchID)
	return true
}

func (m *UserInputManager) CancelByID(id string, scope InteractionScope) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending, ok := m.pending[id]
	if !ok || !pending.Scope.equal(scope) {
		return false
	}
	closePending(pending)
	delete(m.pending, id)
	return true
}

func closePending(pending *PendingUserInput) {
	if pending.ResultCh != nil {
		close(pending.ResultCh)
	}
	if pending.InputCh != nil {
		close(pending.InputCh)
	}
}

func (m *UserInputManager) HasPending(channelID string) bool {
	return m.HasPendingScope(LegacyInteractionScope(channelID))
}

func (m *UserInputManager) HasPendingScope(scope InteractionScope) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, pending := range m.pending {
		if pending.Scope.equal(scope) {
			return true
		}
	}
	return false
}

func (m *UserInputManager) GetPendingForScope(scope InteractionScope) []*PendingUserInput {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*PendingUserInput
	for _, pending := range m.pending {
		if pending.Scope.locationEqual(scope) {
			result = append(result, pending)
		}
	}
	return result
}
