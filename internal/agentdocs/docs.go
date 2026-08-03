// Package agentdocs embeds compact framework documentation for agent self-reference.
// Documents are bundled into the binary at compile time via go:embed so they are
// always available regardless of deployment layout (no source tree required).
package agentdocs

import (
	"embed"
)

//go:embed *.md
var fs embed.FS

// Read returns the content of an embedded doc by filename (e.g. "skills.md").
// ok is false if no such embedded file exists.
func Read(name string) (content string, ok bool) {
	data, err := fs.ReadFile(name)
	if err != nil {
		return "", false
	}
	return string(data), true
}
