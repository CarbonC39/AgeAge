package tools

import (
	"fmt"
	"sync"
	"time"
)

// PendingUserInput is a scoped ask_user request. ID is internal today but
// allows future UIs to present an explicit request selector.
type PendingUserInput struct {
	ID        string
	Scope     InteractionScope
	CreatedAt time.Time
	ResultCh  chan string
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
			close(pending.ResultCh)
			delete(m.pending, id)
		}
	}
	m.seq++
	id := fmt.Sprintf("input_%d", m.seq)
	ch := make(chan string, 1)
	m.pending[id] = &PendingUserInput{ID: id, Scope: scope, CreatedAt: time.Now(), ResultCh: ch}
	return id, ch
}

// Respond preserves the original channel-only API and rejects ambiguity.
func (m *UserInputManager) Respond(channelID, answer string) bool {
	return m.RespondForScope(LegacyInteractionScope(channelID), answer)
}

// RespondForScope delivers an answer only when exactly one request matches.
func (m *UserInputManager) RespondForScope(scope InteractionScope, answer string) bool {
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
	match.ResultCh <- answer
	delete(m.pending, matchID)
	return true
}

// RespondByID delivers an answer after validating the complete scope.
func (m *UserInputManager) RespondByID(id string, scope InteractionScope, answer string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending, ok := m.pending[id]
	if !ok || !pending.Scope.equal(scope) {
		return false
	}
	pending.ResultCh <- answer
	delete(m.pending, id)
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
	close(match.ResultCh)
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
	close(pending.ResultCh)
	delete(m.pending, id)
	return true
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
