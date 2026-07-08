//go:build !unix && !windows

package search

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const indexLockPollInterval = 50 * time.Millisecond

type indexLock struct {
	file *os.File
	path string
}

func lockIndex(ctx context.Context, indexDir string) (*indexLock, error) {
	lockPath := indexDir + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
	if err == nil {
		return &indexLock{file: file, path: lockPath}, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	ticker := time.NewTicker(indexLockPollInterval)
	defer ticker.Stop()
	for {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
		if err == nil {
			return &indexLock{file: file, path: lockPath}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *indexLock) Unlock() error {
	return errors.Join(l.file.Close(), os.Remove(l.path))
}
