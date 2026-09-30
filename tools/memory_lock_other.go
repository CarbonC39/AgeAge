//go:build !unix && !windows

package tools

import (
	"context"
	"errors"
	"os"
	"time"
)

// The exclusive-create lock is the portable fallback for less common Go
// targets. Unix and Windows use kernel-managed locks instead.
func lockMemoryFile(ctx context.Context, path string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lockPath := path + ".held"
	deadline := time.NewTimer(memoryLockTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(memoryLockPollInterval)
	defer ticker.Stop()
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			if ctxErr := ctx.Err(); ctxErr != nil {
				_ = os.Remove(lockPath)
				return nil, ctxErr
			}
			return func() { _ = os.Remove(lockPath) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, ErrMemoryLockTimeout
		case <-ticker.C:
		}
	}
}
