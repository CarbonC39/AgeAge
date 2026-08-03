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

func TestLoadPipelineNormalizesDefaultsAndPreservesInputLiterals(t *testing.T) {
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
	if skill.Pipeline.Vars["count"] != "5" || skill.Pipeline.Vars["enabled"] != "true" {
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
