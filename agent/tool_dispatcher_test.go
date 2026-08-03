package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ageage/config"
	"ageage/creds"
	"ageage/llm"
	"ageage/tools"
)

type dispatchStubTool struct {
	seen   json.RawMessage
	result string
	err    error
	calls  int
}

func (s *dispatchStubTool) Name() string        { return "capture" }
func (s *dispatchStubTool) Description() string { return "capture arguments" }
func (s *dispatchStubTool) Parameters() map[string]interface{} {
	return map[string]interface{}{"type": "object"}
}
func (s *dispatchStubTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	s.calls++
	s.seen = append(s.seen[:0], args...)
	return s.result, s.err
}

func newDispatcherCredManager(t *testing.T, values map[string]string) *creds.Manager {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	m, err := creds.NewManager(filepath.Join(dir, "credentials.toml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range values {
		if err := m.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func TestToolDispatcherSubstitutesForExecutionAndRedactsPresentation(t *testing.T) {
	secret := "s3cr3t\nvalue"
	manager := newDispatcherCredManager(t, map[string]string{"token": secret})
	stub := &dispatchStubTool{result: "response contains " + secret, err: errors.New("failure contains " + secret)}
	registry := tools.NewRegistry()
	registry.Register(stub)
	dispatcher := NewToolDispatcher(registry, manager)

	var startArgs string
	ended := false
	result, err := dispatcher.Execute(
		context.Background(),
		"capture",
		json.RawMessage(`{"token":"{{cred:token}}"}`),
		ToolDispatchHooks{
			Start: func(_ string, args string) { startArgs = args },
			End:   func(string) { ended = true },
		},
	)
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error was not redacted: %v", err)
	}
	if strings.Contains(result, secret) || !strings.Contains(result, "[REDACTED]") {
		t.Fatalf("result was not redacted: %q", result)
	}
	if strings.Contains(startArgs, secret) || !strings.Contains(startArgs, "{{cred:token}}") {
		t.Fatalf("start args were not redacted: %q", startArgs)
	}
	var executed map[string]string
	if err := json.Unmarshal(stub.seen, &executed); err != nil {
		t.Fatal(err)
	}
	if executed["token"] != secret || stub.calls != 1 || !ended {
		t.Fatalf("execution args=%#v calls=%d ended=%v", executed, stub.calls, ended)
	}
}

func TestToolDispatcherBlocksCredentialPathBeforeExecution(t *testing.T) {
	manager := newDispatcherCredManager(t, nil)
	stub := &dispatchStubTool{}
	registry := tools.NewRegistry()
	registry.Register(stub)

	_, err := NewToolDispatcher(registry, manager).Execute(
		context.Background(), "capture",
		json.RawMessage(`{"command":"cat credentials.toml"}`),
		ToolDispatchHooks{},
	)
	if err == nil || !strings.Contains(err.Error(), "system-protected") {
		t.Fatalf("protected path error = %v", err)
	}
	if stub.calls != 0 {
		t.Fatalf("protected call executed %d times", stub.calls)
	}
}

func TestToolDispatcherRequiresRegistry(t *testing.T) {
	_, err := (&ToolDispatcher{}).Execute(context.Background(), "missing", nil, ToolDispatchHooks{})
	if err == nil || !strings.Contains(err.Error(), "no registry") {
		t.Fatalf("missing registry error = %v", err)
	}
}

func TestToolDispatcherPreservesCancellationIdentity(t *testing.T) {
	manager := newDispatcherCredManager(t, map[string]string{"token": "secret"})
	stub := &dispatchStubTool{err: context.Canceled}
	registry := tools.NewRegistry()
	registry.Register(stub)
	_, err := NewToolDispatcher(registry, manager).Execute(
		context.Background(), "capture", json.RawMessage(`{}`), ToolDispatchHooks{},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation identity lost: %v", err)
	}
}

func TestCredentialManagerTestIsolation(t *testing.T) {
	manager := newDispatcherCredManager(t, map[string]string{"x": "y"})
	if _, err := os.Stat(manager.Path()); err != nil {
		t.Fatalf("test credential store not created: %v", err)
	}
}

func TestAgentRedactsCredentialSubstitutedIntoFinishSummary(t *testing.T) {
	secret := "final-response-secret"
	manager := newDispatcherCredManager(t, map[string]string{"token": secret})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"choices":[{
				"index":0,
				"message":{
					"role":"assistant",
					"tool_calls":[{
						"id":"call-finish",
						"type":"function",
						"function":{
							"name":"finish_task",
							"arguments":"{\"status\":\"success\",\"summary\":\"answer {{cred:token}}\"}"
						}
					}]
				},
				"finish_reason":"tool_calls"
			}]
		}`)
	}))
	defer upstream.Close()

	cfg := config.DefaultConfig()
	cfg.Workspace = t.TempDir()
	cfg.WorkDir = cfg.Workspace
	client := llm.NewClient("", upstream.URL, "test-model", false, 0)
	registry := tools.NewRegistry()
	finishTool := &tools.FinishTool{}
	registry.Register(finishTool)
	ag := NewAgent(cfg, client, registry, finishTool, nil, false)
	ag.CredMgr = manager
	ag.Mode.InjectContext = false

	result, err := ag.Run(context.Background(), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result, secret) || !strings.Contains(result, "[REDACTED]") {
		t.Fatalf("final result was not redacted: %q", result)
	}
	if strings.Contains(finishTool.Summary, secret) {
		t.Fatalf("finish tool retained secret: %q", finishTool.Summary)
	}
	messages := ag.Messages()
	if strings.Contains(fmt.Sprint(messages), secret) {
		t.Fatalf("conversation retained secret: %#v", messages)
	}
}
