// Package agentdocs embeds compact framework documentation for agent self-reference.
// Documents are bundled into the binary at compile time via go:embed so they are
// always available regardless of deployment layout (no source tree required).
package agentdocs

import (
	"embed"
	"os"
	"path/filepath"
	"strings"
)

//go:embed *.md
var fs embed.FS

// ExtractTo writes all embedded .md files to dir, creating it if needed.
// Files are always overwritten so they reflect the current binary version.
func ExtractTo(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := fs.ReadDir(".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := fs.ReadFile(e.Name())
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
