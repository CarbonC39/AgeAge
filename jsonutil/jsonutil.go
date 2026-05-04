package jsonutil

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ParseToolArgs attempts to parse a possibly malformed JSON string from LLM output.
// It applies several fixups for common LLM formatting issues before parsing.
func ParseToolArgs(raw string, target interface{}) error {
	cleaned := CleanJSON(raw)
	return json.Unmarshal([]byte(cleaned), target)
}

// SanitizeArgs cleans a tool-call arguments string and re-encodes it through
// Go's json package so that any raw control characters inside string values are
// properly escaped. Some LLM providers re-parse `arguments` server-side as JSON
// and reject requests where it contains unescaped control chars (e.g. literal
// newlines in a `content` field). Returns the original string on parse failure
// so the agent loop can still surface the error downstream.
func SanitizeArgs(raw string) string {
	if raw == "" {
		return raw
	}
	cleaned := CleanJSON(raw)
	var v interface{}
	if err := json.Unmarshal([]byte(cleaned), &v); err != nil {
		return cleaned
	}
	b, err := json.Marshal(v)
	if err != nil {
		return cleaned
	}
	return string(b)
}

// CleanJSON applies fixups for common LLM JSON formatting issues.
func CleanJSON(s string) string {
	s = strings.TrimSpace(s)

	// Strip markdown code fences (```json ... ``` or ``` ... ```)
	s = stripCodeFences(s)

	// Remove trailing commas before } or ]
	s = removeTrailingCommas(s)

	// Fix unescaped newlines inside string values
	s = fixNewlinesInStrings(s)

	return s
}

// stripCodeFences removes markdown code block wrappers.
func stripCodeFences(s string) string {
	re := regexp.MustCompile("(?s)^```(?:json)?\\s*\n?(.*?)\\s*```$")
	if matches := re.FindStringSubmatch(s); len(matches) == 2 {
		return matches[1]
	}
	return s
}

// removeTrailingCommas removes trailing commas before } or ].
func removeTrailingCommas(s string) string {
	re := regexp.MustCompile(`,\s*([}\]])`)
	return re.ReplaceAllString(s, "$1")
}

// fixNewlinesInStrings is a best-effort attempt to escape literal newlines
// that appear inside JSON string values.
func fixNewlinesInStrings(s string) string {
	// Only apply if the JSON itself doesn't parse.
	var tmp interface{}
	if json.Unmarshal([]byte(s), &tmp) == nil {
		return s // Already valid, don't touch it.
	}

	// Simple heuristic: replace unescaped newlines.
	// This won't handle all edge cases but covers the most common LLM outputs.
	result := strings.Builder{}
	inString := false
	escaped := false

	for i := 0; i < len(s); i++ {
		ch := s[i]

		if escaped {
			result.WriteByte(ch)
			escaped = false
			continue
		}

		if ch == '\\' && inString {
			result.WriteByte(ch)
			escaped = true
			continue
		}

		if ch == '"' {
			inString = !inString
			result.WriteByte(ch)
			continue
		}

		if inString && ch == '\n' {
			result.WriteString("\\n")
			continue
		}

		if inString && ch == '\r' {
			continue // Skip \r
		}

		result.WriteByte(ch)
	}

	return result.String()
}

// MustMarshal marshals v to JSON, panicking on error.
// Useful for constructing JSON schemas at init time.
func MustMarshal(v interface{}) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}
