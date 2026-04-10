package tools

import (
	"encoding/json"
	"fmt"
)

// AskUserTool pauses pipeline/agent execution and asks the user a question.
// It is skill-only — injected only when a skill declares required_tools: [ask_user].
//
// The tool blocks indefinitely until the user replies. Cancellation is handled
// by closing the pending-input channel via UserInputManager.Cancel: the tool
// receives ok=false and returns an error, prompting the agent to call
// node_complete(failure).
type AskUserTool struct {
	// ChannelID is the IM channel (or "" for CLI) used as the request key.
	ChannelID string
	// Manager handles the request/response lifecycle.
	Manager *UserInputManager
	// NotifyFuncPtr points to the agent's AskUserNotify field. Dereferenced at
	// Execute() time so that the function value set after tool registration
	// (e.g. by the pipeline executor on a sub-agent) is always used.
	NotifyFuncPtr *func(string, []string)
}

func (t *AskUserTool) Name() string { return "ask_user" }

func (t *AskUserTool) Description() string {
	return "Pause execution and ask the user a question, optionally with multiple-choice options. " +
		"The tool blocks until the user replies. " +
		"If the user cancels (/stop or /session abort), the tool returns an error — " +
		"call node_complete with status=failure in response."
}

func (t *AskUserTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"question": map[string]interface{}{
				"type":        "string",
				"description": "The question to present to the user.",
			},
			"options": map[string]interface{}{
				"type":        "array",
				"items":       map[string]interface{}{"type": "string"},
				"description": "Optional multiple-choice options. If provided, the user should select one.",
			},
		},
		"required": []string{"question"},
	}
}

type askUserArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

func (t *AskUserTool) Execute(args json.RawMessage) (string, error) {
	var a askUserArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if a.Question == "" {
		return "", fmt.Errorf("question is required")
	}
	if t.Manager == nil {
		return "", fmt.Errorf("ask_user: no UserInputManager configured")
	}

	// Register the pending request BEFORE notifying the user to avoid the
	// race where the user replies before RequestInput is called.
	respCh := t.Manager.RequestInput(t.ChannelID)

	// Send the question to the user.
	notifyFn := t.resolveNotifyFunc()
	if notifyFn != nil {
		notifyFn(a.Question, a.Options)
	} else {
		// Plain-text fallback (no custom notify configured).
		msg := "❓ " + a.Question
		for i, opt := range a.Options {
			msg += fmt.Sprintf("\n  %d. %s", i+1, opt)
		}
		if len(a.Options) > 0 {
			msg += "\nType a number or your answer:"
		}
		fmt.Println(msg)
	}

	// Block until the user replies or the request is cancelled.
	answer, ok := <-respCh
	if !ok {
		return "", fmt.Errorf("user input cancelled")
	}
	return answer, nil
}

func (t *AskUserTool) resolveNotifyFunc() func(string, []string) {
	if t.NotifyFuncPtr == nil {
		return nil
	}
	return *t.NotifyFuncPtr
}
