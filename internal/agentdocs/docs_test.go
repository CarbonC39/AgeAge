package agentdocs

import (
	"strings"
	"testing"
)

func TestReadEmbeddedDocs(t *testing.T) {
	content, ok := Read("pipeline.md")
	if !ok || !strings.Contains(content, "Pipeline Skills") {
		t.Fatalf("pipeline doc missing: ok=%v content=%q", ok, content)
	}
	if content, ok := Read("missing.md"); ok || content != "" {
		t.Fatalf("missing doc = (%q, %v)", content, ok)
	}
}
