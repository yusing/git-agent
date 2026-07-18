package search

import (
	"cmp"
	"encoding/gob"
	"os"
	"path/filepath"
	"slices"

	"github.com/go-git/go-git/v6/plumbing"
)

const vectorPackCatalogCacheVersion = 1

type vectorPackCatalogCache struct {
	Version int
	Head    string
	Entries []vectorPackCatalogCacheEntry
}

type vectorPackCatalogCacheEntry struct {
	EmbeddingKey string
	VectorDigest string
	Pack         string
	Slot         uint32
}

func (sync *indexSync) loadVectorPackCatalog() (vectorPackCatalog, error) {
	if sync.repo == nil || sync.packCatalogDirty {
		return scanVectorPackCatalog(sync.dir)
	}
	head, err := sync.repo.Head()
	if err != nil {
		return scanVectorPackCatalog(sync.dir)
	}
	path := sync.vectorPackCatalogCachePath(head.Hash())
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		var cached vectorPackCatalogCache
		if gob.NewDecoder(file).Decode(&cached) == nil && cached.Version == vectorPackCatalogCacheVersion && cached.Head == head.Hash().String() {
			catalog := vectorPackCatalog{}
			valid := true
			for _, entry := range cached.Entries {
				if !canonicalLowerHex(entry.EmbeddingKey, 64) || !canonicalLowerHex(entry.VectorDigest, 64) || !canonicalLowerHex(entry.Pack, 64) {
					valid = false
					break
				}
				catalog.add(entry.EmbeddingKey, entry.VectorDigest, vectorPackSlot{Pack: entry.Pack, Slot: entry.Slot})
			}
			if valid && validateCachedVectorPackCatalog(sync.dir, catalog) {
				return catalog, nil
			}
		}
	}
	return scanVectorPackCatalog(sync.dir)
}

func validateCachedVectorPackCatalog(root string, catalog vectorPackCatalog) bool {
	packs := map[string]vectorPack{}
	for embeddingKey, byDigest := range catalog {
		expectedEmbedding, err := decodeDigest(embeddingKey)
		if err != nil {
			return false
		}
		for vectorDigest, slot := range byDigest {
			expectedVector, err := decodeDigest(vectorDigest)
			if err != nil {
				return false
			}
			pack, ok := packs[slot.Pack]
			if !ok {
				paths, err := filepath.Glob(filepath.Join(root, "packs", "*", slot.Pack+".pack"))
				if err != nil || len(paths) != 1 {
					return false
				}
				pack, err = readVectorPack(paths[0], slot.Pack)
				if err != nil {
					return false
				}
				packs[slot.Pack] = pack
			}
			if uint64(slot.Slot) >= uint64(len(pack.Entries)) {
				return false
			}
			entry := pack.Entries[slot.Slot]
			if entry.EmbeddingKey != expectedEmbedding || entry.VectorDigest != expectedVector {
				return false
			}
		}
	}
	return true
}

func (sync *indexSync) persistVectorPackCatalog(head plumbing.Hash) error {
	if sync.repo == nil || sync.packCatalog == nil {
		return nil
	}
	cache := vectorPackCatalogCache{Version: vectorPackCatalogCacheVersion, Head: head.String()}
	for embeddingKey, byDigest := range sync.packCatalog {
		for vectorDigest, slot := range byDigest {
			cache.Entries = append(cache.Entries, vectorPackCatalogCacheEntry{
				EmbeddingKey: embeddingKey,
				VectorDigest: vectorDigest,
				Pack:         slot.Pack,
				Slot:         slot.Slot,
			})
		}
	}
	slices.SortFunc(cache.Entries, func(a, b vectorPackCatalogCacheEntry) int {
		if a.EmbeddingKey != b.EmbeddingKey {
			return cmp.Compare(a.EmbeddingKey, b.EmbeddingKey)
		}
		if a.VectorDigest != b.VectorDigest {
			return cmp.Compare(a.VectorDigest, b.VectorDigest)
		}
		if a.Pack != b.Pack {
			return cmp.Compare(a.Pack, b.Pack)
		}
		return cmp.Compare(a.Slot, b.Slot)
	})
	dir := filepath.Dir(sync.vectorPackCatalogCachePath(head))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".catalog-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := gob.NewEncoder(temporary).Encode(cache); err != nil {
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
	if err := os.Rename(temporaryPath, sync.vectorPackCatalogCachePath(head)); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	sync.packCatalogDirty = false
	return nil
}

func (sync *indexSync) vectorPackCatalogCachePath(head plumbing.Hash) string {
	return filepath.Join(sync.dir, ".git", "git-agent", "vector-catalog", head.String()+".bin")
}
