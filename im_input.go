package main

import (
	"context"
	"strings"

	"ageage/agent"
	"ageage/channel"
	"ageage/config"
	"ageage/llm"
)

// normalizeIMInput is scheduling independent: the same result can be submitted
// to Agent.RunInput now or an active run's inbox in the future.
func normalizeIMInput(ctx context.Context, msg channel.IncomingMessage, cfg *config.Config, tmp *agent.TmpManager) agent.UserInput {
	text := msg.Text
	if msg.IsGroupChat && msg.SenderID != "" {
		text = "[" + senderDisplayName(msg.ChannelType, msg.SenderID, msg.SenderName) + "]: " + text
	}
	return agent.NormalizeUserInput(ctx, text, msg.Attachments, cfg, tmp)
}

func imInputWarnings(input agent.UserInput) string {
	if len(input.Warnings) == 0 {
		return ""
	}
	return "⚠️ " + strings.Join(input.Warnings, "\n⚠️ ") + "\n\n"
}

// retryUserInput preserves attachments while making any modifier visible in
// the structured content array, which takes precedence over plain Content.
func retryUserInput(message llm.Message, modifier string) agent.UserInput {
	input := agent.UserInput{Text: message.TextContent(), Parts: append([]llm.ContentPart(nil), message.Parts...)}
	if modifier != "" {
		input.Text += "\n\n" + modifier
		if len(input.Parts) > 0 {
			input.Parts = append(input.Parts, llm.ContentPart{Type: "text", Text: modifier})
		}
	}
	return input
}
