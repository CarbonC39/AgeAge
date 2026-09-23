package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ageage/config"
	"ageage/llm"
	"ageage/security"
	"ageage/tools"
)

func TestCronSessionIDUsesSourceOnlyWhenEnabled(t *testing.T) {
	entry := tools.CronEntry{
		ID:                     "cron_123",
		ContinueCurrentSession: true,
		Owner:                  tools.InteractionScope{SessionID: "room-thread"},
	}
	if got := cronSessionID(false, entry); got != "cron-cron_123" {
		t.Fatalf("disabled session ID = %q", got)
	}
	if got := cronSessionID(true, entry); got != "room-thread" {
		t.Fatalf("integrated session ID = %q", got)
	}
	entry.ContinueCurrentSession = false
	if got := cronSessionID(true, entry); got != "cron-cron_123" {
		t.Fatalf("task-owned session ID = %q", got)
	}
}

func TestCreateCronAgentExcludesCronMutationAndControlTools(t *testing.T) {
	workDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Workspace = workDir
	cfg.WorkDir = workDir
	store := tools.NewCronStore(filepath.Join(workDir, "cron.json"))
	service := tools.NewCronService(store, false)
	factory := &AgentFactory{
		Config:          cfg,
		SecurityChecker: security.NewChecker(workDir, nil, nil, nil),
		CronStore:       store,
		CronService:     service,
		UserInputMgr:    tools.NewUserInputManager(),
	}
	ag := factory.CreateCronAgent()
	for _, name := range []string{"cron_add", "cron_remove", "cron_list", "cron_run", "cron_pause", "cron_resume"} {
		if _, ok := ag.registry.Get(name); ok {
			t.Errorf("scheduled Agent retained %s", name)
		}
	}
}

func TestRegisterCronSessionUsesDurableCronBinding(t *testing.T) {
	ageageDir := filepath.Join(t.TempDir(), ".ageage")
	entry := tools.CronEntry{
		ID: "cron_42",
		Owner: tools.InteractionScope{
			ChannelType: "matrix",
			ChannelID:   "!room:example",
			ThreadID:    "$thread",
			SenderID:    "@alice:example",
		},
	}
	if err := registerCronSession(ageageDir, "cron-cron_42", entry); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenSessionRegistry(ageageDir)
	if err != nil {
		t.Fatal(err)
	}
	binding, ok := registry.Get("cron:" + entry.ID)
	if !ok || binding.Kind != "cron" || binding.SessionID != "cron-cron_42" || binding.OwnerID != entry.Owner.SenderID {
		t.Fatalf("cron binding = %#v, %v", binding, ok)
	}
}

func TestExecuteCronEntryQueuesAndContinuesSourceSession(t *testing.T) {
	workDir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Workspace = workDir
	cfg.WorkDir = workDir
	cfg.Agent.Mode = "full"
	cfg.Cron.SessionIntegration = true

	sm := NewSessionManager(cfg.AgeAgeDirPath())
	const sessionID = "matrix-room-thread"
	if err := sm.EnsureSession(sessionID); err != nil {
		t.Fatal(err)
	}
	if err := sm.SaveHistory(sessionID, []llm.Message{
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	}); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenSessionRegistry(cfg.AgeAgeDirPath())
	if err != nil {
		t.Fatal(err)
	}
	chatBinding := SessionBinding{Key: "chat-binding", SessionID: sessionID, ChannelType: "matrix", ChannelID: "!room:example", ThreadID: "$thread", OwnerID: "@alice:example", Kind: "thread"}
	if err := registry.Bind(chatBinding); err != nil {
		t.Fatal(err)
	}

	client := &fakeChatClient{steps: []fakeChatStep{{message: finishMessage("scheduled answer")}}}
	created := 0
	factory := &AgentFactory{Config: cfg}
	factory.cronAgentBuilder = func() *Agent {
		created++
		toolRegistry := tools.NewRegistry()
		finish := &tools.FinishTool{}
		toolRegistry.Register(finish)
		// This must be removed by CreateCronAgent before the request is sent.
		toolRegistry.Register(&tools.CronAddTool{})
		ag := NewAgent(cfg, client, toolRegistry, finish, nil, false)
		ag.Mode.InjectContext = false
		return ag
	}
	entry := tools.CronEntry{
		ID:                     "cron_shared",
		Task:                   "scheduled question",
		Command:                "scheduled question",
		ContinueCurrentSession: true,
		Owner: tools.InteractionScope{
			ChannelType: "matrix", ChannelID: "!room:example", ThreadID: "$thread",
			SessionID: sessionID, SenderID: "@alice:example",
		},
	}

	tx, unlock := sm.BeginSessionTransaction(sessionID)
	_ = tx
	type runResult struct {
		output string
		err    error
	}
	done := make(chan runResult, 1)
	go func() {
		output, err := ExecuteCronEntry(context.Background(), factory, entry)
		done <- runResult{output: output, err: err}
	}()
	select {
	case result := <-done:
		t.Fatalf("cron run did not queue behind the active session: %#v", result)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()

	select {
	case result := <-done:
		if result.err != nil || result.output != "scheduled answer" {
			t.Fatalf("ExecuteCronEntry = (%q, %v)", result.output, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued cron run did not finish")
	}
	if created != 1 {
		t.Fatalf("created cron Agents = %d", created)
	}
	if len(client.calls) != 1 || !messagesContain(client.calls[0].messages, "user", "earlier question") {
		t.Fatalf("source history was not sent to the scheduled Agent: %#v", client.calls)
	}
	for _, def := range client.calls[0].tools {
		if strings.HasPrefix(def.Function.Name, "cron_") {
			t.Fatalf("scheduled Agent exposed cron tool %q", def.Function.Name)
		}
	}
	history, err := sm.LoadHistory(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !messagesContain(history, "user", "scheduled question") {
		t.Fatalf("scheduled turn was not persisted: %#v", history)
	}
	if err := registry.Reload(); err != nil {
		t.Fatal(err)
	}
	if binding, ok := registry.Get(chatBinding.Key); !ok || binding.Kind != "thread" {
		t.Fatalf("shared run relabeled the chat binding: %#v, %v", binding, ok)
	}
}
