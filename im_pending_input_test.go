package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"ageage/agent"
	"ageage/attachment"
	"ageage/channel"
	"ageage/config"
	"ageage/tools"
)

func pendingInputScope(sender string) tools.InteractionScope {
	return tools.InteractionScope{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread", SessionID: "session", SenderID: sender}
}

func TestAnswerPendingIMInputReturnsWhileChatLockHeldAndDeliversImage(t *testing.T) {
	manager := tools.NewUserInputManager()
	scope := pendingInputScope("alice")
	_, answerCh := manager.RequestUserInputScoped(scope)
	chatLock := sync.Mutex{}
	chatLock.Lock()
	defer chatLock.Unlock()

	png := "\x89PNG\r\n\x1a\nimage payload"
	var opens int
	image := attachment.Attachment{Filename: "image", MIMEType: "application/octet-stream", Size: int64(len(png)), Open: func(context.Context) (io.ReadCloser, error) {
		opens++
		return io.NopCloser(strings.NewReader(png)), nil
	}}
	msg := channel.IncomingMessage{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread", SenderID: "alice", Attachments: []attachment.Attachment{image}}
	cfg := config.DefaultConfig()
	cfg.Multimodal.Vision = true
	type result struct {
		handled bool
		reply   string
	}
	done := make(chan result, 1)
	go func() {
		handled, reply := answerPendingIMInput(context.Background(), msg, scope, manager, cfg, nil)
		done <- result{handled, reply}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(time.Second):
		t.Fatal("pending answer waited on the held chat lock")
	}
	if !got.handled || !strings.Contains(got.reply, "Got it") {
		t.Fatalf("answer helper result = %#v", got)
	}
	if opens != 1 {
		t.Fatalf("image Open count = %d", opens)
	}
	select {
	case answer := <-answerCh:
		if len(answer.Parts) != 1 || answer.Parts[0].Type != "image_url" || answer.Parts[0].ImageURL == nil || !strings.HasPrefix(answer.Parts[0].ImageURL.URL, "data:image/png;base64,") {
			t.Fatalf("typed pending answer = %#v", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("typed answer was not delivered")
	}
}

func TestAnswerPendingIMInputDeliversCaptionFilesAndWarningsOnce(t *testing.T) {
	manager := tools.NewUserInputManager()
	scope := pendingInputScope("alice")
	_, answerCh := manager.RequestUserInputScoped(scope)
	png := "\x89PNG\r\n\x1a\nimage"
	var imageOpens, textOpens, failedOpens int
	image := attachment.Attachment{Filename: "photo", MIMEType: "", Open: func(context.Context) (io.ReadCloser, error) {
		imageOpens++
		return io.NopCloser(strings.NewReader(png)), nil
	}}
	text := attachment.Attachment{Filename: "notes.txt", MIMEType: "text/plain", Open: func(context.Context) (io.ReadCloser, error) {
		textOpens++
		return io.NopCloser(strings.NewReader("log details")), nil
	}}
	failed := attachment.Attachment{Filename: "broken.txt", MIMEType: "text/plain", Open: func(context.Context) (io.ReadCloser, error) {
		failedOpens++
		return nil, io.ErrUnexpectedEOF
	}}
	msg := channel.IncomingMessage{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread", SenderID: "alice", Text: "please inspect", Attachments: []attachment.Attachment{image, text, failed}}
	cfg := config.DefaultConfig()
	cfg.Multimodal.Vision = true
	handled, reply := answerPendingIMInput(context.Background(), msg, scope, manager, cfg, nil)
	if !handled || !strings.Contains(reply, "download failed") || !strings.Contains(reply, "Got it") {
		t.Fatalf("warning reply = (handled=%v, %q)", handled, reply)
	}
	if imageOpens != 1 || textOpens != 1 || failedOpens != 1 {
		t.Fatalf("Open counts image=%d text=%d failed=%d", imageOpens, textOpens, failedOpens)
	}
	select {
	case answer := <-answerCh:
		if answer.Text != "please inspect" || len(answer.Warnings) == 0 {
			t.Fatalf("typed answer text/warnings = %#v", answer)
		}
		var sawImage, sawText, sawWarning bool
		for _, part := range answer.Parts {
			if part.Type == "image_url" {
				sawImage = true
			}
			if part.Type == "text" && strings.Contains(part.Text, "log details") {
				sawText = true
			}
			if part.Type == "text" && strings.Contains(part.Text, "download failed") {
				sawWarning = true
			}
		}
		if !sawImage || !sawText || !sawWarning {
			t.Fatalf("image/text/warning not present in one typed answer: %#v", answer.Parts)
		}
	default:
		t.Fatal("typed answer not delivered")
	}
	if manager.RespondInputForScope(scope, agent.UserInput{Text: "duplicate"}) {
		t.Fatal("pending request accepted a second response")
	}
}

func TestAnswerPendingIMInputWrongSenderDoesNotOpenOrConsume(t *testing.T) {
	manager := tools.NewUserInputManager()
	owner := pendingInputScope("alice")
	_, answerCh := manager.RequestUserInputScoped(owner)
	wrong := pendingInputScope("bob")
	opened := false
	msg := channel.IncomingMessage{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread", SenderID: "bob", Attachments: []attachment.Attachment{{Filename: "secret.png", Open: func(context.Context) (io.ReadCloser, error) {
		opened = true
		return io.NopCloser(strings.NewReader("not opened")), nil
	}}}}
	handled, reply := answerPendingIMInput(context.Background(), msg, wrong, manager, config.DefaultConfig(), nil)
	if !handled || !strings.Contains(reply, "not authorised") || opened {
		t.Fatalf("wrong sender handling = (%v, %q), Open=%v", handled, reply, opened)
	}
	if !manager.HasPendingScope(owner) {
		t.Fatal("wrong sender consumed the owner's pending request")
	}
	if !manager.RespondInputForScope(owner, agent.UserInput{Text: "still waiting"}) {
		t.Fatal("owner could not answer after wrong sender attempt")
	}
	if got := <-answerCh; got.Text != "still waiting" {
		t.Fatalf("owner answer = %#v", got)
	}
}

func TestAnswerPendingIMInputWithoutPendingAndLegacyTextWaiter(t *testing.T) {
	manager := tools.NewUserInputManager()
	opened := false
	msg := channel.IncomingMessage{ChannelType: "matrix", ChannelID: "room", ThreadID: "thread", SenderID: "alice", Attachments: []attachment.Attachment{{Filename: "photo.png", Open: func(context.Context) (io.ReadCloser, error) {
		opened = true
		return io.NopCloser(strings.NewReader("unused")), nil
	}}}}
	scope := pendingInputScope("alice")
	if handled, reply := answerPendingIMInput(context.Background(), msg, scope, manager, config.DefaultConfig(), nil); handled || reply != "" || opened {
		t.Fatalf("no-pending result = (%v, %q), Open=%v", handled, reply, opened)
	}

	legacyScope := tools.LegacyInteractionScope("room")
	_, legacyAnswer := manager.RequestInputScoped(legacyScope)
	legacyMsg := channel.IncomingMessage{ChannelType: "", ChannelID: "room", Text: "plain answer"}
	handled, reply := answerPendingIMInput(context.Background(), legacyMsg, legacyScope, manager, config.DefaultConfig(), nil)
	if !handled || !strings.Contains(reply, "Got it") {
		t.Fatalf("legacy input result = (%v, %q)", handled, reply)
	}
	if answer := <-legacyAnswer; answer != "plain answer" {
		t.Fatalf("legacy string waiter answer = %q", answer)
	}
}

func TestAnswerPendingIMInputDoesNotAnswerReplacementQuestion(t *testing.T) {
	manager := tools.NewUserInputManager()
	scope := pendingInputScope("alice")
	_, original := manager.RequestUserInputScoped(scope)
	var replacement <-chan agent.UserInput
	msg := channel.IncomingMessage{Attachments: []attachment.Attachment{{Filename: "answer.txt", Open: func(context.Context) (io.ReadCloser, error) {
		_, replacement = manager.RequestUserInputScoped(scope)
		return io.NopCloser(strings.NewReader("answer for the original question")), nil
	}}}}
	handled, reply := answerPendingIMInput(context.Background(), msg, scope, manager, config.DefaultConfig(), nil)
	if !handled || !strings.Contains(reply, "no longer waiting") {
		t.Fatalf("stale answer must be rejected: handled=%v reply=%q", handled, reply)
	}
	if _, ok := <-original; ok {
		t.Fatal("original request should have been cancelled by replacement")
	}
	select {
	case answer := <-replacement:
		t.Fatalf("stale attachment was delivered to the new question: %#v", answer)
	default:
	}
	if !manager.HasPendingScope(scope) {
		t.Fatal("the replacement question must remain pending")
	}
	manager.CancelForScope(scope)
}
