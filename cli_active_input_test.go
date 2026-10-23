package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ageage/agent"
	"ageage/config"
	"ageage/tools"
	"github.com/spf13/cobra"
)

func TestClassifyCLIActiveInput(t *testing.T) {
	tests := []struct {
		name        string
		text        string
		askPending  bool
		confirmWait bool
		want        cliActiveInputRoute
	}{
		{name: "stop always cancels", text: "/STOP", askPending: true, want: cliInputStop},
		{name: "session abort cancels", text: "/session abort", want: cliInputStop},
		{name: "confirmation reply", text: "a", confirmWait: true, want: cliInputConfirmation},
		{name: "ordinary ask user answer", text: "@/tmp/photo.png please inspect", askPending: true, want: cliInputAskUser},
		{name: "yes answers ask user if no confirmation waits", text: "y", askPending: true, want: cliInputAskUser},
		{name: "history mutation is rejected during a run", text: "/clear", want: cliInputCommand},
		{name: "retry is rejected during a run", text: "/retry update", want: cliInputCommand},
		{name: "unknown skill command steers", text: "/review this", want: cliInputSteer},
		{name: "other text steers", text: "Also check the readme", want: cliInputSteer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCLIActiveInput(tt.text, tt.askPending, tt.confirmWait); got != tt.want {
				t.Fatalf("classifyCLIActiveInput(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestCLIAskUserRouteKeepsImageAttachment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "answer.png")
	if err := os.WriteFile(path, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := tools.NewUserInputManager()
	scope := tools.InteractionScope{ChannelType: "cli", ChannelID: "interactive", SessionID: "s", SenderID: "local"}
	_, answer := manager.RequestUserInputScoped(scope)
	text := "@" + path + " this is the answer"
	if got := classifyCLIActiveInput(text, manager.HasPendingScope(scope), false); got != cliInputAskUser {
		t.Fatalf("route = %v, want ask_user", got)
	}
	cleanText, parts, warnings := agent.ParseCLIInput(text, config.DefaultConfig(), nil)
	if len(warnings) != 0 {
		t.Fatalf("unexpected attachment warnings: %v", warnings)
	}
	if !manager.RespondInputForScope(scope, agent.UserInput{Text: cleanText, Parts: parts, Warnings: warnings}) {
		t.Fatal("could not deliver answer to pending ask_user")
	}
	got := <-answer
	hasImage := false
	for _, part := range got.Parts {
		if part.Type == "image_url" && part.ImageURL != nil {
			hasImage = true
		}
	}
	if got.Text != "this is the answer" || !hasImage {
		t.Fatalf("ask_user did not receive the multimodal answer: %#v", got)
	}
}

func TestCLIAlwaysAllowPersistsOperation(t *testing.T) {
	manager := tools.NewConfirmationManager()
	scope := tools.InteractionScope{ChannelType: "cli", ChannelID: "interactive", SessionID: "s", SenderID: "local"}
	operation := "Run command `git status`"
	_, result := manager.RequestConfirmationScoped(operation, scope, time.Second)
	var persisted string
	if !respondCLIConfirmation(manager, scope, "a", func(value string) { persisted = value }) {
		t.Fatal("confirmation was not delivered")
	}
	if persisted != operation {
		t.Fatalf("always-allow callback received %q, want %q", persisted, operation)
	}
	if !manager.IsAlwaysAllowed(operation) {
		t.Fatal("always-allow choice was not remembered for subsequent confirmation checks")
	}
	if allowed, ok := <-result; !ok || !allowed {
		t.Fatalf("confirmation result = (%v, %v), want allowed", allowed, ok)
	}
}

func TestRunCLIReadsPipeUpdatesDuringActiveRun(t *testing.T) {
	for _, eof := range []bool{true, false} {
		name := "exit"
		if eof {
			name = "eof"
		}
		t.Run(name, func(t *testing.T) { runCLIPipeSteering(t, eof) })
	}
}

func runCLIPipeSteering(t *testing.T, eof bool) {
	firstRequest := make(chan struct{})
	allowFirstResponse := make(chan struct{})
	secondRequest := make(chan []byte, 1)
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read model request: %v", err)
			return
		}
		if requestCount.Add(1) == 1 {
			close(firstRequest)
			select {
			case <-allowFirstResponse:
			case <-r.Context().Done():
				return
			}
		} else {
			secondRequest <- body
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"finish","type":"function","function":{"name":"finish_task","arguments":"{\"status\":\"success\",\"summary\":\"CLI_FOLLOWUP_ACK\"}"}}]},"finish_reason":"tool_calls"}]}`)
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
		_, _ = fmt.Fprintln(w)
	}))
	defer server.Close()

	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.toml")
	configText := fmt.Sprintf("workspace = %q\n\n[llm]\napi_key = %q\nbase_url = %q\nmodel = %q\n\n[agent]\nmode = %q\ntools = [%q]\nmax_iterations = 5\n\n[router]\nenabled = false\n\n[planner]\nenabled = false\n\n[notifications]\npreset = %q\n",
		workspace, "test-key", server.URL+"/v1", "test-model", "full", "finish_task", "quiet")
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)
	oldStdin, oldStdout := os.Stdin, os.Stdout
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutPath := filepath.Join(root, "cli.stdout")
	stdout, err := os.Create(stdoutPath)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin, os.Stdout = stdinRead, stdout
	defer func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		stdinRead.Close()
		stdinWrite.Close()
		stdout.Close()
	}()

	cmd := &cobra.Command{}
	cmd.Flags().String("config", configPath, "")
	cmd.Flags().Bool("soul", false, "")
	cmd.Flags().Bool("think", false, "")
	runDone := make(chan error, 1)
	go func() { runDone <- runCLI(cmd, nil) }()

	if _, err := io.WriteString(stdinWrite, "initial question\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstRequest:
	case <-time.After(10 * time.Second):
		t.Fatal("CLI did not issue the initial model request")
	}
	if _, err := io.WriteString(stdinWrite, "FOLLOWUP_MARKER please include this update\n"); err != nil {
		t.Fatal(err)
	}
	waitForCLIOutput(t, stdoutPath, "Update received (1 pending).", 5*time.Second)
	if eof {
		if err := stdinWrite.Close(); err != nil {
			t.Fatal(err)
		}
	}
	close(allowFirstResponse)

	var followupBody []byte
	select {
	case followupBody = <-secondRequest:
	case <-time.After(10 * time.Second):
		t.Fatalf("active CLI run did not make a second model call; output: %s", readCLIOutput(stdoutPath))
	}
	if !bytes.Contains(followupBody, []byte("FOLLOWUP_MARKER")) {
		t.Fatalf("second model request did not contain steering input: %s", followupBody)
	}
	var second struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(followupBody, &second); err != nil {
		t.Fatalf("decode second model request: %v", err)
	}
	toolResultAt, updateAt := -1, -1
	for i, message := range second.Messages {
		if message.Role == "tool" && message.ToolCallID == "finish" {
			toolResultAt = i
		}
		if message.Role == "user" && strings.Contains(message.Content, "FOLLOWUP_MARKER") {
			updateAt = i
		}
		if message.Role == "assistant" && message.Content == "CLI_FOLLOWUP_ACK" {
			t.Fatalf("second request included a finalized assistant turn before steering: %s", followupBody)
		}
	}
	if toolResultAt < 0 || updateAt != toolResultAt+1 {
		t.Fatalf("steering was not inserted after the finish tool result in the active turn (tool=%d update=%d): %s", toolResultAt, updateAt, followupBody)
	}
	waitForCLIOutput(t, stdoutPath, "CLI_FOLLOWUP_ACK", 10*time.Second)
	if !eof {
		if _, err := io.WriteString(stdinWrite, "exit\n"); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("runCLI returned error: %v; output: %s", err, readCLIOutput(stdoutPath))
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("runCLI did not exit after the exit command; output: %s", readCLIOutput(stdoutPath))
	}
	if count := requestCount.Load(); count != 2 {
		t.Fatalf("model request count = %d, want two calls in one run", count)
	}
}

func waitForCLIOutput(t *testing.T, path, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(readCLIOutput(path), needle) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("CLI output never contained %q; output: %s", needle, readCLIOutput(path))
}

func readCLIOutput(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}
