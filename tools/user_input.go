package tools

import "sync"

// UserInputManager tracks pending ask_user requests.
// At most one request is active per channelID at a time. A second
// RequestInput call while one is pending cancels the first.
type UserInputManager struct {
	mu      sync.Mutex
	pending map[string]chan string
}

// NewUserInputManager creates a ready-to-use UserInputManager.
func NewUserInputManager() *UserInputManager {
	return &UserInputManager{pending: make(map[string]chan string)}
}

// RequestInput registers a pending request for channelID and returns a
// receive-only channel. The caller blocks on it; it will either receive
// the user's answer or be closed (cancelled via Cancel).
// Any existing pending request for the same channelID is cancelled first.
func (m *UserInputManager) RequestInput(channelID string) <-chan string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.pending[channelID]; ok {
		close(prev) // cancel previous waiter
	}
	ch := make(chan string, 1)
	m.pending[channelID] = ch
	return ch
}

// Respond delivers the user's answer to the pending request for channelID.
// Returns true if there was an active request to respond to.
func (m *UserInputManager) Respond(channelID, answer string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.pending[channelID]
	if !ok {
		return false
	}
	ch <- answer
	delete(m.pending, channelID)
	return true
}

// Cancel closes the pending request channel, causing the blocking tool to
// receive ok=false and return an error. Returns true if there was a request.
func (m *UserInputManager) Cancel(channelID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.pending[channelID]
	if !ok {
		return false
	}
	close(ch)
	delete(m.pending, channelID)
	return true
}

// HasPending returns true if there is an active request for channelID.
func (m *UserInputManager) HasPending(channelID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.pending[channelID]
	return ok
}
