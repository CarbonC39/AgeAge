package agent

import (
	"context"
	"fmt"
	"time"

	"ageage/tools"
)

// CronScheduler fires stored cron entries at their scheduled times.
// It ticks once per minute, aligned to the minute boundary, and runs
// matching entries as isolated agents with their own persistent sessions.
type CronScheduler struct {
	store   *tools.CronStore
	factory *AgentFactory
}

// NewCronScheduler creates a scheduler backed by the given store and factory.
func NewCronScheduler(store *tools.CronStore, factory *AgentFactory) *CronScheduler {
	return &CronScheduler{store: store, factory: factory}
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

func (s *CronScheduler) tick(ctx context.Context, t time.Time) {
	entries := s.store.List()
	for _, e := range entries {
		if tools.MatchesCronExpr(e.Schedule, t) {
			go s.runEntry(ctx, e)
		}
	}
}

func (s *CronScheduler) runEntry(ctx context.Context, e tools.CronEntry) {
	sm := NewSessionManager(s.factory.Config.AgeAgeDirPath())
	sessionID := SanitizeSessionID("cron-" + e.ID)
	if err := sm.EnsureSession(sessionID); err != nil {
		fmt.Printf("[cron] %s: session error: %v\n", e.ID, err)
		return
	}

	ag := s.factory.CreateAgent(nil, "cron")
	ag.SessionDir = sm.SessionDir(sessionID)
	ag.Mode.InjectContext = false
	ag.Mode.InjectSoul = false

	// Restore prior history so recurring tasks can build up context over time.
	if msgs, err := sm.LoadHistory(sessionID); err == nil {
		ag.SetMessages(msgs)
	}

	if s.factory.Debug {
		fmt.Printf("[cron] %s firing (%s): %s\n", e.ID, e.Schedule, e.Command)
	}

	result, err := ag.Run(ctx, e.Command, nil)
	if err != nil {
		fmt.Printf("[cron] %s error: %v\n", e.ID, err)
		return
	}

	if s.factory.Debug {
		fmt.Printf("[cron] %s done: %s\n", e.ID, truncateStr(result, 300))
	}

	if err := sm.SaveHistory(sessionID, ag.Messages()); err != nil {
		fmt.Printf("[cron] %s: history save error: %v\n", e.ID, err)
	}
}
