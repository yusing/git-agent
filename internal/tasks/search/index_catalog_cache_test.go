package search

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func TestVectorPackCatalogCacheRoundTripAndSize(t *testing.T) {
	sync, head := newCatalogCacheTestSync(t)
	packs, err := scanVectorPackCatalogCachePacks(sync.dir)
	if err != nil {
		t.Fatal(err)
	}
	catalog := vectorPackCatalogFromCachePacks(packs)
	encoded, err := encodeVectorPackCatalogCache(head, packs)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeVectorPackCatalogCache(encoded, head, packs)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := vectorPackCatalogEntries(decoded), vectorPackCatalogEntries(catalog); got != want {
		t.Fatalf("decoded catalog entries = %d, want %d", got, want)
	}

	legacyData := encodeLegacyVectorPackCatalog(t, head, catalog)
	if len(encoded)*2 >= len(legacyData) {
		t.Fatalf("compact catalog size = %d, legacy Gob size = %d; want compact smaller than half", len(encoded), len(legacyData))
	}
}

func TestVectorPackCatalogCacheRejectsInvalidBinary(t *testing.T) {
	sync, head := newCatalogCacheTestSync(t)
	packs, err := scanVectorPackCatalogCachePacks(sync.dir)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := encodeVectorPackCatalogCache(head, packs)
	if err != nil {
		t.Fatal(err)
	}
	slotOffset := vectorPackCatalogCacheHeaderSize + len(packs)*vectorPackCatalogCachePackRowSize

	tests := []struct {
		name   string
		mutate func([]byte) []byte
		head   plumbing.Hash
		packs  []vectorPackCatalogCachePack
	}{
		{
			name: "wrong head",
			mutate: func(data []byte) []byte {
				return data
			},
			head: plumbing.ZeroHash,
		},
		{
			name: "wrong magic",
			mutate: func(data []byte) []byte {
				data[0] ^= 0xff
				return data
			},
			head: head,
		},
		{
			name: "future version",
			mutate: func(data []byte) []byte {
				binary.LittleEndian.PutUint32(data[8:12], vectorPackCatalogCacheVersion+1)
				return data
			},
			head: head,
		},
		{
			name: "truncated",
			mutate: func(data []byte) []byte {
				return data[:len(data)-1]
			},
			head: head,
		},
		{
			name: "trailing",
			mutate: func(data []byte) []byte {
				return append(data, 0)
			},
			head: head,
		},
		{
			name: "checksum",
			mutate: func(data []byte) []byte {
				data[len(data)-1] ^= 0xff
				return data
			},
			head: head,
		},
		{
			name: "overflowing counts",
			mutate: func(data []byte) []byte {
				binary.LittleEndian.PutUint64(data[36:44], ^uint64(0))
				return data
			},
			head: head,
		},
		{
			name: "reordered packs",
			mutate: func(data []byte) []byte {
				first := data[vectorPackCatalogCacheHeaderSize : vectorPackCatalogCacheHeaderSize+vectorPackCatalogCachePackRowSize]
				second := data[vectorPackCatalogCacheHeaderSize+vectorPackCatalogCachePackRowSize : vectorPackCatalogCacheHeaderSize+2*vectorPackCatalogCachePackRowSize]
				copyData := bytes.Clone(first)
				copy(first, second)
				copy(second, copyData)
				updateVectorPackCatalogCacheChecksum(data)
				return data
			},
			head: head,
		},
		{
			name: "out of range pack index",
			mutate: func(data []byte) []byte {
				binary.LittleEndian.PutUint32(data[slotOffset+64:slotOffset+68], uint32(len(packs)))
				updateVectorPackCatalogCacheChecksum(data)
				return data
			},
			head: head,
		},
		{
			name: "reordered slots",
			mutate: func(data []byte) []byte {
				first := data[slotOffset : slotOffset+vectorPackCatalogCacheSlotRowSize]
				second := data[slotOffset+vectorPackCatalogCacheSlotRowSize : slotOffset+2*vectorPackCatalogCacheSlotRowSize]
				copyData := bytes.Clone(first)
				copy(first, second)
				copy(second, copyData)
				updateVectorPackCatalogCacheChecksum(data)
				return data
			},
			head: head,
		},
		{
			name: "slot mismatch",
			mutate: func(data []byte) []byte {
				data[slotOffset] ^= 0xff
				updateVectorPackCatalogCacheChecksum(data)
				return data
			},
			head: head,
		},
		{
			name: "missing current pack",
			mutate: func(data []byte) []byte {
				return data
			},
			head:  head,
			packs: packs[:len(packs)-1],
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := test.mutate(bytes.Clone(valid))
			testPacks := test.packs
			if testPacks == nil {
				testPacks = packs
			}
			if _, err := decodeVectorPackCatalogCache(data, test.head, testPacks); err == nil {
				t.Fatal("invalid catalog cache was accepted")
			}
		})
	}
}

func TestVectorPackCatalogCacheLoadsAndUpgradesLegacyGob(t *testing.T) {
	sync, head := newCatalogCacheTestSync(t)
	data := encodeLegacyVectorPackCatalog(t, head, sync.packCatalog)
	path := sync.vectorPackCatalogCachePath(head)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	want := vectorPackCatalogEntries(sync.packCatalog)
	sync.packCatalog = nil
	loaded, err := sync.loadVectorPackCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if got := vectorPackCatalogEntries(loaded); got != want {
		t.Fatalf("loaded catalog entries = %d, want %d", got, want)
	}
	upgraded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(upgraded, vectorPackCatalogCacheMagic[:]) {
		t.Fatal("legacy catalog was not upgraded to the compact format")
	}
}

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
func encodeLegacyVectorPackCatalog(
	t *testing.T,
	head plumbing.Hash,
	catalog vectorPackCatalog,
) []byte {
	t.Helper()
	legacy := legacyVectorPackCatalogCache{
		Version: legacyVectorPackCatalogCacheVersion,
		Head:    head.String(),
	}
	for embeddingKey, byDigest := range catalog {
		for vectorDigest, slot := range byDigest {
			legacy.Entries = append(legacy.Entries, legacyVectorPackCatalogCacheEntry{
				EmbeddingKey: embeddingKey,
				VectorDigest: vectorDigest,
				Pack:         slot.Pack,
				Slot:         slot.Slot,
			})
		}
	}
	var data bytes.Buffer
	if err := gob.NewEncoder(&data).Encode(legacy); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func updateVectorPackCatalogCacheChecksum(data []byte) {
	checksumOffset := len(data) - vectorPackCatalogCacheChecksumSize
	checksum := sha256.Sum256(data[:checksumOffset])
	copy(data[checksumOffset:], checksum[:])
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
