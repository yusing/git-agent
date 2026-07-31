//go:build windows

package explore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

type coordLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func lockCoordinator(ctx context.Context, path string, pollInterval time.Duration) (*coordLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &coordLock{file: file}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if err := lock.tryLock(); err == nil {
			return lock, nil
		} else if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			_ = file.Close()
			return nil, fmt.Errorf("acquire advisory lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *coordLock) tryLock() error {
	return windows.LockFileEx(
		windows.Handle(l.file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&l.overlapped,
	)
}

func (l *coordLock) Unlock() error {
	unlockErr := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped)
	return errors.Join(unlockErr, l.file.Close())
}
