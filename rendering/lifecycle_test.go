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

func TestBalancedCardTracksToolAndSubagentActivityInPlace(t *testing.T) {
	surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: "room"}, edit: true}
	run := NewRun(surface, config.DefaultNotificationConfig())
	defer run.Close()
	if err := run.Handle(event.Event{Kind: event.RunStarted}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		ev   event.Event
		want string
	}{
		{event.Event{Kind: event.ModelStarted, Name: "router"}, "Understanding your request"},
		{event.Event{Kind: event.ModelStarted}, "Thinking"},
		{event.Event{Kind: event.ToolStarted, Name: "file_read"}, "Running file_read"},
		{event.Event{Kind: event.ToolCompleted, Name: "file_read"}, "Finished file_read"},
		{event.Event{Kind: event.SubagentStarted}, "Delegating work"},
		{event.Event{Kind: event.SubagentCompleted}, "Sub-agent finished"},
	} {
		if err := run.Handle(step.ev); err != nil {
			t.Fatal(err)
		}
		if err := run.reporter.Flush(); err != nil {
			t.Fatal(err)
		}
		if len(surface.sent) != 1 || len(surface.updated) == 0 || !strings.Contains(surface.updated[len(surface.updated)-1], step.want) {
			t.Fatalf("card failed to track %s: sent=%#v updated=%#v", step.ev.Kind, surface.sent, surface.updated)
		}
	}
}

func TestExcludedSnapshotDataDoesNotLeakIntoLaterCardUpdates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		include []string
		exclude []string
	}{
		{name: "exclude plan", exclude: []string{"plan"}},
		{name: "lifecycle only", include: []string{"lifecycle"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: tc.name}, edit: true}
			policy := config.DefaultNotificationConfig()
			policy.Include, policy.Exclude = tc.include, tc.exclude
			run := NewRun(surface, policy)
			defer run.Close()
			_ = run.Handle(event.Event{Kind: event.RunStarted})
			_ = run.Handle(event.Event{Kind: event.PlanUpdated, Text: "SECRET_PLAN"})
			for _, ev := range []event.Event{
				{Kind: event.Waiting},
				{Kind: event.ModelStarted},
				{Kind: event.ToolStarted, Name: "shell"},
				{Kind: event.ToolCompleted, Name: "shell"},
			} {
				_ = run.Handle(ev)
				if err := run.reporter.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			if err := run.Complete("done", nil); err != nil {
				t.Fatal(err)
			}
			for _, msg := range append(append([]string{}, surface.sent...), surface.updated...) {
				if strings.Contains(msg, "SECRET_PLAN") {
					t.Fatalf("filtered data leaked in snapshot %q", msg)
				}
			}
		})
	}
}

func TestExcludedToolArgumentsDoNotLeakIntoLaterCardUpdates(t *testing.T) {
	surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: "room"}, edit: true}
	policy := config.DefaultNotificationConfig()
	policy.Exclude = []string{"tool"}
	run := NewRun(surface, policy)
	defer run.Close()
	_ = run.Handle(event.Event{Kind: event.RunStarted})
	_ = run.Handle(event.Event{Kind: event.ToolStarted, Name: "file_write", Arguments: `{"path":"SECRET_TOOL_PATH","content":"SECRET_TOOL_CONTENT"}`})
	_ = run.Handle(event.Event{Kind: event.ModelStarted})
	if err := run.reporter.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, msg := range append(append([]string{}, surface.sent...), surface.updated...) {
		if strings.Contains(msg, "SECRET_TOOL") {
			t.Fatalf("filtered tool arguments leaked: %q", msg)
		}
	}
}

func TestExcludedWaitingStillAsksWithoutPersistingWaitingState(t *testing.T) {
	surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: "room"}, edit: true}
	policy := config.DefaultNotificationConfig()
	policy.Exclude = []string{"waiting"}
	run := NewRun(surface, policy)
	defer run.Close()
	_ = run.Handle(event.Event{Kind: event.RunStarted})
	if err := run.Handle(event.Event{Kind: event.ConfirmationRequested, Text: "SECRET_CONFIRM"}); err != nil {
		t.Fatal(err)
	}
	if len(surface.asked) != 1 || !strings.Contains(surface.asked[0], "SECRET_CONFIRM") {
		t.Fatalf("mandatory confirmation not asked: %#v", surface.asked)
	}
	_ = run.Handle(event.Event{Kind: event.ModelStarted})
	if err := run.reporter.Flush(); err != nil {
		t.Fatal(err)
	}
	for _, msg := range append(append([]string{}, surface.sent...), surface.updated...) {
		if strings.Contains(msg, "Waiting for confirmation") {
			t.Fatalf("excluded waiting state leaked: %q", msg)
		}
	}
}

func TestExcludedLifecycleStillPreservesTerminalOutcome(t *testing.T) {
	for _, tc := range []struct {
		kind event.Kind
		want string
	}{
		{event.RunCancelled, "Stopped"}, {event.RunFailed, "Failed"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: string(tc.kind)}, edit: true}
			policy := config.DefaultNotificationConfig()
			policy.Exclude = []string{"lifecycle"}
			policy.Include = []string{"plan"}
			run := NewRun(surface, policy)
			_ = run.Handle(event.Event{Kind: event.RunStarted})
			_ = run.Handle(event.Event{Kind: event.PlanUpdated, Text: "working"})
			if err := run.reporter.Flush(); err != nil {
				t.Fatal(err)
			}
			_ = run.Handle(event.Event{Kind: tc.kind})
			if err := run.Complete("done", nil); err != nil {
				t.Fatal(err)
			}
			all := append(append([]string{}, surface.sent...), surface.updated...)
			found := false
			for _, msg := range all {
				found = found || strings.Contains(msg, tc.want)
			}
			if !found {
				t.Fatalf("terminal outcome missing: %#v", all)
			}
		})
	}
}

func TestStatusActivityHonorsExplicitFiltersAndNoneditableSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    bool
		preset  string
		include []string
		exclude []string
	}{
		{name: "tool exclusion", edit: true, exclude: []string{"tool*"}},
		{name: "lifecycle allowlist", edit: true, include: []string{"lifecycle"}},
		{name: "quiet", edit: true, preset: config.ProgressPresetQuiet},
		{name: "noneditable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: "room"}, edit: tc.edit}
			policy := config.DefaultNotificationConfig()
			if tc.preset != "" {
				policy.Preset = tc.preset
			}
			policy.Include, policy.Exclude = tc.include, tc.exclude
			run := NewRun(surface, policy)
			defer run.Close()
			_ = run.Handle(event.Event{Kind: event.RunStarted})
			before := len(surface.sent)
			_ = run.Handle(event.Event{Kind: event.ToolStarted, Name: "file_read"})
			if err := run.reporter.Flush(); err != nil {
				t.Fatal(err)
			}
			if len(surface.updated) != 0 || len(surface.sent) != before {
				t.Fatalf("suppressed activity was delivered: sent=%#v updated=%#v", surface.sent, surface.updated)
			}
		})
	}
}
