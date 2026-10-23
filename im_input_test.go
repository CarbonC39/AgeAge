package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ageage/agent"
	"ageage/attachment"
	"ageage/channel"
	"ageage/config"
	"ageage/llm"
	"ageage/tools"
)

func imAttachment(name, mime string, data string) attachment.Attachment {
	return attachment.Attachment{Filename: name, MIMEType: mime, Size: int64(len(data)), Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(data)), nil
	}}
}

func TestNormalizeIMInputCombinesCaptionImageAndLogOnce(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Multimodal.Vision = true
	png := "\x89PNG\r\n\x1a\nminimal-image"
	var imageOpens, logOpens int
	image := imAttachment("photo.bin", "application/octet-stream", png)
	image.Open = func(context.Context) (io.ReadCloser, error) {
		imageOpens++
		return io.NopCloser(strings.NewReader(png)), nil
	}
	log := imAttachment("event.log", "text/plain", "event 17")
	log.Open = func(context.Context) (io.ReadCloser, error) {
		logOpens++
		return io.NopCloser(strings.NewReader("event 17")), nil
	}
	msg := channel.IncomingMessage{ChannelType: "telegram", SenderID: "user-1", SenderName: "Mina", IsGroupChat: true, Text: "please inspect", Attachments: []attachment.Attachment{image, log}}
	in := normalizeIMInput(context.Background(), msg, cfg, nil)
	if in.Text != "[Mina]: please inspect" {
		t.Fatalf("Text = %q", in.Text)
	}
	if imageOpens != 1 || logOpens != 1 {
		t.Fatalf("attachment Open counts image=%d log=%d", imageOpens, logOpens)
	}
	if len(in.Parts) != 3 {
		t.Fatalf("expected caption, image, and log parts; got %#v", in.Parts)
	}
	if in.Parts[0].Type != "text" || !strings.Contains(in.Parts[0].Text, "[Mina]: please inspect") {
		t.Fatalf("sender prefix/caption missing: %#v", in.Parts[0])
	}
	if in.Parts[1].Type != "image_url" || in.Parts[1].ImageURL == nil || !strings.HasPrefix(in.Parts[1].ImageURL.URL, "data:image/png;base64,") {
		t.Fatalf("image part missing: %#v", in.Parts[1])
	}
	all := strings.Join(imPartTexts(in.Parts), " ")
	if !strings.Contains(all, "event 17") {
		t.Fatalf("log content absent from input: %#v", in.Parts)
	}
	if strings.Contains(all, "[image attachment]") || strings.Contains(all, "[attachment]") {
		t.Fatalf("generic placeholder leaked: %q", all)
	}
}

func TestNormalizeIMInputWarningsReachUserAndModel(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Multimodal.Vision = false
	good := imAttachment("notes.txt", "text/plain", "read this")
	unsupported := attachment.Attachment{Filename: "voice.ogg", MIMEType: "audio/ogg", Size: 4, UnsupportedReason: "audio download is unsupported"}
	failed := attachment.Attachment{Filename: "lost.txt", MIMEType: "text/plain", Open: func(context.Context) (io.ReadCloser, error) { return nil, io.ErrUnexpectedEOF }}
	in := normalizeIMInput(context.Background(), channel.IncomingMessage{Text: "caption", Attachments: []attachment.Attachment{good, unsupported, failed}}, cfg, nil)
	feedback := imInputWarnings(in)
	if !strings.Contains(feedback, "audio download is unsupported") || !strings.Contains(feedback, "download failed") {
		t.Fatalf("user feedback lacks attachment warnings: %q", feedback)
	}
	modelText := strings.Join(imPartTexts(in.Parts), " ")
	if !strings.Contains(modelText, "Attachment warning") || !strings.Contains(modelText, "read this") {
		t.Fatalf("warning/content parts not supplied to model: %#v", in.Parts)
	}
	if !strings.Contains(modelText, "audio download is unsupported") || !strings.Contains(modelText, "download failed") {
		t.Fatalf("model parts lack warning detail: %q", modelText)
	}
}

func TestNormalizeIMInputVisionTextAndUnsupportedBehavior(t *testing.T) {
	png := "\x89PNG\r\n\x1a\nimage"
	visionCfg := config.DefaultConfig()
	vision := normalizeIMInput(context.Background(), channel.IncomingMessage{Attachments: []attachment.Attachment{imAttachment("p.dat", "", png)}}, visionCfg, nil)
	if len(vision.Parts) != 1 || vision.Parts[0].Type != "image_url" {
		t.Fatalf("vision attachment = %#v", vision.Parts)
	}

	textCfg := config.DefaultConfig()
	textCfg.Multimodal.Vision = false
	textInput := normalizeIMInput(context.Background(), channel.IncomingMessage{Attachments: []attachment.Attachment{imAttachment("note.txt", "text/plain", "plain words")}}, textCfg, nil)
	if len(textInput.Parts) != 1 || !strings.Contains(textInput.Parts[0].Text, "plain words") {
		t.Fatalf("text attachment = %#v", textInput.Parts)
	}

	unsupported := normalizeIMInput(context.Background(), channel.IncomingMessage{Attachments: []attachment.Attachment{{Filename: "clip.mp4", MIMEType: "video/mp4", UnsupportedReason: "video unsupported"}}}, visionCfg, nil)
	if len(unsupported.Warnings) == 0 || !strings.Contains(imInputWarnings(unsupported), "video unsupported") {
		t.Fatalf("unsupported input not reported: %#v", unsupported)
	}
	if len(unsupported.Parts) == 0 || !strings.Contains(strings.Join(imPartTexts(unsupported.Parts), " "), "video unsupported") {
		t.Fatalf("unsupported warning not in model parts: %#v", unsupported.Parts)
	}
}

func TestRetryUserInputPlainModifierAndNoModifier(t *testing.T) {
	plain := retryUserInput(llm.Message{Role: "user", Content: "original"}, "be concise")
	if plain.Text != "original\n\nbe concise" || len(plain.Parts) != 0 {
		t.Fatalf("plain retry = %#v", plain)
	}
	unchanged := retryUserInput(llm.Message{Role: "user", Content: "original"}, "")
	if unchanged.Text != "original" || len(unchanged.Parts) != 0 {
		t.Fatalf("plain retry without modifier = %#v", unchanged)
	}
}

func TestRetryUserInputModifierAppendsPartWithoutMutatingOriginal(t *testing.T) {
	original := make([]llm.ContentPart, 2, 5)
	original[0] = llm.ContentPart{Type: "text", Text: "caption"}
	original[1] = llm.ContentPart{Type: "image_url", ImageURL: &llm.ImageURLPart{URL: "data:image/png;base64,abc"}}
	message := llm.Message{Role: "user", Content: "caption", Parts: original}
	withModifier := retryUserInput(message, "describe details")
	if withModifier.Text != "caption\n\ndescribe details" {
		t.Fatalf("retry Text = %q", withModifier.Text)
	}
	if len(withModifier.Parts) != 3 || withModifier.Parts[0].Text != "caption" || withModifier.Parts[1].Type != "image_url" || withModifier.Parts[1].ImageURL.URL != "data:image/png;base64,abc" {
		t.Fatalf("retry did not preserve original text/image: %#v", withModifier.Parts)
	}
	if withModifier.Parts[2].Type != "text" || withModifier.Parts[2].Text != "describe details" {
		t.Fatalf("modifier not appended as text part: %#v", withModifier.Parts)
	}
	if len(original) != 2 || original[0].Text != "caption" || original[1].Type != "image_url" {
		t.Fatalf("retry append mutated original parts slice: %#v", original)
	}

	withoutModifier := retryUserInput(message, "")
	if len(withoutModifier.Parts) != len(original) || withoutModifier.Parts[0].Text != original[0].Text || withoutModifier.Parts[1].ImageURL.URL != original[1].ImageURL.URL {
		t.Fatalf("retry without modifier did not preserve parts: %#v", withoutModifier.Parts)
	}
}

func imPartTexts(parts []llm.ContentPart) []string {
	var text []string
	for _, part := range parts {
		if part.Type == "text" {
			text = append(text, part.Text)
		}
	}
	return text
}

type imCaptureChat struct{ calls [][]llm.Message }

func (c *imCaptureChat) APIKey() string  { return "" }
func (c *imCaptureChat) BaseURL() string { return "" }
func (c *imCaptureChat) ChatCompletion(_ context.Context, messages []llm.Message, _ []llm.ToolDef, _ float64) (*llm.ChatResponse, error) {
	c.capture(messages)
	return &llm.ChatResponse{Choices: []llm.Choice{{Message: imFinishMessage()}}}, nil
}
func (c *imCaptureChat) ChatCompletionStream(_ context.Context, messages []llm.Message, _ []llm.ToolDef, _ float64, _ llm.StreamCallback, _ llm.ToolCallStreamCb) (*llm.Message, *llm.Usage, error) {
	c.capture(messages)
	m := imFinishMessage()
	return &m, nil, nil
}
func (c *imCaptureChat) capture(messages []llm.Message) {
	copyMessages := append([]llm.Message(nil), messages...)
	c.calls = append(c.calls, copyMessages)
}
func imFinishMessage() llm.Message {
	args, _ := json.Marshal(map[string]string{"status": "success", "summary": "ok"})
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "finish-1", Type: "function", Function: llm.FunctionCall{Name: "finish_task", Arguments: string(args)}}}}
}

func TestRunInputSubmitsNormalizedPartsWithoutSchedulingDependency(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.toml")
	if err := os.WriteFile(configPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workspace = tempDir
	cfg.WorkDir = cfg.Workspace
	cfg.Agent.Mode = "full"
	chat := &imCaptureChat{}
	registry := tools.NewRegistry()
	finish := &tools.FinishTool{}
	registry.Register(finish)
	ag := agent.NewAgent(cfg, chat, registry, finish, nil, false)
	t.Cleanup(ag.ClearHistory)
	ag.Mode.InjectContext = false
	in := normalizeIMInput(context.Background(), channel.IncomingMessage{Text: "hello", Attachments: []attachment.Attachment{imAttachment("note.txt", "text/plain", "payload")}}, cfg, ag.TmpManager())
	if _, err := ag.RunInput(context.Background(), in, nil); err != nil {
		t.Fatalf("RunInput: %v", err)
	}
	if len(chat.calls) != 1 {
		t.Fatalf("chat calls = %d", len(chat.calls))
	}
	var user *llm.Message
	for i := range chat.calls[0] {
		if chat.calls[0][i].Role == "user" {
			user = &chat.calls[0][i]
			break
		}
	}
	if user == nil || len(user.Parts) < len(in.Parts) {
		t.Fatalf("normalized parts did not reach model: user=%#v input=%#v", user, in.Parts)
	}
	for i := range in.Parts {
		if user.Parts[i].Type != in.Parts[i].Type || user.Parts[i].Text != in.Parts[i].Text || (user.Parts[i].ImageURL != nil && in.Parts[i].ImageURL != nil && user.Parts[i].ImageURL.URL != in.Parts[i].ImageURL.URL) {
			t.Fatalf("normalized part %d changed before model: got %#v want %#v", i, user.Parts[i], in.Parts[i])
		}
	}
	joined := strings.Join(imPartTexts(user.Parts), " ")
	if !strings.Contains(joined, "hello") || !strings.Contains(joined, "payload") {
		t.Fatalf("model user message lacks normalized content: %#v", user.Parts)
	}
}
