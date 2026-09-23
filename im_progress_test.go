package main

import (
	"testing"

	"ageage/channel"
	"ageage/config"
	"ageage/tools"
)

type progressTestChannel struct {
	name       string
	threadRoom string
	threadRoot string
	threadLast string
	sent       []string
	editedIn   string
	edits      []string
	typingIn   []string
	typing     []bool
	reactions  []string
	unreacted  []string
}

func (c *progressTestChannel) Name() string                       { return c.name }
func (c *progressTestChannel) Start(channel.MessageHandler) error { return nil }
func (c *progressTestChannel) Stop() error                        { return nil }
func (c *progressTestChannel) Send(_ string, text string) error {
	c.sent = append(c.sent, text)
	return nil
}
func (c *progressTestChannel) Reply(_, _, text string) error {
	c.sent = append(c.sent, text)
	return nil
}
func (c *progressTestChannel) SendMessage(_ string, text string) (string, error) {
	c.sent = append(c.sent, text)
	return "message-1", nil
}
func (c *progressTestChannel) SendMessageInThread(room, root, latest, text string) (string, error) {
	c.threadRoom, c.threadRoot, c.threadLast = room, root, latest
	c.sent = append(c.sent, text)
	return "message-1", nil
}
func (c *progressTestChannel) EditMessage(room, _ string, text string) error {
	c.editedIn = room
	c.edits = append(c.edits, text)
	return nil
}
func (c *progressTestChannel) SendTyping(channelID string, typing bool) error {
	c.typingIn = append(c.typingIn, channelID)
	c.typing = append(c.typing, typing)
	return nil
}
func (c *progressTestChannel) React(_, _ string, emoji string) (string, error) {
	c.reactions = append(c.reactions, emoji)
	return "reaction-1", nil
}
func (c *progressTestChannel) Unreact(_, id string) error {
	c.unreacted = append(c.unreacted, id)
	return nil
}

func TestIMProgressReporterKeepsMatrixThreadAndEdits(t *testing.T) {
	ch := &progressTestChannel{name: "matrix"}
	cfg := config.DefaultNotificationConfig()
	cfg.ThrottleMS = 1
	msg := channel.IncomingMessage{
		ChannelType: "matrix",
		ChannelID:   "!room:example",
		ThreadID:    "$root",
		ReplyTo:     "$latest",
	}
	reporter := newIMProgressReporter(cfg, ch, msg)
	if err := reporter.ReportProgress(channel.ProgressEvent{Category: channel.ProgressLifecycle, Message: "Working"}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.ReportProgress(channel.ProgressEvent{Category: channel.ProgressPlan, Message: "Plan"}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Flush(); err != nil {
		t.Fatal(err)
	}

	if ch.threadRoom != msg.ChannelID || ch.threadRoot != msg.ThreadID || ch.threadLast != msg.ReplyTo {
		t.Fatalf("thread target = (%q, %q, %q)", ch.threadRoom, ch.threadRoot, ch.threadLast)
	}
	if len(ch.sent) != 1 || len(ch.edits) != 1 || ch.editedIn != msg.ChannelID {
		t.Fatalf("sent=%#v edits=%#v editedIn=%q", ch.sent, ch.edits, ch.editedIn)
	}
}

func TestIMRunFeedbackCleanupIsIdempotent(t *testing.T) {
	ch := &progressTestChannel{name: "matrix"}
	feedback := beginIMRunFeedback(ch, channel.IncomingMessage{ChannelID: "room", ThreadID: "thread", ReplyTo: "event"})
	feedback.Close(true)
	feedback.Close(true)

	if len(ch.typing) != 2 || !ch.typing[0] || ch.typing[1] {
		t.Fatalf("typing transitions = %#v", ch.typing)
	}
	if len(ch.typingIn) != 2 || ch.typingIn[0] != "room" || ch.typingIn[1] != "room" {
		t.Fatalf("Matrix thread typing targets = %#v", ch.typingIn)
	}
	if len(ch.unreacted) != 1 || len(ch.reactions) != 2 || ch.reactions[1] != "❌" {
		t.Fatalf("reactions=%#v unreacted=%#v", ch.reactions, ch.unreacted)
	}
}

func TestCronDeliveryTargetPrefersStructuredOwner(t *testing.T) {
	entry := tools.CronEntry{
		Delivery: "matrix:!wrong:example:t:$wrong",
		Owner: tools.InteractionScope{
			ChannelType: "Matrix",
			ChannelID:   "!room:t:example.org",
			ThreadID:    "$thread:t:event",
		},
	}
	channelType, channelID, threadID, ok := cronDeliveryTarget(entry)
	if !ok || channelType != "matrix" || channelID != entry.Owner.ChannelID || threadID != entry.Owner.ThreadID {
		t.Fatalf("target = (%q, %q, %q, %v)", channelType, channelID, threadID, ok)
	}
}

func TestCronDeliveryTargetSupportsLegacyFlatTarget(t *testing.T) {
	entry := tools.CronEntry{Delivery: "telegram:-100123:t:456"}
	channelType, channelID, threadID, ok := cronDeliveryTarget(entry)
	if !ok || channelType != "telegram" || channelID != "-100123" || threadID != "456" {
		t.Fatalf("target = (%q, %q, %q, %v)", channelType, channelID, threadID, ok)
	}
}
