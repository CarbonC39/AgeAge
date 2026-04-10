package channel

import (
	"bytes"
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
	mu             sync.Mutex
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
	}
}

// isAllowedUser returns true when AllowedUsers is empty (allow all) or the
// given userID is present in the whitelist.
func (t *TelegramChannel) isAllowedUser(userID string) bool {
	if len(t.AllowedUsers) == 0 {
		return true
	}
	for _, id := range t.AllowedUsers {
		if id == userID {
			return true
		}
	}
	return false
}

func (t *TelegramChannel) Name() string { return "telegram" }

// Start begins long-polling for Telegram updates.
func (t *TelegramChannel) Start(handler MessageHandler) error {
	me, err := t.getMe()
	if err != nil {
		fmt.Printf("[Telegram] Warning: could not get bot info: %s\n", err)
	} else {
		t.botID = me.ID
		fmt.Printf("[Telegram] Bot ID: %d\n", t.botID)
	}

	// Drain all pending updates so they are not processed after a restart.
	// Uses timeout=0 (non-blocking) and loops until no more batches remain.
	// Retries on transient errors so a brief network hiccup cannot leave
	// offset=0 and cause old messages to be re-delivered.
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
				t.answerCallbackQuery(cq.ID) // dismiss the loading spinner immediately
				if t.AnswerCallback != nil && cq.Message != nil {
					senderID := fmt.Sprintf("%d", cq.From.ID)
					if t.isAllowedUser(senderID) {
						chatID := fmt.Sprintf("%d", cq.Message.Chat.ID)
						t.AnswerCallback(chatID, cq.Data)
					}
				}
				continue
			}

			if update.Message == nil || update.Message.Text == "" {
				continue
			}

			isBot := update.Message.From.ID == t.botID
			if isBot {
				continue
			}

			senderID := fmt.Sprintf("%d", update.Message.From.ID)
			if !t.isAllowedUser(senderID) {
				continue // silently ignore non-whitelisted users
			}

			msg := IncomingMessage{
				ChannelType: "telegram",
				ChannelID:   fmt.Sprintf("%d", update.Message.Chat.ID),
				SenderID:    senderID,
				SenderName:  update.Message.From.FirstName,
				Text:        update.Message.Text,
				ReplyTo:     fmt.Sprintf("%d", update.Message.MessageID),
			}

			process := func(m IncomingMessage) {
				reply := handler(m)
				if reply != "" {
					t.Reply(m.ChannelID, m.ReplyTo, reply)
				}
			}

			// Always run in a goroutine to avoid blocking the polling loop.
			// Serialization is handled by the handler/manager if needed.
			go process(msg)
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
func (t *TelegramChannel) SendMessage(chatID, text string) (string, error) {
	msgID, err := t.doSendMessageWithID(chatID, text, 0)
	return msgID, err
}

// EditMessage edits a previously sent Telegram message using editMessageText.
func (t *TelegramChannel) EditMessage(chatID, messageID, text string) error {
	var msgIDInt int
	fmt.Sscanf(messageID, "%d", &msgIDInt)
	if msgIDInt == 0 {
		return fmt.Errorf("invalid message ID: %q", messageID)
	}

	// Try with Markdown first, fall back to plain text.
	if err := t.doEditMessage(chatID, msgIDInt, text, "Markdown"); err != nil {
		return t.doEditMessage(chatID, msgIDInt, text, "")
	}
	return nil
}

func (t *TelegramChannel) doEditMessage(chatID string, messageID int, text, parseMode string) error {
	payload := map[string]interface{}{
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

// Reply sends a Markdown-formatted reply to a Telegram message.
func (t *TelegramChannel) Reply(chatID, replyToID, text string) error {
	var replyTo int
	fmt.Sscanf(replyToID, "%d", &replyTo)
	return t.sendMessage(chatID, text, replyTo)
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
	MessageID int    `json:"message_id"`
	Text      string `json:"text"`
	Chat      tgChat `json:"chat"`
	From      tgUser `json:"from"`
}

type tgChat struct {
	ID int64 `json:"id"`
}

type tgUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
}

type tgResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

// drainPendingUpdates consumes all updates that are already queued on the
// Telegram server so they are not processed after a restart. It uses
// timeout=0 (non-blocking) and loops until the server returns an empty
// batch. Retries indefinitely on transient errors so that a brief network
// hiccup cannot leave offset=0 and cause old messages to be re-delivered.
// Returns the next offset to use in the main polling loop.
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

// sendMessage sends a message with Markdown formatting and discards the returned ID.
func (t *TelegramChannel) sendMessage(chatID, text string, replyTo int) error {
	_, err := t.doSendMessageWithID(chatID, text, replyTo)
	return err
}

// doSendMessageWithID sends the first chunk with Markdown fallback and returns the message ID.
func (t *TelegramChannel) doSendMessageWithID(chatID, text string, replyTo int) (string, error) {
	chunks := splitTextChunks(text, 4000)
	var firstID string
	for i, chunk := range chunks {
		msgID, err := t.doSendChunk(chatID, chunk, replyTo, "Markdown")
		if err != nil {
			msgID, err = t.doSendChunk(chatID, chunk, replyTo, "")
			if err != nil {
				return firstID, err
			}
		}
		if i == 0 {
			firstID = msgID
		}
		replyTo = 0 // Only reply to the first chunk.
	}
	return firstID, nil
}

func (t *TelegramChannel) doSendChunk(chatID, text string, replyTo int, parseMode string) (string, error) {
	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	if replyTo > 0 {
		payload["reply_parameters"] = map[string]interface{}{
			"message_id": replyTo,
		}
	}

	data, _ := json.Marshal(payload)
	resp, err := t.client.Post(
		t.baseURL+"/sendMessage",
		"application/json",
		bytes.NewReader(data),
	)
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

// answerCallbackQuery acknowledges a Telegram callback query to dismiss the
// loading spinner shown on the button. Errors are silently ignored.
func (t *TelegramChannel) answerCallbackQuery(callbackQueryID string) {
	payload := map[string]interface{}{"callback_query_id": callbackQueryID}
	data, _ := json.Marshal(payload)
	resp, err := t.client.Post(t.baseURL+"/answerCallbackQuery", "application/json", bytes.NewReader(data))
	if err == nil {
		resp.Body.Close()
	}
}

// SendQuestion sends a question to a Telegram chat. When options are provided
// they are rendered as a single-column inline keyboard; otherwise plain text.
func (t *TelegramChannel) SendQuestion(channelID, question string, options []string) error {
	payload := map[string]interface{}{
		"chat_id": channelID,
		"text":    "❓ " + question,
	}
	if len(options) > 0 {
		rows := make([][]tgInlineButton, len(options))
		for i, opt := range options {
			rows[i] = []tgInlineButton{{Text: opt, CallbackData: opt}}
		}
		payload["reply_markup"] = map[string]interface{}{
			"inline_keyboard": rows,
		}
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
// preferring to split at newlines.
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

		// Find the last newline within maxLen.
		splitAt := strings.LastIndex(text[:maxLen], "\n")
		if splitAt < maxLen/2 {
			splitAt = maxLen // Force split.
		}

		chunks = append(chunks, text[:splitAt])
		text = text[splitAt:]
		text = strings.TrimLeft(text, "\n")
	}

	return chunks
}
