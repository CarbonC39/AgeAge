package rendering

import (
	"fmt"
	"strings"
)

// SessionEntry is a presentation model; it carries no storage or platform APIs.
type SessionEntry struct {
	ID, Name, Preview, Link string
	Turns                   int
	Current                 bool
}

func RenderSessions(entries []SessionEntry) string {
	if len(entries) == 0 {
		return "No sessions found."
	}
	var b strings.Builder
	b.WriteString("**Sessions**\n")
	for _, entry := range entries {
		name := escapeLabel(entry.Name)
		if name == "" {
			name = escapeLabel(entry.ID)
		}
		if entry.Link != "" {
			name = "[" + name + "](" + entry.Link + ")"
		}
		marker := ""
		if entry.Current {
			marker = " · current"
		}
		fmt.Fprintf(&b, "• %s · %d turns%s\n  ID: `%s`\n", name, entry.Turns, marker, entry.ID)
		if entry.Preview != "" {
			fmt.Fprintf(&b, "  %s\n", escapeLabel(entry.Preview))
		}
	}
	return strings.TrimSpace(b.String())
}
func RenderSessionRoot(name, id string) string {
	return fmt.Sprintf("**Session: %s**\n\n%s", escapeLabel(name), RenderSessionDetails(id))
}
func RenderSessionDetails(id string) string {
	return fmt.Sprintf("Session `%s`\nReply here to continue this conversation.", id)
}
func escapeLabel(s string) string {
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "`", "\\`", "\n", " ", "\r", " ").Replace(s)
}
