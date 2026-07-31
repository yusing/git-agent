//go:build !unix && !windows

package explore

import (
	"context"
	"errors"
	"os"
	"time"
)

type coordLock struct {
	file *os.File
	path string
}

func lockCoordinator(ctx context.Context, path string, pollInterval time.Duration) (*coordLock, error) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			return &coordLock{file: file, path: path}, nil
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

func (l *coordLock) Unlock() error {
	return errors.Join(l.file.Close(), os.Remove(l.path))
}
