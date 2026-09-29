package channel

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestFilteredProgressReporterDeduplicatesAndEditsOneMessage(t *testing.T) {
	var mu sync.Mutex
	var sends, edits []string
	reporter := NewFilteredProgressReporter(ProgressReporterOptions{
		Send: func(text string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			sends = append(sends, text)
			return "progress-1", nil
		},
		Edit: func(id, text string) error {
			if id != "progress-1" {
				t.Fatalf("unexpected edit ID %q", id)
			}
			mu.Lock()
			defer mu.Unlock()
			edits = append(edits, text)
			return nil
		},
	})

	event := ProgressEvent{Category: ProgressLifecycle, Message: "Working"}
	if err := reporter.ReportProgress(event); err != nil {
		t.Fatal(err)
	}
	if err := reporter.ReportProgress(event); err != nil {
		t.Fatal(err)
	}
	if err := reporter.ReportProgress(ProgressEvent{Category: ProgressPlan, Message: "Step 1"}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Close(); err != nil {
		t.Fatal(err)
	}

	if len(sends) != 1 || sends[0] != "Working" {
		t.Fatalf("sends = %#v, want one initial message", sends)
	}
	if len(edits) != 1 || edits[0] != "Step 1" {
		t.Fatalf("edits = %#v, want one changed update", edits)
	}
}

func TestFilteredProgressReporterCoalescesToLatestUpdate(t *testing.T) {
	var sends, edits []string
	reporter := NewFilteredProgressReporter(ProgressReporterOptions{
		Throttle: time.Hour,
		Send: func(text string) (string, error) {
			sends = append(sends, text)
			return "progress-1", nil
		},
		Edit: func(_ string, text string) error {
			edits = append(edits, text)
			return nil
		},
	})

	_ = reporter.ReportProgress(ProgressEvent{Category: ProgressLifecycle, Message: "Working"})
	_ = reporter.ReportProgress(ProgressEvent{Category: ProgressPlan, Message: "Step 1"})
	_ = reporter.ReportProgress(ProgressEvent{Category: ProgressPlan, Message: "Step 2"})
	if err := reporter.Flush(); err != nil {
		t.Fatal(err)
	}

	if len(sends) != 1 || len(edits) != 1 || edits[0] != "Step 2" {
		t.Fatalf("sends=%#v edits=%#v; want latest pending update only", sends, edits)
	}
}

func TestFilteredProgressReporterDoesNotRestoreDisplayedTextOverPendingUpdate(t *testing.T) {
	var edits []string
	reporter := NewFilteredProgressReporter(ProgressReporterOptions{
		Throttle: time.Hour,
		Send:     func(string) (string, error) { return "progress-1", nil },
		Edit: func(_ string, text string) error {
			edits = append(edits, text)
			return nil
		},
	})
	_ = reporter.ReportProgress(ProgressEvent{Category: ProgressLifecycle, Message: "A"})
	_ = reporter.ReportProgress(ProgressEvent{Category: ProgressPlan, Message: "B"})
	_ = reporter.ReportProgress(ProgressEvent{Category: ProgressLifecycle, Message: "A"})
	if err := reporter.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(edits) != 1 || edits[0] != "B" {
		t.Fatalf("edits = %#v, pending B must not be replaced by already-displayed A", edits)
	}
}

func TestFilteredProgressReporterRetainsDelayedErrors(t *testing.T) {
	wantErr := errors.New("edit failed")
	reporter := NewFilteredProgressReporter(ProgressReporterOptions{
		Throttle: 5 * time.Millisecond,
		Send:     func(string) (string, error) { return "progress-1", nil },
		Edit:     func(string, string) error { return wantErr },
	})
	_ = reporter.ReportProgress(ProgressEvent{Category: ProgressLifecycle, Message: "A"})
	// The edit failure falls back to Send. Make that fallback fail too so the
	// timer has a delivery error to retain.
	reporter.send = func(string) (string, error) { return "", wantErr }
	_ = reporter.ReportProgress(ProgressEvent{Category: ProgressPlan, Message: "B"})
	time.Sleep(20 * time.Millisecond)
	if err := reporter.Flush(); !errors.Is(err, wantErr) {
		t.Fatalf("Flush error = %v, want retained %v", err, wantErr)
	}
}

func TestFilteredProgressReporterAppliesCategoryPolicy(t *testing.T) {
	var sends []string
	reporter := NewFilteredProgressReporter(ProgressReporterOptions{
		Allowed: func(category ProgressCategory) bool { return category != ProgressTool },
		Send: func(text string) (string, error) {
			sends = append(sends, text)
			return "progress-1", nil
		},
	})
	_ = reporter.ReportProgress(ProgressEvent{Category: ProgressTool, Message: "tool spam"})
	_ = reporter.ReportProgress(ProgressEvent{Category: ProgressPlan, Message: "plan"})
	if len(sends) != 1 || sends[0] != "plan" {
		t.Fatalf("sends = %#v, want only allowed category", sends)
	}
}
