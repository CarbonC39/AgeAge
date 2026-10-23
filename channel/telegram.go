package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ageage/attachment"
	"ageage/config"
)

// TelegramChannel connects to Telegram via Bot API (long polling).
type TelegramChannel struct {
	Token        string
	AllowedUsers []string // Telegram user IDs allowed to interact; empty = allow all
	Options      ChannelOptions
	// AnswerCallback receives the full interaction identity for an inline
	// keyboard answer so callers can scope it to the pending request.
	AnswerCallback func(channelID, threadID, senderID, answer string)
	baseURL        string
	client         *http.Client
	stopCh         chan struct{}
	botID          int64
	botUsername    string // "@botname" (without @), used for mention detection
	mu             sync.Mutex
	typingStop     map[string]telegramTypingState // channelID → active keep-alive state
	typingMu       sync.Mutex
}

type telegramTypingState struct {
	cancel context.CancelFunc
	refs   int
}

// NewTelegram creates a new Telegram channel.
func NewTelegram(botToken string, allowedUsers []string, opts ChannelOptions) *TelegramChannel {
	return &TelegramChannel{
		Token:        botToken,
		AllowedUsers: allowedUsers,
		Options:      opts,
		baseURL:      fmt.Sprintf("https://api.telegram.org/bot%s", botToken),
		client:       &http.Client{Timeout: 60 * time.Second},
		stopCh:       make(chan struct{}),
		typingStop:   make(map[string]telegramTypingState),
	}
}

// isAllowedUser returns true when AllowedUsers is empty (allow all) or any of
// the given candidates matches an AllowedUsers entry.
// Candidates are tried in order: typically (numericID, username).
// Matching is normalised: leading/trailing spaces and a leading "@" are stripped
// before comparison so both "123456789" and "@johndoe" or "johndoe" work.
func (t *TelegramChannel) isAllowedUser(candidates ...string) bool {
	if len(t.AllowedUsers) == 0 {
		return true
	}
	normalize := func(s string) string {
		return strings.TrimPrefix(strings.TrimSpace(s), "@")
	}
	for _, entry := range t.AllowedUsers {
		entry = normalize(entry)
		if entry == "" {
			continue
		}
		for _, c := range candidates {
			if normalize(c) == entry {
				return true
			}
		}
	}
	return false
}

func (t *TelegramChannel) Name() string { return config.ChannelTelegram }

// Start begins long-polling for Telegram updates.
func (t *TelegramChannel) Start(handler MessageHandler) error {
	if len(t.AllowedUsers) == 0 {
		fmt.Println("[Telegram] WARN: allowed_users is not configured — group chat messages will be denied. Set allowed_users in config to grant access.")
	}

	me, err := t.getMe()
	if err != nil {
		fmt.Printf("[Telegram] Warning: could not get bot info: %s\n", err)
	} else {
		t.botID = me.ID
		t.botUsername = me.Username
		fmt.Printf("[Telegram] Bot ID: %d, username: @%s\n", t.botID, t.botUsername)
	}

	// Drain all pending updates so they are not processed after a restart.
	offset := t.drainPendingUpdates()

	for {
		select {
		case <-t.stopCh:
			return nil
		default:
		}

		updates, err := t.getUpdates(offset, 30)
		if err != nil {
			fmt.Printf("[Telegram] Error polling: %s\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for _, update := range updates {
			offset = update.UpdateID + 1

			// Handle inline-keyboard button presses.
			if update.CallbackQuery != nil {
				cq := update.CallbackQuery
				t.answerCallbackQuery(cq.ID)
				if t.AnswerCallback != nil && cq.Message != nil {
					senderID := fmt.Sprintf("%d", cq.From.ID)
					if t.isAllowedUser(senderID, cq.From.Username) {
						chatID := fmt.Sprintf("%d", cq.Message.Chat.ID)
						threadID := ""
						if cq.Message.MessageThreadID != 0 {
							threadID = fmt.Sprintf("%d", cq.Message.MessageThreadID)
						}
						t.AnswerCallback(chatID, threadID, senderID, cq.Data)
					}
				}
				continue
			}

			if update.Message == nil {
				continue
			}

			incoming, ok := t.parseIncoming(update.Message)
			if !ok {
				continue
			}
			go func(msg IncomingMessage, topicID int) {
				reply := handler(msg)
				if reply != "" {
					// Fallback when the handler returns a string instead of calling Respond.
					var msgIDInt int
					fmt.Sscanf(msg.ReplyTo, "%d", &msgIDInt)
					_ = t.sendInTopic(msg.ChannelID, topicID, msgIDInt, reply)
				}
			}(incoming, update.Message.MessageThreadID)
		}
	}
}

// parseIncoming extracts text and lazy media streams from a Telegram message.
func (t *TelegramChannel) parseIncoming(message *tgMessage) (IncomingMessage, bool) {
	if message == nil || message.From.ID == t.botID {
		return IncomingMessage{}, false
	}
	text := message.Text
	if text == "" {
		text = message.Caption
	}
	attachments := t.telegramAttachments(message)
	if text == "" && len(attachments) == 0 {
		return IncomingMessage{}, false
	}
	isGroup := message.Chat.Type != "private"
	senderID := fmt.Sprintf("%d", message.From.ID)
	if isGroup && len(t.AllowedUsers) == 0 || !t.isAllowedUser(senderID, message.From.Username) {
		return IncomingMessage{}, false
	}
	mentionTag := "@" + t.botUsername
	hasMention := t.botUsername != "" && strings.Contains(text, mentionTag)
	isReplyToBot := message.ReplyToMessage != nil && t.botID != 0 && message.ReplyToMessage.From.ID == t.botID
	botMentioned := hasMention || isReplyToBot || message.MessageThreadID != 0
	if hasMention {
		text = strings.TrimSpace(strings.ReplaceAll(text, mentionTag, ""))
	}
	threadID := ""
	if message.MessageThreadID != 0 {
		threadID = fmt.Sprintf("%d", message.MessageThreadID)
	}
	out := IncomingMessage{ChannelType: config.ChannelTelegram, ChannelID: fmt.Sprintf("%d", message.Chat.ID), SenderID: senderID, SenderName: message.From.FirstName, Text: text, Attachments: attachments, ReplyTo: fmt.Sprintf("%d", message.MessageID), ThreadID: threadID, IsGroupChat: isGroup, BotMentioned: botMentioned}
	chatID, msgID, topicID := out.ChannelID, message.MessageID, message.MessageThreadID
	out.Respond = func(replyText string) error { return t.sendInTopic(chatID, topicID, msgID, replyText) }
	return out, true
}

func (t *TelegramChannel) telegramAttachments(message *tgMessage) []attachment.Attachment {
	var out []attachment.Attachment
	if n := len(message.Photo); n > 0 {
		var best tgPhotoSize
		for _, photo := range message.Photo {
			if photo.FileID == "" || photo.ParseError != "" {
				continue
			}
			if best.FileID == "" || photo.Width*photo.Height > best.Width*best.Height || photo.Width*photo.Height == best.Width*best.Height && photo.FileSize > best.FileSize {
				best = photo
			}
		}
		if best.FileID != "" {
			out = append(out, t.telegramAttachment(best.FileID, "photo.jpg", "image/jpeg", best.FileSize))
		} else {
			out = append(out, attachment.Attachment{Filename: "photo.jpg", MIMEType: "image/jpeg", UnsupportedReason: "invalid Telegram photo metadata"})
		}
	}
	if message.Document != nil {
		name := message.Document.FileName
		if name == "" {
			name = "document"
		}
		if message.Document.ParseError != "" {
			out = append(out, attachment.Attachment{Filename: name, MIMEType: message.Document.MIMEType, Size: message.Document.FileSize, UnsupportedReason: "invalid Telegram document metadata: " + message.Document.ParseError})
		} else if message.Document.FileID != "" {
			out = append(out, t.telegramAttachment(message.Document.FileID, name, message.Document.MIMEType, message.Document.FileSize))
		} else {
			out = append(out, attachment.Attachment{Filename: name, MIMEType: message.Document.MIMEType, Size: message.Document.FileSize, UnsupportedReason: "Telegram document file_id is missing"})
		}
	}
	return out
}

func (t *TelegramChannel) telegramAttachment(fileID, filename, mime string, size int64) attachment.Attachment {
	return attachment.Attachment{Filename: filename, MIMEType: mime, Size: size, Open: func(ctx context.Context) (io.ReadCloser, error) {
		filePath, err := t.getFilePath(ctx, fileID)
		if err != nil {
			return nil, err
		}
		return t.openTelegramFile(ctx, filePath)
	}}
}

type tgFile struct {
	FilePath string `json:"file_path"`
}

func (t *TelegramChannel) getFilePath(ctx context.Context, fileID string) (string, error) {
	u, err := url.Parse(t.baseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("invalid Telegram API endpoint")
	}
	reqURL := strings.TrimRight(t.baseURL, "/") + "/getFile?file_id=" + url.QueryEscape(fileID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("invalid Telegram file request")
	}
	resp, err := mediaHTTPClient(t.client, u).Do(req)
	if err != nil {
		return "", fmt.Errorf("Telegram getFile request failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return "", fmt.Errorf("Telegram getFile failed (HTTP %d)", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	resp.Body.Close()
	if err != nil || len(body) > 1<<20 {
		return "", fmt.Errorf("Telegram getFile response invalid")
	}
	var result struct {
		OK     bool   `json:"ok"`
		Result tgFile `json:"result"`
	}
	if json.Unmarshal(body, &result) != nil || !result.OK || result.Result.FilePath == "" {
		return "", fmt.Errorf("Telegram getFile response invalid")
	}
	return result.Result.FilePath, nil
}

func (t *TelegramChannel) openTelegramFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	u, err := url.Parse(t.baseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid Telegram API endpoint")
	}
	path := strings.TrimLeft(filePath, "/")
	if path == "" || strings.ContainsAny(path, "\\?#") {
		return nil, fmt.Errorf("invalid Telegram file path")
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return nil, fmt.Errorf("invalid Telegram file path")
		}
	}
	basePath := strings.TrimRight(u.EscapedPath(), "/")
	// Telegram's file API mirrors the token path below /file.
	u.Path = "/file" + basePath + "/" + path
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("invalid Telegram file request")
	}
	resp, err := mediaHTTPClient(t.client, u).Do(req)
	if err != nil {
		return nil, fmt.Errorf("Telegram file download failed")
	}
	if err := checkMediaStatus(resp); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (t *TelegramChannel) Stop() error {
	t.typingMu.Lock()
	for channelID, state := range t.typingStop {
		state.cancel()
		delete(t.typingStop, channelID)
	}
	t.typingMu.Unlock()
	close(t.stopCh)
	return nil
}

// Send sends a Markdown-formatted message to a Telegram chat.
func (t *TelegramChannel) Send(chatID, text string) error {
	_, err := t.SendMessage(chatID, text)
	return err
}

// SendMessage sends a message and returns the Telegram message ID as a string.
// Implements the Editable interface.
func (t *TelegramChannel) SendMessage(chatID, text string) (string, error) {
	return t.doSendMessageWithID(chatID, text, 0, 0)
}

// SendMessageInThread sends an editable progress/todo message inside a
// Telegram forum topic. Implements ThreadEditable.
func (t *TelegramChannel) SendMessageInThread(chatID, threadRootID, latestEventID, text string) (string, error) {
	var topicID, replyTo int
	fmt.Sscanf(threadRootID, "%d", &topicID)
	fmt.Sscanf(latestEventID, "%d", &replyTo)
	if topicID == 0 {
		return "", fmt.Errorf("invalid Telegram topic ID: %q", threadRootID)
	}
	return t.doSendMessageWithID(chatID, text, replyTo, topicID)
}

// EditMessage edits a previously sent Telegram message using editMessageText.
// Implements the Editable interface.
func (t *TelegramChannel) EditMessage(chatID, messageID, text string) error {
	var msgIDInt int
	fmt.Sscanf(messageID, "%d", &msgIDInt)
	if msgIDInt == 0 {
		return fmt.Errorf("invalid message ID: %q", messageID)
	}
	if err := t.doEditMessage(chatID, msgIDInt, text, "Markdown"); err != nil {
		return t.doEditMessage(chatID, msgIDInt, text, "")
	}
	return nil
}

// Reply sends a Markdown-formatted reply to a Telegram message.
func (t *TelegramChannel) Reply(chatID, replyToID, text string) error {
	var replyTo int
	fmt.Sscanf(replyToID, "%d", &replyTo)
	return t.sendInTopic(chatID, 0, replyTo, text)
}

// SendTyping shows a typing indicator in the given chat, kept alive until
// called again with typing=false. Implements TypingIndicator.
func (t *TelegramChannel) SendTyping(channelID string, typing bool) error {
	if typing {
		t.typingMu.Lock()
		if state, ok := t.typingStop[channelID]; ok {
			state.refs++
			t.typingStop[channelID] = state
			t.typingMu.Unlock()
			return nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.typingStop[channelID] = telegramTypingState{cancel: cancel, refs: 1}
		t.typingMu.Unlock()
		go func() {
			for {
				t.sendChatAction(channelID, "typing")
				select {
				case <-ctx.Done():
					return
				case <-time.After(4 * time.Second):
				}
			}
		}()
	} else {
		t.typingMu.Lock()
		if state, ok := t.typingStop[channelID]; ok {
			if state.refs > 1 {
				state.refs--
				t.typingStop[channelID] = state
				t.typingMu.Unlock()
				return nil
			}
			state.cancel()
			delete(t.typingStop, channelID)
		}
		t.typingMu.Unlock()
	}
	return nil
}

// React sets an emoji reaction on a Telegram message.
// Returns a reaction key of the form "chatID|messageID|emoji" for Unreact.
// Implements Reactor. Note: Telegram only allows a limited set of emoji reactions.
func (t *TelegramChannel) React(channelID, eventID, emoji string) (string, error) {
	var msgID int
	fmt.Sscanf(eventID, "%d", &msgID)
	if msgID == 0 {
		return "", fmt.Errorf("invalid message ID: %q", eventID)
	}
	payload := map[string]any{
		"chat_id":    channelID,
		"message_id": msgID,
		"reaction":   []map[string]any{{"type": "emoji", "emoji": emoji}},
		"is_big":     false,
	}
	data, _ := json.Marshal(payload)
	resp, err := t.client.Post(t.baseURL+"/setMessageReaction", "application/json", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	return channelID + "|" + eventID + "|" + emoji, nil
}

// Unreact removes an emoji reaction previously set by React.
// Implements Reactor.
func (t *TelegramChannel) Unreact(channelID, reactionKey string) error {
	parts := strings.SplitN(reactionKey, "|", 3)
	if len(parts) != 3 {
		return fmt.Errorf("invalid reaction key: %q", reactionKey)
	}
	chatID, msgIDStr := parts[0], parts[1]
	var msgID int
	fmt.Sscanf(msgIDStr, "%d", &msgID)
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": msgID,
		"reaction":   []any{}, // empty list = remove reaction
	}
	data, _ := json.Marshal(payload)
	resp, err := t.client.Post(t.baseURL+"/setMessageReaction", "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// sendInTopic sends a message in a supergroup topic (if topicID > 0) or as a
// plain reply (if topicID == 0). It handles multi-chunk splitting.
func (t *TelegramChannel) sendInTopic(chatID string, topicID, replyTo int, text string) error {
	_, err := t.doSendMessageWithID(chatID, text, replyTo, topicID)
	return err
}

// --- Telegram API types ---

type tgUpdate struct {
	UpdateID      int              `json:"update_id"`
	Message       *tgMessage       `json:"message"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}

type tgCallbackQuery struct {
	ID      string     `json:"id"`
	From    tgUser     `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

type tgInlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type tgMessage struct {
	MessageID       int           `json:"message_id"`
	Text            string        `json:"text"`
	Caption         string        `json:"caption"` // Text for photo/document/voice messages
	Chat            tgChat        `json:"chat"`
	From            tgUser        `json:"from"`
	MessageThreadID int           `json:"message_thread_id"` // Supergroup topic ID; 0 if none
	ReplyToMessage  *tgMessage    `json:"reply_to_message"`  // Non-nil when this is a reply
	Photo           []tgPhotoSize `json:"photo"`
	Document        *tgDocument   `json:"document"`
}

type tgPhotoSize struct {
	FileID     string `json:"file_id"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	FileSize   int64  `json:"file_size"`
	ParseError string `json:"-"`
}

type tgDocument struct {
	FileID     string `json:"file_id"`
	FileName   string `json:"file_name"`
	MIMEType   string `json:"mime_type"`
	FileSize   int64  `json:"file_size"`
	ParseError string `json:"-"`
}

func (p *tgPhotoSize) UnmarshalJSON(data []byte) error {
	var raw struct {
		FileID   json.RawMessage `json:"file_id"`
		Width    json.RawMessage `json:"width"`
		Height   json.RawMessage `json:"height"`
		FileSize json.RawMessage `json:"file_size"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		p.ParseError = "invalid photo object"
		return nil
	}
	var problems []string
	var err error
	if p.FileID, err = parseJSONString(raw.FileID); err != nil {
		problems = append(problems, "file_id")
	}
	if p.Width, err = parseJSONInt(raw.Width); err != nil {
		problems = append(problems, "width")
	}
	if p.Height, err = parseJSONInt(raw.Height); err != nil {
		problems = append(problems, "height")
	}
	if raw.FileSize != nil {
		if p.FileSize, err = parseJSONInt64(raw.FileSize); err != nil {
			problems = append(problems, "file_size")
		}
	}
	p.ParseError = strings.Join(problems, ", ")
	return nil
}

func (d *tgDocument) UnmarshalJSON(data []byte) error {
	var raw struct {
		FileID   json.RawMessage `json:"file_id"`
		FileName json.RawMessage `json:"file_name"`
		MIMEType json.RawMessage `json:"mime_type"`
		FileSize json.RawMessage `json:"file_size"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		d.ParseError = "invalid document object"
		return nil
	}
	var err error
	if d.FileID, err = parseJSONString(raw.FileID); err != nil {
		d.ParseError = "file_id"
	}
	d.FileName, _ = parseJSONString(raw.FileName)
	d.MIMEType, _ = parseJSONString(raw.MIMEType)
	if raw.FileSize != nil {
		if d.FileSize, err = parseJSONInt64(raw.FileSize); err != nil {
			d.ParseError = "file_size"
		}
	}
	return nil
}

func parseJSONString(raw json.RawMessage) (string, error) {
	var value string
	if len(raw) == 0 {
		return "", nil
	}
	err := json.Unmarshal(raw, &value)
	return value, err
}
func parseJSONInt(raw json.RawMessage) (int, error) {
	var value int
	if len(raw) == 0 {
		return 0, nil
	}
	err := json.Unmarshal(raw, &value)
	return value, err
}
func parseJSONInt64(raw json.RawMessage) (int64, error) {
	var value int64
	err := json.Unmarshal(raw, &value)
	return value, err
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // "private", "group", "supergroup", "channel"
}

type tgUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type tgResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

// drainPendingUpdates consumes queued updates so they are not replayed on restart.
func (t *TelegramChannel) drainPendingUpdates() int {
	offset := 0
	total := 0
	for {
		select {
		case <-t.stopCh:
			return offset
		default:
		}
		updates, err := t.getUpdates(offset, 0)
		if err != nil {
			fmt.Printf("[Telegram] Error draining pending updates: %s — retrying\n", err)
			time.Sleep(3 * time.Second)
			continue
		}
		if len(updates) == 0 {
			break
		}
		offset = updates[len(updates)-1].UpdateID + 1
		total += len(updates)
	}
	if total > 0 {
		fmt.Printf("[Telegram] Skipped %d historical message(s)\n", total)
	}
	return offset
}

func (t *TelegramChannel) getUpdates(offset, timeoutSec int) ([]tgUpdate, error) {
	url := fmt.Sprintf("%s/getUpdates?offset=%d&timeout=%d", t.baseURL, offset, timeoutSec)
	resp, err := t.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var tgResp tgResponse
	if err := json.Unmarshal(body, &tgResp); err != nil {
		return nil, fmt.Errorf("failed to parse Telegram response: %w", err)
	}
	if !tgResp.OK {
		return nil, fmt.Errorf("Telegram API error: %s", tgResp.Description)
	}
	var updates []tgUpdate
	if err := json.Unmarshal(tgResp.Result, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

func (t *TelegramChannel) getMe() (*tgUser, error) {
	resp, err := t.client.Get(t.baseURL + "/getMe")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var tgResp tgResponse
	if err := json.Unmarshal(body, &tgResp); err != nil {
		return nil, fmt.Errorf("failed to parse Telegram response: %w", err)
	}
	if !tgResp.OK {
		return nil, fmt.Errorf("Telegram API error: %s", tgResp.Description)
	}
	var user tgUser
	if err := json.Unmarshal(tgResp.Result, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

// doSendMessageWithID sends text (split into chunks if needed) and returns the
// first chunk's message ID. topicID > 0 routes the message into a supergroup topic.
func (t *TelegramChannel) doSendMessageWithID(chatID, text string, replyTo, topicID int) (string, error) {
	chunks := splitTextChunks(text, 4000)
	var firstID string
	for i, chunk := range chunks {
		msgID, err := t.doSendChunk(chatID, chunk, replyTo, topicID, "Markdown")
		if err != nil {
			msgID, err = t.doSendChunk(chatID, chunk, replyTo, topicID, "")
			if err != nil {
				return firstID, err
			}
		}
		if i == 0 {
			firstID = msgID
		}
		replyTo = 0 // Only reply-pin to the first chunk.
	}
	return firstID, nil
}

func (t *TelegramChannel) doSendChunk(chatID, text string, replyTo, topicID int, parseMode string) (string, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	if topicID > 0 {
		payload["message_thread_id"] = topicID
	}
	if replyTo > 0 {
		payload["reply_parameters"] = map[string]any{
			"message_id": replyTo,
		}
	}

	data, _ := json.Marshal(payload)
	resp, err := t.client.Post(t.baseURL+"/sendMessage", "application/json", bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("failed to send Telegram message: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var tgResp tgResponse
	if err := json.Unmarshal(body, &tgResp); err != nil {
		return "", err
	}
	if !tgResp.OK {
		return "", fmt.Errorf("Telegram sendMessage failed: %s", tgResp.Description)
	}
	var msg tgMessage
	if err := json.Unmarshal(tgResp.Result, &msg); err == nil && msg.MessageID != 0 {
		return fmt.Sprintf("%d", msg.MessageID), nil
	}
	return "", nil
}

func (t *TelegramChannel) doEditMessage(chatID string, messageID int, text, parseMode string) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	data, _ := json.Marshal(payload)
	resp, err := t.client.Post(t.baseURL+"/editMessageText", "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to edit Telegram message: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var tgResp tgResponse
	if err := json.Unmarshal(body, &tgResp); err != nil {
		return err
	}
	if !tgResp.OK {
		return fmt.Errorf("Telegram editMessageText failed: %s", tgResp.Description)
	}
	return nil
}

// sendChatAction sends a chat action (e.g. "typing") to the Telegram API.
func (t *TelegramChannel) sendChatAction(chatID, action string) {
	payload := map[string]any{"chat_id": chatID, "action": action}
	data, _ := json.Marshal(payload)
	resp, err := t.client.Post(t.baseURL+"/sendChatAction", "application/json", bytes.NewReader(data))
	if err == nil {
		resp.Body.Close()
	}
}

// answerCallbackQuery acknowledges a Telegram callback query.
func (t *TelegramChannel) answerCallbackQuery(callbackQueryID string) {
	payload := map[string]any{"callback_query_id": callbackQueryID}
	data, _ := json.Marshal(payload)
	resp, err := t.client.Post(t.baseURL+"/answerCallbackQuery", "application/json", bytes.NewReader(data))
	if err == nil {
		resp.Body.Close()
	}
}

// SendQuestion sends a question to a Telegram chat. When options are provided
// they are rendered as a single-column inline keyboard; otherwise plain text.
// Implements InteractiveChannel.
func (t *TelegramChannel) SendQuestion(channelID, question string, options []string) error {
	payload := map[string]any{
		"chat_id": channelID,
		"text":    "❓ " + question,
	}
	if len(options) > 0 {
		rows := make([][]tgInlineButton, len(options))
		for i, opt := range options {
			rows[i] = []tgInlineButton{{Text: opt, CallbackData: opt}}
		}
		payload["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	data, _ := json.Marshal(payload)
	resp, err := t.client.Post(t.baseURL+"/sendMessage", "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to send Telegram question: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var tgResp tgResponse
	if err := json.Unmarshal(body, &tgResp); err != nil {
		return err
	}
	if !tgResp.OK {
		return fmt.Errorf("Telegram sendMessage failed: %s", tgResp.Description)
	}
	return nil
}

// splitTextChunks splits text into chunks of at most maxLen characters,
// preferring to split at paragraph breaks then newlines.
func splitTextChunks(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		chunk := text[:maxLen]
		// Prefer paragraph break.
		if idx := strings.LastIndex(chunk, "\n\n"); idx > maxLen/2 {
			chunks = append(chunks, text[:idx])
			text = strings.TrimLeft(text[idx:], "\n")
			continue
		}
		// Fall back to any newline.
		if idx := strings.LastIndex(chunk, "\n"); idx > maxLen/2 {
			chunks = append(chunks, text[:idx])
			text = strings.TrimLeft(text[idx:], "\n")
			continue
		}
		chunks = append(chunks, chunk)
		text = text[maxLen:]
	}
	return chunks
}
