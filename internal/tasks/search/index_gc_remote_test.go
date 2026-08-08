package search

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

func TestGCRemoteDryRunAndCleanup(t *testing.T) {
	remote, baseHead, _ := newRemoteGCFixture(t)
	t.Setenv("HOME", t.TempDir())
	cached, err := openIndexSync(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := cached.dir
	if err := cached.close(); err != nil {
		t.Fatal(err)
	}
	beforeCache := snapshotLocalGCTree(t, cacheDir)
	summary, err := GCAll(t.Context(), GCOptions{
		DryRun:       true,
		RemoteURL:    remote,
		metadataRoot: filepath.Join(t.TempDir(), ".git-agent"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !summary.RemoteConfigured || summary.RemoteRemovedPacks != 1 ||
		summary.RemoteCurrentBytes <= summary.RemoteProjectedBytes {
		t.Fatalf("dry-run summary = %#v", summary)
	}
	afterCache := snapshotLocalGCTree(t, cacheDir)
	if !reflect.DeepEqual(afterCache, beforeCache) {
		t.Fatal("remote dry-run changed the persistent index-sync checkout")
	}
	repo, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	afterDryRun, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if afterDryRun.Hash() != baseHead {
		t.Fatalf("dry-run remote HEAD = %s, want %s", afterDryRun.Hash(), baseHead)
	}

	t.Setenv("HOME", t.TempDir())
	summary, err = GCAll(t.Context(), GCOptions{
		RemoteURL:    remote,
		metadataRoot: filepath.Join(t.TempDir(), ".git-agent"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.RemoteRemovedPacks != 1 ||
		summary.RemoteCurrentBytes <= summary.RemoteProjectedBytes {
		t.Fatalf("normal summary = %#v", summary)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if commit.NumParents() != 1 {
		t.Fatalf("cleanup commit parents = %d, want 1", commit.NumParents())
	}
	parent, err := commit.Parent(0)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Hash != baseHead {
		t.Fatalf("cleanup commit parent = %s, want %s", parent.Hash, baseHead)
	}
	cloned, cleanup, err := cloneIndexSyncReadOnly(t.Context(), remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := cloned.validateV2TreeContents(); err != nil {
		t.Fatal(err)
	}
	stats, err := readTrackedTreeStats(cloned.dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Indexes != 1 || stats.Packs != 1 {
		t.Fatalf("cleaned remote stats = %#v", stats)
	}
}

func TestGCRemoteReplansAfterNonFastForward(t *testing.T) {
	remote, _, orphan := newRemoteGCFixture(t)
	t.Setenv("HOME", t.TempDir())
	competing, err := openIndexSync(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	competingTarget := indexSyncTarget{
		origin:     "https://example.test/acme/competing",
		revision:   strings.Repeat("2", 40),
		model:      orphan.EmbeddingModel,
		dimensions: orphan.Dimensions,
	}
	if _, err := competing.writeSnapshot(competingTarget, []vectorRecord{orphan}); err != nil {
		t.Fatal(err)
	}
	competingPublished := false

	t.Setenv("HOME", t.TempDir())
	summary, err := GCAll(t.Context(), GCOptions{
		RemoteURL:    remote,
		metadataRoot: filepath.Join(t.TempDir(), ".git-agent"),
		afterRemotePlan: func(attempt int) error {
			if attempt != 0 || competingPublished {
				return nil
			}
			competingPublished = true
			if err := competing.commitPending("reference previously unused pack"); err != nil {
				return err
			}
			if err := competing.push(t.Context()); err != nil {
				return err
			}
			return competing.close()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !competingPublished {
		t.Fatal("competing remote update was not published")
	}
	if summary.RemoteRemovedPacks != 0 ||
		summary.RemoteCurrentBytes != summary.RemoteProjectedBytes {
		t.Fatalf("retried summary = %#v", summary)
	}
	cloned, cleanup, err := cloneIndexSyncReadOnly(t.Context(), remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err := cloned.validateV2TreeContents(); err != nil {
		t.Fatal(err)
	}
	stats, err := readTrackedTreeStats(cloned.dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Indexes != 2 || stats.Packs != 2 {
		t.Fatalf("retried remote stats = %#v", stats)
	}
}

func TestGCRemoteFailedPushDoesNotLeakCleanup(t *testing.T) {
	remote, baseHead, _ := newRemoteGCFixture(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	offline := remote + ".offline"
	renamed := false
	_, gcErr := GCAll(t.Context(), GCOptions{
		RemoteURL:    remote,
		metadataRoot: filepath.Join(t.TempDir(), ".git-agent"),
		afterRemotePlan: func(attempt int) error {
			if attempt != 0 {
				return nil
			}
			if err := os.Rename(remote, offline); err != nil {
				return err
			}
			renamed = true
			return nil
		},
	})
	if !renamed {
		t.Fatal("remote was not made unavailable before the GC push")
	}
	if err := os.Rename(offline, remote); err != nil {
		t.Fatal(err)
	}
	if gcErr == nil {
		t.Fatal("remote GC succeeded while the remote was unavailable")
	}
	if _, err := SyncAll(t.Context(), remote, SyncAllOptions{}); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Hash() != baseHead {
		t.Fatalf("ordinary sync published failed GC commit: HEAD = %s, want %s", head.Hash(), baseHead)
	}
	cloned, cleanup, err := cloneIndexSyncReadOnly(t.Context(), remote, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	stats, err := readTrackedTreeStats(cloned.dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Packs != 2 {
		t.Fatalf("packs after failed GC and ordinary sync = %d, want 2", stats.Packs)
	}
}

func newRemoteGCFixture(t *testing.T) (string, plumbing.Hash, vectorRecord) {
	t.Helper()
	remote := newEmptySyncRemote(t)
	t.Setenv("HOME", t.TempDir())
	seed, err := openIndexSync(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.push(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := seed.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateIndex(t.Context(), remote, IndexMigrationOptions{}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", t.TempDir())
	seed, err = openIndexSync(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	live := testSyncedVectorRecord("live", []float64{1, 0, 0})
	liveTarget := indexSyncTarget{
		origin:     "https://example.test/acme/live",
		revision:   strings.Repeat("1", 40),
		model:      live.EmbeddingModel,
		dimensions: live.Dimensions,
	}
	if _, err := seed.writeSnapshot(liveTarget, []vectorRecord{live}); err != nil {
		t.Fatal(err)
	}
	orphan := testSyncedVectorRecord("orphan", []float64{0, 1, 0})
	data := encodeVector(orphan.Vector)
	embeddingKey, err := decodeDigest(vectorStoreKey(
		orphan.EmbeddingInputHash,
		orphan.EmbeddingModel,
		orphan.Dimensions,
	))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeVectorPack(
		orphan.Dimensions,
		syncModelKey(orphan.EmbeddingModel, orphan.Dimensions),
		[]vectorPackItem{{
			EmbeddingKey: embeddingKey,
			VectorDigest: vectorPayloadDigest(data),
			Data:         data,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.publishVectorPack(syncModelKey(orphan.EmbeddingModel, orphan.Dimensions), encoded); err != nil {
		t.Fatal(err)
	}
	if err := seed.commitPending("seed remote GC fixture"); err != nil {
		t.Fatal(err)
	}
	if err := seed.push(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := seed.close(); err != nil {
		t.Fatal(err)
	}
	repo, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return remote, head.Hash(), orphan
}
