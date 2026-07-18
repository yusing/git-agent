package search

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type v2ReconcileState struct {
	dir       string
	snapshots map[string][]byte
}

func (sync *indexSync) validateV2TreeContents() error {
	if err := validateSyncTreeForSchema(sync.dir, indexSyncSchemaV2); err != nil {
		return err
	}
	indexesRoot := filepath.Join(sync.dir, "indexes")
	err := filepath.WalkDir(indexesRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target, err := v2TargetFromSnapshot(data)
		if err != nil {
			return err
		}
		expected, err := snapshotPathForSchema(sync.dir, target, indexSyncSchemaV2)
		if err != nil {
			return err
		}
		if expected != path {
			return fmt.Errorf("synced index v2 metadata does not match path %s", path)
		}
		_, err = sync.decodeV2Snapshot(data, target)
		return err
	})
	if err != nil {
		return err
	}
	packsRoot := filepath.Join(sync.dir, "packs")
	return filepath.WalkDir(packsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		digest := strings.TrimSuffix(entry.Name(), ".pack")
		pack, err := readVectorPack(path, digest)
		if err != nil {
			return err
		}
		if digestHex(pack.ModelKey) != filepath.Base(filepath.Dir(path)) {
			return fmt.Errorf("vector pack %s model key does not match path", digest)
		}
		return nil
	})
}

func (sync *indexSync) captureV2ReconcileState() (v2ReconcileState, error) {
	state := v2ReconcileState{snapshots: map[string][]byte{}}
	base := filepath.Join(sync.dir, ".git", "git-agent")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return state, err
	}
	dir, err := os.MkdirTemp(base, "reconcile-*")
	if err != nil {
		return state, err
	}
	state.dir = dir
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(dir)
		}
	}()
	indexesRoot := filepath.Join(sync.dir, "indexes")
	err = filepath.WalkDir(indexesRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target, err := v2TargetFromSnapshot(data)
		if err != nil {
			return err
		}
		expected, err := sync.snapshotPath(target)
		if err != nil {
			return err
		}
		if expected != path {
			return fmt.Errorf("synced index v2 metadata does not match path %s", path)
		}
		if _, err := sync.decodeV2Snapshot(data, target); err != nil {
			return err
		}
		rel, err := filepath.Rel(sync.dir, path)
		if err != nil {
			return err
		}
		state.snapshots[rel] = data
		return nil
	})
	if err != nil {
		return state, err
	}
	packsRoot := filepath.Join(sync.dir, "packs")
	err = filepath.WalkDir(packsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(sync.dir, path)
		if err != nil {
			return err
		}
		digest := entry.Name()[:len(entry.Name())-len(".pack")]
		if _, err := readVectorPack(path, digest); err != nil {
			return err
		}
		return copyFileDurable(path, filepath.Join(dir, rel))
	})
	if err != nil {
		return state, err
	}
	cleanup = false
	return state, nil
}

func v2TargetFromSnapshot(data []byte) (indexSyncTarget, error) {
	var snapshot syncedIndexV2
	if err := decodeStrictJSON(data, &snapshot); err != nil {
		return indexSyncTarget{}, fmt.Errorf("parse synced index v2: %w", err)
	}
	target := indexSyncTarget{
		origin:     snapshot.Origin,
		revision:   snapshot.Revision,
		model:      snapshot.Model,
		dimensions: snapshot.Dimensions,
	}
	if err := validateSyncTarget(target); err != nil {
		return indexSyncTarget{}, err
	}
	if err := validateV2SnapshotMetadata(snapshot, target); err != nil {
		return indexSyncTarget{}, err
	}
	return target, nil
}

func (sync *indexSync) restoreV2Packs(state v2ReconcileState) error {
	packsRoot := filepath.Join(state.dir, "packs")
	err := filepath.WalkDir(packsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(state.dir, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(sync.dir, rel)
		if existing, err := os.ReadFile(destination); err == nil {
			local, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !bytes.Equal(existing, local) {
				return fmt.Errorf("content-addressed vector pack collision at %s", destination)
			}
			return nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return copyFileDurable(path, destination)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err == nil {
		sync.packCatalog = nil
		sync.packCatalogDirty = true
	}
	return err
}

func (sync *indexSync) mergeV2Snapshots(state v2ReconcileState) error {
	for rel, data := range state.snapshots {
		target, err := v2TargetFromSnapshot(data)
		if err != nil {
			return err
		}
		path := filepath.Join(sync.dir, rel)
		expected, err := sync.snapshotPath(target)
		if err != nil {
			return err
		}
		if path != expected {
			return fmt.Errorf("synced index v2 metadata does not match path %s", path)
		}
		localRecords, err := sync.decodeV2Snapshot(data, target)
		if err != nil {
			return err
		}
		if _, err := sync.writeSnapshotV2(target, localRecords); err != nil {
			return err
		}
	}
	return nil
}

func copyFileDurable(source, destination string) (err error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, input.Close()) }()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".copy-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := io.Copy(temporary, input); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(destination))
}
