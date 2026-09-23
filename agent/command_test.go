package agent

import (
	"testing"

	"ageage/skills"
)

func TestAgentSkillCommandPrefixAndLiteralEscape(t *testing.T) {
	ag := &Agent{skills: []skills.Skill{{Name: "deploy"}}}
	ag.SetCommandPrefix("!")

	matched, remaining := ag.parseSkillCommand("!deploy\tstaging")
	if matched == nil || matched.Name != "deploy" || remaining != "staging" {
		t.Fatalf("matrix skill command = skill %#v, remaining %q", matched, remaining)
	}
	matched, remaining = ag.parseSkillCommand("/deploy staging")
	if matched != nil || remaining != "/deploy staging" {
		t.Fatalf("slash should remain literal for ! agent: skill %#v, remaining %q", matched, remaining)
	}
	matched, remaining = ag.parseSkillCommand("!!deploy staging")
	if matched != nil || remaining != "!deploy staging" {
		t.Fatalf("escaped skill command = skill %#v, remaining %q", matched, remaining)
	}
}
