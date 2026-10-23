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
	"ageage/conversation"
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
	lastMsgIDs    map[string]string // Track last seen message per channel
	botUserID     string            // Bot's own user ID (set in Start)
	groupChannels map[string]bool   // channelID → true if guild channel, false if DM
	threadParent  map[string]string // thread channel ID → parent channel ID
	guildIDs      map[string]string // channel ID → guild ID, for authentic message links
	mu            sync.Mutex
	typingStop    map[string]discordTypingState // channelID → active keep-alive state
	typingMu      sync.Mutex
}

type discordTypingState struct {
	cancel context.CancelFunc
	refs   int
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
		threadParent:  make(map[string]string),
		guildIDs:      make(map[string]string),
		typingStop:    make(map[string]discordTypingState),
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

func (d *DiscordChannel) Name() string { return config.ChannelDiscord }

// TrackConversation restores routing metadata for a persisted Discord thread.
func (d *DiscordChannel) TrackConversation(target conversation.Target) {
	if target.ChannelType != config.ChannelDiscord || target.ChannelID == "" || target.ThreadID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.threadParent[target.ThreadID] = target.ChannelID
	if group, ok := d.groupChannels[target.ChannelID]; ok {
		d.groupChannels[target.ThreadID] = group
	} else {
		d.groupChannels[target.ThreadID] = true
	}
	if guildID := d.guildIDs[target.ChannelID]; guildID != "" {
		d.guildIDs[target.ThreadID] = guildID
	}
}

// CanCreateConversationThread reports whether Discord threads can be created
// for this known target. Unknown targets remain eligible for an API attempt.
func (d *DiscordChannel) CanCreateConversationThread(target conversation.Target) bool {
	if target.ChannelType != config.ChannelDiscord || target.ThreadID != "" {
		return false
	}
	d.mu.Lock()
	group, known := d.groupChannels[target.ChannelID]
	d.mu.Unlock()
	return !known || group
}

// getChannelType returns the Discord channel type integer (0=guild text, 1=DM, etc.).
func (d *DiscordChannel) getChannelType(channelID string) (int, error) {
	t, _, err := d.getChannelInfo(channelID)
	return t, err
}

func (d *DiscordChannel) getChannelInfo(channelID string) (int, string, error) {
	body, err := d.doRequest("GET", fmt.Sprintf("/channels/%s", channelID), nil)
	if err != nil {
		return 0, "", err
	}
	var ch struct {
		Type    int    `json:"type"`
		GuildID string `json:"guild_id"`
	}
	if err := json.Unmarshal(body, &ch); err != nil {
		return 0, "", err
	}
	return ch.Type, ch.GuildID, nil
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
		if chType, guildID, err := d.getChannelInfo(chID); err == nil {
			d.groupChannels[chID] = chType != 1
			d.guildIDs[chID] = guildID
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
	// Restored threads also need a startup cursor so their old messages are not
	// replayed as fresh input.
	d.mu.Lock()
	trackedThreads := make([]string, 0, len(d.threadParent))
	for threadID := range d.threadParent {
		trackedThreads = append(trackedThreads, threadID)
	}
	d.mu.Unlock()
	for _, threadID := range trackedThreads {
		for {
			select {
			case <-d.stopCh:
				return nil
			default:
			}
			msgs, err := d.getLatestMessages(threadID, 1)
			if err != nil {
				fmt.Printf("[Discord] Error initialising thread %s: %s — retrying\n", threadID, err)
				time.Sleep(3 * time.Second)
				continue
			}
			d.mu.Lock()
			if len(msgs) > 0 {
				d.lastMsgIDs[threadID] = msgs[0].ID
			} else {
				d.lastMsgIDs[threadID] = "0"
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
			// Poll configured channels plus any threads that were created at runtime.
			d.mu.Lock()
			channels := make([]string, len(d.ChannelIDs))
			copy(channels, d.ChannelIDs)
			for tid := range d.threadParent {
				channels = append(channels, tid)
			}
			d.mu.Unlock()
			for _, chID := range channels {
				d.pollChannel(chID, handler)
			}
		}
	}
}

func (d *DiscordChannel) Stop() error {
	d.typingMu.Lock()
	for channelID, state := range d.typingStop {
		state.cancel()
		delete(d.typingStop, channelID)
	}
	d.typingMu.Unlock()
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

// SendMessageInThread sends an editable progress/todo message to a Discord
// thread channel. Discord thread IDs are channel IDs. Implements ThreadEditable.
func (d *DiscordChannel) SendMessageInThread(_ string, threadRootID, _ string, text string) (string, error) {
	return d.SendMessage(threadRootID, text)
}

// EditMessage replaces a previously sent Discord message.
// Implements the Editable interface.
func (d *DiscordChannel) EditMessage(channelID, messageID, text string) error {
	// Edits are single-message only; Surface.Update sends a chunked replacement
	// on this same channel/thread when the text exceeds the edit limit.
	if len(text) > 1900 {
		return fmt.Errorf("Discord edit exceeds single-message limit")
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
		d.typingMu.Lock()
		if state, ok := d.typingStop[channelID]; ok {
			state.refs++
			d.typingStop[channelID] = state
			d.typingMu.Unlock()
			return nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		d.typingStop[channelID] = discordTypingState{cancel: cancel, refs: 1}
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
		if state, ok := d.typingStop[channelID]; ok {
			if state.refs > 1 {
				state.refs--
				d.typingStop[channelID] = state
				d.typingMu.Unlock()
				return nil
			}
			state.cancel()
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
	ID                string              `json:"id"`
	Content           string              `json:"content"`
	Author            discordUser         `json:"author"`
	ChannelID         string              `json:"channel_id"`
	ReferencedMessage *discordMessage     `json:"referenced_message"` // Non-nil when this is a reply
	Attachments       []discordAttachment `json:"attachments"`
}

type discordAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
	ParseError  string `json:"-"`
}

// UnmarshalJSON keeps one malformed attachment from discarding its containing
// message (and any valid sibling attachments). Field errors are surfaced as an
// unsupported attachment by incomingAttachments.
func (a *discordAttachment) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		a.ParseError = "invalid attachment metadata"
		return nil
	}
	var errors []string
	decode := func(key string, dst any) {
		value, ok := fields[key]
		if !ok || string(value) == "null" {
			return
		}
		if err := json.Unmarshal(value, dst); err != nil {
			errors = append(errors, "invalid "+key)
		}
	}
	decode("filename", &a.Filename)
	decode("content_type", &a.ContentType)
	decode("size", &a.Size)
	decode("url", &a.URL)
	a.ParseError = strings.Join(errors, ", ")
	return nil
}

type discordUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

func (d *DiscordChannel) incomingAttachments(items []discordAttachment) []attachment.Attachment {
	result := make([]attachment.Attachment, 0, len(items))
	for _, item := range items {
		a := attachment.Attachment{Filename: item.Filename, MIMEType: item.ContentType, Size: item.Size}
		if item.ParseError != "" {
			a.UnsupportedReason = item.ParseError
			result = append(result, a)
			continue
		}
		parsed, err := url.Parse(item.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
			a.UnsupportedReason = "invalid attachment URL"
		} else {
			a.URL = item.URL
			mediaURL := item.URL
			a.Open = func(ctx context.Context) (io.ReadCloser, error) {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaURL, nil)
				if err != nil {
					return nil, fmt.Errorf("invalid attachment request")
				}
				// CDN URLs are public signed URLs; never send the Discord bot token here.
				resp, err := d.client.Do(req)
				if err != nil {
					return nil, fmt.Errorf("download attachment failed")
				}
				if resp.StatusCode < 200 || resp.StatusCode >= 300 {
					resp.Body.Close()
					return nil, fmt.Errorf("download attachment returned HTTP %d", resp.StatusCode)
				}
				return resp.Body, nil
			}
		}
		result = append(result, a)
	}
	return result
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

// createThread creates a public thread from a message and returns the thread channel ID.
func (d *DiscordChannel) createThread(channelID, messageID, name string) (string, error) {
	if len([]rune(name)) > 100 {
		name = string([]rune(name)[:100])
	}
	if name == "" {
		name = "Conversation"
	}
	payload := map[string]any{
		"name":                  name,
		"auto_archive_duration": 1440, // archive after 24 h of inactivity
	}
	body, err := d.doRequest("POST",
		fmt.Sprintf("/channels/%s/messages/%s/threads", channelID, messageID), payload)
	if err != nil {
		return "", err
	}
	var thread struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &thread); err != nil || thread.ID == "" {
		return "", fmt.Errorf("thread creation returned no ID")
	}
	return thread.ID, nil
}

func (d *DiscordChannel) DeliveryID(target conversation.Target) string {
	if target.ThreadID != "" {
		return target.ThreadID
	}
	return target.ChannelID
}

func (d *DiscordChannel) ReplyMessage(target conversation.Target, text string) (string, error) {
	return d.createMessageWithID(d.DeliveryID(target), text, target.ReplyTo)
}

func (d *DiscordChannel) CreateConversationThread(target conversation.Target, title, body string) (conversation.Target, conversation.Reference, error) {
	if target.ThreadID != "" {
		return conversation.Target{}, conversation.Reference{}, ErrUnsupported
	}
	rootText := strings.TrimSpace(body)
	if strings.TrimSpace(title) != "" {
		if rootText != "" {
			rootText = strings.TrimSpace(title) + "\n\n" + rootText
		} else {
			rootText = strings.TrimSpace(title)
		}
	}
	if rootText == "" {
		rootText = "Conversation"
	}
	rootID, err := d.createMessageWithID(target.ChannelID, rootText, "")
	if err != nil {
		return conversation.Target{}, conversation.Reference{}, err
	}
	if rootID == "" {
		return conversation.Target{}, conversation.Reference{}, fmt.Errorf("Discord root message returned no ID")
	}
	threadID, err := d.createThread(target.ChannelID, rootID, title)
	if err != nil {
		return conversation.Target{}, conversation.Reference{}, err
	}
	d.mu.Lock()
	d.lastMsgIDs[threadID] = "0"
	d.threadParent[threadID] = target.ChannelID
	d.groupChannels[threadID] = true
	d.guildIDs[threadID] = d.guildIDs[target.ChannelID]
	d.mu.Unlock()
	threadTarget := target
	threadTarget.ThreadID, threadTarget.ReplyTo = threadID, rootID
	return threadTarget, conversation.Reference{Target: threadTarget, MessageID: rootID}, nil
}

func (d *DiscordChannel) ConversationLink(ref conversation.Reference) string {
	if ref.MessageID == "" {
		return ""
	}
	target := ref.Target
	d.mu.Lock()
	guildID := d.guildIDs[target.ChannelID]
	if target.ThreadID != "" {
		if id := d.guildIDs[target.ThreadID]; id != "" {
			guildID = id
		}
		if ref.MessageID != target.ReplyTo {
			target.ChannelID = target.ThreadID
		}
	}
	d.mu.Unlock()
	if guildID == "" {
		return ""
	}
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, target.ChannelID, ref.MessageID)
}

func (d *DiscordChannel) pollChannel(channelID string, handler MessageHandler) {
	botID := d.botUserID

	// Determine whether this channel is a thread we created.
	d.mu.Lock()
	parentID, isThread := d.threadParent[channelID]
	d.mu.Unlock()

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

		if strings.TrimSpace(msg.Content) == "" && len(msg.Attachments) == 0 {
			d.mu.Lock()
			d.lastMsgIDs[channelID] = msg.ID
			d.mu.Unlock()
			continue
		}

		d.mu.Lock()
		d.lastMsgIDs[channelID] = msg.ID
		d.mu.Unlock()

		// For thread channels, report the parent channel as ChannelID so the
		// session key (discord:parentChannelID:t:threadChannelID) matches the
		// one set when the thread was created.
		effectiveChannelID := channelID
		threadID := ""
		if isThread {
			effectiveChannelID = parentID
			threadID = channelID
		}

		isGroup := d.groupChannels[effectiveChannelID] // false (DM) when absent

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
		//   4. Message in a thread the bot is managing — always directed at the bot
		mentionTag := fmt.Sprintf("<@%s>", botID)
		nickMentionTag := fmt.Sprintf("<@!%s>", botID)
		hasMention := strings.Contains(msg.Content, mentionTag) ||
			strings.Contains(msg.Content, nickMentionTag)
		isReplyToBot := msg.ReferencedMessage != nil &&
			botID != "" &&
			msg.ReferencedMessage.Author.ID == botID
		botMentioned := hasMention || isReplyToBot || isThread

		// Strip explicit mention tags from the content.
		content := msg.Content
		if hasMention {
			content = strings.ReplaceAll(content, mentionTag, "")
			content = strings.ReplaceAll(content, nickMentionTag, "")
			content = strings.TrimSpace(content)
		}

		// For guild channel messages (not already a thread) where the bot is
		// mentioned: create a thread so the conversation stays contained.
		// The thread is added to the poll list for subsequent messages.
		sendChannelID := channelID // where responses go
		if isGroup && !isThread && botMentioned && !strings.HasPrefix(strings.TrimSpace(content), "/") {
			name := content
			if len([]rune(name)) > 100 {
				name = string([]rune(name)[:100])
			}
			if tid, err := d.createThread(channelID, msg.ID, name); err == nil {
				sendChannelID = tid
				threadID = tid
				d.mu.Lock()
				d.lastMsgIDs[tid] = "0"
				d.threadParent[tid] = channelID
				d.groupChannels[tid] = true
				d.guildIDs[tid] = d.guildIDs[channelID]
				d.mu.Unlock()
			}
			// Thread creation failure falls back to sending in the original channel.
		}

		capturedSendChannelID := sendChannelID

		incoming := IncomingMessage{
			ChannelType:  config.ChannelDiscord,
			ChannelID:    effectiveChannelID,
			SenderID:     msg.Author.ID,
			SenderName:   msg.Author.Username,
			Text:         content,
			Attachments:  d.incomingAttachments(msg.Attachments),
			ReplyTo:      msg.ID,
			ThreadID:     threadID,
			Conversation: conversation.Target{ChannelType: config.ChannelDiscord, ChannelID: effectiveChannelID, ThreadID: threadID, ReplyTo: msg.ID, SenderID: msg.Author.ID, SenderName: msg.Author.Username},
			IsGroupChat:  isGroup,
			BotMentioned: botMentioned,
		}
		incoming.Respond = func(text string) error {
			return d.Send(capturedSendChannelID, text)
		}

		go func(m IncomingMessage, sendCh string) {
			reply := handler(m)
			if reply != "" {
				d.Send(sendCh, reply)
			}
		}(incoming, capturedSendChannelID)
	}
}
