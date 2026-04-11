package channel

import (
	"fmt"
	"strings"
	"sync"
)

// IncomingMessage represents a message received from any channel.
type IncomingMessage struct {
	ChannelType string // "matrix", "telegram", "discord"
	ChannelID   string // Room/Chat/Channel ID
	SenderID    string // User identifier
	SenderName  string // Display name
	Text        string // Message content
	ReplyTo     string // Message ID to reply to (if applicable)
	ThreadID    string // Thread/topic ID within the channel (Discord threads, etc.); empty if not in a thread
}

// ChannelOptions holds common settings passed to all channel connectors.
type ChannelOptions struct {
	Parallel bool // If true, process messages concurrently
}

// Channel is the interface all IM connectors must implement.
type Channel interface {
	// Name returns the channel type name (e.g., "matrix", "telegram").
	Name() string

	// Start begins listening for incoming messages. Blocking call.
	Start(handler MessageHandler) error

	// Stop gracefully shuts down the channel.
	Stop() error

	// Send sends a message to a specific channel/room.
	// The text is assumed to be Markdown-formatted.
	Send(channelID, text string) error

	// Reply sends a reply to a specific message.
	// The text is assumed to be Markdown-formatted.
	Reply(channelID, replyToID, text string) error
}

// Editable is an optional interface for channels that support message editing.
// Channels that implement this allow todo notifications to be updated in place
// rather than sending a new message on every update_todos call.
type Editable interface {
	// SendMessage sends a message and returns the platform-native message ID.
	SendMessage(channelID, text string) (messageID string, err error)
	// EditMessage replaces the content of a previously sent message.
	EditMessage(channelID, messageID, text string) error
}

// InteractiveChannel is an optional interface for channels that support
// rendering multiple-choice options as interactive elements (e.g. Telegram
// inline keyboard buttons). Channels that don't implement this receive a
// plain numbered text list instead.
type InteractiveChannel interface {
	// SendQuestion sends a question to channelID. If options is non-empty and
	// the channel supports interactive elements, they are rendered as buttons.
	SendQuestion(channelID, question string, options []string) error
}

// MessageHandler is called when a message is received from a channel.
type MessageHandler func(msg IncomingMessage) string

// Manager manages multiple channel connections.
type Manager struct {
	channels []Channel
	handler  MessageHandler
	mu       sync.Mutex
	running  bool
}

// NewManager creates a new channel manager.
func NewManager(handler MessageHandler) *Manager {
	return &Manager{
		handler: handler,
	}
}

// Register adds a channel to the manager.
func (m *Manager) Register(ch Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.channels = append(m.channels, ch)
}

// StartAll starts all registered channels concurrently.
// Blocks until all channels exit or an error occurs.
func (m *Manager) StartAll() error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return fmt.Errorf("channel manager already running")
	}
	m.running = true
	m.mu.Unlock()

	if len(m.channels) == 0 {
		return fmt.Errorf("no channels registered")
	}

	errCh := make(chan error, len(m.channels))

	for _, ch := range m.channels {
		go func(c Channel) {
			fmt.Printf("Starting channel: %s\n", c.Name())
			if err := c.Start(m.handler); err != nil {
				errCh <- fmt.Errorf("channel %s failed: %w", c.Name(), err)
			}
		}(ch)
	}

	// Wait for the first error; stop all channels so remaining goroutines exit.
	err := <-errCh
	m.StopAll()
	return err
}

// StopAll gracefully stops all channels.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, ch := range m.channels {
		fmt.Printf("Stopping channel: %s\n", ch.Name())
		ch.Stop()
	}
	m.running = false
}

// ChannelCount returns the number of registered channels.
func (m *Manager) ChannelCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.channels)
}

// Send sends a message to a specific channel ID across all registered channels.
func (m *Manager) Send(channelType, channelID, text string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, ch := range m.channels {
		if ch.Name() == channelType {
			ch.Send(channelID, text)
		}
	}
}

// SendQuestion sends a question to a specific channel, using interactive
// buttons when the channel supports it. Falls back to a numbered text list.
func (m *Manager) SendQuestion(channelType, channelID, question string, options []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, ch := range m.channels {
		if ch.Name() != channelType {
			continue
		}
		if ic, ok := ch.(InteractiveChannel); ok {
			if err := ic.SendQuestion(channelID, question, options); err != nil {
				fmt.Printf("[%s] SendQuestion error: %s\n", channelType, err)
			}
			return
		}
		// Plain-text fallback.
		text := "❓ " + question
		for i, opt := range options {
			text += fmt.Sprintf("\n%d. %s", i+1, opt)
		}
		if len(options) > 0 {
			text += "\n" + strings.Repeat("-", 20) + "\nReply with a number or your answer."
		}
		ch.Send(channelID, text)
		return
	}
}
