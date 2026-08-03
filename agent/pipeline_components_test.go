package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ageage/tools"
)

func writeAgentTestFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func validationMessages(errs []ValidationError) string {
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "\n")
}

func TestValidatePipelineAcceptsValidContract(t *testing.T) {
	path := writeAgentTestFile(t, "valid.yaml", `
name: valid
description: valid pipeline
tier: medium
returns: answer
vars:
  topic: ""
pipeline:
  - id: prepare
    prompt: "Prepare $vars.topic"
    outputs:
      query: result
  - id: fetch
    type: auto
    tool: web_search
    inputs:
      query: $vars.query
    outputs:
      answer: result
`)
	if errs := ValidateSkillFile(path, nil); len(errs) != 0 {
		t.Fatalf("valid pipeline errors:\n%s", validationMessages(errs))
	}
}

func TestValidatePipelineReportsStructuralProblems(t *testing.T) {
	path := writeAgentTestFile(t, "invalid.yaml", `
name: invalid
description: invalid pipeline
tier: medium
returns: missing
pipeline:
  - id: fetch
    type: auto
    tool: not_a_tool
    outputs:
      value: result
`)
	messages := validationMessages(ValidateSkillFile(path, nil))
	for _, want := range []string{"first node must be type: agent", `unknown tool "not_a_tool"`, "no node produces"} {
		if !strings.Contains(messages, want) {
			t.Errorf("missing %q in:\n%s", want, messages)
		}
	}
}

func TestNodeCompletePublishesOneStructuredResult(t *testing.T) {
	results := make(chan NodeResult, 1)
	finish := &tools.FinishTool{}
	tool := &NodeCompleteTool{resultCh: results, finishTool: finish}

	got, err := tool.Execute(context.Background(), json.RawMessage(`{"status":"success","vars":{"answer":"done"}}`))
	if err != nil || !strings.Contains(got, "Node complete") {
		t.Fatalf("Execute = (%q, %v)", got, err)
	}
	result := <-results
	if result.Status != "success" || result.Vars["answer"] != "done" || !finish.Finished {
		t.Fatalf("result=%#v finish=%#v", result, finish)
	}
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"status":"success"}`)); err == nil {
		t.Fatal("second node_complete call should fail")
	}
}

func TestPipelineTierFallbackOrder(t *testing.T) {
	executor := &PipelineExecutor{}
	if got := executor.tierFallbacks("strong"); !slices.Equal(got, []string{"strong", "medium"}) {
		t.Fatalf("strong fallback = %#v", got)
	}
	if got := executor.tierFallbacks("medium"); !slices.Equal(got, []string{"medium", ""}) {
		t.Fatalf("medium fallback = %#v", got)
	}
	if got := executor.tierFallbacks("base"); !slices.Equal(got, []string{"base"}) {
		t.Fatalf("base fallback = %#v", got)
	}
}

func TestSkillPatchRejectsInvalidContentWithoutChangingOriginal(t *testing.T) {
	original := "---\nname: example\ndescription: example skill\ntier: base\n---\nDo the task.\n"
	path := writeAgentTestFile(t, "example.md", original)
	tool := &skillPatchTool{skillFilePath: path}

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"content":"not a valid skill"}`)); err == nil {
		t.Fatal("invalid patch was accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("invalid patch changed original:\n%s", data)
	}

	replacement := "---\nname: example\ndescription: improved skill\ntier: medium\n---\nDo it better.\n"
	args, _ := json.Marshal(map[string]string{"content": replacement})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("valid patch failed: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != replacement {
		t.Fatalf("replacement mismatch:\n%s", data)
	}
}

func TestSkillFingerprintChangesOnAddModifyAndDelete(t *testing.T) {
	dir := t.TempDir()
	factory := &AgentFactory{}
	initial := factory.skillFingerprint(dir)
	path := filepath.Join(dir, "one.md")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	added := factory.skillFingerprint(dir)
	if added == initial {
		t.Fatal("adding a skill did not change fingerprint")
	}
	if err := os.WriteFile(path, []byte("different size"), 0o644); err != nil {
		t.Fatal(err)
	}
	modified := factory.skillFingerprint(dir)
	if modified == added {
		t.Fatal("modifying a skill did not change fingerprint")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if deleted := factory.skillFingerprint(dir); deleted != initial {
		t.Fatalf("deletion fingerprint = %q, want %q", deleted, initial)
	}
}
