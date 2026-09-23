package tools

// InteractionScope identifies the conversation and user that owns an
// interactive request. Empty fields are retained for CLI/backward-compatible
// callers; scoped channel requests should populate every field available.
type InteractionScope struct {
	ChannelType string `json:"channel_type,omitempty"`
	ChannelID   string `json:"channel_id,omitempty"`
	ThreadID    string `json:"thread_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	SenderID    string `json:"sender_id,omitempty"`
}

// Legacy returns the channel-only scope used by the original CLI and tool
// APIs. New channel integrations should use the complete scope.
func LegacyInteractionScope(channelID string) InteractionScope {
	return InteractionScope{ChannelID: channelID}
}

func (s InteractionScope) locationEqual(other InteractionScope) bool {
	return s.ChannelType == other.ChannelType &&
		s.ChannelID == other.ChannelID &&
		s.ThreadID == other.ThreadID &&
		s.SessionID == other.SessionID
}

func (s InteractionScope) equal(other InteractionScope) bool {
	return s.locationEqual(other) && s.SenderID == other.SenderID
}

func (s InteractionScope) isLegacy() bool {
	return s.ChannelType == "" && s.ThreadID == "" && s.SessionID == "" && s.SenderID == ""
}
