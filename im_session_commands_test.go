package main

import (
	"strings"
	"testing"

	"ageage/agent"
	"ageage/channel"
	"ageage/conversation"
	"ageage/llm"
)

type sessionUXSurface struct {
	channel.Surface
	target       conversation.Target
	capabilities channel.Capabilities
	link         string
	child        channel.Surface
	root         conversation.Reference
	openedTitle  string
	openedBody   string
	sent         []string
}

func (s *sessionUXSurface) Target() conversation.Target        { return s.target }
func (s *sessionUXSurface) Capabilities() channel.Capabilities { return s.capabilities }
func (s *sessionUXSurface) Link(conversation.Reference) string { return s.link }
func (s *sessionUXSurface) Send(text string) (conversation.Reference, error) {
	s.sent = append(s.sent, text)
	return conversation.Reference{Target: s.target}, nil
}
func (s *sessionUXSurface) OpenThread(title, body string) (channel.Surface, conversation.Reference, error) {
	s.openedTitle, s.openedBody = title, body
	return s.child, s.root, nil
}

func newSessionUXFixture(t *testing.T) (*imSessions, *agent.SessionManager) {
	t.Helper()
	root := t.TempDir()
	store := agent.NewSessionManager(root)
	registry, err := agent.OpenSessionRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	return newIMSessions(store, registry), store
}

func sessionUXCommand(args string) imCommand {
	return imCommand{Prefix: "!", Name: "session", Args: args, Found: true}
}

func TestIMSessionNewCreatesDistinctThreadRootAndDeepLink(t *testing.T) {
	sessions, _ := newSessionUXFixture(t)
	room := conversation.Target{ChannelType: "matrix", ChannelID: "!room:example.org"}
	oldRoomID, err := sessions.Resolve(room)
	if err != nil {
		t.Fatal(err)
	}
	userReplyTarget := room
	userReplyTarget.ReplyTo = "$user-reply"
	userReplyTarget.SenderID = "@alice:example.org"
	createdThreadTarget := room
	createdThreadTarget.ThreadID = "$new-session-root"
	createdThreadTarget.ReplyTo = "$new-session-root"
	child := &sessionUXSurface{target: createdThreadTarget, link: "https://matrix.to/#/%21room%3Aexample.org/%24new-session-root"}
	roomSurface := &sessionUXSurface{
		target:       userReplyTarget,
		capabilities: channel.Capabilities{CreateThread: true},
		child:        child,
		root:         conversation.Reference{Target: createdThreadTarget, MessageID: "$new-session-root"},
	}
	commands := imSessionCommands{sessions: sessions, surfaceFor: func(target conversation.Target) channel.Surface {
		if target.ThreadID != "" {
			return child
		}
		return roomSurface
	}}

	reply, err := commands.handle(roomSurface, sessionUXCommand("new 计划 Draft 2"))
	if err != nil {
		t.Fatal(err)
	}
	if roomSurface.openedTitle != "Session: 计划 Draft 2" || strings.Contains(roomSurface.openedBody, "**Session:") || !strings.Contains(roomSurface.openedBody, "Session `") || !strings.Contains(roomSurface.openedBody, "Reply here to continue") {
		t.Fatalf("opened thread title/body = %q / %q", roomSurface.openedTitle, roomSurface.openedBody)
	}
	if !strings.Contains(reply, child.link) {
		t.Fatalf("new-session reply omitted thread deeplink: %q", reply)
	}
	if strings.Contains(roomSurface.openedBody, "$user-reply") || createdThreadTarget.ThreadID == userReplyTarget.ReplyTo {
		t.Fatalf("session thread root reused user reply target: body=%q target=%#v", roomSurface.openedBody, createdThreadTarget)
	}
	newID := sessions.Current(createdThreadTarget)
	if newID == "" || newID == oldRoomID || sessions.Current(room) != oldRoomID {
		t.Fatalf("new thread/current room bindings changed: thread=%q room=%q old=%q", newID, sessions.Current(room), oldRoomID)
	}
	if destination, ok := sessions.Destination(newID); !ok || destination.ThreadID != "$new-session-root" {
		t.Fatalf("new session destination = %#v, %v", destination, ok)
	}
}

func TestIMSessionThreadSwitchNavigatesWithoutRebinding(t *testing.T) {
	sessions, store := newSessionUXFixture(t)
	room := conversation.Target{ChannelType: "matrix", ChannelID: "!room:example.org"}
	first := room
	first.ThreadID = "$thread-one"
	second := room
	second.ThreadID = "$thread-two"
	firstID, err := sessions.Resolve(first)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := sessions.Resolve(second)
	if err != nil {
		t.Fatal(err)
	}
	firstSurface := &sessionUXSurface{target: first}
	secondSurface := &sessionUXSurface{target: second, link: "https://matrix.to/#/room/thread-two"}
	commands := imSessionCommands{sessions: sessions, surfaceFor: func(target conversation.Target) channel.Surface {
		if target.ThreadID == second.ThreadID {
			return secondSurface
		}
		return firstSurface
	}}
	reply, err := commands.handle(firstSurface, sessionUXCommand("switch "+secondID))
	if err != nil || !strings.Contains(reply, secondSurface.link) || !strings.Contains(reply, "keeps its session") {
		t.Fatalf("thread switch result = %q, %v", reply, err)
	}
	if sessions.Current(first) != firstID || sessions.Current(second) != secondID {
		t.Fatalf("thread switch changed bindings: first=%q second=%q", sessions.Current(first), sessions.Current(second))
	}

	unboundID, _, err := sessions.Create(room, "Unbound room session")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("sentinel"); err != nil {
		t.Fatal(err)
	}
	if _, err := commands.handle(firstSurface, sessionUXCommand("switch "+unboundID)); err == nil {
		t.Fatal("thread switched to an unbound session instead of refusing")
	}
	if sessions.Current(first) != firstID {
		t.Fatal("refused thread switch changed its stable binding")
	}
}

func TestIMSessionRoomNewFallsBackToSendAndSwitchesRoomBinding(t *testing.T) {
	sessions, _ := newSessionUXFixture(t)
	room := conversation.Target{ChannelType: "telegram", ChannelID: "chat-7"}
	oldID, err := sessions.Resolve(room)
	if err != nil {
		t.Fatal(err)
	}
	surface := &sessionUXSurface{target: room}
	commands := imSessionCommands{sessions: sessions, surfaceFor: func(conversation.Target) channel.Surface { return surface }}
	if reply, err := commands.handle(surface, imCommand{Prefix: "/", Name: "session", Args: "new Team Notes", Found: true}); err != nil || reply != "" {
		t.Fatalf("room fallback result = %q, %v", reply, err)
	}
	if len(surface.sent) != 1 || !strings.Contains(surface.sent[0], "**Session: Team Notes**") {
		t.Fatalf("fallback root messages = %#v", surface.sent)
	}
	if current := sessions.Current(room); current == "" || current == oldID {
		t.Fatalf("room binding did not switch: current=%q old=%q", current, oldID)
	}
}

func TestIMSessionNewInNonCreatingTopicRefusesWithoutRebinding(t *testing.T) {
	sessions, _ := newSessionUXFixture(t)
	room := conversation.Target{ChannelType: "telegram", ChannelID: "chat-8"}
	topic := room
	topic.ThreadID = "topic-1"
	threadID, err := sessions.Resolve(topic)
	if err != nil {
		t.Fatal(err)
	}
	roomSurface := &sessionUXSurface{target: room}
	threadSurface := &sessionUXSurface{target: topic}
	commands := imSessionCommands{sessions: sessions, surfaceFor: func(target conversation.Target) channel.Surface {
		if target.ThreadID != "" {
			return threadSurface
		}
		return roomSurface
	}}
	reply, err := commands.handle(threadSurface, imCommand{Prefix: "/", Name: "session", Args: "new Nested", Found: true})
	if err == nil || !strings.Contains(err.Error(), "keeps its session") || reply != "" {
		t.Fatalf("topic new result = %q, %v", reply, err)
	}
	if sessions.Current(topic) != threadID || len(roomSurface.sent) != 0 {
		t.Fatalf("topic new changed binding or sent fallback: current=%q sent=%#v", sessions.Current(topic), roomSurface.sent)
	}
}

func TestIMSessionListRendersNamesPreviewTurnsLinkAndIDSelection(t *testing.T) {
	sessions, store := newSessionUXFixture(t)
	room := conversation.Target{ChannelType: "discord", ChannelID: "room-9"}
	if _, err := sessions.Resolve(room); err != nil {
		t.Fatal(err)
	}
	firstID, _, err := sessions.Create(room, "Shared Plan")
	if err != nil {
		t.Fatal(err)
	}
	secondID, _, err := sessions.Create(room, "Shared Plan")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveHistory(secondID, []llm.Message{{Role: "user", Content: "Summarize this thread"}}); err != nil {
		t.Fatal(err)
	}
	link := "https://discord.example/conversation"
	surface := &sessionUXSurface{target: room, link: link}
	commands := imSessionCommands{sessions: sessions, surfaceFor: func(conversation.Target) channel.Surface { return surface }}
	shortID := strings.TrimPrefix(secondID, imSessionPrefix(room)+"-")
	if reply, err := commands.handle(surface, imCommand{Prefix: "/", Name: "session", Args: "switch " + shortID, Found: true}); err != nil || !strings.Contains(reply, "Shared Plan") {
		t.Fatalf("switch by ID = %q, %v", reply, err)
	}
	if got := sessions.Current(room); got != secondID || got == firstID {
		t.Fatalf("switch by canonical ID selected %q, want %q", got, secondID)
	}
	list, err := commands.handle(surface, imCommand{Prefix: "/", Name: "session", Args: "list", Found: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Shared Plan", "Summarize this thread", "1 turns", link, "current", shortID} {
		if !strings.Contains(list, want) {
			t.Fatalf("session list missing %q: %s", want, list)
		}
	}
}

func TestIMSessionCommandsDoNotExposeOtherOwnerSessionsOrLinks(t *testing.T) {
	sessions, store := newSessionUXFixture(t)
	room := conversation.Target{ChannelType: "matrix", ChannelID: "!shared:example.org"}
	alice := room
	alice.OwnerID = "alice"
	bob := room
	bob.OwnerID = "bob"
	privateTarget := alice
	privateTarget.ThreadID = "$alice-thread"
	privateID, err := sessions.Resolve(privateTarget)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveHistory(privateID, []llm.Message{{Role: "user", Content: "Alice confidential preview"}}); err != nil {
		t.Fatal(err)
	}
	aliceSurface := &sessionUXSurface{target: privateTarget, link: "https://chat.invalid/alice-thread"}
	bobSurface := &sessionUXSurface{target: bob}
	commands := imSessionCommands{sessions: sessions, surfaceFor: func(target conversation.Target) channel.Surface {
		if target.OwnerID == alice.OwnerID {
			return aliceSurface
		}
		return bobSurface
	}}
	list, err := commands.handle(bobSurface, sessionUXCommand("list"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"Alice confidential preview", privateID, aliceSurface.link} {
		if strings.Contains(list, secret) {
			t.Fatalf("Bob's list leaks %q: %s", secret, list)
		}
	}
	if _, err := sessions.Find(bob, privateID); err == nil {
		t.Fatal("Bob found Alice's session by ID")
	}
	reply, err := commands.handle(bobSurface, sessionUXCommand("switch "+privateID))
	if err == nil || strings.Contains(reply, "alice-thread") {
		t.Fatalf("Bob switch exposed Alice's destination: reply=%q err=%v", reply, err)
	}
}
