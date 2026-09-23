package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"ageage/agent"
	"ageage/channel"
	"ageage/config"
	"ageage/tools"
)

// cronDeliveryTarget prefers the structured owner location used by
// Agent-created entries. Delivery remains a compatibility fallback for
// administrative and legacy entries that have no structured owner.
func cronDeliveryTarget(entry tools.CronEntry) (channelType, channelID, threadID string, ok bool) {
	if entry.Owner.ChannelType != "" || entry.Owner.ChannelID != "" || entry.Owner.ThreadID != "" {
		channelType = strings.ToLower(strings.TrimSpace(entry.Owner.ChannelType))
		channelID = strings.TrimSpace(entry.Owner.ChannelID)
		threadID = strings.TrimSpace(entry.Owner.ThreadID)
		return channelType, channelID, threadID, channelType != "" && channelID != ""
	}
	parts := strings.SplitN(entry.Delivery, ":", 2)
	if len(parts) != 2 {
		return "", "", "", false
	}
	channelType = strings.ToLower(strings.TrimSpace(parts[0]))
	channelID = strings.TrimSpace(parts[1])
	if idx := strings.LastIndex(channelID, ":t:"); idx >= 0 {
		threadID = channelID[idx+3:]
		channelID = channelID[:idx]
	}
	return channelType, channelID, threadID, channelType != "" && channelID != ""
}

// newIMProgressReporter creates one run-scoped progress sink. The send target
// is fixed here so delayed/coalesced updates cannot escape the originating
// room or thread.
func newIMProgressReporter(cfg config.NotificationConfig, ch channel.Channel, msg channel.IncomingMessage) *channel.FilteredProgressReporter {
	policy := cfg.EffectiveForChannel(msg.ChannelType)
	if ch == nil {
		return channel.NewFilteredProgressReporter(channel.ProgressReporterOptions{
			Allowed: func(category channel.ProgressCategory) bool { return policy.Allows(string(category)) },
		})
	}

	var send func(string) (string, error)
	var edit func(string, string) error
	if editable, ok := ch.(channel.Editable); ok {
		editChannelID := msg.ChannelID
		if msg.ChannelType == "discord" && msg.ThreadID != "" {
			// Discord threads are channels in their own right.
			editChannelID = msg.ThreadID
		}
		edit = func(messageID, text string) error {
			return editable.EditMessage(editChannelID, messageID, text)
		}

		if msg.ThreadID != "" {
			if threadEditable, ok := ch.(channel.ThreadEditable); ok {
				send = func(text string) (string, error) {
					return threadEditable.SendMessageInThread(msg.ChannelID, msg.ThreadID, msg.ReplyTo, text)
				}
			}
		}
		if send == nil && msg.ThreadID == "" {
			send = func(text string) (string, error) {
				return editable.SendMessage(msg.ChannelID, text)
			}
		}
	}
	if send == nil && msg.Respond != nil {
		send = func(text string) (string, error) { return "", msg.Respond(text) }
		edit = nil
	}
	if send == nil {
		send = func(text string) (string, error) { return "", ch.Send(msg.ChannelID, text) }
		edit = nil
	}

	return channel.NewFilteredProgressReporter(channel.ProgressReporterOptions{
		Allowed:  func(category channel.ProgressCategory) bool { return policy.Allows(string(category)) },
		Throttle: time.Duration(policy.ThrottleMS) * time.Millisecond,
		Send:     send,
		Edit:     edit,
	})
}

// installIMProgressCallbacks maps the Agent's existing callback surface onto
// stable progress categories. It returns a restoration function because Agent
// instances persist across turns while reporters are deliberately per-run.
func installIMProgressCallbacks(ag *agent.Agent, reporter channel.ProgressReporter, askUser func(string, []string)) func() {
	if ag == nil || reporter == nil {
		return func() {}
	}
	saved := ag.Callbacks
	report := func(category channel.ProgressCategory, name, message string) {
		if err := reporter.ReportProgress(channel.ProgressEvent{Category: category, Name: name, Message: message}); err != nil {
			fmt.Printf("[progress] report error: %s\n", err)
		}
	}

	ag.Callbacks.Notify = func(message string) {
		report(channel.ProgressLifecycle, "notification", message)
	}
	ag.Callbacks.AskUser = func(question string, options []string) {
		report(channel.ProgressWaiting, "ask_user", "Waiting for your input…")
		if askUser != nil {
			askUser(question, options)
		}
	}
	ag.Callbacks.TodoSend = func(text string) string {
		report(channel.ProgressPlan, "plan", text)
		if current, ok := reporter.(interface{ MessageID() string }); ok {
			return current.MessageID()
		}
		return ""
	}
	ag.Callbacks.TodoEdit = func(_ string, text string) error {
		return reporter.ReportProgress(channel.ProgressEvent{Category: channel.ProgressPlan, Name: "plan", Message: text})
	}
	ag.Callbacks.ToolStart = func(name, args string) {
		category, message := formatIMToolProgress(name, args, false)
		report(category, name, message)
	}
	ag.Callbacks.ToolEnd = func(name string) {
		category, message := formatIMToolProgress(name, "", true)
		report(category, name, message)
	}

	return func() { ag.Callbacks = saved }
}

func formatIMToolProgress(name, args string, finished bool) (channel.ProgressCategory, string) {
	label := strings.TrimSpace(name)
	if label == "" {
		label = "tool"
	}
	if name == "delegate" || name == "escalate" {
		if finished {
			return channel.ProgressSubagent, "Sub-agent finished."
		}
		return channel.ProgressSubagent, "Delegating work to a sub-agent…"
	}
	if finished {
		return channel.ProgressTool, fmt.Sprintf("Finished `%s`.", label)
	}

	// File updates are the one selected tool activity where a compact diff is
	// more useful than a generic tool name. They remain hidden in balanced mode.
	switch name {
	case "file_write":
		var p struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(args), &p) == nil && p.Path != "" {
			return channel.ProgressTool, imDiffWrite(p.Path, p.Content)
		}
	case "file_edit":
		var p struct {
			Path    string `json:"path"`
			Search  string `json:"search"`
			Replace string `json:"replace"`
		}
		if json.Unmarshal([]byte(args), &p) == nil && p.Path != "" {
			return channel.ProgressTool, imDiffEdit(p.Path, p.Search, p.Replace)
		}
	}
	return channel.ProgressTool, fmt.Sprintf("Running `%s`…", label)
}

// imRunFeedback owns the ephemeral typing indicator and pending reaction for
// one handler invocation. Close is idempotent so normal and deferred cleanup
// can safely share it.
type imRunFeedback struct {
	typing     channel.TypingIndicator
	reactor    channel.Reactor
	roomID     string
	typingID   string
	messageID  string
	reactionID string
	closed     bool
}

func beginIMRunFeedback(ch channel.Channel, msg channel.IncomingMessage) *imRunFeedback {
	f := &imRunFeedback{roomID: msg.ChannelID, typingID: msg.ChannelID, messageID: msg.ReplyTo}
	if msg.ChannelType == "discord" && msg.ThreadID != "" {
		f.roomID = msg.ThreadID
		f.typingID = msg.ThreadID
	}
	if typing, ok := ch.(channel.TypingIndicator); ok {
		f.typing = typing
		_ = typing.SendTyping(f.typingID, true)
	}
	if reactor, ok := ch.(channel.Reactor); ok {
		f.reactor = reactor
		f.reactionID, _ = reactor.React(f.roomID, msg.ReplyTo, "⏳")
	}
	return f
}

func (f *imRunFeedback) Close(failed bool) {
	if f == nil || f.closed {
		return
	}
	f.closed = true
	if f.typing != nil {
		_ = f.typing.SendTyping(f.typingID, false)
	}
	if f.reactor != nil && f.reactionID != "" {
		_ = f.reactor.Unreact(f.roomID, f.reactionID)
		if failed {
			_, _ = f.reactor.React(f.roomID, f.messageID, "❌")
		}
	}
}
