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
	// allowedResponders is an optional per-channel allowlist. The originating
	// sender is always allowed; entries here may additionally approve requests
	// in a channel after the scope location has matched.
	allowedResponders map[string]map[string]struct{}
	mu                sync.RWMutex
}

// PendingConfirmation represents a confirmation request awaiting user response.
type PendingConfirmation struct {
	ID        string
	Operation string
	ChannelID string
	Scope     InteractionScope
	Timestamp time.Time
	ResultCh  chan bool // Receives true (allow) or false (deny)
}

// NewConfirmationManager creates a new confirmation manager.
func NewConfirmationManager() *ConfirmationManager {
	return &ConfirmationManager{
		pendingConfirmations: make(map[string]*PendingConfirmation),
		allowedResponders:    make(map[string]map[string]struct{}),
	}
}

// RequestConfirmation creates a new confirmation request and returns its ID and result channel.
// The result channel will receive true (allow) or false (deny) when the user responds,
// or close (time out) after the timeout.
func (cm *ConfirmationManager) RequestConfirmation(operation, channelID string, timeout time.Duration) (string, chan bool) {
	return cm.RequestConfirmationScoped(operation, LegacyInteractionScope(channelID), timeout)
}

// RequestConfirmationScoped creates a confirmation bound to a complete
// interaction scope. The request's ID remains available for diagnostics and
// future explicit-approval UIs, but channel replies are accepted only when
// their scope and authorisation match.
func (cm *ConfirmationManager) RequestConfirmationScoped(operation string, scope InteractionScope, timeout time.Duration) (string, chan bool) {
	id := fmt.Sprintf("confirm_%d", time.Now().UnixNano())
	resultCh := make(chan bool, 1)

	pc := &PendingConfirmation{
		ID:        id,
		Operation: operation,
		ChannelID: scope.ChannelID,
		Scope:     scope,
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
	return cm.respondToConfirmation(id, InteractionScope{}, allowed, false)
}

// RespondToConfirmationScoped processes a response only if the request ID
// and complete interaction scope match. The originating sender or a sender
// explicitly allowed for the channel may respond.
func (cm *ConfirmationManager) RespondToConfirmationScoped(id string, scope InteractionScope, allowed bool) bool {
	return cm.respondToConfirmation(id, scope, allowed, true)
}

func (cm *ConfirmationManager) respondToConfirmation(id string, scope InteractionScope, allowed bool, checkScope bool) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	pc, ok := cm.pendingConfirmations[id]
	if !ok {
		return false
	}
	// The legacy ID-only API remains valid for CLI-created, channel-less
	// requests. Scoped channel requests must always use a scope-checked method.
	if !checkScope && !pc.Scope.isLegacy() {
		return false
	}
	if checkScope && (!pc.Scope.locationEqual(scope) || !cm.authorizedLocked(pc, scope.SenderID)) {
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

// RespondForScope responds only when exactly one pending request matches the
// scope. It intentionally refuses ambiguous replies instead of selecting an
// arbitrary pending request.
func (cm *ConfirmationManager) RespondForScope(scope InteractionScope, allowed bool) bool {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	var match *PendingConfirmation
	for _, pc := range cm.pendingConfirmations {
		if pc.Scope.locationEqual(scope) && cm.authorizedLocked(pc, scope.SenderID) {
			if match != nil {
				return false
			}
			match = pc
		}
	}
	if match == nil {
		return false
	}
	select {
	case match.ResultCh <- allowed:
		close(match.ResultCh)
	default:
		return false
	}
	delete(cm.pendingConfirmations, match.ID)
	return true
}

// SetAllowedResponders configures optional channel-level approvers. The
// request's originating sender remains authorised even when this list is set.
func (cm *ConfirmationManager) SetAllowedResponders(channelID string, senderIDs ...string) {
	cm.setAllowedResponders(responderScopeKey("", channelID), senderIDs...)
}

// SetAllowedRespondersForScope configures approvers for one channel type and
// channel ID. Channel type is part of the key so equal room IDs on different
// connectors cannot inherit each other's authorization policy.
func (cm *ConfirmationManager) SetAllowedRespondersForScope(scope InteractionScope, senderIDs ...string) {
	cm.setAllowedResponders(responderScopeKey(scope.ChannelType, scope.ChannelID), senderIDs...)
}

func (cm *ConfirmationManager) setAllowedResponders(key string, senderIDs ...string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.allowedResponders == nil {
		cm.allowedResponders = make(map[string]map[string]struct{})
	}
	set := make(map[string]struct{}, len(senderIDs))
	for _, senderID := range senderIDs {
		if senderID != "" {
			set[senderID] = struct{}{}
		}
	}
	cm.allowedResponders[key] = set
}

func (cm *ConfirmationManager) authorizedLocked(pc *PendingConfirmation, senderID string) bool {
	if senderID == pc.Scope.SenderID {
		return true
	}
	_, ok := cm.allowedResponders[responderScopeKey(pc.Scope.ChannelType, pc.Scope.ChannelID)][senderID]
	return ok
}

func responderScopeKey(channelType, channelID string) string {
	return channelType + "\x00" + channelID
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

// GetPendingForScope returns requests at the same location and is intended
// for diagnostics. Callers that need to answer should use RespondForScope,
// which rejects ambiguity.
func (cm *ConfirmationManager) GetPendingForScope(scope InteractionScope) []*PendingConfirmation {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	var result []*PendingConfirmation
	for _, pc := range cm.pendingConfirmations {
		if pc.Scope.locationEqual(scope) {
			result = append(result, pc)
		}
	}
	return result
}
