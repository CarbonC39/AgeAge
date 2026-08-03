package tools

import (
	"fmt"
	"sync"
	"time"
)

// ConfirmationManager manages async confirmation requests for supervised mode.
// Used in channel mode where confirmations require async user responses.
type ConfirmationManager struct {
	pendingConfirmations map[string]*PendingConfirmation
	mu                   sync.RWMutex
}

// PendingConfirmation represents a confirmation request awaiting user response.
type PendingConfirmation struct {
	ID        string
	Operation string
	ChannelID string
	Timestamp time.Time
	ResultCh  chan bool // Receives true (allow) or false (deny)
}

// NewConfirmationManager creates a new confirmation manager.
func NewConfirmationManager() *ConfirmationManager {
	return &ConfirmationManager{
		pendingConfirmations: make(map[string]*PendingConfirmation),
	}
}

// RequestConfirmation creates a new confirmation request and returns its ID and result channel.
// The result channel will receive true (allow) or false (deny) when the user responds,
// or close (time out) after the timeout.
func (cm *ConfirmationManager) RequestConfirmation(operation, channelID string, timeout time.Duration) (string, chan bool) {
	id := fmt.Sprintf("confirm_%d", time.Now().UnixNano())
	resultCh := make(chan bool, 1)

	pc := &PendingConfirmation{
		ID:        id,
		Operation: operation,
		ChannelID: channelID,
		Timestamp: time.Now(),
		ResultCh:  resultCh,
	}

	cm.mu.Lock()
	cm.pendingConfirmations[id] = pc
	cm.mu.Unlock()

	// Timeout handler
	go func() {
		time.Sleep(timeout)
		cm.mu.Lock()
		if _, exists := cm.pendingConfirmations[id]; exists {
			delete(cm.pendingConfirmations, id)
			close(resultCh)
		}
		cm.mu.Unlock()
	}()

	return id, resultCh
}

// RespondToConfirmation processes a user's response to a pending confirmation.
// Returns true if the confirmation was found and processed, false otherwise.
func (cm *ConfirmationManager) RespondToConfirmation(id string, allowed bool) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	pc, ok := cm.pendingConfirmations[id]
	if !ok {
		return false
	}

	select {
	case pc.ResultCh <- allowed:
		close(pc.ResultCh)
	default:
		// Channel already has value or closed
	}

	delete(cm.pendingConfirmations, id)
	return true
}

// GetAllPending returns all pending confirmations for a specific channel.
func (cm *ConfirmationManager) GetAllPending(channelID string) []*PendingConfirmation {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	var result []*PendingConfirmation
	for _, pc := range cm.pendingConfirmations {
		if pc.ChannelID == channelID {
			result = append(result, pc)
		}
	}
	return result
}
