package creds

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testManager(values map[string]string) *Manager {
	m := &Manager{
		credPath: filepath.Join("tmp", "credentials.toml"),
		store:    values,
	}
	m.rebuildReplacers()
	return m
}

func TestSubstituteJSONEscapesSecretsAndPreservesTypes(t *testing.T) {
	secret := "line 1\n\"quoted\"\\tail"
	m := testManager(map[string]string{"token": secret})
	raw := []byte(`{"command":"send {{cred:token}}","nested":["{{cred:token}}",7,true],"unknown":"{{cred:missing}}"}`)

	got, err := m.SubstituteJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("substituted output is invalid JSON: %v\n%s", err, got)
	}
	if decoded["command"] != "send "+secret {
		t.Fatalf("command = %#v", decoded["command"])
	}
	nested := decoded["nested"].([]any)
	if nested[0] != secret || nested[1] != float64(7) || nested[2] != true {
		t.Fatalf("nested values changed: %#v", nested)
	}
	if decoded["unknown"] != "{{cred:missing}}" {
		t.Fatalf("unknown placeholder changed: %#v", decoded["unknown"])
	}
}

func TestSubstituteJSONFastPathAndMalformedInput(t *testing.T) {
	m := testManager(map[string]string{"token": "secret"})
	raw := []byte(`{"n":9007199254740993}`)
	got, err := m.SubstituteJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, raw) {
		t.Fatalf("fast path changed JSON: %s", got)
	}
	if _, err := m.SubstituteJSON([]byte(`{"x":"{{cred:token}}"`)); err == nil {
		t.Fatal("expected malformed JSON error")
	}
}

func TestScrubUsesLongestValuesAndListsNames(t *testing.T) {
	m := testManager(map[string]string{
		"short": "abc",
		"long":  "abcdef",
	})
	if got := m.Scrub("abcdef abc"); got != "[REDACTED] [REDACTED]" {
		t.Fatalf("scrubbed = %q", got)
	}
	if got := m.List(); !reflect.DeepEqual(got, []string{"long", "short"}) {
		t.Fatalf("names = %#v", got)
	}
	if hint := m.PromptHint(); !strings.Contains(hint, "long") || strings.Contains(hint, "abcdef") {
		t.Fatalf("unsafe prompt hint: %q", hint)
	}
}

func TestContainsCredPathRecognizesAbsoluteSlashAndBasename(t *testing.T) {
	m := testManager(nil)
	for _, text := range []string{
		m.credPath,
		filepath.ToSlash(m.credPath),
		"cat credentials.toml",
	} {
		if !m.ContainsCredPath(text) {
			t.Errorf("did not recognize credential path in %q", text)
		}
	}
	if m.ContainsCredPath("credentials.toml.bak") == false {
		t.Fatal("basename embedded in a larger argument should remain protected")
	}
}
