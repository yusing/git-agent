package search

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
)

const (
	ProgressStatusGCScanningRemote = "gc_scanning_remote"
	ProgressStatusGCPruningRemote  = "gc_pruning_remote"
)

type GCSummary struct {
	Local                LocalGCSummary
	RemoteConfigured     bool
	RemoteRemovedPacks   int
	RemoteCurrentBytes   int64
	RemoteProjectedBytes int64
}

type GCOptions struct {
	DryRun      bool
	RemoteURL   string
	ProgressLog func(Progress) error

	metadataRoot    string
	afterRemotePlan func(int) error
}

type remoteGCPlan struct {
	removePaths    []string
	currentBytes   int64
	projectedBytes int64
}

func GCAll(ctx context.Context, opts GCOptions) (summary GCSummary, err error) {
	summary.Local, err = GCAllLocal(ctx, LocalGCOptions{
		DryRun:       opts.DryRun,
		ProgressLog:  opts.ProgressLog,
		metadataRoot: opts.metadataRoot,
	})
	if err != nil {
		return summary, err
	}
	if strings.TrimSpace(opts.RemoteURL) == "" {
		return summary, nil
	}
	summary.RemoteConfigured = true
	remote, err := gcRemote(ctx, opts)
	if err != nil {
		return summary, err
	}
	summary.RemoteRemovedPacks = len(remote.removePaths)
	summary.RemoteCurrentBytes = remote.currentBytes
	summary.RemoteProjectedBytes = remote.projectedBytes
	return summary, nil
}

func gcRemote(ctx context.Context, opts GCOptions) (plan remoteGCPlan, err error) {
	if opts.DryRun {
		sync, cleanup, err := cloneIndexSyncReadOnly(ctx, opts.RemoteURL, opts.ProgressLog, nil)
		if err != nil {
			return plan, err
		}
		defer func() { err = errors.Join(err, cleanup()) }()
		plan, err = sync.planRemoteGC()
		if err != nil {
			return plan, err
		}
		if err := reportProgress(opts.ProgressLog, Progress{Status: ProgressStatusGCPruningRemote}); err != nil {
			return plan, err
		}
		if err := applyRemoteGCPlan(sync, plan); err != nil {
			return plan, err
		}
		return plan, nil
	}

	sync, err := openIndexSync(ctx, opts.RemoteURL, opts.ProgressLog)
	if err != nil {
		return plan, err
	}
	defer func() { err = errors.Join(err, sync.close()) }()
	for attempt := range 3 {
		plan, err = sync.planRemoteGC()
		if err != nil {
			return plan, err
		}
		if opts.afterRemotePlan != nil {
			if err := opts.afterRemotePlan(attempt); err != nil {
				return plan, err
			}
		}
		if err := reportProgress(opts.ProgressLog, Progress{Status: ProgressStatusGCPruningRemote}); err != nil {
			return plan, err
		}
		if len(plan.removePaths) == 0 {
			return plan, nil
		}
		base, err := sync.repo.Head()
		if err != nil {
			return plan, err
		}
		if err := applyRemoteGCPlan(sync, plan); err != nil {
			return plan, restoreRemoteGCBase(sync, base.Hash(), err)
		}
		if err := sync.commitPending("Prune unreferenced vector packs"); err != nil {
			return plan, restoreRemoteGCBase(sync, base.Hash(), err)
		}
		err = sync.push(ctx)
		if err == nil {
			return plan, nil
		}
		if !strings.Contains(err.Error(), "non-fast-forward") || attempt == 2 {
			return plan, restoreRemoteGCBase(sync, base.Hash(), err)
		}
		if checkoutErr := sync.checkoutRemoteForMigration(ctx); checkoutErr != nil {
			return plan, restoreRemoteGCBase(sync, base.Hash(), errors.Join(err, checkoutErr))
		}
	}
	return plan, err
}

func restoreRemoteGCBase(sync *indexSync, base plumbing.Hash, cause error) error {
	if err := sync.checkoutBranch(base); err != nil {
		return errors.Join(cause, fmt.Errorf("restore index sync checkout after failed GC: %w", err))
	}
	return cause
}

func (sync *indexSync) planRemoteGC() (remoteGCPlan, error) {
	var plan remoteGCPlan
	if sync.schema != indexSyncSchemaV2 {
		return plan, fmt.Errorf("index gc requires schema v2; found schema v%d", sync.schema)
	}
	if err := reportProgress(sync.progressLog, Progress{Status: ProgressStatusGCScanningRemote}); err != nil {
		return plan, err
	}
	if err := sync.validateV2TreeContents(); err != nil {
		return plan, err
	}
	stats, err := readTrackedTreeStats(sync.dir)
	if err != nil {
		return plan, err
	}
	plan.currentBytes = stats.Bytes
	plan.projectedBytes = stats.Bytes
	live := map[string]bool{}
	indexesRoot := filepath.Join(sync.dir, "indexes")
	err = filepath.WalkDir(indexesRoot, func(path string, entry fs.DirEntry, walkErr error) error {
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
		var snapshot syncedIndexV2
		if err := decodeStrictJSON(data, &snapshot); err != nil {
			return err
		}
		modelKey := digestHex(syncModelKey(snapshot.Model, snapshot.Dimensions))
		for _, digest := range snapshot.Packs {
			live[filepath.ToSlash(filepath.Join("packs", modelKey, digest+".pack"))] = true
		}
		return nil
	})
	if err != nil {
		return plan, err
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
		relative, err := filepath.Rel(sync.dir, path)
		if err != nil {
			return err
		}
		if live[filepath.ToSlash(relative)] {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		plan.removePaths = append(plan.removePaths, path)
		plan.projectedBytes -= info.Size()
		return nil
	})
	slices.Sort(plan.removePaths)
	return plan, err
}

func applyRemoteGCPlan(sync *indexSync, plan remoteGCPlan) error {
	directories := map[string]bool{}
	for _, path := range plan.removePaths {
		if err := os.Remove(path); err != nil {
			return err
		}
		directories[filepath.Dir(path)] = true
	}
	for _, directory := range slices.Sorted(maps.Keys(directories)) {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	sync.packCatalog = nil
	sync.packCatalogDirty = true
	return sync.validateV2TreeContents()
}
