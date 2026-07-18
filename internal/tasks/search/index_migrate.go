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
	var sync *indexSync
	var cleanup func() error
	if opts.DryRun {
		sync, cleanup, err = cloneIndexSyncReadOnly(ctx, remoteURL, opts.ProgressLog)
	} else {
		sync, err = openIndexSync(ctx, remoteURL, opts.ProgressLog)
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
		if err != nil || sync.schema == indexSyncSchemaV2 && summary.From == indexSyncSchemaV2 {
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
	if err := validateSyncTreeForSchema(temporary, indexSyncSchemaV2); err != nil {
		return summary, err
	}
	for _, byDigest := range targetSync.packCatalog {
		summary.UniqueVectors += len(byDigest)
	}
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

func cloneIndexSyncReadOnly(ctx context.Context, remoteURL string, progressLog func(Progress) error) (*indexSync, func() error, error) {
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
	sync := &indexSync{remoteURL: remoteURL, dir: dir, repo: repo, worktree: worktree, progressLog: progressLog}
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
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var snapshot syncedIndex
		if err := decodeStrictJSON(data, &snapshot); err != nil {
			return fmt.Errorf("parse synced index v1 %s: %w", path, err)
		}
		target := indexSyncTarget{origin: snapshot.Origin, revision: snapshot.Revision, model: snapshot.Model, dimensions: snapshot.Dimensions}
		if err := validateSnapshot(snapshot, target); err != nil {
			return err
		}
		expected, err := sync.snapshotPath(target)
		if err != nil {
			return err
		}
		if expected != path {
			return fmt.Errorf("synced index v1 metadata does not match path %s", path)
		}
		records, ok := compatibleIndexRecords(snapshot.Records, snapshot.Model, snapshot.Dimensions)
		if !ok {
			return fmt.Errorf("synced index v1 %s contains incompatible records", path)
		}
		return visit(v1MigrationSnapshot{target: target, records: records})
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
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

func installMigratedTree(root, generated string) (err error) {
	backupRoot, err := os.MkdirTemp(filepath.Join(root, ".git", "git-agent"), "migration-backup-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(backupRoot)) }()
	oldIndexes := filepath.Join(root, "indexes")
	backupIndexes := filepath.Join(backupRoot, "indexes")
	if err := os.Rename(oldIndexes, backupIndexes); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	installedIndexes := false
	installedPacks := false
	rollback := func() {
		if installedIndexes {
			_ = os.RemoveAll(filepath.Join(root, "indexes"))
		}
		if installedPacks {
			_ = os.RemoveAll(filepath.Join(root, "packs"))
		}
		_ = os.Rename(backupIndexes, oldIndexes)
		_ = writeIndexSyncSchema(root, indexSyncSchemaV1)
	}
	if err := os.Rename(filepath.Join(generated, "indexes"), filepath.Join(root, "indexes")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		rollback()
		return err
	} else if err == nil {
		installedIndexes = true
	}
	if err := os.Rename(filepath.Join(generated, "packs"), filepath.Join(root, "packs")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		rollback()
		return err
	} else if err == nil {
		installedPacks = true
	}
	if err := writeIndexSyncSchema(root, indexSyncSchemaV2); err != nil {
		rollback()
		return err
	}
	if err := validateSyncTreeForSchema(root, indexSyncSchemaV2); err != nil {
		rollback()
		return err
	}
	return syncDirectory(root)
}
