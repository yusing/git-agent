package search

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

func loadVectors(dir string) ([]vectorRecord, error) {
	return loadVectorsContext(context.Background(), dir)
}

func sharedVectorPayloadPath(metadataDir string) string {
	return filepath.Join(newVectorStore(metadataDir).dir, vectorStorePayloadName)
}

func (store vectorStore) put(ctx context.Context, records []vectorRecord, forceKeys map[string]bool) (keys map[string]vectorStoreEntry, err error) {
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := lockIndex(ctx, store.dir)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	return store.putLocked(records, forceKeys)
}

