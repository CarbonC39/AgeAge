// Package creds manages named credentials with AES-256-GCM encryption.
//
// On-disk format: [12-byte nonce][AES-256-GCM ciphertext+tag]
// In-memory format: flat TOML  name = "value"
//
// The master key is stored separately at os.UserConfigDir()/ageage/master.key
// and is auto-generated on first use.  credentials.toml lives alongside
// config.toml in the workspace directory.
//
// All public methods are safe for concurrent use.
package creds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// Manager stores named credentials, persisting them encrypted to disk.
type Manager struct {
	credPath string
	key      [32]byte

	mu    sync.RWMutex
	store map[string]string

	// Cached replacers — rebuilt on every store mutation (Set/Remove/Reload).
	// strings.NewReplacer is O(n) per Replace call, so building once and reusing
	// is critical for performance in the tool dispatch hot path.
	subst    *strings.Replacer // {{cred:name}} → actual value
	scrubber *strings.Replacer // actual value → [REDACTED]  (longest match first)
	names    []string          // sorted name list for listing and prompts
}

// NewManager creates a Manager for the given credentials file path.
// The master key is loaded or auto-generated from os.UserConfigDir()/ageage/.
// An existing credentials file is decrypted and loaded; a missing file is
// treated as an empty store (created on first Set).
func NewManager(credPath string) (*Manager, error) {
	key, err := loadOrGenerateKey()
	if err != nil {
		return nil, fmt.Errorf("credentials master key: %w", err)
	}
	m := &Manager{
		credPath: credPath,
		key:      key,
		store:    make(map[string]string),
	}
	m.rebuildReplacers() // build empty replacers
	if _, err := os.Stat(credPath); err == nil {
		if err := m.load(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// Reload re-reads and decrypts the credentials file from disk.
// Safe to call concurrently (e.g., from the /cred reload command).
func (m *Manager) Reload() error {
	return m.load()
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.credPath)
	if err != nil {
		return fmt.Errorf("read credentials file %s: %w", m.credPath, err)
	}
	if len(data) == 0 {
		m.mu.Lock()
		m.store = make(map[string]string)
		m.rebuildReplacers()
		m.mu.Unlock()
		return nil
	}
	plain, err := decrypt(m.key, data)
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	var store map[string]string
	if _, err := toml.Decode(string(plain), &store); err != nil {
		return fmt.Errorf("parse credentials TOML: %w", err)
	}
	if store == nil {
		store = make(map[string]string)
	}
	m.mu.Lock()
	m.store = store
	m.rebuildReplacers()
	m.mu.Unlock()
	return nil
}

// Set adds or updates a credential and atomically persists to disk.
// Returns an error (including permission errors) if the write fails.
func (m *Manager) Set(name, value string) error {
	if name == "" {
		return fmt.Errorf("credential name must not be empty")
	}
	if strings.ContainsAny(name, " \t\n\r\"'=") {
		return fmt.Errorf("credential name %q contains invalid characters", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store[name] = value
	m.rebuildReplacers()
	return m.saveLocked()
}

// Remove deletes a credential and persists to disk.
func (m *Manager) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.store[name]; !ok {
		return fmt.Errorf("credential %q not found", name)
	}
	delete(m.store, name)
	m.rebuildReplacers()
	return m.saveLocked()
}

// List returns the sorted list of credential names. Values are never exposed.
func (m *Manager) List() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.names))
	copy(out, m.names)
	return out
}

// Has reports whether a credential with name exists.
func (m *Manager) Has(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.store[name]
	return ok
}

// Substitute replaces all {{cred:name}} occurrences in text with the stored
// value for name.  Unknown placeholders are left as-is.
// The fast path (no "{{cred:" in text) costs only a single Contains check.
func (m *Manager) Substitute(text string) string {
	if !strings.Contains(text, "{{cred:") {
		return text
	}
	m.mu.RLock()
	r := m.subst
	m.mu.RUnlock()
	return r.Replace(text)
}

// SubstituteJSON replaces credential placeholders only inside JSON string
// values, then re-encodes the document. This preserves valid JSON when a secret
// contains quotes, backslashes, newlines, or other escaped characters.
func (m *Manager) SubstituteJSON(data []byte) ([]byte, error) {
	if !bytes.Contains(data, []byte("{{cred:")) {
		return data, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}

	value = m.substituteJSONValue(value)
	return json.Marshal(value)
}

func (m *Manager) substituteJSONValue(value any) any {
	switch v := value.(type) {
	case string:
		return m.Substitute(v)
	case []any:
		for i := range v {
			v[i] = m.substituteJSONValue(v[i])
		}
		return v
	case map[string]any:
		for key, item := range v {
			v[key] = m.substituteJSONValue(item)
		}
		return v
	default:
		return value
	}
}

// Scrub replaces any known credential values in text with [REDACTED].
// Replacements are applied longest-value-first to avoid partial shadowing
// (e.g. a prefix value accidentally masking a longer one).
func (m *Manager) Scrub(text string) string {
	m.mu.RLock()
	r := m.scrubber
	n := len(m.names)
	m.mu.RUnlock()
	if n == 0 {
		return text
	}
	return r.Replace(text)
}

// ContainsCredPath reports whether text references the credentials file path,
// checked both in OS-native and forward-slash form, plus the bare filename.
// Used by the agent to block tools from receiving the credentials path.
func (m *Manager) ContainsCredPath(text string) bool {
	if m.credPath == "" {
		return false
	}
	return strings.Contains(text, m.credPath) ||
		strings.Contains(text, filepath.ToSlash(m.credPath)) ||
		strings.Contains(text, filepath.Base(m.credPath))
}

// Path returns the credentials file path.
func (m *Manager) Path() string { return m.credPath }

// PromptHint returns a system-prompt snippet listing credential names.
// Returns "" when no credentials exist.
func (m *Manager) PromptHint() string {
	m.mu.RLock()
	names := m.names
	m.mu.RUnlock()
	if len(names) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Stored Credentials\n")
	sb.WriteString("Use `{{cred:name}}` as a placeholder in tool call arguments. Available names:\n")
	for _, n := range names {
		fmt.Fprintf(&sb, "- {{cred:%s}}\n", n)
	}
	sb.WriteString("The credentials file is system-protected. Do not attempt to read it directly.\n\n")
	return sb.String()
}

// saveLocked serialises, encrypts, and atomically writes the store.
// Must be called with m.mu held (write lock).
func (m *Manager) saveLocked() error {
	// Encode store as flat TOML.
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m.store); err != nil {
		return fmt.Errorf("serialize credentials: %w", err)
	}

	cipherBlob, err := encrypt(m.key, buf.Bytes())
	if err != nil {
		return fmt.Errorf("encrypt credentials: %w", err)
	}

	// Atomic write: temp file in the same directory, then rename.
	dir := filepath.Dir(m.credPath)
	tmp, err := os.CreateTemp(dir, ".credentials-*.tmp")
	if err != nil {
		if os.IsPermission(err) {
			return fmt.Errorf("permission denied: cannot create credentials file in %s", dir)
		}
		return fmt.Errorf("create temp credentials file: %w", err)
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(cipherBlob)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(tmpName)
		if writeErr != nil {
			return fmt.Errorf("write credentials temp file: %w", writeErr)
		}
		return fmt.Errorf("close credentials temp file: %w", closeErr)
	}
	if err := os.Rename(tmpName, m.credPath); err != nil {
		os.Remove(tmpName)
		if os.IsPermission(err) {
			return fmt.Errorf("permission denied: cannot write %s", m.credPath)
		}
		return fmt.Errorf("finalize credentials file: %w", err)
	}
	_ = os.Chmod(m.credPath, 0o600) // best-effort; rename preserves existing perms on some OSes
	return nil
}

// rebuildReplacers reconstructs the substitute and scrub Replacers from
// the current store.  Must be called with m.mu held (write lock).
func (m *Manager) rebuildReplacers() {
	if len(m.store) == 0 {
		m.subst = strings.NewReplacer()
		m.scrubber = strings.NewReplacer()
		m.names = nil
		return
	}

	substPairs := make([]string, 0, len(m.store)*2)
	names := make([]string, 0, len(m.store))

	type valEntry struct{ v string }
	entries := make([]valEntry, 0, len(m.store))

	for name, val := range m.store {
		substPairs = append(substPairs, "{{cred:"+name+"}}", val)
		names = append(names, name)
		if val != "" {
			entries = append(entries, valEntry{val})
		}
	}

	// Sort values longest-first so the scrubber replaces longer secrets before
	// shorter ones that might be a prefix of a longer value.
	sort.Slice(entries, func(i, j int) bool {
		return len(entries[i].v) > len(entries[j].v)
	})
	scrubPairs := make([]string, 0, len(entries)*2)
	for _, e := range entries {
		scrubPairs = append(scrubPairs, e.v, "[REDACTED]")
	}

	sort.Strings(names)

	m.subst = strings.NewReplacer(substPairs...)
	m.scrubber = strings.NewReplacer(scrubPairs...)
	m.names = names
}
