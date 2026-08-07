package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"ageage/tools"
)

// CronScheduler fires stored cron entries at their scheduled times.
// It ticks once per minute, aligned to the minute boundary, and runs matching
// entries as isolated, unsupervised agents with their own persistent sessions.
type CronScheduler struct {
	store   *tools.CronStore
	factory *AgentFactory
	// deliver, when non-nil, receives every completed run's result (entry +
	// result text). In CLI mode it prints; in connect/serve mode it posts to
	// the entry's configured delivery target.
	deliver func(tools.CronEntry, string)

	running   map[string]bool // entry ID → executing right now (prevents overlap)
	runningMu sync.Mutex
}

// NewCronScheduler creates a scheduler backed by the given store and factory.
// deliver is optional; it is invoked with each finished run's result.
func NewCronScheduler(store *tools.CronStore, factory *AgentFactory, deliver func(tools.CronEntry, string)) *CronScheduler {
	return &CronScheduler{
		store:   store,
		factory: factory,
		deliver: deliver,
		running: make(map[string]bool),
	}
}

// Run blocks until ctx is cancelled.
// The first tick is aligned to the next minute boundary so that a schedule
// of "0 9 * * *" fires at exactly 09:00:00 rather than up to 59 s late.
func (s *CronScheduler) Run(ctx context.Context) {
	// Wait until the start of the next minute.
	now := time.Now()
	delay := time.Until(now.Truncate(time.Minute).Add(time.Minute))
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}

	s.catchUp(ctx, time.Now())
	s.tick(ctx, time.Now())

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			s.tick(ctx, t)
		}
	}
}

// catchUp runs the most recent missed trigger once per enabled entry after a
// process restart, when [cron].catch_up is true. Only triggers within the last
// week are considered, to avoid an avalanche of backdated runs.
func (s *CronScheduler) catchUp(ctx context.Context, now time.Time) {
	if !s.factory.Config.Cron.CatchUp {
		return
	}
	for _, e := range s.store.List() {
		if !e.Enabled {
			continue
		}
		var since time.Time
		if e.LastRun != "" {
			if t, err := time.Parse(time.RFC3339, e.LastRun); err == nil {
				since = t
			}
		}
		if since.IsZero() {
			continue // never ran before — nothing to catch up
		}
		limit := now.AddDate(0, 0, -7)
		t := now.Truncate(time.Minute)
		for t.After(since) && t.After(limit) {
			if tools.MatchesCronExpr(e.Schedule, t) {
				if s.factory.Debug {
					fmt.Printf("[cron] %s catch-up: missed trigger at %s\n", e.ID, t.Format(time.RFC3339))
				}
				// Run async so a slow catch-up never delays the first scheduled tick.
				go s.runEntry(ctx, e)
				break
			}
			t = t.Add(-time.Minute)
		}
	}
}

func (s *CronScheduler) tick(ctx context.Context, t time.Time) {
	for _, e := range s.store.List() {
		if !e.Enabled {
			continue
		}
		if tools.MatchesCronExpr(e.Schedule, t) {
			go s.runEntry(ctx, e)
		}
	}
}

// runEntry executes a single entry, guarding against overlapping runs (a slow
// task must not fire again at the next minute boundary), then records the
// outcome on the store and delivers the result.
func (s *CronScheduler) runEntry(ctx context.Context, e tools.CronEntry) {
	s.runningMu.Lock()
	if s.running[e.ID] {
		s.runningMu.Unlock()
		return
	}
	s.running[e.ID] = true
	s.runningMu.Unlock()
	defer func() {
		s.runningMu.Lock()
		delete(s.running, e.ID)
		s.runningMu.Unlock()
	}()

	if s.factory.Debug {
		fmt.Printf("[cron] %s firing (%s): %s\n", e.ID, e.Schedule, e.Command)
	}

	summary, err := ExecuteCronEntry(ctx, s.factory, e)

	status := "success"
	errMsg := ""
	if err != nil {
		status = "error"
		errMsg = err.Error()
	}
	maxOut := s.factory.Config.Cron.MaxOutput
	if maxOut <= 0 {
		maxOut = 2000
	}
	_, _, _ = s.store.UpdateResult(e.ID, time.Now(), status, errMsg, truncateStr(summary, maxOut))

	if s.factory.Debug {
		if err != nil {
			fmt.Printf("[cron] %s error: %v\n", e.ID, err)
		} else {
			fmt.Printf("[cron] %s done: %s\n", e.ID, truncateStr(summary, 300))
		}
	}

	message := summary
	if err != nil {
		message = fmt.Sprintf("Error: %s\n%s", err.Error(), summary)
	}
	if s.deliver != nil {
		s.deliver(e, message)
	}
}

// ExecuteCronEntry runs a single cron entry as an isolated, unsupervised agent.
//
// Cron tasks always run in full mode (no interactive confirmations — there is
// no human at the terminal). Hard security rules still apply: blocked_commands,
// security.forbid_rm, path allowlists, and the credentials file block.
//
// A command of the form "skill:<name> [args]" invokes the named skill or
// pipeline explicitly; any text after the skill name is passed as its argument.
// Each entry uses a persistent session (cron-<id>) so recurring tasks build up
// context across runs.
func ExecuteCronEntry(ctx context.Context, factory *AgentFactory, e tools.CronEntry) (string, error) {
	sm := NewSessionManager(factory.Config.AgeAgeDirPath())
	sessionID := SanitizeSessionID("cron-" + e.ID)
	if err := sm.EnsureSession(sessionID); err != nil {
		return "", fmt.Errorf("session error: %w", err)
	}

	ag := factory.CreateCronAgent()
	ag.SessionDir = sm.SessionDir(sessionID)
	ag.Mode.InjectContext = false
	ag.Mode.InjectSoul = false

	// Restore prior history so recurring tasks can build up context over time.
	if msgs, err := sm.LoadHistory(sessionID); err == nil && len(msgs) > 0 {
		ag.SetMessages(msgs)
	}

	input := e.Command
	if strings.HasPrefix(input, "skill:") {
		rest := strings.TrimPrefix(input, "skill:")
		parts := strings.SplitN(rest, " ", 2)
		input = "/" + parts[0]
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			input += " " + strings.TrimSpace(parts[1])
		}
	}

	result, err := ag.Run(ctx, input, nil)
	saveErr := sm.SaveHistory(sessionID, ag.Messages())
	if err != nil {
		return "", err
	}
	if saveErr != nil {
		return result, fmt.Errorf("history save failed: %w", saveErr)
	}
	return result, nil
}
