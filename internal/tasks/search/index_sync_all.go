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

	"github.com/yusing/git-agent/internal/giturl"
)

type SyncSummary struct {
	Indexes int
	Records int
	Skipped int
}

type SyncAllOptions struct {
	ProgressLog func(Progress) error
}

const (
	ProgressStatusScanning = "scanning"
	ProgressStatusSyncing  = "syncing"
	ProgressStatusPushing  = "pushing"
)

func SyncAll(ctx context.Context, remoteURL string, opts SyncAllOptions) (summary SyncSummary, err error) {
	if strings.TrimSpace(remoteURL) == "" {
		return summary, errors.New("index.remote is not configured")
	}
	if err := reportSyncProgress(opts, Progress{Status: ProgressStatusFetching}); err != nil {
		return summary, err
	}
	sync, err := openIndexSync(ctx, remoteURL)
	if err != nil {
		return summary, err
	}
	defer func() { err = errors.Join(err, sync.close()) }()

	home, err := os.UserHomeDir()
	if err != nil {
		return summary, err
	}
	metadataRoot := filepath.Join(home, ".git-agent")
	indexSyncRoot := filepath.Join(metadataRoot, "index-sync")
	if err := reportSyncProgress(opts, Progress{Status: ProgressStatusScanning}); err != nil {
		return summary, err
	}
	targets, skipped, err := inventorySyncTargets(metadataRoot, indexSyncRoot)
	summary.Skipped += skipped
	if err != nil {
		return summary, err
	}
	started := time.Now()
	if err := reportSyncProgress(opts, Progress{Status: ProgressStatusSyncing, Total: len(targets)}); err != nil {
		return summary, err
	}
	for i, target := range targets {
		records, synced, err := syncLocalTarget(ctx, sync, target)
		if err != nil {
			return summary, err
		}
		if synced {
			summary.Indexes++
			summary.Records += records
		} else {
			summary.Skipped++
		}
		if err := reportSyncProgress(opts, Progress{
			Status:  ProgressStatusSyncing,
			Done:    i + 1,
			Total:   len(targets),
			Elapsed: time.Since(started),
		}); err != nil {
			return summary, err
		}
	}
	if err := reportSyncProgress(opts, Progress{Status: ProgressStatusPushing}); err != nil {
		return summary, err
	}
	if err := sync.commitPending(fmt.Sprintf("Sync %d revision indexes", summary.Indexes)); err != nil {
		return summary, err
	}
	if err := sync.pushWithRetry(ctx); err != nil {
		return summary, err
	}
	return summary, nil
}

func inventorySyncTargets(metadataRoot, indexSyncRoot string) (targets []indexSyncTarget, skipped int, err error) {
	err = filepath.WalkDir(metadataRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			skipped++
			return nil
		}
		if path == indexSyncRoot {
			return filepath.SkipDir
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "repo.git" || entry.Name() == "sessions" || entry.Name() == "query-locks") {
			return filepath.SkipDir
		}
		if entry.IsDir() || entry.Name() != "manifest.json" || metadataDirForIndex(path) == "" {
			return nil
		}
		if !entry.Type().IsRegular() {
			skipped++
			return nil
		}
		dir := filepath.Dir(path)
		found, err := loadManifest(dir)
		if err != nil {
			skipped++
			return nil
		}
		if found.Mode != "revision" && found.Mode != "remote" {
			return nil
		}
		target, ok := syncTargetFromManifest(dir, found)
		if !ok {
			skipped++
			return nil
		}
		targets = append(targets, target)
		return nil
	})
	return targets, skipped, err
}

func syncLocalTarget(ctx context.Context, sync *indexSync, target indexSyncTarget) (records int, synced bool, err error) {
	var localRecords []vectorRecord
	err = withIndexLock(ctx, target.indexDir, func() error {
		var loadErr error
		localRecords, loadErr = loadVectors(target.indexDir)
		return loadErr
	})
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return 0, false, err
	}
	if err != nil {
		return 0, false, nil
	}
	compatible, ok := compatibleIndexRecords(localRecords, target.model, target.dimensions)
	if !ok {
		return 0, false, nil
	}
	records, err = sync.writeSnapshot(target, compatible)
	return records, err == nil, err
}

func reportSyncProgress(opts SyncAllOptions, progress Progress) error {
	if opts.ProgressLog == nil {
		return nil
	}
	return opts.ProgressLog(progress)
}

func syncTargetFromManifest(dir string, found manifest) (indexSyncTarget, bool) {
	if !canonicalObjectID(found.ResolvedRev) {
		return indexSyncTarget{}, false
	}
	originIdentity := found.OriginIdentity
	if originIdentity == "" && giturl.Stable(found.Remote) {
		originIdentity = giturl.Identity(found.Remote)
	}
	if originIdentity == "" {
		return indexSyncTarget{}, false
	}
	return indexSyncTarget{
		origin:     originIdentity,
		revision:   found.ResolvedRev,
		model:      found.EmbeddingModel,
		dimensions: found.Dimensions,
		indexDir:   dir,
	}, true
}
