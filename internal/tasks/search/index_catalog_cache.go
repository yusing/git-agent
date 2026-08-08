package search

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
)

const (
	legacyVectorPackCatalogCacheVersion = 1
	vectorPackCatalogCacheVersion       = 2
	vectorPackCatalogCacheHeaderSize    = 8 + 4 + 20 + 4 + 8
	vectorPackCatalogCachePackRowSize   = 32 + 4
	vectorPackCatalogCacheSlotRowSize   = 32 + 32 + 4 + 4
	vectorPackCatalogCacheChecksumSize  = 32
)

var vectorPackCatalogCacheMagic = [8]byte{'G', 'I', 'T', 'A', 'G', 'C', 'T', 0}

type legacyVectorPackCatalogCache struct {
	Version int
	Head    string
	Entries []legacyVectorPackCatalogCacheEntry
}

type legacyVectorPackCatalogCacheEntry struct {
	EmbeddingKey string
	VectorDigest string
	Pack         string
	Slot         uint32
}

type vectorPackCatalogCachePack struct {
	digest  [32]byte
	entries []vectorPackEntry
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
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) >= len(vectorPackCatalogCacheMagic) &&
			bytes.Equal(data[:len(vectorPackCatalogCacheMagic)], vectorPackCatalogCacheMagic[:]) {
			packs, scanErr := scanVectorPackCatalogCachePacks(sync.dir)
			if scanErr == nil {
				if catalog, decodeErr := decodeVectorPackCatalogCache(data, head.Hash(), packs); decodeErr == nil {
					return catalog, nil
				}
			}
		} else {
			var cached legacyVectorPackCatalogCache
			if gob.NewDecoder(bytes.NewReader(data)).Decode(&cached) == nil &&
				cached.Version == legacyVectorPackCatalogCacheVersion &&
				cached.Head == head.Hash().String() {
				catalog := vectorPackCatalog{}
				valid := true
				for _, entry := range cached.Entries {
					if !canonicalLowerHex(entry.EmbeddingKey, 64) ||
						!canonicalLowerHex(entry.VectorDigest, 64) ||
						!canonicalLowerHex(entry.Pack, 64) {
						valid = false
						break
					}
					catalog.add(entry.EmbeddingKey, entry.VectorDigest, vectorPackSlot{
						Pack: entry.Pack,
						Slot: entry.Slot,
					})
				}
				if valid && validateCachedVectorPackCatalog(sync.dir, catalog) {
					packs, scanErr := scanVectorPackCatalogCachePacks(sync.dir)
					if scanErr == nil {
						authoritative := vectorPackCatalogFromCachePacks(packs)
						_ = sync.persistVectorPackCatalogPacks(head.Hash(), packs, authoritative)
						return authoritative, nil
					}
				}
			}
		}
	}
	return scanVectorPackCatalog(sync.dir)
}

func validateCachedVectorPackCatalog(root string, catalog vectorPackCatalog) bool {
	type expectedSlot struct {
		embeddingKey [32]byte
		vectorDigest [32]byte
		slot         uint32
	}
	expectedByPack := map[string][]expectedSlot{}
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
			expectedByPack[slot.Pack] = append(expectedByPack[slot.Pack], expectedSlot{
				embeddingKey: expectedEmbedding,
				vectorDigest: expectedVector,
				slot:         slot.Slot,
			})
		}
	}
	for packDigest, expected := range expectedByPack {
		paths, err := filepath.Glob(filepath.Join(root, "packs", "*", packDigest+".pack"))
		if err != nil || len(paths) != 1 {
			return false
		}
		pack, err := readVectorPack(paths[0], packDigest)
		if err != nil {
			return false
		}
		for _, item := range expected {
			if uint64(item.slot) >= uint64(len(pack.Entries)) {
				return false
			}
			entry := pack.Entries[item.slot]
			if entry.EmbeddingKey != item.embeddingKey || entry.VectorDigest != item.vectorDigest {
				return false
			}
		}
	}
	return true
}

func scanVectorPackCatalogCachePacks(root string) ([]vectorPackCatalogCachePack, error) {
	var packs []vectorPackCatalogCachePack
	packsRoot := filepath.Join(root, "packs")
	err := filepath.WalkDir(packsRoot, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".pack" {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("vector pack %s is not a regular file", path)
		}
		relative, err := filepath.Rel(packsRoot, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) != 2 {
			return fmt.Errorf("vector pack %s is outside the pack tree layout", path)
		}
		packDigest := strings.TrimSuffix(parts[1], ".pack")
		if !canonicalLowerHex(parts[0], 64) || !canonicalLowerHex(packDigest, 64) {
			return fmt.Errorf("vector pack %s has an invalid path", path)
		}
		digest, err := decodeDigest(packDigest)
		if err != nil {
			return err
		}
		pack, err := readVectorPack(path, packDigest)
		if err != nil {
			return err
		}
		if digestHex(pack.ModelKey) != parts[0] {
			return fmt.Errorf("vector pack %s model key does not match its directory", path)
		}
		packs = append(packs, vectorPackCatalogCachePack{
			digest:  digest,
			entries: slices.Clone(pack.Entries),
		})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return packs, nil
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(packs, func(a, b vectorPackCatalogCachePack) int {
		return bytes.Compare(a.digest[:], b.digest[:])
	})
	for i := 1; i < len(packs); i++ {
		if packs[i-1].digest == packs[i].digest {
			return nil, errors.New("vector pack tree contains a duplicate pack digest")
		}
	}
	return packs, nil
}

func vectorPackCatalogFromCachePacks(packs []vectorPackCatalogCachePack) vectorPackCatalog {
	catalog := vectorPackCatalog{}
	for _, pack := range packs {
		packDigest := digestHex(pack.digest)
		for slot, item := range pack.entries {
			catalog.add(
				digestHex(item.EmbeddingKey),
				digestHex(item.VectorDigest),
				vectorPackSlot{Pack: packDigest, Slot: uint32(slot)},
			)
		}
	}
	return catalog
}

func encodeVectorPackCatalogCache(head plumbing.Hash, packs []vectorPackCatalogCachePack) ([]byte, error) {
	if uint64(len(packs)) > uint64(^uint32(0)) {
		return nil, errors.New("vector pack catalog pack count overflows")
	}
	var entryCount uint64
	for i, pack := range packs {
		if i > 0 && bytes.Compare(packs[i-1].digest[:], pack.digest[:]) >= 0 {
			return nil, errors.New("vector pack catalog packs are not strictly sorted")
		}
		if uint64(len(pack.entries)) > uint64(^uint32(0)) ||
			entryCount > ^uint64(0)-uint64(len(pack.entries)) {
			return nil, errors.New("vector pack catalog entry count overflows")
		}
		entryCount += uint64(len(pack.entries))
	}
	size, err := vectorPackCatalogCacheSize(uint64(len(packs)), entryCount)
	if err != nil {
		return nil, err
	}
	headBytes := head.Bytes()
	if len(headBytes) != 20 {
		return nil, errors.New("vector pack catalog cache requires a SHA-1 HEAD")
	}
	data := make([]byte, size)
	copy(data[:8], vectorPackCatalogCacheMagic[:])
	binary.LittleEndian.PutUint32(data[8:12], vectorPackCatalogCacheVersion)
	copy(data[12:32], headBytes)
	binary.LittleEndian.PutUint32(data[32:36], uint32(len(packs)))
	binary.LittleEndian.PutUint64(data[36:44], entryCount)

	offset := vectorPackCatalogCacheHeaderSize
	for _, pack := range packs {
		copy(data[offset:offset+32], pack.digest[:])
		binary.LittleEndian.PutUint32(data[offset+32:offset+36], uint32(len(pack.entries)))
		offset += vectorPackCatalogCachePackRowSize
	}
	for packIndex, pack := range packs {
		for slot, entry := range pack.entries {
			copy(data[offset:offset+32], entry.EmbeddingKey[:])
			copy(data[offset+32:offset+64], entry.VectorDigest[:])
			binary.LittleEndian.PutUint32(data[offset+64:offset+68], uint32(packIndex))
			binary.LittleEndian.PutUint32(data[offset+68:offset+72], uint32(slot))
			offset += vectorPackCatalogCacheSlotRowSize
		}
	}
	checksum := sha256.Sum256(data[:offset])
	copy(data[offset:], checksum[:])
	return data, nil
}

func decodeVectorPackCatalogCache(
	data []byte,
	expectedHead plumbing.Hash,
	packs []vectorPackCatalogCachePack,
) (vectorPackCatalog, error) {
	if len(data) < vectorPackCatalogCacheHeaderSize+vectorPackCatalogCacheChecksumSize {
		return nil, errors.New("vector pack catalog cache is truncated")
	}
	if !bytes.Equal(data[:8], vectorPackCatalogCacheMagic[:]) {
		return nil, errors.New("vector pack catalog cache magic is invalid")
	}
	if version := binary.LittleEndian.Uint32(data[8:12]); version != vectorPackCatalogCacheVersion {
		return nil, fmt.Errorf("unsupported vector pack catalog cache version %d", version)
	}
	expectedHeadBytes := expectedHead.Bytes()
	if len(expectedHeadBytes) != 20 || !bytes.Equal(data[12:32], expectedHeadBytes) {
		return nil, errors.New("vector pack catalog cache HEAD does not match")
	}
	packCount := uint64(binary.LittleEndian.Uint32(data[32:36]))
	entryCount := binary.LittleEndian.Uint64(data[36:44])
	size, err := vectorPackCatalogCacheSize(packCount, entryCount)
	if err != nil {
		return nil, err
	}
	if len(data) != size {
		return nil, errors.New("vector pack catalog cache size does not match its counts")
	}
	checksumOffset := len(data) - vectorPackCatalogCacheChecksumSize
	expectedChecksum := sha256.Sum256(data[:checksumOffset])
	if !bytes.Equal(data[checksumOffset:], expectedChecksum[:]) {
		return nil, errors.New("vector pack catalog cache checksum mismatch")
	}
	if packCount != uint64(len(packs)) {
		return nil, errors.New("vector pack catalog cache pack set does not match the current tree")
	}

	offset := vectorPackCatalogCacheHeaderSize
	var declaredEntries uint64
	for i := range int(packCount) {
		digest := data[offset : offset+32]
		slotCount := uint64(binary.LittleEndian.Uint32(data[offset+32 : offset+36]))
		if i > 0 && bytes.Compare(data[offset-vectorPackCatalogCachePackRowSize:offset-vectorPackCatalogCachePackRowSize+32], digest) >= 0 {
			return nil, errors.New("vector pack catalog cache packs are not strictly sorted")
		}
		if !bytes.Equal(digest, packs[i].digest[:]) || slotCount != uint64(len(packs[i].entries)) {
			return nil, errors.New("vector pack catalog cache pack set does not match the current tree")
		}
		if declaredEntries > ^uint64(0)-slotCount {
			return nil, errors.New("vector pack catalog cache entry count overflows")
		}
		declaredEntries += slotCount
		offset += vectorPackCatalogCachePackRowSize
	}
	if declaredEntries != entryCount {
		return nil, errors.New("vector pack catalog cache slot coverage does not match pack counts")
	}

	catalog := vectorPackCatalog{}
	var seenEntries uint64
	for packIndex, pack := range packs {
		for slot, expected := range pack.entries {
			if seenEntries >= entryCount {
				return nil, errors.New("vector pack catalog cache slot coverage is incomplete")
			}
			rowPackIndex := binary.LittleEndian.Uint32(data[offset+64 : offset+68])
			rowSlot := binary.LittleEndian.Uint32(data[offset+68 : offset+72])
			if uint64(rowPackIndex) >= packCount {
				return nil, errors.New("vector pack catalog cache pack index is out of range")
			}
			if rowPackIndex != uint32(packIndex) || rowSlot != uint32(slot) {
				return nil, errors.New("vector pack catalog cache slots are not strictly ordered")
			}
			if !bytes.Equal(data[offset:offset+32], expected.EmbeddingKey[:]) ||
				!bytes.Equal(data[offset+32:offset+64], expected.VectorDigest[:]) {
				return nil, errors.New("vector pack catalog cache slot does not match its vector pack")
			}
			catalog.add(
				digestHex(expected.EmbeddingKey),
				digestHex(expected.VectorDigest),
				vectorPackSlot{Pack: digestHex(pack.digest), Slot: uint32(slot)},
			)
			offset += vectorPackCatalogCacheSlotRowSize
			seenEntries++
		}
	}
	if seenEntries != entryCount || offset != checksumOffset {
		return nil, errors.New("vector pack catalog cache slot coverage is incomplete")
	}
	return catalog, nil
}

func vectorPackCatalogCacheSize(packCount, entryCount uint64) (int, error) {
	maxInt := uint64(^uint(0) >> 1)
	base := uint64(vectorPackCatalogCacheHeaderSize + vectorPackCatalogCacheChecksumSize)
	if packCount > (maxInt-base)/vectorPackCatalogCachePackRowSize {
		return 0, errors.New("vector pack catalog cache size overflows platform limits")
	}
	total := base + packCount*vectorPackCatalogCachePackRowSize
	if entryCount > (maxInt-total)/vectorPackCatalogCacheSlotRowSize {
		return 0, errors.New("vector pack catalog cache size overflows platform limits")
	}
	return int(total + entryCount*vectorPackCatalogCacheSlotRowSize), nil
}

func (sync *indexSync) persistVectorPackCatalog(head plumbing.Hash) error {
	if sync.repo == nil || sync.packCatalog == nil {
		return nil
	}
	packs, err := scanVectorPackCatalogCachePacks(sync.dir)
	if err != nil {
		return err
	}
	catalog := vectorPackCatalogFromCachePacks(packs)
	return sync.persistVectorPackCatalogPacks(head, packs, catalog)
}

func (sync *indexSync) persistVectorPackCatalogPacks(
	head plumbing.Hash,
	packs []vectorPackCatalogCachePack,
	catalog vectorPackCatalog,
) error {
	data, err := encodeVectorPackCatalogCache(head, packs)
	if err != nil {
		return err
	}
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
	if _, err := temporary.Write(data); err != nil {
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
	if err := sync.pruneVectorPackCatalogCachesForHead(head); err != nil {
		return err
	}
	sync.packCatalog = catalog
	sync.packCatalogDirty = false
	return nil
}

func (sync *indexSync) pruneVectorPackCatalogCaches() error {
	if sync.repo == nil || sync.schema != indexSyncSchemaV2 {
		return nil
	}
	head, err := sync.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return sync.pruneVectorPackCatalogCachesForHead(head.Hash())
}

func (sync *indexSync) pruneVectorPackCatalogCachesForHead(head plumbing.Hash) error {
	cachePath := sync.vectorPackCatalogCachePath(head)
	dir := filepath.Dir(cachePath)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	current := filepath.Base(cachePath)
	removed := false
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == current || !ownedVectorPackCatalogCacheName(entry.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return syncDirectory(dir)
	}
	return nil
}

func ownedVectorPackCatalogCacheName(name string) bool {
	if strings.HasPrefix(name, ".catalog-") && strings.HasSuffix(name, ".tmp") {
		return true
	}
	hash, ok := strings.CutSuffix(name, ".bin")
	return ok && canonicalLowerHex(hash, len(plumbing.ZeroHash.String()))
}

func (sync *indexSync) vectorPackCatalogCachePath(head plumbing.Hash) string {
	return filepath.Join(sync.dir, ".git", "git-agent", "vector-catalog", head.String()+".bin")
}
