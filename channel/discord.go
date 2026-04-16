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
)

// DiscordChannel connects to Discord via Bot API (REST polling).
type DiscordChannel struct {
	Token         string   // Bot token (without "Bot " prefix)
	ChannelIDs    []string // Channel IDs to monitor
	AllowedUsers  []string // Discord user IDs allowed to interact; empty = allow all
	Options       ChannelOptions
	baseURL       string
	client        *http.Client
	stopCh        chan struct{}
	lastMsgIDs    map[string]string            // Track last seen message per channel
	botUserID     string                       // Bot's own user ID (set in Start)
	groupChannels map[string]bool              // channelID → true if guild channel, false if DM
	mu            sync.Mutex
	typingStop    map[string]context.CancelFunc // channelID → cancel func for keep-alive typing
	typingMu      sync.Mutex
}

// NewDiscord creates a new Discord channel.
func NewDiscord(botToken string, channelIDs []string, allowedUsers []string, opts ChannelOptions) *DiscordChannel {
	return &DiscordChannel{
		Token:         botToken,
		ChannelIDs:    channelIDs,
		AllowedUsers:  allowedUsers,
		Options:       opts,
		baseURL:       "https://discord.com/api/v10",
		client:        &http.Client{Timeout: 30 * time.Second},
		stopCh:        make(chan struct{}),
		lastMsgIDs:    make(map[string]string),
		groupChannels: make(map[string]bool),
		typingStop:    make(map[string]context.CancelFunc),
	}
}

// isAllowedUser returns true when AllowedUsers is empty (allow all) or any of
// the given candidates matches an AllowedUsers entry.
// Candidates are tried in order: typically (snowflakeID, username).
// Matching trims surrounding whitespace; username comparison is case-insensitive.
func (d *DiscordChannel) isAllowedUser(candidates ...string) bool {
	if len(d.AllowedUsers) == 0 {
		return true
	}
	for _, entry := range d.AllowedUsers {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		for _, c := range candidates {
			c = strings.TrimSpace(c)
			if c != "" && strings.EqualFold(entry, c) {
				return true
			}
		}
	}
	return false
}

func (d *DiscordChannel) Name() string { return "discord" }

// getChannelType returns the Discord channel type integer (0=guild text, 1=DM, etc.).
func (d *DiscordChannel) getChannelType(channelID string) (int, error) {
	body, err := d.doRequest("GET", fmt.Sprintf("/channels/%s", channelID), nil)
	if err != nil {
		return 0, err
	}
	var ch struct {
		Type int `json:"type"`
	}
	if err := json.Unmarshal(body, &ch); err != nil {
		return 0, err
	}
	return ch.Type, nil
}

// Start begins polling Discord channels for new messages.
func (d *DiscordChannel) Start(handler MessageHandler) error {
	if len(d.ChannelIDs) == 0 {
		return fmt.Errorf("no Discord channel IDs configured")
	}

	if len(d.AllowedUsers) == 0 {
		fmt.Println("[Discord] WARN: allowed_users is not configured — group channel messages will be denied. Set allowed_users in config to grant access.")
	}

	botID, err := d.getBotUserID()
	if err != nil {
		return fmt.Errorf("failed to get bot user info: %w", err)
	}
	d.botUserID = botID

	// Initialise the cursor for every channel so the main loop only sees
	// messages that arrive after startup. Also detect group vs DM channels.
	for _, chID := range d.ChannelIDs {
		// Detect channel type: type 1 = DM, anything else = guild/group.
		if chType, err := d.getChannelType(chID); err == nil {
			d.groupChannels[chID] = chType != 1
		} else {
			d.groupChannels[chID] = true // default to group for safety
		}
		for {
			select {
			case <-d.stopCh:
				return nil
			default:
			}
			msgs, err := d.getLatestMessages(chID, 1)
			if err != nil {
				fmt.Printf("[Discord] Error initialising channel %s: %s — retrying\n", chID, err)
				time.Sleep(3 * time.Second)
				continue
			}
			d.mu.Lock()
			if len(msgs) > 0 {
				d.lastMsgIDs[chID] = msgs[0].ID
			} else {
				d.lastMsgIDs[chID] = "0"
			}
			d.mu.Unlock()
			break
		}
	}
	fmt.Println("[Discord] Skipped historical messages")

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopCh:
			return nil
		case <-ticker.C:
			for _, chID := range d.ChannelIDs {
				d.pollChannel(chID, handler)
			}
		}
	}
}

func (d *DiscordChannel) Stop() error {
	close(d.stopCh)
	return nil
}

// Send sends a Markdown-formatted message. Discord natively supports Markdown.
func (d *DiscordChannel) Send(channelID, text string) error {
	for _, chunk := range splitTextChunks(text, 1900) {
		if err := d.createMessage(channelID, chunk, ""); err != nil {
			return err
		}
	}
	return nil
}

// SendMessage sends a message and returns the Discord message ID.
// Implements the Editable interface.
func (d *DiscordChannel) SendMessage(channelID, text string) (string, error) {
	chunks := splitTextChunks(text, 1900)
	var firstID string
	for i, chunk := range chunks {
		id, err := d.createMessageWithID(channelID, chunk, "")
		if err != nil {
			return firstID, err
		}
		if i == 0 {
			firstID = id
		}
	}
	return firstID, nil
}

// EditMessage replaces a previously sent Discord message.
// Implements the Editable interface.
func (d *DiscordChannel) EditMessage(channelID, messageID, text string) error {
	// Truncate to Discord's limit; edits are single-message only.
	if len(text) > 1900 {
		text = text[:1900]
	}
	payload := map[string]any{"content": text}
	_, err := d.doRequest("PATCH", fmt.Sprintf("/channels/%s/messages/%s", channelID, messageID), payload)
	return err
}

// Reply sends a Markdown-formatted reply referencing a specific message.
func (d *DiscordChannel) Reply(channelID, replyToID, text string) error {
	for i, chunk := range splitTextChunks(text, 1900) {
		ref := ""
		if i == 0 {
			ref = replyToID
		}
		if err := d.createMessage(channelID, chunk, ref); err != nil {
			return err
		}
	}
	return nil
}

// SendTyping shows a typing indicator in a Discord channel, kept alive until
// called again with typing=false. Implements TypingIndicator.
func (d *DiscordChannel) SendTyping(channelID string, typing bool) error {
	if typing {
		ctx, cancel := context.WithCancel(context.Background())
		d.typingMu.Lock()
		if old, ok := d.typingStop[channelID]; ok {
			old()
		}
		d.typingStop[channelID] = cancel
		d.typingMu.Unlock()
		go func() {
			for {
				// Discord typing indicator lasts ~10 s; refresh every 8 s.
				d.doRequest("POST", fmt.Sprintf("/channels/%s/typing", channelID), nil)
				select {
				case <-ctx.Done():
					return
				case <-time.After(8 * time.Second):
				}
			}
		}()
	} else {
		d.typingMu.Lock()
		if cancel, ok := d.typingStop[channelID]; ok {
			cancel()
			delete(d.typingStop, channelID)
		}
		d.typingMu.Unlock()
	}
	return nil
}

// React adds an emoji reaction to a Discord message.
// Returns a reaction key "messageID|emoji" for use with Unreact.
// Implements Reactor.
func (d *DiscordChannel) React(channelID, messageID, emoji string) (string, error) {
	path := fmt.Sprintf("/channels/%s/messages/%s/reactions/%s/@me",
		channelID, messageID, url.PathEscape(emoji))
	_, err := d.doRequest("PUT", path, nil)
	return messageID + "|" + emoji, err
}

// Unreact removes an emoji reaction previously added by React.
// Implements Reactor.
func (d *DiscordChannel) Unreact(channelID, reactionKey string) error {
	idx := strings.LastIndex(reactionKey, "|")
	if idx < 0 {
		return fmt.Errorf("invalid reaction key: %q", reactionKey)
	}
	messageID, emoji := reactionKey[:idx], reactionKey[idx+1:]
	path := fmt.Sprintf("/channels/%s/messages/%s/reactions/%s/@me",
		channelID, messageID, url.PathEscape(emoji))
	_, err := d.doRequest("DELETE", path, nil)
	return err
}

// --- Discord API types ---

type discordMessage struct {
	ID                string          `json:"id"`
	Content           string          `json:"content"`
	Author            discordUser     `json:"author"`
	ChannelID         string          `json:"channel_id"`
	ReferencedMessage *discordMessage `json:"referenced_message"` // Non-nil when this is a reply
}

type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

func (d *DiscordChannel) doRequest(method, path string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, d.baseURL+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bot "+d.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "AgeAge (https://github.com/ageage, 1.0)")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Discord API %s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

func (d *DiscordChannel) getBotUserID() (string, error) {
	body, err := d.doRequest("GET", "/users/@me", nil)
	if err != nil {
		return "", err
	}
	var user discordUser
	if err := json.Unmarshal(body, &user); err != nil {
		return "", err
	}
	return user.ID, nil
}

func (d *DiscordChannel) getLatestMessages(channelID string, limit int) ([]discordMessage, error) {
	path := fmt.Sprintf("/channels/%s/messages?limit=%d", channelID, limit)
	body, err := d.doRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}
	var msgs []discordMessage
	if err := json.Unmarshal(body, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

func (d *DiscordChannel) getMessages(channelID string, limit int) ([]discordMessage, error) {
	d.mu.Lock()
	lastID, ok := d.lastMsgIDs[channelID]
	d.mu.Unlock()

	if ok && lastID != "" {
		path := fmt.Sprintf("/channels/%s/messages?after=%s&limit=%d", channelID, lastID, limit)
		body, err := d.doRequest("GET", path, nil)
		if err != nil {
			return nil, err
		}
		var msgs []discordMessage
		if err := json.Unmarshal(body, &msgs); err != nil {
			return nil, err
		}
		return msgs, nil
	}

	return []discordMessage{}, nil
}

func (d *DiscordChannel) createMessage(channelID, content, replyToID string) error {
	_, err := d.createMessageWithID(channelID, content, replyToID)
	return err
}

func (d *DiscordChannel) createMessageWithID(channelID, content, replyToID string) (string, error) {
	payload := map[string]any{"content": content}
	if replyToID != "" {
		payload["message_reference"] = map[string]any{
			"message_id": replyToID,
		}
		// fail_if_not_exists=false prevents errors when the referenced message is deleted.
		payload["message_reference"].(map[string]any)["fail_if_not_exists"] = false
	}
	body, err := d.doRequest("POST", fmt.Sprintf("/channels/%s/messages", channelID), payload)
	if err != nil {
		return "", err
	}
	var msg discordMessage
	if err := json.Unmarshal(body, &msg); err == nil && msg.ID != "" {
		return msg.ID, nil
	}
	return "", nil
}

func (d *DiscordChannel) pollChannel(channelID string, handler MessageHandler) {
	botID := d.botUserID
	msgs, err := d.getMessages(channelID, 10)
	if err != nil {
		return
	}

	// Discord returns messages oldest-first when using "after".
	for _, msg := range msgs {
		if msg.Author.Bot || msg.Author.ID == botID {
			d.mu.Lock()
			d.lastMsgIDs[channelID] = msg.ID
			d.mu.Unlock()
			continue
		}

		if strings.TrimSpace(msg.Content) == "" {
			d.mu.Lock()
			d.lastMsgIDs[channelID] = msg.ID
			d.mu.Unlock()
			continue
		}

		d.mu.Lock()
		d.lastMsgIDs[channelID] = msg.ID
		d.mu.Unlock()

		isGroup := d.groupChannels[channelID] // false (DM) when absent, true for guild channels

		// Security: deny all group messages when no allowlist is configured.
		if isGroup && len(d.AllowedUsers) == 0 {
			continue
		}

		if !d.isAllowedUser(msg.Author.ID, msg.Author.Username) {
			continue
		}

		// Detect whether this message is directed at the bot:
		//   1. <@botID>  — standard Discord mention
		//   2. <@!botID> — nickname mention (deprecated since 2022 but still seen in some clients)
		//   3. Reply to bot — the referenced_message was authored by the bot
		mentionTag := fmt.Sprintf("<@%s>", botID)
		nickMentionTag := fmt.Sprintf("<@!%s>", botID)
		hasMention := strings.Contains(msg.Content, mentionTag) ||
			strings.Contains(msg.Content, nickMentionTag)
		isReplyToBot := msg.ReferencedMessage != nil &&
			botID != "" &&
			msg.ReferencedMessage.Author.ID == botID
		botMentioned := hasMention || isReplyToBot

		// Strip explicit mention tags from the content (not applicable for reply-to-bot).
		content := msg.Content
		if hasMention {
			content = strings.ReplaceAll(content, mentionTag, "")
			content = strings.ReplaceAll(content, nickMentionTag, "")
			content = strings.TrimSpace(content)
		}

		capturedChannelID := msg.ChannelID
		capturedMsgID := msg.ID

		incoming := IncomingMessage{
			ChannelType:  "discord",
			ChannelID:    msg.ChannelID,
			SenderID:     msg.Author.ID,
			SenderName:   msg.Author.Username,
			Text:         content,
			ReplyTo:      msg.ID,
			IsGroupChat:  isGroup,
			BotMentioned: botMentioned,
		}
		incoming.Respond = func(text string) error {
			return d.Reply(capturedChannelID, capturedMsgID, text)
		}

		go func(m IncomingMessage) {
			reply := handler(m)
			if reply != "" {
				// Fallback when handler returns a string instead of calling Respond.
				d.Reply(m.ChannelID, m.ReplyTo, reply)
			}
		}(incoming)
	}
}
