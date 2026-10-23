//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package agent

import (
	"context"
	"os/exec"
)

// stdout and file reads remain bounded on systems without POSIX ulimit.
func limitedConverterCommand(ctx context.Context, args []string) *exec.Cmd {
	return exec.CommandContext(ctx, args[0], args[1:]...)
}
