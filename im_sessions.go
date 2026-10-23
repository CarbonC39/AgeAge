package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"

	"ageage/agent"
	"ageage/conversation"
)

// imSessions owns durable conversation bindings and display names. Native
// messaging and presentation are supplied by surfaces and renderers elsewhere.
type imSessions struct {
	mu       sync.Mutex
	store    *agent.SessionManager
	registry *agent.SessionRegistry
	active   map[string]string
	targets  map[string]conversation.Target
}

func newIMSessions(store *agent.SessionManager, registry *agent.SessionRegistry) *imSessions {
	s := &imSessions{store: store, registry: registry, active: make(map[string]string), targets: make(map[string]conversation.Target)}
	if registry != nil {
		for _, binding := range registry.List() {
			if !binding.Target().Valid() || agent.IsReservedSessionID(binding.SessionID) {
				continue
			}
			if info, err := os.Stat(store.SessionDir(binding.SessionID)); err == nil && info.IsDir() {
				s.active[binding.Target().BindingKey()] = binding.SessionID
				s.targets[binding.SessionID] = binding.Target()
			}
		}
	}
	return s
}

func imSessionPrefix(target conversation.Target) string {
	legacy := imSessionLegacyPrefix(target)
	if target.OwnerID == "" {
		return legacy
	}
	roomKey := target.Room().BindingKey()
	return legacy + "-owner-" + roomKey[:20]
}

func imSessionLegacyPrefix(target conversation.Target) string {
	return agent.SanitizeSessionID(target.ChannelType + ":" + target.ChannelID)
}

func isOwnedSessionNamespace(id, legacyPrefix string) bool {
	marker := legacyPrefix + "-owner-"
	if !strings.HasPrefix(id, marker) {
		return false
	}
	rest := strings.TrimPrefix(id, marker)
	if len(rest) <= 20 || rest[20] != '-' {
		return false
	}
	for _, r := range rest[:20] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// Current does no history I/O: pending ask_user answers must be routed while
// the running agent owns the session transaction lock.
func (s *imSessions) Current(target conversation.Target) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[target.BindingKey()]
}

func (s *imSessions) Resolve(target conversation.Target) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id := s.active[target.BindingKey()]; id != "" {
		return id, nil
	}
	if !target.Valid() {
		return "", fmt.Errorf("conversation target is incomplete")
	}
	if s.registry != nil {
		if binding, ok := s.registry.ResolveTarget(target); ok {
			if info, err := os.Stat(s.store.SessionDir(binding.SessionID)); err == nil && info.IsDir() {
				s.active[target.BindingKey()] = binding.SessionID
				return binding.SessionID, nil
			}
			if err := s.registry.Unbind(binding.Key); err != nil {
				return "", err
			}
		}
	}
	// External identity is represented by the durable key, never by a lossy
	// sanitization of a sender-supplied room/thread ID or session name.
	id := imSessionPrefix(target) + "-c" + target.BindingKey()[:20]
	if _, err := os.Stat(s.store.SessionDir(id)); os.IsNotExist(err) {
		if err = s.store.CreateSession(id); err != nil {
			return "", err
		}
		if err = s.store.SetDisplayName(id, "Conversation "+target.BindingKey()[:6]); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if err := s.bindLocked(target, id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *imSessions) Bind(target conversation.Target, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bindLocked(target, id)
}
func (s *imSessions) bindLocked(target conversation.Target, id string) error {
	if !target.Valid() {
		return fmt.Errorf("conversation target is incomplete")
	}
	if current := s.active[target.BindingKey()]; target.ThreadID != "" && current != "" && current != id {
		return fmt.Errorf("this thread is already bound to another session")
	}
	for key, current := range s.active {
		if current == id && key != target.BindingKey() {
			return fmt.Errorf("session is already bound to another conversation")
		}
	}
	if s.registry != nil {
		kind := "room"
		if target.ThreadID != "" {
			kind = "thread"
		}
		if err := s.registry.BindTarget(target, id, kind); err != nil {
			return err
		}
	}
	if previous := s.active[target.BindingKey()]; previous != "" && previous != id {
		delete(s.targets, previous)
	}
	s.active[target.BindingKey()] = id
	s.targets[id] = target
	return nil
}

// Create allocates an opaque ID. Human labels are persisted independently and
// can contain spaces, Unicode, and characters unsuitable for directory names.
func (s *imSessions) Create(target conversation.Target, name string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = strings.TrimSpace(name)
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", "", err
	}
	if name == "" {
		name = "Session " + hex.EncodeToString(random[:3])
	}
	id := imSessionPrefix(target) + "-s" + hex.EncodeToString(random[:])
	if err := s.store.CreateSession(id); err != nil {
		return "", "", err
	}
	if err := s.store.SetDisplayName(id, name); err != nil {
		_ = s.store.Delete(id)
		return "", "", err
	}
	return id, name, nil
}

func (s *imSessions) List(target conversation.Target) ([]agent.SessionInfo, error) {
	return s.listLocked(target)
}
func (s *imSessions) listLocked(target conversation.Target) ([]agent.SessionInfo, error) {
	legacyPrefix := imSessionLegacyPrefix(target)
	infos, err := s.store.ListWithPrefix(legacyPrefix)
	if err != nil {
		return nil, err
	}
	result := make([]agent.SessionInfo, 0, len(infos))
	for _, info := range infos {
		bindings := []agent.SessionBinding(nil)
		if s.registry != nil {
			bindings = s.registry.GetForSession(info.FullID)
		}
		if len(bindings) > 0 {
			binding := bindings[0]
			if binding.ChannelType != target.ChannelType || binding.ChannelID != target.ChannelID || binding.OwnerID != target.OwnerID {
				continue
			}
			if target.OwnerID != "" && strings.HasPrefix(info.FullID, imSessionPrefix(target)+"-") {
				info.ID = strings.TrimPrefix(info.FullID, imSessionPrefix(target)+"-")
			}
			result = append(result, info)
			continue
		}
		if target.OwnerID != "" {
			ownedPrefix := imSessionPrefix(target) + "-"
			if !strings.HasPrefix(info.FullID, ownedPrefix) {
				continue
			}
			info.ID = strings.TrimPrefix(info.FullID, ownedPrefix)
		} else if isOwnedSessionNamespace(info.FullID, legacyPrefix) {
			// An unbound session in an owner namespace remains private even after
			// its room binding is switched away.
			continue
		}
		result = append(result, info)
	}
	return result, nil
}

func (s *imSessions) Find(target conversation.Target, name string) (agent.SessionInfo, error) {
	infos, err := s.listLocked(target)
	if err != nil {
		return agent.SessionInfo{}, err
	}
	var matches []agent.SessionInfo
	for _, info := range infos {
		if info.FullID == name || info.ID == name {
			return info, nil
		}
		if strings.EqualFold(info.DisplayName, name) {
			matches = append(matches, info)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return agent.SessionInfo{}, fmt.Errorf("more than one session is named %q; use its ID", name)
	}
	return agent.SessionInfo{}, fmt.Errorf("session %q does not exist", name)
}

func (s *imSessions) Destination(id string) (conversation.Target, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registry != nil {
		for _, binding := range s.registry.GetForSession(id) {
			return binding.Target(), true
		}
		return conversation.Target{}, false
	}
	target, ok := s.targets[id]
	return target, ok
}

func (s *imSessions) Remove(target conversation.Target, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[target.BindingKey()] == id {
		return fmt.Errorf("cannot remove the current session")
	}
	for _, current := range s.active {
		if current == id {
			return fmt.Errorf("session is bound to another conversation")
		}
	}
	if s.registry != nil && len(s.registry.GetForSession(id)) > 0 {
		return fmt.Errorf("session is bound to another conversation")
	}
	return s.store.Trash(id)
}
