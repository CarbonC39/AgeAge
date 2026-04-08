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

// DiscordChannel connects to Discord via Bot API (REST polling).
type DiscordChannel struct {
	Token      string   // Bot token (without "Bot " prefix)
	ChannelIDs []string // Channel IDs to monitor
	Options    ChannelOptions
	baseURL    string
	client     *http.Client
	stopCh     chan struct{}
	lastMsgIDs map[string]string // Track last seen message per channel
	mu         sync.Mutex
}

// NewDiscord creates a new Discord channel.
func NewDiscord(botToken string, channelIDs []string, opts ChannelOptions) *DiscordChannel {
	return &DiscordChannel{
		Token:      botToken,
		ChannelIDs: channelIDs,
		Options:    opts,
		baseURL:    "https://discord.com/api/v10",
		client:     &http.Client{Timeout: 30 * time.Second},
		stopCh:     make(chan struct{}),
		lastMsgIDs: make(map[string]string),
	}
}

func (d *DiscordChannel) Name() string { return "discord" }

// Start begins polling Discord channels for new messages.
func (d *DiscordChannel) Start(handler MessageHandler) error {
	if len(d.ChannelIDs) == 0 {
		return fmt.Errorf("no Discord channel IDs configured")
	}

	botID, err := d.getBotUserID()
	if err != nil {
		return fmt.Errorf("failed to get bot user info: %w", err)
	}

	// Initialize the cursor for every channel so the main loop only sees
	// messages that arrive after startup. Retry each channel individually
	// so a single bad channel cannot prevent the others from initialising.
	// Falls back to "0" when the channel has no messages yet.
	for _, chID := range d.ChannelIDs {
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
				d.lastMsgIDs[chID] = "0" // empty channel; accept all future messages
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
				d.pollChannel(chID, botID, handler)
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
	chunks := splitTextChunks(text, 1900) // Discord limit is 2000.

	for _, chunk := range chunks {
		if err := d.createMessage(channelID, chunk, ""); err != nil {
			return err
		}
	}
	return nil
}

// Reply sends a Markdown-formatted reply. Discord natively supports Markdown.
func (d *DiscordChannel) Reply(channelID, replyToID, text string) error {
	chunks := splitTextChunks(text, 1900)

	for i, chunk := range chunks {
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

// --- Discord API types ---

type discordMessage struct {
	ID        string      `json:"id"`
	Content   string      `json:"content"`
	Author    discordUser `json:"author"`
	ChannelID string      `json:"channel_id"`
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

// getLatestMessages returns the latest N messages (newest first), ignoring lastMsgIDs.
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

	// If we have a last position, use "after" to get only newer messages
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

		// Discord returns messages oldest first when using "after"
		return msgs, nil
	}

	// No last position set, return empty (caller should initialize lastMsgIDs first)
	return []discordMessage{}, nil
}

func (d *DiscordChannel) createMessage(channelID, content, replyToID string) error {
	payload := map[string]interface{}{
		"content": content,
	}
	if replyToID != "" {
		payload["message_reference"] = map[string]interface{}{
			"message_id": replyToID,
		}
	}

	_, err := d.doRequest("POST", fmt.Sprintf("/channels/%s/messages", channelID), payload)
	return err
}

func (d *DiscordChannel) pollChannel(channelID, botID string, handler MessageHandler) {
	msgs, err := d.getMessages(channelID, 10)
	if err != nil {
		return
	}

	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]

		if msg.Author.Bot || msg.Author.ID == botID {
			continue
		}

		if strings.TrimSpace(msg.Content) == "" {
			continue
		}

		d.mu.Lock()
		d.lastMsgIDs[channelID] = msg.ID
		d.mu.Unlock()

		incoming := IncomingMessage{
			ChannelType: "discord",
			ChannelID:   msg.ChannelID,
			SenderID:    msg.Author.ID,
			SenderName:  msg.Author.Username,
			Text:        msg.Content,
			ReplyTo:     msg.ID,
		}

		process := func(m IncomingMessage) {
			reply := handler(m)
			if reply != "" {
				d.Reply(m.ChannelID, m.ReplyTo, reply)
			}
		}

		// Always run in a goroutine to avoid blocking the polling loop.
		go process(incoming)
	}
}
