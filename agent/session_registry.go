package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const sessionRegistryVersion = 1

// SessionBinding is the durable association between an external conversation
// and a session. It is intentionally independent of main.go's transport
// types, so CLI, IM, and cron can use the same registry later.
type SessionBinding struct {
	Key         string    `json:"key"`
	SessionID   string    `json:"session_id"`
	ChannelType string    `json:"channel_type,omitempty"`
	ChannelID   string    `json:"channel_id,omitempty"`
	ThreadID    string    `json:"thread_id,omitempty"`
	OwnerID     string    `json:"owner_id,omitempty"`
	Kind        string    `json:"kind,omitempty"` // cli, room, thread, cron
	UpdatedAt   time.Time `json:"updated_at"`
}

type sessionRegistryFile struct {
	Version  int                       `json:"version"`
	Bindings map[string]SessionBinding `json:"bindings"`
}

// SessionRegistry persists active bindings separately from conversation
// history. Missing registry files are treated as an empty registry for
// migration compatibility with existing installations.
type SessionRegistry struct {
	path     string
	mu       sync.RWMutex
	bindings map[string]SessionBinding
}

var registryLocks sync.Map // absolute registry path -> *sync.Mutex

func registryLock(path string) *sync.Mutex {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	lock := &sync.Mutex{}
	actual, _ := registryLocks.LoadOrStore(abs, lock)
	return actual.(*sync.Mutex)
}

// NewSessionRegistry opens a registry file. Existing files from a future
// version are rejected rather than silently overwritten.
func NewSessionRegistry(path string) (*SessionRegistry, error) {
	r := &SessionRegistry{path: path, bindings: make(map[string]SessionBinding)}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// OpenSessionRegistry uses the standard location under the sessions directory
// so existing session data remains grouped together.
func OpenSessionRegistry(ageageDir string) (*SessionRegistry, error) {
	return NewSessionRegistry(filepath.Join(ageageDir, "sessions", "index.json"))
}

func (r *SessionRegistry) load() error {
	lock := registryLock(r.path)
	lock.Lock()
	defer lock.Unlock()
	return r.reloadLocked()
}

// reloadLocked refreshes the in-memory snapshot while the process-wide file
// lock is held. Mutations use this immediately before applying their change,
// preventing two registry instances from overwriting one another's bindings.
func (r *SessionRegistry) reloadLocked() error {
	r.bindings = make(map[string]SessionBinding)
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read session registry: %w", err)
	}
	var file sessionRegistryFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("parse session registry: %w", err)
	}
	if file.Version != 0 && file.Version != sessionRegistryVersion {
		return fmt.Errorf("unsupported session registry version %d", file.Version)
	}
	bindings := make(map[string]SessionBinding, len(file.Bindings))
	for key, binding := range file.Bindings {
		if binding.Key == "" {
			binding.Key = key
		}
		if binding.SessionID != "" {
			bindings[binding.Key] = binding
		}
	}
	r.bindings = bindings
	return nil
}

func (r *SessionRegistry) Path() string { return r.path }

// Reload refreshes the in-memory snapshot from disk. It is useful when a
// long-lived process shares the registry with another SessionRegistry owner.
func (r *SessionRegistry) Reload() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	lock := registryLock(r.path)
	lock.Lock()
	defer lock.Unlock()
	return r.reloadLocked()
}

// BindingKey creates a stable, collision-resistant key for an external
// location. Length-prefixing prevents delimiter collisions in IDs.
func BindingKey(channelType, channelID, threadID, ownerID string) string {
	parts := []string{channelType, channelID, threadID, ownerID}
	var b strings.Builder
	for _, part := range parts {
		fmt.Fprintf(&b, "%d:%s|", len(part), part)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// KeyForBinding returns the binding key, deriving one from its scope when the
// caller did not set an explicit key. Existing explicit keys are preserved.
func KeyForBinding(binding SessionBinding) string {
	if binding.Key != "" {
		return binding.Key
	}
	return BindingKey(binding.ChannelType, binding.ChannelID, binding.ThreadID, binding.OwnerID)
}

// Bind upserts one external-to-session association and persists it atomically.
func (r *SessionRegistry) Bind(binding SessionBinding) error {
	if binding.SessionID == "" {
		return fmt.Errorf("session binding has empty session ID")
	}
	binding.Key = KeyForBinding(binding)
	binding.UpdatedAt = time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	fileLock := registryLock(r.path)
	fileLock.Lock()
	defer fileLock.Unlock()
	if err := r.reloadLocked(); err != nil {
		return err
	}
	for key, existing := range r.bindings {
		if key != binding.Key && existing.SessionID == binding.SessionID && existing.Kind != "cron" {
			return fmt.Errorf("session %q is already bound to another conversation", binding.SessionID)
		}
	}
	r.bindings[binding.Key] = binding
	return r.saveFileLocked()
}

func (r *SessionRegistry) Get(key string) (SessionBinding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	binding, ok := r.bindings[key]
	return binding, ok
}

// GetForLocation returns bindings with the same channel/thread location,
// regardless of owner. Results are sorted for deterministic callers.
func (r *SessionRegistry) GetForLocation(channelType, channelID, threadID string) []SessionBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]SessionBinding, 0)
	for _, binding := range r.bindings {
		if binding.ChannelType == channelType && binding.ChannelID == channelID && binding.ThreadID == threadID {
			result = append(result, binding)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

// GetForSession returns every external binding currently pointing at a
// session. Callers use this to prevent one mutable Agent from being shared by
// multiple concurrent chat keys.
func (r *SessionRegistry) GetForSession(sessionID string) []SessionBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]SessionBinding, 0)
	for _, binding := range r.bindings {
		if binding.SessionID == sessionID {
			result = append(result, binding)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

func (r *SessionRegistry) List() []SessionBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]SessionBinding, 0, len(r.bindings))
	for _, binding := range r.bindings {
		result = append(result, binding)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

func (r *SessionRegistry) Unbind(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	fileLock := registryLock(r.path)
	fileLock.Lock()
	defer fileLock.Unlock()
	if err := r.reloadLocked(); err != nil {
		return err
	}
	if _, ok := r.bindings[key]; !ok {
		return nil
	}
	delete(r.bindings, key)
	return r.saveFileLocked()
}

// RemoveSession unbinds every external location pointing at sessionID.
func (r *SessionRegistry) RemoveSession(sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	fileLock := registryLock(r.path)
	fileLock.Lock()
	defer fileLock.Unlock()
	if err := r.reloadLocked(); err != nil {
		return err
	}
	changed := false
	for key, binding := range r.bindings {
		if binding.SessionID == sessionID {
			delete(r.bindings, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.saveFileLocked()
}

// RenameSession updates every binding for oldID. It is intentionally separate
// from SessionManager.Rename so callers can reject active bindings or update
// them according to their own ownership policy.
func (r *SessionRegistry) RenameSession(oldID, newID string) error {
	if oldID == "" || newID == "" {
		return fmt.Errorf("session IDs must not be empty")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	fileLock := registryLock(r.path)
	fileLock.Lock()
	defer fileLock.Unlock()
	if err := r.reloadLocked(); err != nil {
		return err
	}
	for _, binding := range r.bindings {
		if binding.SessionID == newID && binding.SessionID != oldID {
			return fmt.Errorf("session %q already has an active binding", newID)
		}
	}
	changed := false
	for key, binding := range r.bindings {
		if binding.SessionID == oldID {
			binding.SessionID = newID
			binding.UpdatedAt = time.Now().UTC()
			r.bindings[key] = binding
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.saveFileLocked()
}

func (r *SessionRegistry) saveFileLocked() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sessionRegistryFile{Version: sessionRegistryVersion, Bindings: r.bindings}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".session-bindings-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, r.path); err != nil {
		return err
	}
	// The temporary file was already chmodded before the atomic rename. A
	// post-rename chmod could report failure after the durable state changed,
	// leaving callers to believe the mutation was rolled back when it was not.
	return nil
}
