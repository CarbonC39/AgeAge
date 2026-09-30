//go:build windows

package tools

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func lockMemoryFile(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	deadline := time.NewTimer(memoryLockTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(memoryLockPollInterval)
	defer ticker.Stop()
	overlapped := new(windows.Overlapped)
	for {
		err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
		if err == nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
				f.Close()
				return nil, ctxErr
			}
			return func() { _ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped); _ = f.Close() }, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-deadline.C:
			f.Close()
			return nil, ErrMemoryLockTimeout
		case <-ticker.C:
		}
	}
}
