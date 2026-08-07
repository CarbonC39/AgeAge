package skills

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeSkillTestFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOutputsMapAcceptsMapListAndScalar(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want OutputsMap
	}{
		{"map", "answer: result\ncount: total\n", OutputsMap{"answer": "result", "count": "total"}},
		{"list", "[answer, summary]\n", OutputsMap{"answer": "answer", "summary": "summary"}},
		{"scalar", "answer\n", OutputsMap{"answer": "result"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got OutputsMap
			if err := yaml.Unmarshal([]byte(tt.yaml), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("outputs = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestLoadPipelinePreservesDefaultsAndInputLiterals(t *testing.T) {
	path := writeSkillTestFile(t, "pipeline.yaml", `
name: sample
description: sample pipeline
tier: medium
returns: answer
vars:
  count: 5
  enabled: true
  items: [a, b]
pipeline:
  - id: prepare
    prompt: prepare
    inputs:
      timeout: 30
      flag: true
    outputs: answer
`)
	skill, err := LoadSkillByPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if !skill.IsPipeline() || skill.Pipeline.Returns != "answer" {
		t.Fatalf("pipeline metadata = %#v", skill.Pipeline)
	}
	if skill.Pipeline.Vars["count"] != 5 || skill.Pipeline.Vars["enabled"] != true {
		t.Fatalf("primitive defaults = %#v", skill.Pipeline.Vars)
	}
	if _, ok := skill.Pipeline.Vars["items"].([]interface{}); !ok {
		t.Fatalf("list default lost type: %#v", skill.Pipeline.Vars["items"])
	}
	node := skill.Pipeline.Pipeline[0]
	if node.Inputs["timeout"] != 30 || node.Inputs["flag"] != true {
		t.Fatalf("literal inputs changed type: %#v", node.Inputs)
	}
	if node.Outputs["answer"] != "result" {
		t.Fatalf("scalar output = %#v", node.Outputs)
	}
}

func TestLoadPipelineRejectsUnknownFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "top level",
			body: `
name: typo
description: typo
tier: base
retuns: answer
pipeline:
  - id: prepare
    prompt: prepare
    outputs: answer
`,
			want: "field retuns not found",
		},
		{
			name: "node",
			body: `
name: typo
description: typo
tier: base
pipeline:
  - id: prepare
    promtp: prepare
    outputs: answer
`,
			want: "field promtp not found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeSkillTestFile(t, "unknown.yaml", tt.body)
			_, err := LoadSkillByPath(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("unknown field error = %v", err)
			}
		})
	}
}

func TestLoadPipelineRejectsDuplicateIDsAndMissingAutoTool(t *testing.T) {
	duplicate := writeSkillTestFile(t, "duplicate.yaml", `
name: duplicate
description: duplicate
tier: base
pipeline:
  - id: same
    prompt: one
  - id: same
    prompt: two
`)
	if _, err := LoadSkillByPath(duplicate); err == nil || !strings.Contains(err.Error(), "duplicate node id") {
		t.Fatalf("duplicate error = %v", err)
	}

	missingTool := writeSkillTestFile(t, "missing-tool.yaml", `
name: missing-tool
description: missing tool
tier: base
pipeline:
  - id: auto
    type: auto
`)
	if _, err := LoadSkillByPath(missingTool); err == nil || !strings.Contains(err.Error(), "no tool") {
		t.Fatalf("missing tool error = %v", err)
	}
}

func TestLoadMarkdownSkillAndCommandName(t *testing.T) {
	path := writeSkillTestFile(t, "review.md", `---
name: Code Review
description: Review code
tier: strong
required_tools: [grep, file_read]
---
Inspect the repository carefully.
`)
	skill, err := LoadSkillByPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if skill.IsPipeline() || skill.CommandName() != "code-review" || !strings.Contains(skill.Prompt, "Inspect") {
		t.Fatalf("loaded skill = %#v", skill)
	}
	if !reflect.DeepEqual(skill.RequiredTools, []string{"grep", "file_read"}) {
		t.Fatalf("required tools = %#v", skill.RequiredTools)
	}
}

func TestLoadSkillsMissingDirectoryIsEmpty(t *testing.T) {
	got, err := LoadSkills(filepath.Join(t.TempDir(), "missing"))
	if err != nil || got != nil {
		t.Fatalf("LoadSkills = (%#v, %v)", got, err)
	}
}

func TestStripSkillComments(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"single line", "a <!-- note --> b", "a  b"},
		{"multiline comment", "a <!-- one\ntwo --> b", "a \n b"},
		{"inside fence preserved", "x\n```\n<!-- literal -->\n```\ny", "x\n```\n<!-- literal -->\n```\ny"},
		{"fence with tilde", "```go\n<!-- keep -->\n```\n<!-- drop -->\n~~~\n<!-- keep -->\n~~~", "```go\n<!-- keep -->\n```\n\n~~~\n<!-- keep -->\n~~~"},
		{"unterminated comment", "a <!-- nope", "a "},
		{"multiple on one line", "a <!-- one --> b <!-- two --> c", "a  b  c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripSkillComments(tt.in); got != tt.want {
				t.Fatalf("stripSkillComments(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestLoadSegmentedSkill(t *testing.T) {
	path := writeSkillTestFile(t, "segmented.md", `---
name: segmented
description: Segmented skill
tier: medium
segmented: true
---
First segment instructions.
<!-- author note, must be stripped -->
====
Second segment instructions.

===
Third segment instructions.
`)
	skill, err := LoadSkillByPath(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"First segment instructions.",
		"Second segment instructions.",
		"Third segment instructions.",
	}
	if !reflect.DeepEqual(skill.Segments, want) {
		t.Fatalf("segments = %#v, want %#v", skill.Segments, want)
	}
	if strings.Contains(skill.Prompt, "author note") {
		t.Fatal("comment was not stripped from segmented prompt")
	}
	if !strings.Contains(skill.Prompt, "===") {
		t.Fatal("raw prompt should retain separator lines")
	}
}

func TestLoadSegmentedSkillRejectsTooFewSegments(t *testing.T) {
	path := writeSkillTestFile(t, "single.md", `---
name: single
description: only one segment
segmented: true
---
Just one segment here.
`)
	if _, err := LoadSkillByPath(path); err == nil || !strings.Contains(err.Error(), "segmented") {
		t.Fatalf("expected segmented error, got %v", err)
	}
}

func TestLoadSegmentedSkillRequiresExplicitOptIn(t *testing.T) {
	path := writeSkillTestFile(t, "plain.md", `---
name: plain
description: not segmented
---
Part one.

---

Part two.
`)
	skill, err := LoadSkillByPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if skill.Segmented || len(skill.Segments) != 0 {
		t.Fatalf("non-segmented skill got Segments = %#v", skill.Segments)
	}
	if !strings.Contains(skill.Prompt, "Part two.") {
		t.Fatalf("non-segmented prompt should keep separator content: %q", skill.Prompt)
	}
}
