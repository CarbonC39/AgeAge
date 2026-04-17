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

// MatrixChannel connects to a Matrix homeserver via Client-Server API.
type MatrixChannel struct {
	Homeserver   string   // e.g., "https://matrix.org"
	UserID       string   // e.g., "@bot:matrix.org"
	Token        string   // Access token
	RoomIDs      []string // Rooms to monitor; empty = all joined rooms
	AllowedUsers []string // Matrix user IDs allowed to interact; empty = allow all
	Options      ChannelOptions
	client       *http.Client
	stopCh       chan struct{}
	since         string              // Sync token for /sync
	groupRooms    map[string]bool     // roomID → true if multi-user (group), false if DM
	directMap     map[string][]string // peerID → []roomID
	directFetched time.Time
	mu            sync.RWMutex
}

// NewMatrix creates a new Matrix channel.
func NewMatrix(homeserver, userID, accessToken string, roomIDs []string, allowedUsers []string, opts ChannelOptions) *MatrixChannel {
	return &MatrixChannel{
		Homeserver:   strings.TrimRight(homeserver, "/"),
		UserID:       userID,
		Token:        accessToken,
		RoomIDs:      roomIDs,
		AllowedUsers: allowedUsers,
		Options:      opts,
		client:       &http.Client{Timeout: 60 * time.Second},
		stopCh:       make(chan struct{}),
		groupRooms:   make(map[string]bool),
	}
}

// isAllowedUser returns true when AllowedUsers is empty (allow all) or the
// given userID matches an AllowedUsers entry.
// Matching trims surrounding whitespace from configured entries.
// Matrix user IDs are always fully-qualified (@user:homeserver) and case-sensitive.
func (m *MatrixChannel) isAllowedUser(userID string) bool {
	if len(m.AllowedUsers) == 0 {
		return true
	}
	for _, id := range m.AllowedUsers {
		if strings.TrimSpace(id) == userID {
			return true
		}
	}
	return false
}

func (m *MatrixChannel) Name() string { return "matrix" }

// isGroupRoom reports whether roomID is a multi-user room (not a DM).
func (m *MatrixChannel) isGroupRoom(roomID string) bool {
	m.mu.RLock()
	isGroup, ok := m.groupRooms[roomID]
	m.mu.RUnlock()

	if !ok {
		// Lazily determine if it's a DM.
		isGroup = !m.isRoomDM(roomID)
		m.mu.Lock()
		m.groupRooms[roomID] = isGroup
		m.mu.Unlock()
	}
	return isGroup
}

// getRoomMemberCount returns the number of joined members in a room.
func (m *MatrixChannel) getRoomMemberCount(roomID string) (int, error) {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/joined_members", roomID)
	body, err := m.doRequest("GET", path, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Joined map[string]any `json:"joined"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, err
	}
	return len(resp.Joined), nil
}

// isRoomDM returns true when roomID is a 1-on-1 direct message room.
func (m *MatrixChannel) isRoomDM(roomID string) bool {
	// Primary: check cached m.direct account data.
	directMap := m.getDirectMap()
	for _, rooms := range directMap {
		for _, r := range rooms {
			if r == roomID {
				return true
			}
		}
	}

	// Fallback: exactly 2 joined members → DM (bot + 1 user).
	count, err := m.getRoomMemberCount(roomID)
	if err != nil {
		return false // indeterminate → treat as group for safety
	}
	return count <= 2
}

func (m *MatrixChannel) getDirectMap() map[string][]string {
	m.mu.RLock()
	if m.directMap != nil && time.Since(m.directFetched) < 1*time.Minute {
		defer m.mu.RUnlock()
		return m.directMap
	}
	m.mu.RUnlock()

	path := fmt.Sprintf("/_matrix/client/v3/user/%s/account_data/m.direct", m.UserID)
	body, err := m.doRequest("GET", path, nil)
	if err != nil {
		return nil
	}

	var directMap map[string][]string
	if err := json.Unmarshal(body, &directMap); err != nil {
		return nil
	}

	m.mu.Lock()
	m.directMap = directMap
	m.directFetched = time.Now()
	m.mu.Unlock()

	return directMap
}

// parseInviteIsDirect extracts is_direct and the inviter's user ID from an
// invited room's sync data. Returns (true, senderID) when the room is a DM.
func (m *MatrixChannel) parseInviteIsDirect(inviteData json.RawMessage) (bool, string) {
	var invite struct {
		InviteState struct {
			Events []struct {
				Type     string `json:"type"`
				StateKey string `json:"state_key"`
				Sender   string `json:"sender"`
				Content  struct {
					IsDirect bool `json:"is_direct"`
				} `json:"content"`
			} `json:"events"`
		} `json:"invite_state"`
	}
	if err := json.Unmarshal(inviteData, &invite); err != nil {
		return false, ""
	}
	for _, ev := range invite.InviteState.Events {
		if ev.Type == "m.room.member" && ev.StateKey == m.UserID && ev.Content.IsDirect {
			return true, ev.Sender
		}
	}
	return false, ""
}

// updateDirectRooms records roomID in the bot's m.direct account data under
// peerID so that isRoomDM can find it on subsequent starts.
func (m *MatrixChannel) updateDirectRooms(peerID, roomID string) {
	path := fmt.Sprintf("/_matrix/client/v3/user/%s/account_data/m.direct", m.UserID)

	// Fetch current map (ignore error — may not exist yet).
	var directMap map[string][]string
	if body, err := m.doRequest("GET", path, nil); err == nil {
		json.Unmarshal(body, &directMap) //nolint:errcheck
	}
	if directMap == nil {
		directMap = make(map[string][]string)
	}

	// Append roomID if not already tracked under peerID.
	for _, r := range directMap[peerID] {
		if r == roomID {
			return
		}
	}
	directMap[peerID] = append(directMap[peerID], roomID)
	m.doRequest("PUT", path, directMap) //nolint:errcheck
}

// Start begins listening for Matrix events via long-polling /sync.
func (m *MatrixChannel) Start(handler MessageHandler) error {
	if len(m.AllowedUsers) == 0 {
		fmt.Println("[Matrix] WARN: allowed_users is not configured — group chat messages will be denied. Set allowed_users in config to grant access.")
	}

	for _, roomID := range m.RoomIDs {
		m.joinRoom(roomID)
		isDM := m.isRoomDM(roomID)
		m.mu.Lock()
		m.groupRooms[roomID] = !isDM
		m.mu.Unlock()
	}

	roomSet := make(map[string]bool, len(m.RoomIDs))
	for _, r := range m.RoomIDs {
		roomSet[r] = true
	}

	// Advance the since token to the current server position so the main loop
	// only receives truly new events.
	for {
		select {
		case <-m.stopCh:
			return nil
		default:
		}
		syncResp, err := m.doSync()
		if err != nil {
			fmt.Printf("[Matrix] Initial sync error: %s — retrying\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		// Discovery: Identify DMs from the initial sync state.
		for roomID := range syncResp.Rooms.Join {
			m.mu.RLock()
			_, ok := m.groupRooms[roomID]
			m.mu.RUnlock()
			if !ok {
				isDM := m.isRoomDM(roomID)
				m.mu.Lock()
				m.groupRooms[roomID] = !isDM
				m.mu.Unlock()
			}
		}

		fmt.Println("[Matrix] Skipped historical messages")
		break
	}

	for {
		select {
		case <-m.stopCh:
			return nil
		default:
		}

		syncResp, err := m.doSync()
		if err != nil {
			fmt.Printf("[Matrix] Sync error: %s\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		for roomID, roomData := range syncResp.Rooms.Join {
			if len(m.RoomIDs) > 0 && !roomSet[roomID] {
				continue
			}
			for _, event := range roomData.Timeline.Events {
				if event.Type != "m.room.message" {
					continue
				}

				msgType, _ := event.Content["msgtype"].(string)
				if msgType != "m.text" {
					continue
				}

				body, _ := event.Content["body"].(string)
				if body == "" {
					continue
				}

				if event.Sender == m.UserID {
					continue
				}

				isGroup := m.isGroupRoom(roomID)

				// Security: deny all messages from group rooms when no allowlist is set.
				if isGroup && len(m.AllowedUsers) == 0 {
					continue
				}

				if !m.isAllowedUser(event.Sender) {
					continue
				}

				// Parse m.relates_to to determine thread context.
				relatesTo, _ := event.Content["m.relates_to"].(map[string]interface{})
				relType, _ := relatesTo["rel_type"].(string)

				// Skip edit events — they are not new messages.
				if relType == "m.replace" {
					continue
				}

				// Determine thread ID: set only when message is already in a thread.
				threadID := ""
				if relType == "m.thread" {
					threadID, _ = relatesTo["event_id"].(string)
				}

				// Detect @mention: bot's UserID appears in the message body.
				// DM rooms and thread messages are always directed at the bot.
				botMentioned := !isGroup || threadID != "" || strings.Contains(body, m.UserID)

				// Strip the @mention prefix from the body so the agent sees clean text.
				if botMentioned && strings.Contains(body, m.UserID) {
					body = strings.ReplaceAll(body, m.UserID, "")
					body = strings.TrimSpace(body)
				}

				// Capture loop-local values for closure safety.
				capturedRoomID := roomID
				capturedEventID := event.EventID
				capturedThreadID := threadID

				incoming := IncomingMessage{
					ChannelType:  "matrix",
					ChannelID:    roomID,
					SenderID:     event.Sender,
					SenderName:   event.Sender,
					Text:         body,
					ReplyTo:      event.EventID,
					ThreadID:     threadID,
					IsGroupChat:  isGroup,
					BotMentioned: botMentioned,
				}

				incoming.Respond = func(text string) error {
					if capturedThreadID != "" {
						return m.SendInThread(capturedRoomID, capturedThreadID, capturedEventID, text)
					}
					return m.Send(capturedRoomID, text)
				}

				go func(msg IncomingMessage) {
					reply := handler(msg)
					// Fallback for handlers that return a string instead of calling Respond.
					if reply != "" {
						if msg.ThreadID != "" {
							_ = m.SendInThread(msg.ChannelID, msg.ThreadID, msg.ReplyTo, reply)
						} else {
							_ = m.Send(msg.ChannelID, reply)
						}
					}
				}(incoming)
			}
		}

		for roomID, inviteData := range syncResp.Rooms.Invite {
			isDirect, peerID := m.parseInviteIsDirect(inviteData)
			fmt.Printf("[Matrix] Auto-joining invited room: %s (DM: %v)\n", roomID, isDirect)
			m.joinRoom(roomID)
			m.mu.Lock()
			if isDirect {
				m.groupRooms[roomID] = false
				m.mu.Unlock()
				if peerID != "" {
					go m.updateDirectRooms(peerID, roomID)
				}
			} else {
				isDM := m.isRoomDM(roomID)
				m.groupRooms[roomID] = !isDM
				m.mu.Unlock()
			}
		}
	}
}

func (m *MatrixChannel) Stop() error {
	close(m.stopCh)
	return nil
}

// Send sends a Markdown-formatted message to a Matrix room, splitting long
// messages at paragraph boundaries if needed.
func (m *MatrixChannel) Send(roomID, text string) error {
	for i, chunk := range splitMessages(text, 4000) {
		if i > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		if _, err := m.sendRaw(roomID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// SendMessage sends a message and returns the event ID assigned by the server.
// Implements the Editable interface. For long messages, returns the last event ID.
func (m *MatrixChannel) SendMessage(roomID, text string) (string, error) {
	chunks := splitMessages(text, 4000)
	var lastID string
	var err error
	for i, chunk := range chunks {
		if i > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		lastID, err = m.sendRaw(roomID, chunk)
		if err != nil {
			return lastID, err
		}
	}
	return lastID, nil
}

// sendRaw sends a single (unsplit) message to a room and returns its event ID.
func (m *MatrixChannel) sendRaw(roomID, text string) (string, error) {
	htmlBody := markdownToHTML(text)
	content := map[string]any{
		"msgtype":        "m.text",
		"body":           text,
		"format":         "org.matrix.custom.html",
		"formatted_body": htmlBody,
	}
	return m.sendEvent(roomID, "m.room.message", content)
}

// SendInThread sends text inside a Matrix thread, splitting if necessary.
func (m *MatrixChannel) SendInThread(roomID, threadRootID, latestEventID, text string) error {
	chunks := splitMessages(text, 4000)
	replyTo := latestEventID
	for i, chunk := range chunks {
		if i > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		htmlBody := markdownToHTML(chunk)
		content := map[string]any{
			"msgtype":        "m.text",
			"body":           chunk,
			"format":         "org.matrix.custom.html",
			"formatted_body": htmlBody,
			"m.relates_to": map[string]any{
				"rel_type": "m.thread",
				"event_id": threadRootID,
				"m.in_reply_to": map[string]any{
					"event_id": replyTo,
				},
			},
		}
		eventID, err := m.sendEvent(roomID, "m.room.message", content)
		if err != nil {
			return err
		}
		// Each subsequent chunk replies to the previous one to maintain order.
		if eventID != "" {
			replyTo = eventID
		}
	}
	return nil
}

// EditMessage replaces the content of a previously sent Matrix event using m.replace.
func (m *MatrixChannel) EditMessage(roomID, eventID, text string) error {
	htmlBody := markdownToHTML(text)
	payload := map[string]interface{}{
		"msgtype": "m.text",
		"body":    "* " + text,
		"format":  "org.matrix.custom.html",
		"m.new_content": map[string]interface{}{
			"msgtype":        "m.text",
			"body":           text,
			"format":         "org.matrix.custom.html",
			"formatted_body": htmlBody,
		},
		"m.relates_to": map[string]interface{}{
			"rel_type": "m.replace",
			"event_id": eventID,
		},
	}

	_, err := m.sendEvent(roomID, "m.room.message", payload)
	return err
}

func (m *MatrixChannel) Reply(roomID, replyToID, text string) error {
	return m.Send(roomID, text)
}

// SendTyping sends or clears a typing indicator in a room.
// Implements the TypingIndicator interface.
func (m *MatrixChannel) SendTyping(channelID string, typing bool) error {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/typing/%s", channelID, m.UserID)
	payload := map[string]any{"typing": typing}
	if typing {
		payload["timeout"] = 30000
	}
	_, err := m.doRequest("PUT", path, payload)
	return err
}

// SendReadReceipt marks an event as read.
// Implements the ReadReceiptSender interface.
func (m *MatrixChannel) SendReadReceipt(channelID, eventID string) error {
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/receipt/m.read/%s", channelID, eventID)
	_, err := m.doRequest("POST", path, map[string]any{})
	return err
}

// React adds an emoji reaction to an event and returns the reaction event ID.
// Implements the Reactor interface.
func (m *MatrixChannel) React(channelID, eventID, emoji string) (string, error) {
	content := map[string]any{
		"m.relates_to": map[string]any{
			"rel_type": "m.annotation",
			"event_id": eventID,
			"key":      emoji,
		},
	}
	return m.sendEvent(channelID, "m.reaction", content)
}

// Unreact removes a reaction by redacting the reaction event.
// Implements the Reactor interface.
func (m *MatrixChannel) Unreact(channelID, reactionEventID string) error {
	txnID := fmt.Sprintf("ageage_redact_%d", time.Now().UnixNano())
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/redact/%s/%s", channelID, reactionEventID, txnID)
	_, err := m.doRequest("PUT", path, map[string]any{})
	return err
}

// splitMessages splits text into chunks of at most maxLen runes, preferring
// paragraph breaks (\n\n) when they fall in the second half of the chunk.
func splitMessages(text string, maxLen int) []string {
	runes := []rune(text)
	if len(runes) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= maxLen {
			chunks = append(chunks, string(runes))
			break
		}
		chunk := string(runes[:maxLen])
		// Prefer to break at a paragraph boundary in the back half of the chunk.
		if idx := strings.LastIndex(chunk, "\n\n"); idx > maxLen/2 {
			chunks = append(chunks, string(runes[:idx]))
			runes = []rune(strings.TrimLeft(string(runes[idx:]), "\n"))
		} else {
			chunks = append(chunks, chunk)
			runes = runes[maxLen:]
		}
	}
	return chunks
}

// --- Matrix API types ---

type matrixSyncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join   map[string]matrixJoinedRoom `json:"join"`
		Invite map[string]json.RawMessage  `json:"invite"`
	} `json:"rooms"`
}

type matrixJoinedRoom struct {
	Timeline struct {
		Events []matrixEvent `json:"events"`
	} `json:"timeline"`
}

type matrixEvent struct {
	Type    string                 `json:"type"`
	Sender  string                 `json:"sender"`
	EventID string                 `json:"event_id"`
	Content map[string]interface{} `json:"content"`
}

// sendEvent sends a Matrix state/message event and returns the assigned event ID.
func (m *MatrixChannel) sendEvent(roomID, eventType string, content map[string]any) (string, error) {
	txnID := fmt.Sprintf("ageage_%d", time.Now().UnixNano())
	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/%s/%s", roomID, eventType, txnID)
	respBytes, err := m.doRequest("PUT", path, content)
	if err != nil {
		return "", err
	}
	var result struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(respBytes, &result); err != nil {
		return "", nil // non-fatal: message was sent, we just lost the ID
	}
	return result.EventID, nil
}

func (m *MatrixChannel) doRequest(method, path string, body interface{}) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reqBody = bytes.NewReader(data)
	}

	url := m.Homeserver + path

	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Matrix API %s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

func (m *MatrixChannel) doSync() (*matrixSyncResponse, error) {
	path := "/_matrix/client/v3/sync?timeout=30000"
	if m.since != "" {
		path += "&since=" + m.since
	}

	body, err := m.doRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}

	var syncResp matrixSyncResponse
	if err := json.Unmarshal(body, &syncResp); err != nil {
		return nil, fmt.Errorf("failed to parse sync response: %w", err)
	}

	m.since = syncResp.NextBatch
	return &syncResp, nil
}

func (m *MatrixChannel) joinRoom(roomID string) {
	path := fmt.Sprintf("/_matrix/client/v3/join/%s", roomID)
	m.doRequest("POST", path, map[string]interface{}{})
}
