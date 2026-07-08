//go:build windows

package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

const indexLockPollInterval = 50 * time.Millisecond

type indexLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func lockIndex(ctx context.Context, indexDir string) (*indexLock, error) {
	lockPath := indexDir + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	lock := &indexLock{file: file}
	if err := lock.tryLock(); err == nil {
		return lock, nil
	} else if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		file.Close()
		return nil, fmt.Errorf("lock search index: %w", err)
	}
	ticker := time.NewTicker(indexLockPollInterval)
	defer ticker.Stop()
	for {
		if err := lock.tryLock(); err == nil {
			return lock, nil
		} else if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			file.Close()
			return nil, fmt.Errorf("lock search index: %w", err)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *indexLock) tryLock() error {
	return windows.LockFileEx(
		windows.Handle(l.file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&l.overlapped,
	)
}

func (l *indexLock) Unlock() error {
	unlockErr := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped)
	closeErr := l.file.Close()
	return errors.Join(unlockErr, closeErr)
}
