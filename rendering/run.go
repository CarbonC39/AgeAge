// Package rendering adapts semantic runtime events to a conversation surface.
package rendering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ageage/channel"
	"ageage/config"
	"ageage/conversation"
	"ageage/event"
)

// State is the compact, replaceable progress card shown during a run.
type State struct {
	Phase    string
	Plan     string
	Activity string
}

// Render formats one state snapshot. It deliberately describes the latest
// state rather than appending an event log, so edits remain compact.
func Render(state State) string {
	phase := state.Phase
	if phase == "" {
		phase = "Working"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Agent — %s**", phase)
	if state.Plan != "" {
		fmt.Fprintf(&b, "\nPlan: %s", state.Plan)
	}
	if state.Activity != "" {
		fmt.Fprintf(&b, "\nLast activity: %s", state.Activity)
	}
	return b.String()
}

// Run renders one event stream to a single conversation surface. Each instance
// owns its state and reporter, so concurrent runs cannot edit one another's card.
type Run struct {
	mu              sync.Mutex
	refMu           sync.Mutex
	surface         channel.Surface
	policy          config.NotificationConfig
	statusLifecycle bool
	reporter        *channel.FilteredProgressReporter
	ref             conversation.Reference
	state           State
	closed          bool
	complete        bool
}

func NewRun(surface channel.Surface, policy config.NotificationConfig) *Run {
	r := &Run{surface: surface}
	if surface != nil {
		r.policy = policy.EffectiveForChannel(surface.Target().ChannelType)
	} else {
		r.policy = policy.EffectiveForChannel("")
	}
	capabilities := channel.Capabilities{}
	if surface != nil {
		capabilities = surface.Capabilities()
	}
	r.statusLifecycle = cardLifecycleAllowed(r.policy, capabilities)
	r.reporter = channel.NewFilteredProgressReporter(channel.ProgressReporterOptions{
		Allowed: func(category channel.ProgressCategory) bool {
			if category == channel.ProgressLifecycle && r.statusLifecycle {
				return true
			}
			return r.policy.Allows(string(category))
		},
		Throttle: durationMS(r.policy.ThrottleMS),
		Send: func(text string) (string, error) {
			if r.surface == nil {
				return "", channel.ErrUnsupported
			}
			ref, err := r.surface.Send(text)
			if err == nil {
				r.refMu.Lock()
				r.ref = ref
				r.refMu.Unlock()
			}
			return ref.MessageID, err
		},
		Edit: func(_ string, text string) error {
			if r.surface == nil {
				return channel.ErrUnsupported
			}
			r.refMu.Lock()
			ref := r.ref
			r.refMu.Unlock()
			updated, err := r.surface.Update(ref, text)
			if err == nil {
				r.refMu.Lock()
				r.ref = updated
				r.refMu.Unlock()
			}
			return err
		},
	})
	return r
}

func durationMS(ms int) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

func cardLifecycleAllowed(policy config.NotificationConfig, capabilities channel.Capabilities) bool {
	if policy.Allows(config.ProgressLifecycle) {
		return true
	}
	if !capabilities.Edit || policy.Preset != config.ProgressPresetBalanced || len(policy.Include) != 0 {
		return false
	}
	for _, excluded := range policy.Exclude {
		excluded = strings.ToLower(strings.TrimSpace(excluded))
		if excluded == "*" || excluded == config.ProgressLifecycle ||
			strings.HasSuffix(excluded, "*") && strings.HasPrefix(config.ProgressLifecycle, strings.TrimSuffix(excluded, "*")) {
			return false
		}
	}
	return true
}

func (r *Run) Handle(ev event.Event) error {
	if r == nil {
		return nil
	}
	category, known := categoryFor(ev.Kind)
	if !known {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	if ev.Kind == event.ConfirmationRequested {
		if r.surface == nil {
			r.mu.Unlock()
			return channel.ErrUnsupported
		}
		r.state.Phase = "Waiting"
		r.state.Activity = "Waiting for confirmation"
		question := fmt.Sprintf("Agent confirmation required: %s\nReply with y (allow once), n (deny), or a (always allow).", ev.Text)
		options := append([]string(nil), ev.Options...)
		if len(options) == 0 {
			options = []string{"y", "n", "a"}
		}
		askErr := r.surface.Ask(question, options)
		message := Render(r.state)
		statusCategory := category
		if r.statusLifecycle {
			statusCategory = channel.ProgressLifecycle
		}
		reportErr := r.reporter.ReportProgress(channel.ProgressEvent{Category: statusCategory, Name: string(ev.Kind), Message: message, At: ev.At})
		r.mu.Unlock()
		return errors.Join(askErr, reportErr)
	}
	if ev.Kind == event.Notification || ev.Kind == event.Warning || ev.Kind == event.CronCompleted {
		if !r.policy.Allows(string(category)) || ev.Text == "" {
			r.mu.Unlock()
			return nil
		}
		if r.surface == nil {
			r.mu.Unlock()
			return channel.ErrUnsupported
		}
		text := ev.Text
		if ev.Kind == event.CronCompleted {
			text = fmt.Sprintf("⏰ Cron **%s** ran:\n\n%s", ev.Name, ev.Text)
		}
		_, err := r.surface.Send(text)
		r.mu.Unlock()
		return err
	}

	switch ev.Kind {
	case event.RunStarted:
		r.state.Phase = "Working"
		r.state.Activity = "Starting"
	case event.RunCompleted:
		r.state.Phase = "Completed"
		r.state.Activity = "Finished"
	case event.RunFailed:
		r.state.Phase = "Failed"
		r.state.Activity = ev.Text
	case event.RunCancelled:
		r.state.Phase = "Stopped"
		r.state.Activity = "Cancelled"
	case event.PlanUpdated:
		r.state.Phase = "Working"
		r.state.Plan = oneLine(ev.Text)
		r.state.Activity = "Plan updated"
	case event.Waiting:
		r.state.Phase = "Waiting"
		r.state.Activity = "Waiting for your answer"
	case event.ToolStarted:
		r.state.Phase = "Working"
		r.state.Activity = toolActivity(ev.Name, ev.Arguments, false)
	case event.ToolCompleted:
		r.state.Phase = "Working"
		r.state.Activity = toolActivity(ev.Name, "", true)
		if ev.Name == "ask_user" {
			r.state.Activity = "Continuing with your answer"
		}
	case event.SubagentStarted:
		r.state.Phase = "Working"
		r.state.Activity = "Delegating work to a sub-agent"
	case event.SubagentCompleted:
		r.state.Phase = "Working"
		r.state.Activity = "Sub-agent finished"
	}
	if ev.Kind == event.RunCompleted || ev.Kind == event.RunFailed || ev.Kind == event.RunCancelled {
		r.mu.Unlock()
		return nil
	}
	message := Render(r.state)
	statusCategory := category
	if r.statusLifecycle && (ev.Kind == event.Waiting || ev.Kind == event.ToolCompleted && ev.Name == "ask_user") {
		waitingPolicy := r.policy
		waitingPolicy.Preset = config.ProgressPresetVerbose
		waitingPolicy.Include = nil
		if waitingPolicy.Allows(config.ProgressWaiting) {
			statusCategory = channel.ProgressLifecycle
		}
	}
	err := r.reporter.ReportProgress(channel.ProgressEvent{Category: statusCategory, Name: ev.Name, Message: message, At: ev.At})
	r.mu.Unlock()
	return err
}

// Complete flushes and closes pending progress before replacing the card with
// a terminal state, then delivers the final result exactly once.
func (r *Run) Complete(result string, runErr error) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.complete {
		r.mu.Unlock()
		return nil
	}
	r.complete = true
	r.closed = true
	if runErr != nil {
		if errors.Is(runErr, context.Canceled) {
			r.state.Phase = "Stopped"
		} else {
			r.state.Phase = "Failed"
		}
		r.state.Activity = "Run finished with an error"
	} else if r.state.Phase == "Stopped" {
		r.state.Activity = "Cancelled"
	} else {
		r.state.Phase = "Completed"
		r.state.Activity = "Finished"
	}
	// Retire temporary plan/tool details when the result replaces the card.
	terminal := Render(State{Phase: r.state.Phase})
	r.mu.Unlock()

	err := r.reporter.Close()
	if r.surface == nil {
		return errors.Join(err, channel.ErrUnsupported)
	}
	final := result
	if runErr != nil {
		if final == "" {
			final = "❌ " + runErr.Error()
		} else {
			final = "❌ " + runErr.Error() + "\n\n" + final
		}
	}
	r.refMu.Lock()
	ref := r.ref
	r.refMu.Unlock()
	if ref.MessageID != "" {
		finalCard := terminal
		if final != "" {
			finalCard += "\n\n" + final
		}
		updated, updateErr := r.surface.Update(ref, finalCard)
		err = errors.Join(err, updateErr)
		if updateErr == nil {
			r.refMu.Lock()
			r.ref = updated
			r.refMu.Unlock()
		}
		return err
	}
	if final != "" {
		_, sendErr := r.surface.Send(final)
		err = errors.Join(err, sendErr)
	}
	return err
}

func (r *Run) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.mu.Unlock()
	return r.reporter.Close()
}

func (r *Run) MessageID() string {
	if r == nil {
		return ""
	}
	r.refMu.Lock()
	defer r.refMu.Unlock()
	return r.ref.MessageID
}

func categoryFor(kind event.Kind) (channel.ProgressCategory, bool) {
	switch kind {
	case event.RunStarted, event.RunCompleted, event.RunFailed, event.RunCancelled, event.Notification, event.Warning:
		return channel.ProgressLifecycle, true
	case event.ToolStarted, event.ToolCompleted:
		return channel.ProgressTool, true
	case event.SubagentStarted, event.SubagentCompleted:
		return channel.ProgressSubagent, true
	case event.Waiting:
		return channel.ProgressWaiting, true
	case event.ConfirmationRequested:
		return channel.ProgressWaiting, true
	case event.PlanUpdated:
		return channel.ProgressPlan, true
	case event.CronCompleted:
		return channel.ProgressCron, true
	default:
		return "", false
	}
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func toolActivity(name, args string, finished bool) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "tool"
	}
	if finished {
		return fmt.Sprintf("Finished %s", name)
	}
	if name == "file_write" {
		var data struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(args), &data) == nil && data.Path != "" {
			return diffWrite(data.Path, data.Content)
		}
	}
	if name == "file_edit" {
		var data struct {
			Path    string `json:"path"`
			Search  string `json:"search"`
			Replace string `json:"replace"`
		}
		if json.Unmarshal([]byte(args), &data) == nil && data.Path != "" {
			return diffEdit(data.Path, data.Search, data.Replace)
		}
	}
	return fmt.Sprintf("Running %s", name)
}

func diffWrite(path, content string) string {
	const maxLines = 30
	lines := strings.Split(content, "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "Writing %s", path)
	for i, line := range lines {
		if i >= maxLines {
			fmt.Fprintf(&b, "\n… %d more lines", len(lines)-maxLines)
			break
		}
		fmt.Fprintf(&b, "\n+ %s", line)
	}
	return b.String()
}

func diffEdit(path, oldText, newText string) string {
	const maxLines = 15
	oldLines, newLines := strings.Split(oldText, "\n"), strings.Split(newText, "\n")
	var b strings.Builder
	fmt.Fprintf(&b, "Editing %s", path)
	for i, line := range oldLines {
		if i >= maxLines {
			fmt.Fprintf(&b, "\n… %d more removed lines", len(oldLines)-maxLines)
			break
		}
		fmt.Fprintf(&b, "\n- %s", line)
	}
	for i, line := range newLines {
		if i >= maxLines {
			fmt.Fprintf(&b, "\n… %d more added lines", len(newLines)-maxLines)
			break
		}
		fmt.Fprintf(&b, "\n+ %s", line)
	}
	return b.String()
}
