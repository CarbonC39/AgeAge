//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package agent

import (
	"context"
	"os/exec"
	"syscall"
)

// POSIX file-size limits apply to children only. Positional arguments preserve
// paths containing spaces and ensure configuration never becomes shell source.
// The child may write at most 1 MiB per file; model-facing text is capped at 512 KiB.
func limitedConverterCommand(ctx context.Context, args []string) *exec.Cmd {
	command := []string{"-c", `ulimit -c 0; ulimit -f 1024 || exit 125; exec "$@"`, "ageage-converter"}
	command = append(command, args...)
	cmd := exec.CommandContext(ctx, "sh", command...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd
}
