package channel

import (
	"ageage/conversation"
	"errors"
)

var ErrUnsupported = errors.New("conversation capability is unavailable")

// Capabilities describes available actions, not platform names.
type Capabilities struct {
	Edit, Reply, Threads, CreateThread, Reactions, Typing, InteractiveQuestions, DeepLinks, ReadReceipts bool
}

// Surface binds every operation to one conversation target. Platform adapters
// own native IDs, thread routing and fallbacks; callers express semantic intent.
type Surface interface {
	Target() conversation.Target
	Capabilities() Capabilities
	Send(string) (conversation.Reference, error)
	Reply(string) (conversation.Reference, error)
	Update(conversation.Reference, string) (conversation.Reference, error)
	OpenThread(title, body string) (Surface, conversation.Reference, error)
	React(conversation.Reference, string) (conversation.Reference, error)
	Unreact(conversation.Reference) error
	Typing(bool) error
	Read() error
	Ask(question string, options []string) error
	Link(conversation.Reference) string
}

// The following optional connector interfaces localize platform adaptations.
type DeliveryLocator interface {
	DeliveryID(conversation.Target) string
}
type ThreadCreator interface {
	CreateConversationThread(conversation.Target, string, string) (conversation.Target, conversation.Reference, error)
}
type ThreadAvailability interface {
	CanCreateConversationThread(conversation.Target) bool
}
type TargetedQuestion interface {
	AskInConversation(conversation.Target, string, []string) error
}
type MessageReplier interface {
	ReplyMessage(conversation.Target, string) (string, error)
}
type TargetedEditor interface {
	EditInConversation(conversation.Target, string, string) error
}
type TargetedReader interface {
	ReadInConversation(conversation.Target) error
}
type DeepLinker interface {
	ConversationLink(conversation.Reference) string
}

// ConversationTracker lets a connector resume polling durable conversation
// targets restored by the runtime after a restart.
type ConversationTracker interface {
	TrackConversation(conversation.Target)
}
