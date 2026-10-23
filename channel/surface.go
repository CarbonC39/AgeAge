package channel

import (
	"errors"
	"fmt"
	"strings"

	"ageage/conversation"
)

var ErrWrongSurface = errors.New("message reference belongs to another conversation")

type surface struct {
	ch     Channel
	target conversation.Target
}

// NewSurface binds all connector operations to one logical conversation target.
func NewSurface(ch Channel, target conversation.Target) Surface {
	if ch != nil && target.ChannelType == "" {
		target.ChannelType = ch.Name()
	}
	return &surface{ch: ch, target: target}
}

func (s *surface) Target() conversation.Target { return s.target }

func (s *surface) Capabilities() Capabilities {
	_, edit := s.ch.(Editable)
	_, targetedEdit := s.ch.(TargetedEditor)
	_, replier := s.ch.(MessageReplier)
	_, threadEdit := s.ch.(ThreadEditable)
	_, locator := s.ch.(DeliveryLocator)
	_, creator := s.ch.(ThreadCreator)
	canCreateThread := creator && s.canCreateThread()
	_, reactions := s.ch.(Reactor)
	_, typing := s.ch.(TypingIndicator)
	_, questions := s.ch.(TargetedQuestion)
	if !questions && s.target.ThreadID == "" {
		_, questions = s.ch.(InteractiveChannel)
	}
	_, links := s.ch.(DeepLinker)
	_, read := s.ch.(ReadReceiptSender)
	_, targetedRead := s.ch.(TargetedReader)
	return Capabilities{Edit: edit || targetedEdit, Reply: replier || s.ch != nil, Threads: threadEdit || creator || locator, CreateThread: canCreateThread, Reactions: reactions, Typing: typing, InteractiveQuestions: questions, DeepLinks: links, ReadReceipts: read || targetedRead}
}

func (s *surface) canCreateThread() bool {
	if s.target.ThreadID != "" {
		return false
	}
	if availability, ok := s.ch.(ThreadAvailability); ok {
		return availability.CanCreateConversationThread(s.target)
	}
	return true
}

func (s *surface) Send(text string) (conversation.Reference, error) {
	if s.ch == nil {
		return conversation.Reference{}, ErrUnsupported
	}
	var id string
	if s.target.ThreadID != "" {
		if threaded, ok := s.ch.(ThreadEditable); ok {
			var err error
			id, err = threaded.SendMessageInThread(s.target.ChannelID, s.target.ThreadID, s.target.ReplyTo, text)
			return conversation.Reference{Target: s.target, MessageID: id}, err
		}
		if _, ok := s.ch.(DeliveryLocator); !ok {
			return conversation.Reference{}, ErrUnsupported
		}
	}
	if editable, ok := s.ch.(Editable); ok {
		var err error
		id, err = editable.SendMessage(s.deliveryID(), text)
		return conversation.Reference{Target: s.target, MessageID: id}, err
	}
	return conversation.Reference{Target: s.target}, s.ch.Send(s.deliveryID(), text)
}

func (s *surface) Reply(text string) (conversation.Reference, error) {
	if s.ch == nil {
		return conversation.Reference{}, ErrUnsupported
	}
	target := s.target
	var id string
	var err error
	if replier, ok := s.ch.(MessageReplier); ok {
		id, err = replier.ReplyMessage(target, text)
	} else if target.ThreadID != "" {
		if threaded, ok := s.ch.(ThreadEditable); ok {
			id, err = threaded.SendMessageInThread(target.ChannelID, target.ThreadID, target.ReplyTo, text)
		} else if _, ok := s.ch.(DeliveryLocator); ok {
			err = s.ch.Send(s.deliveryID(), text)
		} else {
			err = ErrUnsupported
		}
	} else if target.ReplyTo != "" {
		err = s.ch.Reply(s.deliveryID(), target.ReplyTo, text)
	} else {
		err = s.ch.Send(s.deliveryID(), text)
	}
	return conversation.Reference{Target: s.target, MessageID: id}, err
}

func (s *surface) Update(ref conversation.Reference, text string) (conversation.Reference, error) {
	if !sameConversation(ref.Target, s.target) {
		return conversation.Reference{}, ErrWrongSurface
	}
	if ref.MessageID == "" {
		return s.Send(text)
	}
	if targeted, ok := s.ch.(TargetedEditor); ok {
		if err := targeted.EditInConversation(s.target, ref.MessageID, text); err == nil {
			return ref, nil
		}
		return s.Send(text)
	}
	if editable, ok := s.ch.(Editable); ok {
		if err := editable.EditMessage(s.deliveryID(), ref.MessageID, text); err == nil {
			return ref, nil
		}
	}
	// Edits may be unsupported or fail (e.g. an expired platform edit window).
	// Send the replacement to this same surface so thread context is retained.
	return s.Send(text)
}

func (s *surface) OpenThread(title, body string) (Surface, conversation.Reference, error) {
	creator, ok := s.ch.(ThreadCreator)
	if !ok || !s.canCreateThread() {
		return nil, conversation.Reference{}, ErrUnsupported
	}
	target, ref, err := creator.CreateConversationThread(s.target, title, body)
	if err != nil {
		return nil, conversation.Reference{}, err
	}
	return NewSurface(s.ch, target), ref, nil
}

func (s *surface) React(ref conversation.Reference, emoji string) (conversation.Reference, error) {
	if !sameConversation(ref.Target, s.target) {
		return conversation.Reference{}, ErrWrongSurface
	}
	reactor, ok := s.ch.(Reactor)
	if !ok {
		return conversation.Reference{}, ErrUnsupported
	}
	id, err := reactor.React(s.deliveryID(), ref.MessageID, emoji)
	return conversation.Reference{Target: s.target, MessageID: id}, err
}

func (s *surface) Unreact(ref conversation.Reference) error {
	if !sameConversation(ref.Target, s.target) {
		return ErrWrongSurface
	}
	reactor, ok := s.ch.(Reactor)
	if !ok {
		return ErrUnsupported
	}
	return reactor.Unreact(s.deliveryID(), ref.MessageID)
}

func (s *surface) Typing(typing bool) error {
	indicator, ok := s.ch.(TypingIndicator)
	if !ok {
		return ErrUnsupported
	}
	return indicator.SendTyping(s.deliveryID(), typing)
}

func (s *surface) Read() error {
	if reader, ok := s.ch.(TargetedReader); ok {
		if s.target.ReplyTo == "" {
			return fmt.Errorf("read receipt requires a message reference")
		}
		return reader.ReadInConversation(s.target)
	}
	reader, ok := s.ch.(ReadReceiptSender)
	if !ok {
		return ErrUnsupported
	}
	if s.target.ReplyTo == "" {
		return fmt.Errorf("read receipt requires a message reference")
	}
	return reader.SendReadReceipt(s.deliveryID(), s.target.ReplyTo)
}

func (s *surface) Ask(question string, options []string) error {
	if targeted, ok := s.ch.(TargetedQuestion); ok {
		return targeted.AskInConversation(s.target, question, options)
	}
	if s.target.ThreadID == "" {
		if interactive, ok := s.ch.(InteractiveChannel); ok {
			return interactive.SendQuestion(s.deliveryID(), question, options)
		}
	}
	var b strings.Builder
	b.WriteString("❓ ")
	b.WriteString(question)
	for i, option := range options {
		fmt.Fprintf(&b, "\n%d. %s", i+1, option)
	}
	if len(options) > 0 {
		b.WriteString("\nReply with a number or your answer.")
	}
	_, err := s.Send(b.String())
	return err
}

func (s *surface) Link(ref conversation.Reference) string {
	if !sameConversation(ref.Target, s.target) {
		return ""
	}
	linker, ok := s.ch.(DeepLinker)
	if !ok {
		return ""
	}
	return linker.ConversationLink(ref)
}

func (s *surface) deliveryID() string {
	if locator, ok := s.ch.(DeliveryLocator); ok {
		return locator.DeliveryID(s.target)
	}
	return s.target.ChannelID
}

func sameConversation(a, b conversation.Target) bool {
	return a.BindingKey() == b.BindingKey()
}
