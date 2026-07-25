package search

import (
	"context"
	"path/filepath"

	"github.com/yusing/git-agent/internal/metadata"
)

const searchIndexProducerLockName = "search-index-producer"

func lockSearchIndexProducer(ctx context.Context, progressLog func(Progress) error) (*indexLock, error) {
	root, err := metadata.Root()
	if err != nil {
		return nil, err
	}
	return lockIndex(ctx, filepath.Join(root, searchIndexProducerLockName), func() error {
		return reportProgress(progressLog, Progress{Status: ProgressStatusWaiting})
	})
}
