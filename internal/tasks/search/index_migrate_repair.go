package search

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type indexMigrationRepair struct {
	ctx         context.Context
	dryRun      bool
	progressLog func(Progress) error
	seen        map[string]bool
	summary     IndexMigrationSummary
	changed     bool
}

func newIndexMigrationRepair(ctx context.Context, dryRun bool, progressLog func(Progress) error) *indexMigrationRepair {
	return &indexMigrationRepair{
		ctx:         ctx,
		dryRun:      dryRun,
		progressLog: progressLog,
		seen:        map[string]bool{},
		summary:     IndexMigrationSummary{From: indexSyncSchemaV2, To: indexSyncSchemaV2},
	}
}

func (repair *indexMigrationRepair) result() IndexMigrationSummary {
	return repair.summary
}

func (sync *indexSync) repairMixedV2Tree() (err error) {
	repair := sync.migrationRepair
	if repair == nil {
		return nil
	}
	legacyPaths, err := legacyV1ManifestPaths(sync.dir)
	if err != nil {
		return err
	}
	if len(legacyPaths) == 0 {
		return nil
	}
	if err := repair.ctx.Err(); err != nil {
		return err
	}
	if err := reportProgress(repair.progressLog, Progress{Status: ProgressStatusScanning}); err != nil {
		return err
	}
	currentStats, err := readTrackedTreeStats(sync.dir)
	if err != nil {
		return err
	}
	snapshots := make([]v1MigrationSnapshot, len(legacyPaths))
	for i, path := range legacyPaths {
		if err := repair.ctx.Err(); err != nil {
			return err
		}
		snapshots[i], err = readV1MigrationSnapshot(sync.dir, path)
		if err != nil {
			return err
		}
	}

	base := filepath.Join(sync.dir, ".git", "git-agent")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	candidate, err := os.MkdirTemp(base, "mixed-v2-repair-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(candidate)) }()
	if err := writeIndexSyncSchema(candidate, indexSyncSchemaV2); err != nil {
		return err
	}
	legacy := make(map[string]bool, len(legacyPaths))
	for _, path := range legacyPaths {
		rel, err := filepath.Rel(sync.dir, path)
		if err != nil {
			return err
		}
		legacy[rel] = true
	}
	if err := copyStrictV2Tree(sync.dir, candidate, legacy); err != nil {
		return err
	}
	targetSync := &indexSync{dir: candidate, schema: indexSyncSchemaV2}
	started := time.Now()
	if err := reportProgress(repair.progressLog, Progress{Status: ProgressStatusBuilding, Total: len(snapshots)}); err != nil {
		return err
	}
	for i, snapshot := range snapshots {
		if err := repair.ctx.Err(); err != nil {
			return err
		}
		if _, err := targetSync.writeSnapshotV2(snapshot.target, snapshot.records); err != nil {
			return err
		}
		if err := reportProgress(repair.progressLog, Progress{
			Status:  ProgressStatusBuilding,
			Done:    i + 1,
			Total:   len(snapshots),
			Elapsed: time.Since(started),
		}); err != nil {
			return err
		}
	}
	if err := targetSync.validateV2TreeContents(); err != nil {
		return err
	}
	projectedStats, err := readTrackedTreeStats(candidate)
	if err != nil {
		return err
	}
	if !repair.dryRun {
		if err := reportProgress(repair.progressLog, Progress{Status: ProgressStatusInstalling}); err != nil {
			return err
		}
	}
	if err := installRepairedV2Tree(sync.dir, candidate, legacyPaths); err != nil {
		return err
	}
	sync.packCatalog = targetSync.packCatalog
	sync.packCatalogDirty = true
	repair.changed = true
	if repair.summary.CurrentBytes == 0 {
		repair.summary.CurrentBytes = currentStats.Bytes
	}
	repair.summary.ProjectedBytes = projectedStats.Bytes
	repair.summary.Packs = projectedStats.Packs
	repair.summary.UniqueVectors = vectorPackCatalogEntries(targetSync.packCatalog)
	for i, path := range legacyPaths {
		rel, err := filepath.Rel(sync.dir, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if repair.seen[key] {
			continue
		}
		repair.seen[key] = true
		repair.summary.Indexes++
		repair.summary.Records += len(snapshots[i].records)
	}
	return nil
}

func legacyV1ManifestPaths(root string) ([]string, error) {
	var result []string
	err := walkSafeSyncTree(root, func(path, rel string, directory bool) error {
		if validSyncTreeEntryForSchema(rel, directory, indexSyncSchemaV2) {
			return nil
		}
		if !directory && validSyncTreeEntryForSchema(rel, false, indexSyncSchemaV1) {
			result = append(result, path)
			return nil
		}
		return fmt.Errorf("index sync repository contains unsafe path %s", path)
	})
	return result, err
}

func copyStrictV2Tree(root, destination string, legacy map[string]bool) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if rel == "schema.json" || legacy[rel] {
			return nil
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if strings.HasPrefix(filepath.ToSlash(rel), "packs/") {
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := os.Link(path, target); err == nil {
				return nil
			}
		}
		return copyFileDurable(path, target)
	})
}

func installRepairedV2Tree(root, candidate string, legacyPaths []string) error {
	if err := publishV2CandidateFiles(root, candidate); err != nil {
		return err
	}
	for _, path := range legacyPaths {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	}
	sync := &indexSync{dir: root, schema: indexSyncSchemaV2}
	return sync.validateV2TreeContents()
}

func publishV2CandidateFiles(root, candidate string) error {
	return filepath.WalkDir(candidate, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(candidate, path)
		if err != nil {
			return err
		}
		if rel == "." || rel == "schema.json" {
			return nil
		}
		if rel == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		destination := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		if strings.HasPrefix(filepath.ToSlash(rel), "packs/") {
			if _, err := os.Stat(destination); err == nil {
				return nil
			} else if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		return copyFileDurable(path, destination)
	})
}

func vectorPackCatalogEntries(catalog vectorPackCatalog) int {
	total := 0
	for _, byDigest := range catalog {
		total += len(byDigest)
	}
	return total
}
