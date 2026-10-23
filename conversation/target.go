// Package conversation describes transport-independent conversation locations.
package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Target identifies an IM surface. Sender is interaction context; OwnerID is
// optional durable binding ownership. Neither replies nor the current sender
// change a shared room/thread's session binding.
type Target struct {
	ChannelType string `json:"channel_type,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	ThreadID    string `json:"thread_id,omitempty"`
	ReplyTo     string `json:"reply_to,omitempty"`
	SenderID    string `json:"sender_id,omitempty"`
	SenderName  string `json:"sender_name,omitempty"`
	OwnerID     string `json:"owner_id,omitempty"`
}

func (t Target) BindingKey() string {
	return BindingKey(t.ChannelType, t.ChannelID, t.ThreadID, t.OwnerID)
}
func (t Target) Room() Target         { t.ThreadID = ""; t.ReplyTo = ""; return t }
func (t Target) WithoutReply() Target { t.ReplyTo = ""; return t }
func (t Target) Valid() bool          { return t.ChannelType != "" && t.ChannelID != "" }

// BindingKey preserves the existing SessionRegistry key format, so migration
// does not change durable associations. Length prefixes avoid ID collisions.
func BindingKey(channelType, channelID, threadID, ownerID string) string {
	var b strings.Builder
	for _, part := range []string{channelType, channelID, threadID, ownerID} {
		fmt.Fprintf(&b, "%d:%s|", len(part), part)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// Reference identifies one message or reaction in its originating surface.
type Reference struct {
	Target    Target `json:"target"`
	MessageID string `json:"message_id"`
}
