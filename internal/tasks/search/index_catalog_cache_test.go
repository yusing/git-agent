package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestVectorPackCatalogCachePrunesHistoricalHeads(t *testing.T) {
	sync, firstHead := newCatalogCacheTestSync(t)
	if err := sync.persistVectorPackCatalog(firstHead); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Dir(sync.vectorPackCatalogCachePath(firstHead))
	staleHash := strings.Repeat("f", len(firstHead.String()))
	if staleHash == firstHead.String() {
		staleHash = strings.Repeat("e", len(firstHead.String()))
	}
	stalePath := filepath.Join(dir, staleHash+".bin")
	temporaryPath := filepath.Join(dir, ".catalog-abandoned.tmp")
	unknownPath := filepath.Join(dir, "keep.txt")
	for path, data := range map[string]string{
		stalePath:     "stale",
		temporaryPath: "temporary",
		unknownPath:   "unrelated",
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unknownDir := filepath.Join(dir, strings.Repeat("d", len(firstHead.String()))+".bin")
	if err := os.Mkdir(unknownDir, 0o700); err != nil {
		t.Fatal(err)
	}

	writeFile(t, sync.dir, "next.txt", "next\n")
	if _, err := sync.worktree.Add("next.txt"); err != nil {
		t.Fatal(err)
	}
	signature := &object.Signature{Name: "Search Test", Email: "search@example.test", When: time.Unix(2, 0)}
	secondHead, err := sync.worktree.Commit("advance", &git.CommitOptions{Author: signature, Committer: signature})
	if err != nil {
		t.Fatal(err)
	}
	if err := sync.persistVectorPackCatalog(secondHead); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		sync.vectorPackCatalogCachePath(firstHead),
		stalePath,
		temporaryPath,
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale catalog path %s remains: %v", path, err)
		}
	}
	for _, path := range []string{
		sync.vectorPackCatalogCachePath(secondHead),
		unknownPath,
		unknownDir,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preserved catalog path %s: %v", path, err)
		}
	}

	lateStalePath := filepath.Join(dir, strings.Repeat("c", len(firstHead.String()))+".bin")
	lateTemporaryPath := filepath.Join(dir, ".catalog-interrupted.tmp")
	if err := os.WriteFile(lateStalePath, []byte("late"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lateTemporaryPath, []byte("late"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sync.pruneVectorPackCatalogCaches(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{lateStalePath, lateTemporaryPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("open-time stale catalog path %s remains: %v", path, err)
		}
	}
}

func TestVectorPackCatalogCacheFallsBackAfterCorruption(t *testing.T) {
	sync, head := newCatalogCacheTestSync(t)
	if err := sync.persistVectorPackCatalog(head); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sync.vectorPackCatalogCachePath(head), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}

	want := vectorPackCatalogEntries(sync.packCatalog)
	sync.packCatalog = nil
	loaded, err := sync.loadVectorPackCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if got := vectorPackCatalogEntries(loaded); got != want {
		t.Fatalf("rebuilt catalog entries = %d, want %d", got, want)
	}
}

func newCatalogCacheTestSync(t *testing.T) (*indexSync, plumbing.Hash) {
	t.Helper()
	root := t.TempDir()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeIndexSyncSchema(root, indexSyncSchemaV2); err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("schema.json"); err != nil {
		t.Fatal(err)
	}
	signature := &object.Signature{Name: "Search Test", Email: "search@example.test", When: time.Unix(1, 0)}
	head, err := worktree.Commit("initialize", &git.CommitOptions{Author: signature, Committer: signature})
	if err != nil {
		t.Fatal(err)
	}
	return &indexSync{
		dir:         root,
		repo:        repo,
		worktree:    worktree,
		schema:      indexSyncSchemaV2,
		packCatalog: writeTestVectorPackCatalog(t, root, 2, 3, 4),
	}, head
}
