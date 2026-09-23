package main

import (
	"testing"

	"ageage/skills"
)

func TestParseIMCommandUsesChannelPrefixAndExactTokens(t *testing.T) {
	tests := []struct {
		name    string
		channel string
		input   string
		prefix  string
		nameOut string
		args    string
		found   bool
		escaped bool
	}{
		{name: "matrix command", channel: "matrix", input: "!session new work", prefix: "!", nameOut: "session", args: "new work", found: true},
		{name: "matrix slash is text", channel: "matrix", input: "/help", prefix: "!"},
		{name: "telegram slash command", channel: "telegram", input: "/help", prefix: "/", nameOut: "help", found: true},
		{name: "discord keeps slash", channel: "discord", input: "/retry revise", prefix: "/", nameOut: "retry", args: "revise", found: true},
		{name: "other prefix is text", channel: "telegram", input: "!help", prefix: "/"},
		{name: "exact token", channel: "matrix", input: "!sessionist", prefix: "!", nameOut: "sessionist", found: true},
		{name: "matrix escaped", channel: "matrix", input: "!!help", prefix: "!", escaped: true},
		{name: "slash escaped", channel: "telegram", input: "//help", prefix: "/", escaped: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseIMCommand(tt.channel, tt.input)
			if got.Prefix != tt.prefix || got.Name != tt.nameOut || got.Args != tt.args || got.Found != tt.found || got.Escaped != tt.escaped {
				t.Fatalf("parseIMCommand(%q, %q) = %#v", tt.channel, tt.input, got)
			}
		})
	}
}

func TestIMCommandNamesDoNotUsePrefixMatching(t *testing.T) {
	if parseIMCommand("matrix", "!sessionist").is("session") {
		t.Fatal("sessionist must not match session")
	}
	if parseIMCommand("matrix", "!stop now").is("stop") == false {
		t.Fatal("command token should remain available for exact argument checks")
	}
}

func TestRecognizedIMCommandForGroupGate(t *testing.T) {
	loaded := []skills.Skill{{Name: "Deploy App"}}
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{name: "matrix built in", text: "!help", want: true},
		{name: "matrix dynamic skill", text: "!deploy-app staging", want: true},
		{name: "unknown prefixed text", text: "!unknown", want: false},
		{name: "escaped built in remains literal", text: "!!help", want: false},
		{name: "matrix slash is text", text: "/help", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := isRecognizedIMCommand(parseIMCommand("matrix", tc.text), loaded)
			if got != tc.want {
				t.Fatalf("recognized(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
