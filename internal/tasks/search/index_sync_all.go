package search

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yusing/git-agent/internal/giturl"
)

type SyncSummary struct {
	Indexes int
	Records int
	Skipped int
}

func SyncAll(ctx context.Context, remoteURL string) (summary SyncSummary, err error) {
	if strings.TrimSpace(remoteURL) == "" {
		return summary, errors.New("index.remote is not configured")
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
	err = filepath.WalkDir(metadataRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			summary.Skipped++
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
			summary.Skipped++
			return nil
		}
		dir := filepath.Dir(path)
		found, err := loadManifest(dir)
		if err != nil {
			summary.Skipped++
			return nil
		}
		if found.Mode != "revision" && found.Mode != "remote" {
			return nil
		}
		target, ok := syncTargetFromManifest(dir, found)
		if !ok {
			summary.Skipped++
			return nil
		}
		var localRecords []vectorRecord
		err = withIndexLock(ctx, target.indexDir, func() error {
			var loadErr error
			localRecords, loadErr = loadVectors(target.indexDir)
			return loadErr
		})
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err != nil {
			summary.Skipped++
			return nil
		}
		compatible, ok := compatibleIndexRecords(localRecords, target.model, target.dimensions)
		if !ok {
			summary.Skipped++
			return nil
		}
		records, err := sync.writeSnapshot(target, compatible)
		if err != nil {
			return err
		}
		summary.Indexes++
		summary.Records += records
		return nil
	})
	if err != nil {
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
