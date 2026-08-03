package jsonutil

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCleanJSONRepairsCommonModelOutput(t *testing.T) {
	raw := "```json\n{\"content\":\"line one\nline two\",\"items\":[1,2,],}\n```"
	cleaned := CleanJSON(raw)
	var got map[string]any
	if err := json.Unmarshal([]byte(cleaned), &got); err != nil {
		t.Fatalf("cleaned JSON is invalid: %v\n%s", err, cleaned)
	}
	if got["content"] != "line one\nline two" {
		t.Fatalf("content = %#v", got["content"])
	}
}

func TestParseToolArgsAndSanitizeArgs(t *testing.T) {
	var got struct {
		Path string `json:"path"`
	}
	if err := ParseToolArgs("```json\n{\"path\":\"a\",}\n```", &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != "a" {
		t.Fatalf("path = %q", got.Path)
	}

	sanitized := SanitizeArgs("{\"content\":\"a\nb\"}")
	if strings.Contains(sanitized, "a\nb") || !strings.Contains(sanitized, `a\nb`) {
		t.Fatalf("arguments were not escaped: %q", sanitized)
	}
	if got := SanitizeArgs("not-json"); got != "not-json" {
		t.Fatalf("invalid input changed unexpectedly: %q", got)
	}
}

func TestMustMarshal(t *testing.T) {
	if got := MustMarshal(map[string]int{"x": 1}); got != `{"x":1}` {
		t.Fatalf("MustMarshal = %q", got)
	}
}
