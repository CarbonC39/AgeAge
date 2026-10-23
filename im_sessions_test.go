package main

import (
	"strings"
	"testing"
	"time"

	"ageage/agent"
	"ageage/conversation"
	"ageage/llm"
)

func newTestIMSessions(t *testing.T) (*imSessions, *agent.SessionManager, *agent.SessionRegistry, string) {
	t.Helper()
	root := t.TempDir()
	store := agent.NewSessionManager(root)
	registry, err := agent.OpenSessionRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	return newIMSessions(store, registry), store, registry, root
}

func TestIMSessionsCreateSeparatesOpaqueIDFromUnicodeDisplayName(t *testing.T) {
	sessions, store, _, _ := newTestIMSessions(t)
	target := conversation.Target{ChannelType: "matrix", ChannelID: "!room:example.org"}
	id, name, err := sessions.Create(target, "项目计划 Draft 2")
	if err != nil {
		t.Fatal(err)
	}
	if name != "项目计划 Draft 2" {
		t.Fatalf("Create display name = %q", name)
	}
	if strings.Contains(id, "项目") || strings.Contains(id, " ") || strings.Contains(id, "Draft") {
		t.Fatalf("session ID is not opaque: %q", id)
	}
	got, err := store.DisplayName(id)
	if err != nil || got != name {
		t.Fatalf("stored display name = %q, %v", got, err)
	}
}

func TestIMSessionsFindDuplicateNamesByID(t *testing.T) {
	sessions, _, _, _ := newTestIMSessions(t)
	target := conversation.Target{ChannelType: "discord", ChannelID: "123"}
	first, _, err := sessions.Create(target, "Planning Notes")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := sessions.Create(target, "Planning Notes")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("Create returned duplicate IDs: %q", first)
	}
	if _, err := sessions.Find(target, "Planning Notes"); err == nil || !strings.Contains(err.Error(), "use its ID") {
		t.Fatalf("duplicate display-name resolution error = %v", err)
	}
	for _, id := range []string{first, second} {
		info, err := sessions.Find(target, id)
		if err != nil || info.FullID != id || info.DisplayName != "Planning Notes" {
			t.Fatalf("Find by ID %q = %#v, %v", id, info, err)
		}
	}
}

func TestIMSessionsThreadBindingStableAndRoomBindingSwitchable(t *testing.T) {
	sessions, store, _, _ := newTestIMSessions(t)
	thread := conversation.Target{ChannelType: "matrix", ChannelID: "!room:example.org", ThreadID: "$root"}
	threadID, err := sessions.Resolve(thread)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("other-thread-session"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Bind(thread, "other-thread-session"); err == nil || !strings.Contains(err.Error(), "thread") {
		t.Fatalf("thread binding switch error = %v", err)
	}
	if got, err := sessions.Resolve(thread); err != nil || got != threadID {
		t.Fatalf("thread binding changed: id=%q err=%v want=%q", got, err, threadID)
	}

	room := thread.Room()
	roomID, err := sessions.Resolve(room)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession("room-session-next"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Bind(room, "room-session-next"); err != nil {
		t.Fatalf("room binding switch rejected: %v", err)
	}
	if got := sessions.Current(room); got != "room-session-next" || got == roomID {
		t.Fatalf("Current room session = %q, previous %q", got, roomID)
	}
}

func TestIMSessionsResolveRestoresStableThreadBinding(t *testing.T) {
	sessions, store, _, root := newTestIMSessions(t)
	target := conversation.Target{ChannelType: "matrix", ChannelID: "!room:example.org", ThreadID: "$event-root"}
	id, err := sessions.Resolve(target)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agent.OpenSessionRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newIMSessions(agent.NewSessionManager(root), registry)
	if got := restarted.Current(target); got != id {
		t.Fatalf("restored Current = %q, want %q", got, id)
	}
	if got, err := restarted.Resolve(target); err != nil || got != id {
		t.Fatalf("restored Resolve = %q, %v; want %q", got, err, id)
	}
	if _, err := store.DisplayName(id); err != nil {
		t.Fatalf("restored session is absent: %v", err)
	}
}

func TestIMSessionsRoomSwitchDoesNotLeaveStaleDestination(t *testing.T) {
	sessions, _, _, _ := newTestIMSessions(t)
	room := conversation.Target{ChannelType: "telegram", ChannelID: "123"}
	first, err := sessions.Resolve(room)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := sessions.Create(room, "Next")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Bind(room, second); err != nil {
		t.Fatal(err)
	}
	if _, bound := sessions.Destination(first); bound {
		t.Fatal("unbound old room session still reports a destination")
	}
	if err := sessions.Bind(room, first); err != nil {
		t.Fatal(err)
	}
	if sessions.Current(room) != first {
		t.Fatal("could not switch back to the old room session")
	}
}

func TestIMSessionsSanitizationCollisionsRemainIsolated(t *testing.T) {
	sessions, store, _, _ := newTestIMSessions(t)
	first := conversation.Target{ChannelType: "matrix", ChannelID: "a:b"}
	second := conversation.Target{ChannelType: "matrix", ChannelID: "a b"}
	if imSessionPrefix(first) != imSessionPrefix(second) {
		t.Fatalf("test targets do not collide after sanitization: %q vs %q", imSessionPrefix(first), imSessionPrefix(second))
	}
	firstID, err := sessions.Resolve(first)
	if err != nil {
		t.Fatal(err)
	}
	secondID, err := sessions.Resolve(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID {
		t.Fatalf("distinct targets share session ID %q", firstID)
	}
	for _, item := range []struct {
		target  conversation.Target
		own     string
		foreign string
	}{{first, firstID, secondID}, {second, secondID, firstID}} {
		infos, err := sessions.List(item.target)
		if err != nil {
			t.Fatal(err)
		}
		foundOwn, foundForeign := false, false
		for _, info := range infos {
			foundOwn = foundOwn || info.FullID == item.own
			foundForeign = foundForeign || info.FullID == item.foreign
		}
		if !foundOwn || foundForeign {
			t.Fatalf("target %q sees own=%v foreign=%v sessions=%#v", item.target.ChannelID, foundOwn, foundForeign, infos)
		}
	}
	if _, err := store.LoadHistory(firstID); err != nil {
		t.Fatal(err)
	}
}

func TestIMSessionsListPreservesPreviewAndCurrentDoesNotWaitForHistory(t *testing.T) {
	sessions, store, _, _ := newTestIMSessions(t)
	target := conversation.Target{ChannelType: "telegram", ChannelID: "42", ThreadID: "topic"}
	id, err := sessions.Resolve(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveHistory(id, []llm.Message{{Role: "user", Content: "preview survives target filtering"}}); err != nil {
		t.Fatal(err)
	}
	infos, err := sessions.List(target)
	if err != nil || len(infos) != 1 || infos[0].FullID != id || infos[0].Preview != "preview survives target filtering" {
		t.Fatalf("session list = %#v, %v", infos, err)
	}

	_, unlock := store.BeginSessionTransaction(id)
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	listDone := make(chan struct{})
	go func() {
		_, _ = sessions.List(target)
		close(listDone)
	}()
	currentDone := make(chan string, 1)
	go func() { currentDone <- sessions.Current(target) }()
	select {
	case current := <-currentDone:
		if current != id {
			t.Fatalf("Current = %q, want %q", current, id)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Current waited on the history/session transaction lock")
	}
	unlock()
	locked = false
	select {
	case <-listDone:
	case <-time.After(time.Second):
		t.Fatal("List did not finish after history transaction released")
	}
}

func TestIMSessionsOwnerNamespacesAndBindingsAreIsolated(t *testing.T) {
	sessions, store, registry, _ := newTestIMSessions(t)
	room := conversation.Target{ChannelType: "matrix", ChannelID: "!shared:example.org"}
	alice := room
	alice.OwnerID = "@alice:example.org"
	bob := room
	bob.OwnerID = "@bob:example.org"

	aliceID, err := sessions.Resolve(alice)
	if err != nil {
		t.Fatal(err)
	}
	bobID, err := sessions.Resolve(bob)
	if err != nil {
		t.Fatal(err)
	}
	if aliceID == bobID || !strings.Contains(aliceID, "-owner-") || !strings.Contains(bobID, "-owner-") {
		t.Fatalf("owner session IDs are not namespaced: Alice=%q Bob=%q", aliceID, bobID)
	}
	if err := store.SaveHistory(aliceID, []llm.Message{{Role: "user", Content: "Alice private preview"}}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []conversation.Target{alice, bob, room} {
		infos, err := sessions.List(target)
		if err != nil {
			t.Fatal(err)
		}
		wantID := ""
		if target.OwnerID == alice.OwnerID {
			wantID = aliceID
		} else if target.OwnerID == bob.OwnerID {
			wantID = bobID
		}
		for _, info := range infos {
			if info.FullID == aliceID && info.FullID != wantID {
				t.Fatalf("target owner %q sees Alice's private session: %#v", target.OwnerID, info)
			}
			if info.FullID == bobID && info.FullID != wantID {
				t.Fatalf("target owner %q sees Bob's private session: %#v", target.OwnerID, info)
			}
		}
		if wantID != "" {
			found := false
			for _, info := range infos {
				found = found || info.FullID == wantID
			}
			if !found {
				t.Fatalf("target owner %q cannot see own session %q: %#v", target.OwnerID, wantID, infos)
			}
		}
	}
	if _, err := sessions.Find(bob, aliceID); err == nil {
		t.Fatal("Bob found Alice's session by canonical ID")
	}

	// Unbound sessions remain visible only inside their owner's namespace,
	// including after their prior room binding is switched away.
	aliceUnbound, _, err := sessions.Create(alice, "Alice draft")
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Bind(alice, aliceUnbound); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Bind(alice, aliceID); err != nil {
		t.Fatal(err)
	}
	for _, target := range []conversation.Target{alice, bob, room} {
		infos, err := sessions.List(target)
		if err != nil {
			t.Fatal(err)
		}
		for _, info := range infos {
			if info.FullID == aliceUnbound && target.OwnerID != alice.OwnerID {
				t.Fatalf("target owner %q sees Alice's unbound session: %#v", target.OwnerID, info)
			}
		}
	}

	// A legacy owner binding keeps its original ID and remains discoverable.
	legacy := imSessionLegacyPrefix(alice) + "-legacy-private"
	if err := store.CreateSession(legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDisplayName(legacy, "Legacy private"); err != nil {
		t.Fatal(err)
	}
	legacyTarget := alice
	legacyTarget.ThreadID = "$legacy-thread"
	if err := registry.BindTarget(legacyTarget, legacy, "thread"); err != nil {
		t.Fatal(err)
	}
	if got, err := sessions.Resolve(legacyTarget); err != nil || got != legacy {
		t.Fatalf("legacy owner binding resolve = %q, %v; want %q", got, err, legacy)
	}
	legacyInfo, err := sessions.Find(alice, legacy)
	if err != nil || legacyInfo.FullID != legacy {
		t.Fatalf("owner could not find legacy binding: %#v, %v", legacyInfo, err)
	}
	if _, err := sessions.Find(bob, legacy); err == nil {
		t.Fatal("Bob found Alice's legacy owner session")
	}
}
