package rendering

import (
	"strings"
	"testing"

	"ageage/config"
	"ageage/conversation"
	"ageage/event"
)

func TestBalancedCardShowsWaitingThenResumes(t *testing.T) {
	surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: "room"}, edit: true}
	run := NewRun(surface, config.DefaultNotificationConfig())
	defer run.Close()
	_ = run.Handle(event.Event{Kind: event.RunStarted})
	_ = run.Handle(event.Event{Kind: event.Waiting, Text: "send a screenshot"})
	if err := run.reporter.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(surface.updated) == 0 || !strings.Contains(surface.updated[len(surface.updated)-1], "Waiting") {
		t.Fatalf("card did not show waiting: %#v", surface.updated)
	}
	_ = run.Handle(event.Event{Kind: event.ToolCompleted, Name: "ask_user"})
	if err := run.reporter.Flush(); err != nil {
		t.Fatal(err)
	}
	last := surface.updated[len(surface.updated)-1]
	if !strings.Contains(last, "Working") || !strings.Contains(last, "Continuing with your answer") {
		t.Fatalf("card did not resume: %q", last)
	}
}

func TestBalancedWaitingCardHonorsWildcardExclusion(t *testing.T) {
	surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: "room"}, edit: true}
	policy := config.DefaultNotificationConfig()
	policy.Exclude = []string{"wait*"}
	run := NewRun(surface, policy)
	defer run.Close()
	_ = run.Handle(event.Event{Kind: event.RunStarted})
	_ = run.Handle(event.Event{Kind: event.Waiting})
	if err := run.reporter.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(surface.updated) != 0 {
		t.Fatalf("excluded waiting was shown: %#v", surface.updated)
	}
}

func TestCancelledRunCompletionKeepsStoppedCard(t *testing.T) {
	surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: "room"}, edit: true}
	run := NewRun(surface, config.DefaultNotificationConfig())
	_ = run.Handle(event.Event{Kind: event.RunStarted})
	_ = run.Handle(event.Event{Kind: event.RunCancelled})
	if err := run.Complete("Task stopped by user", nil); err != nil {
		t.Fatal(err)
	}
	if len(surface.updated) == 0 {
		t.Fatal("stopped card was not updated")
	}
	last := surface.updated[len(surface.updated)-1]
	if !strings.Contains(last, "Stopped") || strings.Contains(last, "Completed") {
		t.Fatalf("cancelled run mislabeled: %q", last)
	}
}
