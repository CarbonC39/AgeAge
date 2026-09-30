//go:build unix

package tools

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
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
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
				return nil, ctxErr
			}
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
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
