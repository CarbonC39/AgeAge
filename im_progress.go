package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"ageage/agent"
	"ageage/channel"
	"ageage/config"
	"ageage/conversation"
	"ageage/event"
	"ageage/rendering"
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

// newIMProgressReporter retains the filtered sink API for integrations which
// submit pre-rendered messages. The surface owns all native routing decisions.
func newIMProgressReporter(cfg config.NotificationConfig, ch channel.Channel, msg channel.IncomingMessage) *channel.FilteredProgressReporter {
	policy := cfg.EffectiveForChannel(msg.Target().ChannelType)
	var send func(string) (string, error)
	var edit func(string, string) error
	if ch != nil {
		surface := channel.NewSurface(ch, msg.Target())
		var ref conversation.Reference
		send = func(text string) (string, error) {
			var err error
			ref, err = surface.Send(text)
			return ref.MessageID, err
		}
		if surface.Capabilities().Edit {
			edit = func(_ string, text string) error { var err error; ref, err = surface.Update(ref, text); return err }
		}
	}
	return channel.NewFilteredProgressReporter(channel.ProgressReporterOptions{Allowed: func(category channel.ProgressCategory) bool { return policy.Allows(string(category)) }, Throttle: time.Duration(policy.ThrottleMS) * time.Millisecond, Send: send, Edit: edit})
}

// installIMRunCallbacks keeps the runtime event stream independent of transport
// formatting. AskUser remains an interaction, sent through the same surface.
func installIMRunCallbacks(ag *agent.Agent, run *rendering.Run, surface channel.Surface) func() {
	saved := ag.Callbacks
	ag.Callbacks = agent.AgentCallbacks{
		BackgroundEvent: saved.BackgroundEvent,
		Event: func(ev event.Event) {
			if err := run.Handle(ev); err != nil {
				fmt.Printf("[progress] %s\n", err)
			}
		},
		AskUser: func(question string, options []string) {
			if err := surface.Ask(question, options); err != nil {
				fmt.Printf("[ask_user] %s\n", err)
			}
		},
	}
	return func() { ag.Callbacks = saved }
}

// imRunFeedback owns ephemeral affordances, never native room or thread IDs.
type imRunFeedback struct {
	mu       sync.Mutex
	surface  channel.Surface
	message  conversation.Reference
	reaction conversation.Reference
	closed   bool
}

func beginIMRunFeedback(ch channel.Channel, msg channel.IncomingMessage) *imRunFeedback {
	return beginSurfaceFeedback(channel.NewSurface(ch, msg.Target()))
}
func beginSurfaceFeedback(surface channel.Surface) *imRunFeedback {
	target := surface.Target()
	f := &imRunFeedback{surface: surface, message: conversation.Reference{Target: target, MessageID: target.ReplyTo}}
	_ = surface.Typing(true)
	if f.message.MessageID != "" {
		f.reaction, _ = surface.React(f.message, "⏳")
	}
	return f
}
func (f *imRunFeedback) Close(failed bool) {
	if failed {
		f.finish("❌")
	} else {
		f.finish("")
	}
}
func (f *imRunFeedback) Stop() { f.finish("🛑") }
func (f *imRunFeedback) finish(emoji string) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	_ = f.surface.Typing(false)
	if f.reaction.MessageID != "" {
		_ = f.surface.Unreact(f.reaction)
	}
	if emoji != "" && f.message.MessageID != "" {
		_, _ = f.surface.React(f.message, emoji)
	}
}
