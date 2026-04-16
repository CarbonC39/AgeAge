package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TelegramChannel connects to Telegram via Bot API (long polling).
type TelegramChannel struct {
	Token          string
	AllowedUsers   []string // Telegram user IDs allowed to interact; empty = allow all
	Options        ChannelOptions
	AnswerCallback func(channelID, answer string) // Optional: called when an inline-keyboard button is pressed
	baseURL        string
	client         *http.Client
	stopCh         chan struct{}
	botID          int64
	botUsername    string // "@botname" (without @), used for mention detection
	mu             sync.Mutex
	typingStop     map[string]context.CancelFunc // channelID → cancel func for keep-alive typing
	typingMu       sync.Mutex
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
		typingStop:   make(map[string]context.CancelFunc),
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

func (t *TelegramChannel) Name() string { return "telegram" }

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
						t.AnswerCallback(chatID, cq.Data)
					}
				}
				continue
			}

			if update.Message == nil {
				continue
			}

			// Accept both text messages and media with captions.
			text := update.Message.Text
			if text == "" {
				text = update.Message.Caption
			}
			if text == "" {
				continue
			}

			if update.Message.From.ID == t.botID {
				continue
			}

			isGroup := update.Message.Chat.Type != "private"
			senderID := fmt.Sprintf("%d", update.Message.From.ID)
			username := update.Message.From.Username

			// Security: deny all group messages when no allowlist is configured.
			if isGroup && len(t.AllowedUsers) == 0 {
				continue
			}

			if !t.isAllowedUser(senderID, username) {
				continue
			}

			// Detect whether this message is directed at the bot:
			//   1. @mention — text contains "@botUsername"
			//   2. Reply to bot — the replied-to message was sent by the bot
			//   3. Supergroup topic thread — Telegram only delivers topic messages to bots
			//      that are members of the topic (treated as implicitly directed)
			mentionTag := "@" + t.botUsername
			hasMention := t.botUsername != "" && strings.Contains(text, mentionTag)
			isReplyToBot := update.Message.ReplyToMessage != nil &&
				t.botID != 0 &&
				update.Message.ReplyToMessage.From.ID == t.botID
			botMentioned := hasMention || isReplyToBot || update.Message.MessageThreadID != 0

			// Strip the @mention text only when an explicit @mention is present.
			if hasMention {
				text = strings.ReplaceAll(text, mentionTag, "")
				text = strings.TrimSpace(text)
			}

			chatID := fmt.Sprintf("%d", update.Message.Chat.ID)
			msgID := fmt.Sprintf("%d", update.Message.MessageID)
			threadID := ""
			if update.Message.MessageThreadID != 0 {
				threadID = fmt.Sprintf("%d", update.Message.MessageThreadID)
			}

			incoming := IncomingMessage{
				ChannelType:  "telegram",
				ChannelID:    chatID,
				SenderID:     senderID,
				SenderName:   update.Message.From.FirstName,
				Text:         text,
				ReplyTo:      msgID,
				ThreadID:     threadID,
				IsGroupChat:  isGroup,
				BotMentioned: botMentioned,
			}

			// Capture values for the Respond closure.
			capturedChatID := chatID
			capturedMsgID := update.Message.MessageID
			capturedThreadID := update.Message.MessageThreadID
			incoming.Respond = func(replyText string) error {
				return t.sendInTopic(capturedChatID, capturedThreadID, capturedMsgID, replyText)
			}

			go func(msg IncomingMessage, topicID int) {
				reply := handler(msg)
				if reply != "" {
					// Fallback when the handler returns a string instead of calling Respond.
					var msgIDInt int
					fmt.Sscanf(msg.ReplyTo, "%d", &msgIDInt)
					_ = t.sendInTopic(msg.ChannelID, topicID, msgIDInt, reply)
				}
			}(incoming, capturedThreadID)
		}
	}
}

func (t *TelegramChannel) Stop() error {
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
		ctx, cancel := context.WithCancel(context.Background())
		t.typingMu.Lock()
		if old, ok := t.typingStop[channelID]; ok {
			old()
		}
		t.typingStop[channelID] = cancel
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
		if cancel, ok := t.typingStop[channelID]; ok {
			cancel()
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
	MessageID       int        `json:"message_id"`
	Text            string     `json:"text"`
	Caption         string     `json:"caption"`          // Text for photo/document/voice messages
	Chat            tgChat     `json:"chat"`
	From            tgUser     `json:"from"`
	MessageThreadID int        `json:"message_thread_id"` // Supergroup topic ID; 0 if none
	ReplyToMessage  *tgMessage `json:"reply_to_message"`  // Non-nil when this is a reply
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
