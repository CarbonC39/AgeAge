package channel

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ProgressCategory names the stable runtime categories used by notification
// policies. Keep these values aligned with config.Progress* constants without
// coupling the channel package to the config package.
type ProgressCategory string

const (
	ProgressLifecycle ProgressCategory = "lifecycle"
	ProgressPlan      ProgressCategory = "plan"
	ProgressWaiting   ProgressCategory = "waiting"
	ProgressTool      ProgressCategory = "tool"
	ProgressSubagent  ProgressCategory = "subagent"
	ProgressCron      ProgressCategory = "cron"
)

// ProgressEvent is the channel-facing representation of an Agent lifecycle
// update. It is intentionally separate from model/tool schemas.
type ProgressEvent struct {
	Category  ProgressCategory
	Name      string
	Message   string
	ChannelID string
	ThreadID  string
	At        time.Time
}

// ProgressReporter receives structured events after runtime policy filtering.
// Implementations may coalesce/edit events instead of sending every event as
// a separate message.
type ProgressReporter interface {
	ReportProgress(ProgressEvent) error
}

// ProgressReporterOptions configures a filtered, coalescing progress sink.
// Send and Edit are supplied by the channel integration so thread routing is
// explicit at the call site. Edit may be nil for channels without editable
// messages; in that case accepted updates are sent as separate messages.
type ProgressReporterOptions struct {
	Allowed  func(ProgressCategory) bool
	Throttle time.Duration
	Send     func(text string) (messageID string, err error)
	Edit     func(messageID, text string) error
}

// FilteredProgressReporter applies category filtering, de-duplicates identical
// text, and coalesces bursts into one latest update. When Edit is available it
// keeps one message and edits it in place for the lifetime of the reporter.
// A reporter is intentionally scoped to one run and one channel/thread target.
type FilteredProgressReporter struct {
	mu       sync.Mutex
	allowed  func(ProgressCategory) bool
	throttle time.Duration
	send     func(string) (string, error)
	edit     func(string, string) error

	messageID  string
	lastText   string
	lastSent   time.Time
	pending    string
	pendingErr error
	timer      *time.Timer
	closed     bool
}

// NewFilteredProgressReporter constructs a progress reporter. A nil Allowed
// function allows every category; a non-positive throttle disables coalescing.
func NewFilteredProgressReporter(opts ProgressReporterOptions) *FilteredProgressReporter {
	return &FilteredProgressReporter{
		allowed:  opts.Allowed,
		throttle: opts.Throttle,
		send:     opts.Send,
		edit:     opts.Edit,
	}
}

// ReportProgress delivers one event after applying policy and coalescing. An
// event with an empty message is ignored. Errors from an immediate send/edit
// are returned to the caller; errors from a delayed timer are retained for the
// next explicit Flush or Close call.
func (r *FilteredProgressReporter) ReportProgress(event ProgressEvent) error {
	if r == nil || r.allowed != nil && !r.allowed(event.Category) || event.Message == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("progress reporter is closed")
	}
	if event.Message == r.lastText || event.Message == r.pending {
		return nil
	}
	if r.lastSent.IsZero() || r.throttle <= 0 {
		return r.applyLocked(event.Message)
	}
	remaining := r.throttle - time.Since(r.lastSent)
	if remaining <= 0 {
		r.pending = ""
		if r.timer != nil {
			r.timer.Stop()
			r.timer = nil
		}
		return r.applyLocked(event.Message)
	}
	r.pending = event.Message
	if r.timer == nil {
		r.timer = time.AfterFunc(remaining, r.flushTimer)
	}
	return nil
}

func (r *FilteredProgressReporter) flushTimer() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.timer = nil
	if r.pending == "" || r.closed {
		return
	}
	text := r.pending
	r.pending = ""
	if err := r.applyLocked(text); err != nil {
		r.pendingErr = errors.Join(r.pendingErr, err)
	}
}

// Flush sends the latest coalesced event immediately. It is useful at the end
// of a run and makes the reporter deterministic in tests.
func (r *FilteredProgressReporter) Flush() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	err := r.pendingErr
	r.pendingErr = nil
	if r.pending == "" || r.closed {
		return err
	}
	text := r.pending
	r.pending = ""
	return errors.Join(err, r.applyLocked(text))
}

// Close flushes the final coalesced event and rejects future reports.
func (r *FilteredProgressReporter) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	text := r.pending
	r.pending = ""
	r.closed = true
	err := r.pendingErr
	r.pendingErr = nil
	if text != "" {
		err = errors.Join(err, r.applyLocked(text))
	}
	r.mu.Unlock()
	return err
}

// MessageID returns the current editable message ID, if the transport
// supplied one. It is primarily useful for diagnostics and tests.
func (r *FilteredProgressReporter) MessageID() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.messageID
}

func (r *FilteredProgressReporter) applyLocked(text string) error {
	if r.send == nil && (r.messageID == "" || r.edit == nil) {
		return fmt.Errorf("progress reporter has no send function")
	}
	if r.messageID != "" && r.edit != nil {
		if err := r.edit(r.messageID, text); err == nil {
			r.lastText = text
			r.lastSent = time.Now()
			return nil
		}
		// Editing can fail after a message has been deleted or expired. Fall
		// back to a new message and continue tracking the replacement ID.
	}
	if r.send == nil {
		return fmt.Errorf("progress reporter has no send function")
	}
	id, err := r.send(text)
	if err != nil {
		return err
	}
	r.messageID = id
	r.lastText = text
	r.lastSent = time.Now()
	return nil
}
