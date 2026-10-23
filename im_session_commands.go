package main

import (
	"fmt"
	"strings"

	"ageage/channel"
	"ageage/conversation"
	"ageage/rendering"
)

type imSessionCommands struct {
	sessions   *imSessions
	surfaceFor func(conversation.Target) channel.Surface
}

func (c imSessionCommands) list(target conversation.Target) (string, error) {
	infos, err := c.sessions.List(target)
	if err != nil {
		return "", err
	}
	entries := make([]rendering.SessionEntry, 0, len(infos))
	for _, info := range infos {
		entry := rendering.SessionEntry{ID: info.ID, Name: info.DisplayName, Preview: info.Preview, Turns: info.TurnCount, Current: info.FullID == c.sessions.Current(target)}
		if destination, ok := c.sessions.Destination(info.FullID); ok {
			entry.Link = c.surfaceFor(destination).Link(conversation.Reference{Target: destination, MessageID: destination.ThreadID})
		}
		entries = append(entries, entry)
	}
	return rendering.RenderSessions(entries), nil
}

func (c imSessionCommands) handle(surface channel.Surface, cmd imCommand) (string, error) {
	target := surface.Target()
	if cmd.is("sessions") {
		return c.list(target)
	}
	args := strings.TrimSpace(cmd.Args)
	sub, argument := "", ""
	if fields := strings.Fields(args); len(fields) > 0 {
		sub = fields[0]
		argument = strings.TrimSpace(args[len(sub):])
	}
	switch strings.ToLower(sub) {
	case "list", "ls":
		return c.list(target)
	case "new", "n":
		roomSurface := c.surfaceFor(target.Room())
		if target.ThreadID != "" && !roomSurface.Capabilities().CreateThread {
			return "", fmt.Errorf("this thread keeps its session; create a new topic or start a session in the main chat")
		}
		id, name, err := c.sessions.Create(target, argument)
		if err != nil {
			return "", err
		}
		destination := target.Room()
		shortID := strings.TrimPrefix(id, imSessionPrefix(target)+"-")
		body := rendering.RenderSessionRoot(name, shortID)
		if roomSurface.Capabilities().CreateThread {
			child, root, openErr := roomSurface.OpenThread("Session: "+name, rendering.RenderSessionDetails(shortID))
			if openErr != nil {
				_ = c.sessions.store.Delete(id)
				return "", openErr
			}
			destination = child.Target()
			if err = c.sessions.Bind(destination, id); err != nil {
				_, _ = child.Update(root, "Session could not be bound: "+err.Error())
				_ = c.sessions.store.Delete(id)
				return "", err
			}
			link := child.Link(root)
			if link != "" {
				return fmt.Sprintf("✅ Session **%s** created. Continue in [its thread](%s).", name, link), nil
			}
			return fmt.Sprintf("✅ Session **%s** created. Continue in its new thread.", name), nil
		}
		if err = c.sessions.Bind(destination, id); err != nil {
			_ = c.sessions.store.Delete(id)
			return "", err
		}
		if _, err = roomSurface.Send(body); err != nil {
			return "", err
		}
		return "", nil
	case "switch", "sw":
		if argument == "" {
			return "", fmt.Errorf("usage: %ssession switch <name or ID>", cmd.Prefix)
		}
		info, err := c.sessions.Find(target, argument)
		if err != nil {
			return "", err
		}
		if info.FullID == c.sessions.Current(target) {
			return "✅ This conversation already uses **" + info.DisplayName + "**.", nil
		}
		if destination, ok := c.sessions.Destination(info.FullID); ok {
			if destination.ChannelType != target.ChannelType || destination.ChannelID != target.ChannelID || destination.OwnerID != target.OwnerID {
				return "", fmt.Errorf("session belongs to another conversation")
			}
			link := c.surfaceFor(destination).Link(conversation.Reference{Target: destination, MessageID: destination.ThreadID})
			if link != "" {
				return fmt.Sprintf("Continue **%s** [in its existing conversation](%s). This conversation keeps its session.", info.DisplayName, link), nil
			}
			return "", fmt.Errorf("session %q is bound to another conversation; this conversation keeps its session", info.DisplayName)
		}
		if target.ThreadID != "" {
			return "", fmt.Errorf("this thread keeps its session; use a session's existing thread or switch from the main chat")
		}
		if err = c.sessions.Bind(target, info.FullID); err != nil {
			return "", err
		}
		return "✅ Switched to **" + info.DisplayName + "**.", nil
	case "remove", "rm", "delete":
		if argument == "" {
			return "", fmt.Errorf("usage: %ssession remove <name or ID>", cmd.Prefix)
		}
		info, err := c.sessions.Find(target, argument)
		if err != nil {
			return "", err
		}
		if err = c.sessions.Remove(target, info.FullID); err != nil {
			return "", err
		}
		return "🗑️ Removed **" + info.DisplayName + "**.", nil
	case "":
		id := c.sessions.Current(target)
		if id == "" {
			return "No session is active here. Use " + cmd.Prefix + "session new [name].", nil
		}
		info, err := c.sessions.Find(target, id)
		name := info.DisplayName
		if err != nil {
			return "", err
		}
		if name == "" {
			name = strings.TrimPrefix(id, imSessionPrefix(target)+"-")
		}
		return "**Current session:** " + name + "\nID: `" + id + "`", nil
	default:
		return "", fmt.Errorf("usage: %ssession [list | new [name] | switch <name or ID> | remove <name or ID>]", cmd.Prefix)
	}
}
