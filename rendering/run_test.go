package rendering

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ageage/channel"
	"ageage/config"
	"ageage/conversation"
	"ageage/event"
)

type fakeSurface struct {
	mu          sync.Mutex
	target      conversation.Target
	sent        []string
	updated     []string
	updateErr   error
	updateCalls int
	edit        bool
	asked       []string
	options     [][]string
	nextMessage int
}

func (s *fakeSurface) Target() conversation.Target        { return s.target }
func (s *fakeSurface) Capabilities() channel.Capabilities { return channel.Capabilities{Edit: s.edit} }
func (s *fakeSurface) Send(text string) (conversation.Reference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, text)
	s.nextMessage++
	return conversation.Reference{Target: s.target, MessageID: s.target.ChannelID + "-" + itoa(s.nextMessage)}, nil
}
func (s *fakeSurface) Reply(text string) (conversation.Reference, error) { return s.Send(text) }
func (s *fakeSurface) Update(_ conversation.Reference, text string) (conversation.Reference, error) {
	s.mu.Lock()
	s.updateCalls++
	if !s.edit || s.updateErr != nil {
		s.mu.Unlock()
		return s.Send(text)
	}
	s.updated = append(s.updated, text)
	s.mu.Unlock()
	return conversation.Reference{Target: s.target, MessageID: s.target.ChannelID + "-1"}, nil
}
func (*fakeSurface) OpenThread(string, string) (channel.Surface, conversation.Reference, error) {
	return nil, conversation.Reference{}, channel.ErrUnsupported
}
func (*fakeSurface) React(conversation.Reference, string) (conversation.Reference, error) {
	return conversation.Reference{}, channel.ErrUnsupported
}
func (*fakeSurface) Unreact(conversation.Reference) error { return channel.ErrUnsupported }
func (*fakeSurface) Typing(bool) error                    { return nil }
func (*fakeSurface) Read() error                          { return nil }
func (s *fakeSurface) Ask(question string, options []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, question)
	s.options = append(s.options, append([]string(nil), options...))
	return nil
}
func (*fakeSurface) Link(conversation.Reference) string { return "" }

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

func TestQuietRunAlwaysDeliversFinalResult(t *testing.T) {
	surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: "room"}, edit: true}
	policy := config.DefaultNotificationConfig()
	policy.Preset = config.ProgressPresetQuiet
	run := NewRun(surface, policy)
	if err := run.Handle(event.Event{Kind: event.RunStarted}); err != nil {
		t.Fatal(err)
	}
	if len(surface.sent) != 0 {
		t.Fatalf("quiet policy sent progress: %#v", surface.sent)
	}
	if err := run.Complete("final answer", nil); err != nil {
		t.Fatal(err)
	}
	if len(surface.sent) != 1 || surface.sent[0] != "final answer" {
		t.Fatalf("final messages = %#v", surface.sent)
	}
	if len(surface.updated) != 0 {
		t.Fatalf("quiet run unexpectedly created a progress card: %#v", surface.updated)
	}
}

func TestQuietConfirmationIsAlwaysAskedWithoutDuplicateNotification(t *testing.T) {
	surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: "room"}}
	policy := config.DefaultNotificationConfig()
	policy.Preset = config.ProgressPresetQuiet
	run := NewRun(surface, policy)
	if err := run.Handle(event.Event{Kind: event.ConfirmationRequested, Text: "write /tmp/a", Options: []string{"y", "n", "a"}}); err != nil {
		t.Fatal(err)
	}
	if len(surface.asked) != 1 || !strings.Contains(surface.asked[0], "write /tmp/a") || !strings.Contains(surface.asked[0], "always allow") {
		t.Fatalf("confirmation prompt = %#v", surface.asked)
	}
	if len(surface.options) != 1 || strings.Join(surface.options[0], ",") != "y,n,a" {
		t.Fatalf("confirmation options = %#v", surface.options)
	}
	if len(surface.sent) != 0 {
		t.Fatalf("confirmation was duplicated as a normal quiet notification: %#v", surface.sent)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBalancedRunCreatesEditableStatusCardAndCompletesInPlace(t *testing.T) {
	surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: "room"}, edit: true}
	policy := config.DefaultNotificationConfig()
	run := NewRun(surface, policy)
	if err := run.Handle(event.Event{Kind: event.RunStarted}); err != nil {
		t.Fatal(err)
	}
	if len(surface.sent) != 1 || !strings.Contains(surface.sent[0], "Working") {
		t.Fatalf("balanced run did not start an editable status card: %#v", surface.sent)
	}
	if err := run.Handle(event.Event{Kind: event.RunCompleted}); err != nil {
		t.Fatal(err)
	}
	if len(surface.updated) != 0 {
		t.Fatalf("RunCompleted emitted an empty terminal update before Complete: %#v", surface.updated)
	}
	if err := run.Handle(event.Event{Kind: event.Notification, Text: "lifecycle notice"}); err != nil {
		t.Fatal(err)
	}
	if len(surface.sent) != 1 {
		t.Fatalf("balanced status-card exception opened notifications: %#v", surface.sent)
	}
	if err := run.Complete("final answer", nil); err != nil {
		t.Fatal(err)
	}
	if len(surface.sent) != 1 || len(surface.updated) != 1 || !strings.Contains(surface.updated[0], "Completed") || !strings.HasSuffix(surface.updated[0], "final answer") {
		t.Fatalf("final answer was not delivered in the same card: sent=%#v updated=%#v", surface.sent, surface.updated)
	}
}

func TestBalancedStatusCardHonorsExplicitLifecyclePolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		include []string
		exclude []string
	}{
		{name: "other include", include: []string{config.ProgressPlan}},
		{name: "lifecycle excluded", exclude: []string{config.ProgressLifecycle}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: tc.name}, edit: true}
			policy := config.DefaultNotificationConfig()
			policy.Include = tc.include
			policy.Exclude = tc.exclude
			run := NewRun(surface, policy)
			if err := run.Handle(event.Event{Kind: event.RunStarted}); err != nil {
				t.Fatal(err)
			}
			if len(surface.sent) != 0 {
				t.Fatalf("explicit lifecycle policy was overridden: %#v", surface.sent)
			}
			if err := run.Complete("final", nil); err != nil {
				t.Fatal(err)
			}
			if len(surface.sent) != 1 || surface.sent[0] != "final" {
				t.Fatalf("final answer missing after suppressed status card: %#v", surface.sent)
			}
		})
	}
}

func TestRunCoalescesUpdatesClosesTimerAndDeliversResultOnce(t *testing.T) {
	surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: "room"}, edit: true}
	policy := config.DefaultNotificationConfig()
	policy.Preset = config.ProgressPresetVerbose
	policy.ThrottleMS = 80
	run := NewRun(surface, policy)
	if err := run.Handle(event.Event{Kind: event.RunStarted}); err != nil {
		t.Fatal(err)
	}
	if err := run.Handle(event.Event{Kind: event.PlanUpdated, Text: "Step 1\nStep 2"}); err != nil {
		t.Fatal(err)
	}
	if err := run.Handle(event.Event{Kind: event.ToolStarted, Name: "read_file"}); err != nil {
		t.Fatal(err)
	}
	if run.MessageID() == "" {
		t.Fatal("progress card did not get a message id")
	}
	if err := run.Complete("final output", nil); err != nil {
		t.Fatal(err)
	}
	if len(surface.sent) != 1 {
		t.Fatalf("sends = %#v", surface.sent)
	}
	if len(surface.updated) < 2 {
		t.Fatalf("expected coalesced status and terminal update: %#v", surface.updated)
	}
	last := surface.updated[len(surface.updated)-1]
	if !strings.Contains(last, "Completed") || strings.Contains(last, "Step 1 Step 2") || strings.Contains(last, "Last activity:") || !strings.Contains(last, "final output") {
		t.Fatalf("terminal card = %q", last)
	}
	updates := len(surface.updated)
	time.Sleep(100 * time.Millisecond)
	if len(surface.updated) != updates {
		t.Fatalf("timer updated closed run: before=%d after=%d", updates, len(surface.updated))
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteFallsBackToNewCardWhenEditFails(t *testing.T) {
	surface := &fakeSurface{target: conversation.Target{ChannelType: "discord", ChannelID: "thread"}, edit: true}
	policy := config.DefaultNotificationConfig()
	policy.Preset = config.ProgressPresetVerbose
	run := NewRun(surface, policy)
	if err := run.Handle(event.Event{Kind: event.RunStarted}); err != nil {
		t.Fatal(err)
	}
	surface.updateErr = errors.New("expired message")
	longResult := strings.Repeat("long final response ", 5000)
	if err := run.Complete(longResult, nil); err != nil {
		t.Fatal(err)
	}
	if len(surface.sent) != 2 || !strings.Contains(surface.sent[1], "Completed") || !strings.HasSuffix(surface.sent[1], longResult) {
		t.Fatalf("fallback should carry complete result once: sends=%d final length=%d", len(surface.sent), len(surface.sent[len(surface.sent)-1]))
	}
	if run.MessageID() != "thread-2" {
		t.Fatalf("MessageID after fallback = %q", run.MessageID())
	}
}

func TestConcurrentRunsKeepCardsIsolated(t *testing.T) {
	policy := config.DefaultNotificationConfig()
	policy.Preset = config.ProgressPresetVerbose
	policy.ThrottleMS = 0
	surfaceA := &fakeSurface{target: conversation.Target{ChannelType: "telegram", ChannelID: "a"}, edit: true}
	surfaceB := &fakeSurface{target: conversation.Target{ChannelType: "telegram", ChannelID: "b"}, edit: true}
	runA, runB := NewRun(surfaceA, policy), NewRun(surfaceB, policy)
	if err := runA.Handle(event.Event{Kind: event.PlanUpdated, Text: "plan A"}); err != nil {
		t.Fatal(err)
	}
	if err := runB.Handle(event.Event{Kind: event.PlanUpdated, Text: "plan B"}); err != nil {
		t.Fatal(err)
	}
	if runA.MessageID() != "a-1" || runB.MessageID() != "b-1" {
		t.Fatalf("isolated ids = %q, %q", runA.MessageID(), runB.MessageID())
	}
	if err := runA.Complete("answer A", nil); err != nil {
		t.Fatal(err)
	}
	if err := runB.Complete("answer B", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(surfaceA.updated, " "), "plan B") || strings.Contains(strings.Join(surfaceB.updated, " "), "plan A") {
		t.Fatalf("progress cards crossed runs: A=%#v B=%#v", surfaceA.updated, surfaceB.updated)
	}
}

func TestNotificationPolicyAndWaitingCardRendering(t *testing.T) {
	surface := &fakeSurface{target: conversation.Target{ChannelType: "matrix", ChannelID: "room"}, edit: true}
	policy := config.DefaultNotificationConfig()
	policy.Preset = config.ProgressPresetVerbose
	policy.ThrottleMS = 0
	run := NewRun(surface, policy)
	if err := run.Handle(event.Event{Kind: event.Waiting, Name: "ask_user", Text: "private answer detail"}); err != nil {
		t.Fatal(err)
	}
	if len(surface.sent) != 1 || !strings.Contains(surface.sent[0], "Waiting") || strings.Contains(surface.sent[0], "private answer detail") {
		t.Fatalf("waiting card should show status only: %#v", surface.sent)
	}
	if err := run.Handle(event.Event{Kind: event.Notification, Text: "notice"}); err != nil {
		t.Fatal(err)
	}
	if len(surface.sent) != 2 || surface.sent[1] != "notice" {
		t.Fatalf("notification delivery = %#v", surface.sent)
	}
	_ = run.Close()
}
