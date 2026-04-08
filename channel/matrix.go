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
	Homeserver string   // e.g., "https://matrix.org"
	UserID     string   // e.g., "@bot:matrix.org"
	Token      string   // Access token
	RoomIDs    []string // Rooms to monitor
	Options    ChannelOptions
	client     *http.Client
	stopCh     chan struct{}
	since      string // Sync token for /sync
	mu         sync.Mutex
}

// NewMatrix creates a new Matrix channel.
func NewMatrix(homeserver, userID, accessToken string, roomIDs []string, opts ChannelOptions) *MatrixChannel {
	return &MatrixChannel{
		Homeserver: strings.TrimRight(homeserver, "/"),
		UserID:     userID,
		Token:      accessToken,
		RoomIDs:    roomIDs,
		Options:    opts,
		client:     &http.Client{Timeout: 60 * time.Second},
		stopCh:     make(chan struct{}),
	}
}

func (m *MatrixChannel) Name() string { return "matrix" }

// Start begins listening for Matrix events via long-polling /sync.
func (m *MatrixChannel) Start(handler MessageHandler) error {
	for _, roomID := range m.RoomIDs {
		m.joinRoom(roomID)
	}

	roomSet := make(map[string]bool, len(m.RoomIDs))
	for _, r := range m.RoomIDs {
		roomSet[r] = true
	}

	// Advance the since token to the current server position so the main loop
	// only receives truly new events. Retry until the sync succeeds — if we
	// enter the main loop with since="" a full sync would return recent
	// history and re-process already-handled messages.
	for {
		select {
		case <-m.stopCh:
			return nil
		default:
		}
		if _, err := m.doSync(); err != nil {
			fmt.Printf("[Matrix] Initial sync error: %s — retrying\n", err)
			time.Sleep(5 * time.Second)
			continue
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

		// Process only new user messages (not history)
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

				msg := IncomingMessage{
					ChannelType: "matrix",
					ChannelID:   roomID,
					SenderID:    event.Sender,
					SenderName:  event.Sender,
					Text:        body,
					ReplyTo:     event.EventID,
				}

				process := func(incoming IncomingMessage) {
					reply := handler(incoming)
					if reply != "" {
						m.Send(incoming.ChannelID, reply)
					}
				}

				// Always run in a goroutine to avoid blocking the sync loop.
				go process(msg)
			}
		}

		for roomID := range syncResp.Rooms.Invite {
			fmt.Printf("[Matrix] Auto-joining invited room: %s\n", roomID)
			m.joinRoom(roomID)
		}
	}
}

func (m *MatrixChannel) Stop() error {
	close(m.stopCh)
	return nil
}

// Send sends a Markdown-formatted message to a Matrix room.
// Matrix supports org.matrix.custom.html format for rich text.
func (m *MatrixChannel) Send(roomID, text string) error {
	_, err := m.SendMessage(roomID, text)
	return err
}

// SendMessage sends a message and returns the event ID assigned by the server.
func (m *MatrixChannel) SendMessage(roomID, text string) (string, error) {
	txnID := fmt.Sprintf("ageage_%d", time.Now().UnixNano())

	htmlBody := markdownToHTML(text)
	payload := map[string]interface{}{
		"msgtype":        "m.text",
		"body":           text,
		"format":         "org.matrix.custom.html",
		"formatted_body": htmlBody,
	}

	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		roomID, txnID)

	respBytes, err := m.doRequest("PUT", path, payload)
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

// EditMessage replaces the content of a previously sent Matrix event using m.replace.
func (m *MatrixChannel) EditMessage(roomID, eventID, text string) error {
	txnID := fmt.Sprintf("ageage_edit_%d", time.Now().UnixNano())

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

	path := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		roomID, txnID)

	_, err := m.doRequest("PUT", path, payload)
	return err
}

func (m *MatrixChannel) Reply(roomID, replyToID, text string) error {
	// Matrix doesn't have simple reply-to in the basic API; just send.
	return m.Send(roomID, text)
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
