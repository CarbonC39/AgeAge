package main

import (
	"strings"

	"ageage/agent"
	"ageage/skills"
)

// imCommand is the channel-aware command token extracted from an IM message.
// CLI command parsing remains separate and continues to use slash commands.
type imCommand struct {
	Prefix  string
	Name    string
	Args    string
	Found   bool
	Escaped bool // true when the input used the literal escape (e.g. !!help)
}

// imCommandPrefix returns the command prefix for an IM connector. Matrix uses
// ! so Matrix-native slash commands remain ordinary user text; existing
// Telegram and Discord integrations retain their documented slash syntax.
func imCommandPrefix(channelType string) string {
	if strings.EqualFold(channelType, "matrix") {
		return "!"
	}
	return "/"
}

// parseIMCommand parses exactly the first token after the channel's command
// prefix. It deliberately does not classify the command name, so dynamic
// skill names can be handled by Agent's skill registry. Callers should use
// Found and compare Name exactly rather than using HasPrefix.
//
// A doubled prefix escapes a command: !!help becomes the literal !help input
// for Matrix (and //help becomes /help for the other IM connectors).
func parseIMCommand(channelType, input string) imCommand {
	prefix := imCommandPrefix(channelType)
	result := imCommand{Prefix: prefix}
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, prefix) {
		return result
	}
	if strings.HasPrefix(input, prefix+prefix) {
		result.Escaped = true
		return result
	}

	body := strings.TrimSpace(input[len(prefix):])
	if body == "" {
		return result
	}
	name := body
	args := ""
	if idx := strings.IndexAny(body, " \t\r\n"); idx >= 0 {
		name = body[:idx]
		args = strings.TrimSpace(body[idx:])
	}
	result.Name = strings.ToLower(name)
	result.Args = args
	result.Found = result.Name != ""
	return result
}

func (c imCommand) is(name string) bool {
	return c.Found && !c.Escaped && c.Name == name
}

// isRecognizedIMCommand reports whether a prefixed message is an actual
// built-in command or a command for one of the currently loaded skills. This
// is used by the group-chat gate: known commands are directed at the bot even
// without an @mention, while unknown prefixed text remains ordinary text and
// must still include a mention.
func isRecognizedIMCommand(cmd imCommand, loaded []skills.Skill) bool {
	if !cmd.Found || cmd.Escaped {
		return false
	}
	switch cmd.Name {
	case "stop", "session", "cred", "sessions", "build", "clear", "summarize", "undo", "help", "retry":
		return true
	}
	commandName := agent.NormalizeSkillName(cmd.Name)
	for i := range loaded {
		if agent.NormalizeSkillName(loaded[i].Name) == commandName {
			return true
		}
	}
	return false
}
