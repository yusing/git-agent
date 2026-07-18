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

	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/yusing/git-agent/internal/giturl"
)

type IndexMigrationOptions struct {
	DryRun      bool
	ProgressLog func(Progress) error
}

const (
	ProgressStatusBuilding   = "building"
	ProgressStatusInstalling = "installing"
)

type IndexMigrationSummary struct {
	From           int
	To             int
	Indexes        int
	Records        int
	UniqueVectors  int
	Packs          int
	CurrentBytes   int64
	ProjectedBytes int64
}

type v1MigrationSnapshot struct {
	target  indexSyncTarget
	records []vectorRecord
}

func MigrateIndex(ctx context.Context, remoteURL string, opts IndexMigrationOptions) (summary IndexMigrationSummary, err error) {
	if strings.TrimSpace(remoteURL) == "" {
		return summary, errors.New("index.remote is not configured")
	}
	repair := newIndexMigrationRepair(ctx, opts.DryRun, opts.ProgressLog)
	var sync *indexSync
	var cleanup func() error
	if opts.DryRun {
		sync, cleanup, err = cloneIndexSyncReadOnly(ctx, remoteURL, opts.ProgressLog, repair)
	} else {
		sync, err = openIndexSyncWithMigration(ctx, remoteURL, opts.ProgressLog, repair)
		if err == nil {
			cleanup = sync.close
		}
	}
	if err != nil {
		return summary, err
	}
	defer func() { err = errors.Join(err, cleanup()) }()
	if opts.DryRun {
		return sync.prepareIndexMigration(true)
	}
	for attempt := range 3 {
		summary, err = sync.prepareIndexMigration(false)
		if err != nil || sync.schema == indexSyncSchemaV2 && summary.From == indexSyncSchemaV2 && !repair.changed {
			return summary, err
		}
		err = sync.push(ctx)
		if err == nil {
			return summary, nil
		}
		if !strings.Contains(err.Error(), "non-fast-forward") || attempt == 2 {
			return summary, err
		}
		if err := sync.checkoutRemoteForMigration(ctx); err != nil {
			return summary, err
		}
	}
	return summary, err
}

func (sync *indexSync) prepareIndexMigration(dryRun bool) (summary IndexMigrationSummary, err error) {
	summary.From = sync.schema
	summary.To = indexSyncSchemaV2
	if sync.schema == indexSyncSchemaV2 {
		if sync.migrationRepair != nil {
			summary = sync.migrationRepair.result()
			if !dryRun && sync.migrationRepair.changed {
				if err := sync.commitPending("Repair mixed schema v2 index store"); err != nil {
					return summary, err
				}
			}
		}
		return summary, nil
	}
	if err := reportProgress(sync.progressLog, Progress{Status: ProgressStatusScanning}); err != nil {
		return summary, err
	}
	currentStats, err := readTrackedTreeStats(sync.dir)
	if err != nil {
		return summary, err
	}
	summary.CurrentBytes = currentStats.Bytes
	base := filepath.Join(sync.dir, ".git", "git-agent")
	if dryRun {
		base = os.TempDir()
	} else if err := os.MkdirAll(base, 0o700); err != nil {
		return summary, err
	}
	temporary, err := os.MkdirTemp(base, "migration-v2-*")
	if err != nil {
		return summary, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(temporary)) }()
	if err := writeIndexSyncSchema(temporary, indexSyncSchemaV2); err != nil {
		return summary, err
	}
	targetSync := &indexSync{dir: temporary, schema: indexSyncSchemaV2}
	started := time.Now()
	if err := reportProgress(sync.progressLog, Progress{Status: ProgressStatusBuilding, Total: currentStats.Indexes}); err != nil {
		return summary, err
	}
	if err := sync.walkV1MigrationSnapshots(func(snapshot v1MigrationSnapshot) error {
		summary.Indexes++
		summary.Records += len(snapshot.records)
		if _, err := targetSync.writeSnapshotV2(snapshot.target, snapshot.records); err != nil {
			return err
		}
		return reportProgress(sync.progressLog, Progress{
			Status:  ProgressStatusBuilding,
			Done:    summary.Indexes,
			Total:   currentStats.Indexes,
			Elapsed: time.Since(started),
		})
	}); err != nil {
		return summary, err
	}
	if err := targetSync.validateV2TreeContents(); err != nil {
		return summary, err
	}
	summary.UniqueVectors = vectorPackCatalogEntries(targetSync.packCatalog)
	generatedStats, err := readTrackedTreeStats(temporary)
	summary.ProjectedBytes = generatedStats.Bytes
	summary.Packs = generatedStats.Packs
	if err != nil || dryRun {
		return summary, err
	}
	if err := reportProgress(sync.progressLog, Progress{Status: ProgressStatusInstalling}); err != nil {
		return summary, err
	}
	if err := installMigratedTree(sync.dir, temporary); err != nil {
		return summary, err
	}
	sync.schema = indexSyncSchemaV2
	sync.packCatalog = targetSync.packCatalog
	if err := sync.commitPending("Migrate index store to schema v2"); err != nil {
		return summary, err
	}
	return summary, nil
}

func (sync *indexSync) checkoutRemoteForMigration(ctx context.Context) error {
	remote, err := sync.repo.Remote("origin")
	if err != nil {
		return err
	}
	refs, err := remote.ListContext(ctx, &git.ListOptions{ClientOptions: remoteClientOptions()})
	if err != nil {
		return sync.remoteError("reach", err)
	}
	branch, remoteHash, ok := remoteDefaultBranch(refs)
	if !ok {
		return errors.New("index remote has no default branch during migration retry")
	}
	sync.branch = branch
	if err := reportProgress(sync.progressLog, Progress{Status: ProgressStatusFetching}); err != nil {
		return err
	}
	refspec := gitconfig.RefSpec("+" + branch.String() + ":" + remoteTrackingRef(branch).String())
	progress := newRemoteProgressWriter(sync.progressLog, sync.remoteURL, ProgressStatusFetching)
	err = sync.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName:    "origin",
		ClientOptions: remoteClientOptions(),
		RefSpecs:      []gitconfig.RefSpec{refspec},
		Tags:          plumbing.NoTags,
		Force:         true,
		Prune:         true,
		Progress:      progress,
	})
	if progress != nil {
		err = errors.Join(err, progress.Flush())
	}
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) && !errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return sync.remoteError("fetch", err)
	}
	return sync.checkoutBranch(remoteHash)
}

func cloneIndexSyncReadOnly(ctx context.Context, remoteURL string, progressLog func(Progress) error, repair *indexMigrationRepair) (*indexSync, func() error, error) {
	dir, err := os.MkdirTemp("", "git-agent-index-migration-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	if err := reportProgress(progressLog, Progress{Status: ProgressStatusFetching}); err != nil {
		_ = cleanup()
		return nil, nil, err
	}
	progress := newRemoteProgressWriter(progressLog, remoteURL, ProgressStatusFetching)
	repo, err := git.PlainCloneContext(ctx, dir, &git.CloneOptions{
		URL:            remoteURL,
		ClientOptions:  remoteClientOptions(),
		AllowEmptyRepo: true,
		Progress:       progress,
	})
	if progress != nil {
		err = errors.Join(err, progress.Flush())
	}
	if err != nil {
		_ = cleanup()
		return nil, nil, sanitizeMigrationCloneError(remoteURL, err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		_ = cleanup()
		return nil, nil, err
	}
	sync := &indexSync{remoteURL: remoteURL, dir: dir, repo: repo, worktree: worktree, progressLog: progressLog, migrationRepair: repair}
	if err := sync.ensureSchema(); err != nil {
		_ = cleanup()
		return nil, nil, err
	}
	return sync, cleanup, nil
}

func sanitizeMigrationCloneError(remoteURL string, err error) error {
	sanitized := giturl.Sanitize(remoteURL)
	return fmt.Errorf("index remote clone failed for %s: %s", sanitized, sanitizeRemoteError(err, remoteURL, sanitized))
}

func (sync *indexSync) walkV1MigrationSnapshots(visit func(v1MigrationSnapshot) error) error {
	root := filepath.Join(sync.dir, "indexes")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		snapshot, err := readV1MigrationSnapshot(sync.dir, path)
		if err != nil {
			return err
		}
		return visit(snapshot)
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func readV1MigrationSnapshot(root, path string) (v1MigrationSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return v1MigrationSnapshot{}, err
	}
	var snapshot syncedIndex
	if err := decodeStrictJSON(data, &snapshot); err != nil {
		return v1MigrationSnapshot{}, fmt.Errorf("parse synced index v1 %s: %w", path, err)
	}
	target := indexSyncTarget{origin: snapshot.Origin, revision: snapshot.Revision, model: snapshot.Model, dimensions: snapshot.Dimensions}
	if err := validateSnapshot(snapshot, target); err != nil {
		return v1MigrationSnapshot{}, err
	}
	expected, err := snapshotPathForSchema(root, target, indexSyncSchemaV1)
	if err != nil {
		return v1MigrationSnapshot{}, err
	}
	if expected != path {
		return v1MigrationSnapshot{}, fmt.Errorf("synced index v1 metadata does not match path %s", path)
	}
	records, ok := compatibleIndexRecords(snapshot.Records, snapshot.Model, snapshot.Dimensions)
	if !ok {
		return v1MigrationSnapshot{}, fmt.Errorf("synced index v1 %s contains incompatible records", path)
	}
	return v1MigrationSnapshot{target: target, records: records}, nil
}

type trackedTreeStats struct {
	Bytes   int64
	Packs   int
	Indexes int
}

func readTrackedTreeStats(root string) (trackedTreeStats, error) {
	var result trackedTreeStats
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == filepath.Join(root, ".git") && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			result.Bytes += info.Size()
			if filepath.Ext(path) == ".pack" {
				result.Packs++
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if filepath.Ext(path) == ".json" && strings.HasPrefix(filepath.ToSlash(rel), "indexes/") {
				result.Indexes++
			}
		}
		return nil
	})
	return result, err
}

func installMigratedTree(root, generated string) error {
	if err := writeIndexSyncSchema(root, indexSyncSchemaV2); err != nil {
		return err
	}
	legacyPaths, err := legacyV1ManifestPaths(root)
	if err != nil {
		return err
	}
	return installRepairedV2Tree(root, generated, legacyPaths)
}
