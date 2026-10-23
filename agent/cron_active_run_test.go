package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ageage/config"
	"ageage/llm"
	"ageage/tools"
)

type cronBlockingClient struct {
	fakeChatClient
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *cronBlockingClient) unblock() { c.once.Do(func() { close(c.release) }) }

func (c *cronBlockingClient) ChatCompletion(ctx context.Context, msgs []llm.Message, defs []llm.ToolDef, temperature float64) (*llm.ChatResponse, error) {
	close(c.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.release:
		return c.fakeChatClient.ChatCompletion(ctx, msgs, defs, temperature)
	}
}

func cronAuditFixture(t *testing.T) (*SessionManager, *tools.CronService, tools.CronEntry, *cronBlockingClient, *atomic.Int32, <-chan struct{}) {
	t.Helper()
	work := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Workspace, cfg.WorkDir = work, work
	cfg.Cron.SessionIntegration = true
	sm := NewSessionManager(cfg.AgeAgeDirPath())
	if err := sm.EnsureSession("source-session"); err != nil {
		t.Fatal(err)
	}
	client := &cronBlockingClient{
		fakeChatClient: fakeChatClient{steps: []fakeChatStep{{message: finishMessage("scheduled complete")}}},
		started:        make(chan struct{}),
		release:        make(chan struct{}),
	}
	t.Cleanup(client.unblock)
	created := &atomic.Int32{}
	factory := &AgentFactory{Config: cfg}
	factory.cronAgentBuilder = func() *Agent {
		created.Add(1)
		registry := tools.NewRegistry()
		finish := &tools.FinishTool{}
		registry.Register(finish)
		return NewAgent(cfg, client, registry, finish, nil, false)
	}
	service := tools.NewCronService(tools.NewCronStore(filepath.Join(work, "cron.json")), true)
	started := make(chan struct{})
	service.ExecuteFunc = func(ctx context.Context, entry tools.CronEntry) (string, error) {
		close(started)
		return ExecuteCronEntry(ctx, factory, entry)
	}
	entry, err := service.Add(tools.CronActor{Scope: tools.InteractionScope{
		ChannelType: "matrix", ChannelID: "room", SessionID: "source-session", SenderID: "alice",
	}}, "* * * * *", "scheduled task", "", true, true)
	if err != nil {
		t.Fatal(err)
	}
	return sm, service, entry, client, created, started
}

func TestCronWaitsForActiveSessionAndAuditsOnlyItsOwnCompletion(t *testing.T) {
	for _, stopSource := range []bool{false, true} {
		name := "completed source"
		if stopSource {
			name = "stopped source"
		}
		t.Run(name, func(t *testing.T) {
			sm, service, entry, client, created, started := cronAuditFixture(t)
			source, _ := sm.ClaimRun(context.Background(), entry.Owner.SessionID)
			t.Cleanup(source.Close)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			type outcome struct {
				output string
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				output, err := service.Run(ctx, tools.AdminCronActor(), entry.ID)
				done <- outcome{output, err}
			}()
			<-started
			select {
			case result := <-done:
				t.Fatalf("queued cron returned before execution: %#v", result)
			case <-time.After(20 * time.Millisecond):
			}
			before, _ := service.Store.Get(entry.ID)
			if before.RunCount != 0 || source.Pending() != 0 || created.Load() != 0 {
				t.Fatalf("waiting cron changed audit/inbox: audit=%#v pending=%d created=%d", before, source.Pending(), created.Load())
			}
			if stopSource {
				source.Cancel()
			}
			source.Close()
			select {
			case <-client.started:
			case <-time.After(2 * time.Second):
				t.Fatal("cron did not start after the source run released its session")
			}
			inFlight, _ := service.Store.Get(entry.ID)
			if inFlight.RunCount != 0 {
				t.Fatalf("cron was audited while its model call was still running: %#v", inFlight)
			}
			client.unblock()
			select {
			case result := <-done:
				if result.err != nil || result.output != "scheduled complete" {
					t.Fatalf("cron result=%#v", result)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cron did not complete")
			}
			after, _ := service.Store.Get(entry.ID)
			if after.LastStatus != "success" || after.LastOutput != "scheduled complete" || after.RunCount != 1 || created.Load() != 1 {
				t.Fatalf("cron audit does not describe actual execution: %#v created=%d", after, created.Load())
			}
			history, err := sm.LoadHistory(entry.Owner.SessionID)
			if err != nil || !messagesContain(history, "user", "scheduled task") || !messagesContain(history, "assistant", "scheduled complete") {
				t.Fatalf("scheduled result was not persisted: history=%#v error=%v", history, err)
			}
		})
	}
}

func TestCronWaitingCancellationAndTimeoutAreAuditedWithoutAffectingActiveRun(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "cancelled"
		if timeout {
			name = "timed out"
		}
		t.Run(name, func(t *testing.T) {
			sm, service, entry, _, created, started := cronAuditFixture(t)
			source, _ := sm.ClaimRun(context.Background(), entry.Owner.SessionID)
			defer source.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if timeout {
				service.Timeout = 30 * time.Millisecond
			}
			done := make(chan error, 1)
			go func() {
				_, err := service.Run(ctx, tools.AdminCronActor(), entry.ID)
				done <- err
			}()
			<-started
			want := context.DeadlineExceeded
			if !timeout {
				want = context.Canceled
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("waiting cron error=%v, want %v", err, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("waiting cron ignored cancellation")
			}
			audit, _ := service.Store.Get(entry.ID)
			if audit.LastStatus != "error" || audit.RunCount != 1 || !strings.Contains(audit.LastError, want.Error()) {
				t.Fatalf("incorrect failure audit: %#v", audit)
			}
			if created.Load() != 0 || source.Pending() != 0 || source.Context().Err() != nil || sm.ActiveRun(entry.Owner.SessionID) != source {
				t.Fatal("waiting cron executed work or changed the active user run")
			}
		})
	}
}

func TestCancelledExecutingCronIsAuditedAsErrorAndPersistsAcceptedInput(t *testing.T) {
	sm, service, entry, client, _, _ := cronAuditFixture(t)
	done := make(chan error, 1)
	go func() {
		_, err := service.Run(context.Background(), tools.AdminCronActor(), entry.ID)
		done <- err
	}()
	select {
	case <-client.started:
	case <-time.After(2 * time.Second):
		t.Fatal("cron did not start")
	}
	run := sm.ActiveRun(entry.Owner.SessionID)
	if run == nil || !run.Submit(UserInput{Text: "accepted user update"}) {
		t.Fatal("could not submit input to the running cron session")
	}
	run.Cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled cron error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cron ignored cancellation")
	}
	audit, _ := service.Store.Get(entry.ID)
	if audit.LastStatus != "error" || !strings.Contains(audit.LastError, "canceled") {
		t.Fatalf("cancelled execution was marked successful: %#v", audit)
	}
	history, err := sm.LoadHistory(entry.Owner.SessionID)
	if err != nil || !messagesContain(history, "user", "accepted user update") {
		t.Fatalf("accepted input lost on cron cancellation: history=%#v error=%v", history, err)
	}
	if sm.ActiveRun(entry.Owner.SessionID) != nil {
		t.Fatal("cancelled cron retained session ownership")
	}
}

func TestCronReportedFailureIsAuditedAsError(t *testing.T) {
	_, service, entry, client, _, _ := cronAuditFixture(t)
	client.steps = []fakeChatStep{{message: toolCallMessage("finish_task", `{"status":"failure","summary":"required source unavailable"}`, "failed")}}
	client.unblock()
	output, err := service.Run(context.Background(), tools.AdminCronActor(), entry.ID)
	if err == nil || output != "required source unavailable" {
		t.Fatalf("reported failure result=(%q, %v)", output, err)
	}
	audit, _ := service.Store.Get(entry.ID)
	if audit.LastStatus != "error" || !strings.Contains(audit.LastError, "required source unavailable") || audit.LastOutput != output {
		t.Fatalf("reported failure audited incorrectly: %#v", audit)
	}
}
