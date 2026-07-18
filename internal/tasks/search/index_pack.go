package search

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

const (
	vectorPackVersion       = 1
	vectorPackHeaderSize    = 8 + 4 + 4 + 4 + 32
	vectorPackEntrySize     = 32 + 32 + 4
	vectorPackTargetPayload = 16 << 20
)

var vectorPackMagic = [8]byte{'G', 'I', 'T', 'A', 'G', 'P', 'K', 0}

type vectorPackEntry struct {
	EmbeddingKey [32]byte
	VectorDigest [32]byte
	Checksum     uint32
}

type vectorPack struct {
	Dimensions int
	ModelKey   [32]byte
	Entries    []vectorPackEntry
	payload    []byte
}

type vectorPackItem struct {
	EmbeddingKey [32]byte
	VectorDigest [32]byte
	Data         []byte
}

type vectorPackSlot struct {
	Pack string
	Slot uint32
}

type vectorPackCatalog map[string]map[string]vectorPackSlot

func syncModelKey(model string, dimensions int) [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", model, dimensions)))
}

func vectorPayloadDigest(data []byte) [32]byte {
	return sha256.Sum256(data)
}

func digestHex(value [32]byte) string {
	return hex.EncodeToString(value[:])
}

func decodeDigest(value string) ([32]byte, error) {
	var result [32]byte
	if !canonicalLowerHex(value, 64) {
		return result, fmt.Errorf("invalid SHA-256 digest %q", value)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil {
		return result, err
	}
	copy(result[:], decoded)
	return result, nil
}

func encodeVectorPack(dimensions int, modelKey [32]byte, items []vectorPackItem) ([]byte, error) {
	if dimensions < 1 || len(items) < 1 || uint64(len(items)) > uint64(^uint32(0)) {
		return nil, errors.New("invalid vector pack dimensions or item count")
	}
	maxInt := int(^uint(0) >> 1)
	if dimensions > (maxInt-vectorPackEntrySize)/4 {
		return nil, errors.New("vector pack size overflows platform limits")
	}
	vectorBytes := dimensions * 4
	if len(items) > (maxInt-vectorPackHeaderSize)/(vectorPackEntrySize+vectorBytes) {
		return nil, errors.New("vector pack size overflows platform limits")
	}
	items = slices.Clone(items)
	slices.SortFunc(items, compareVectorPackItems)
	for i, item := range items {
		if len(item.Data) != vectorBytes || vectorPayloadDigest(item.Data) != item.VectorDigest {
			return nil, fmt.Errorf("vector pack item %d has invalid payload", i)
		}
		if i > 0 && compareVectorPackItems(items[i-1], item) == 0 {
			return nil, errors.New("vector pack contains duplicate identity")
		}
	}
	total := vectorPackHeaderSize + len(items)*vectorPackEntrySize + len(items)*vectorBytes
	data := make([]byte, total)
	copy(data[:8], vectorPackMagic[:])
	binary.LittleEndian.PutUint32(data[8:12], vectorPackVersion)
	binary.LittleEndian.PutUint32(data[12:16], uint32(dimensions))
	binary.LittleEndian.PutUint32(data[16:20], uint32(len(items)))
	copy(data[20:52], modelKey[:])
	entriesOffset := vectorPackHeaderSize
	payloadOffset := vectorPackHeaderSize + len(items)*vectorPackEntrySize
	for i, item := range items {
		entryOffset := entriesOffset + i*vectorPackEntrySize
		copy(data[entryOffset:entryOffset+32], item.EmbeddingKey[:])
		copy(data[entryOffset+32:entryOffset+64], item.VectorDigest[:])
		binary.LittleEndian.PutUint32(data[entryOffset+64:entryOffset+68], crc32.ChecksumIEEE(item.Data))
		copy(data[payloadOffset+i*vectorBytes:payloadOffset+(i+1)*vectorBytes], item.Data)
	}
	return data, nil
}

func compareVectorPackItems(a, b vectorPackItem) int {
	if order := bytes.Compare(a.EmbeddingKey[:], b.EmbeddingKey[:]); order != 0 {
		return order
	}
	return bytes.Compare(a.VectorDigest[:], b.VectorDigest[:])
}

func decodeVectorPack(data []byte, expectedDigest string) (vectorPack, error) {
	if len(data) < vectorPackHeaderSize {
		return vectorPack{}, errors.New("vector pack header is truncated")
	}
	if !bytes.Equal(data[:8], vectorPackMagic[:]) {
		return vectorPack{}, errors.New("vector pack magic is invalid")
	}
	if version := binary.LittleEndian.Uint32(data[8:12]); version != vectorPackVersion {
		return vectorPack{}, fmt.Errorf("unsupported vector pack version %d", version)
	}
	dimensions64 := uint64(binary.LittleEndian.Uint32(data[12:16]))
	count64 := uint64(binary.LittleEndian.Uint32(data[16:20]))
	if dimensions64 == 0 || count64 == 0 {
		return vectorPack{}, errors.New("vector pack dimensions and count must be positive")
	}
	perItemBytes := uint64(vectorPackEntrySize) + dimensions64*4
	remainingBytes := uint64(len(data) - vectorPackHeaderSize)
	if count64 > remainingBytes/perItemBytes || count64*perItemBytes != remainingBytes {
		return vectorPack{}, errors.New("vector pack size does not match its dimensions and count")
	}
	actualDigest := sha256.Sum256(data)
	if expectedDigest != "" && digestHex(actualDigest) != expectedDigest {
		return vectorPack{}, errors.New("vector pack content digest mismatch")
	}
	var modelKey [32]byte
	copy(modelKey[:], data[20:52])
	dimensions := int(dimensions64)
	count := int(count64)
	entries := make([]vectorPackEntry, count)
	payloadOffset := vectorPackHeaderSize + count*vectorPackEntrySize
	payload := data[payloadOffset:]
	for i := range count {
		offset := vectorPackHeaderSize + i*vectorPackEntrySize
		copy(entries[i].EmbeddingKey[:], data[offset:offset+32])
		copy(entries[i].VectorDigest[:], data[offset+32:offset+64])
		entries[i].Checksum = binary.LittleEndian.Uint32(data[offset+64 : offset+68])
		if i > 0 && compareVectorPackEntries(entries[i-1], entries[i]) >= 0 {
			return vectorPack{}, errors.New("vector pack entries are not strictly sorted")
		}
		vectorData := payload[i*dimensions*4 : (i+1)*dimensions*4]
		if crc32.ChecksumIEEE(vectorData) != entries[i].Checksum {
			return vectorPack{}, fmt.Errorf("vector pack slot %d checksum mismatch", i)
		}
		if vectorPayloadDigest(vectorData) != entries[i].VectorDigest {
			return vectorPack{}, fmt.Errorf("vector pack slot %d digest mismatch", i)
		}
	}
	return vectorPack{Dimensions: dimensions, ModelKey: modelKey, Entries: entries, payload: payload}, nil
}

func compareVectorPackEntries(a, b vectorPackEntry) int {
	if order := bytes.Compare(a.EmbeddingKey[:], b.EmbeddingKey[:]); order != 0 {
		return order
	}
	return bytes.Compare(a.VectorDigest[:], b.VectorDigest[:])
}

func (pack vectorPack) vector(slot uint32) ([]float64, error) {
	if uint64(slot) >= uint64(len(pack.Entries)) {
		return nil, errors.New("vector pack slot is out of range")
	}
	start := int(slot) * pack.Dimensions * 4
	entry := vectorStoreEntry{
		Offset:     int64(start),
		Dimensions: pack.Dimensions,
		Checksum:   pack.Entries[slot].Checksum,
	}
	return readStoredVector(bytes.NewReader(pack.payload), entry)
}

func readVectorPack(path, expectedDigest string) (vectorPack, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return vectorPack{}, err
	}
	pack, err := decodeVectorPack(data, expectedDigest)
	if err != nil {
		return vectorPack{}, fmt.Errorf("decode vector pack %s: %w", path, err)
	}
	return pack, nil
}

func vectorPackBatches(items []vectorPackItem, dimensions int) [][]vectorPackItem {
	items = slices.Clone(items)
	slices.SortFunc(items, compareVectorPackItems)
	perPack := max(1, vectorPackTargetPayload/(dimensions*4))
	var batches [][]vectorPackItem
	for len(items) > 0 {
		count := min(perPack, len(items))
		batches = append(batches, items[:count:count])
		items = items[count:]
	}
	return batches
}

func (sync *indexSync) publishVectorPack(modelKey [32]byte, data []byte) (string, error) {
	digest := digestHex(sha256.Sum256(data))
	dir := filepath.Join(sync.dir, "packs", digestHex(modelKey))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, digest+".pack")
	if existing, err := os.ReadFile(path); err == nil {
		if !bytes.Equal(existing, data) {
			return "", errors.New("content-addressed vector pack path contains different bytes")
		}
		return digest, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	temporary, err := os.CreateTemp(dir, ".pack-*.tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", err
	}
	if err := syncDirectory(dir); err != nil {
		return "", err
	}
	return digest, nil
}

func (catalog vectorPackCatalog) add(embeddingKey, vectorDigest string, slot vectorPackSlot) {
	byDigest := catalog[embeddingKey]
	if byDigest == nil {
		byDigest = map[string]vectorPackSlot{}
		catalog[embeddingKey] = byDigest
	}
	current, ok := byDigest[vectorDigest]
	if !ok || slot.Pack < current.Pack || slot.Pack == current.Pack && slot.Slot < current.Slot {
		byDigest[vectorDigest] = slot
	}
}

func scanVectorPackCatalog(root string) (vectorPackCatalog, error) {
	catalog := vectorPackCatalog{}
	packsRoot := filepath.Join(root, "packs")
	err := filepath.WalkDir(packsRoot, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".pack" {
			return nil
		}
		digest := entry.Name()[:len(entry.Name())-len(".pack")]
		pack, err := readVectorPack(path, digest)
		if err != nil {
			return err
		}
		modelDir := filepath.Base(filepath.Dir(path))
		if digestHex(pack.ModelKey) != modelDir {
			return fmt.Errorf("vector pack %s model key does not match its directory", path)
		}
		for i, item := range pack.Entries {
			catalog.add(digestHex(item.EmbeddingKey), digestHex(item.VectorDigest), vectorPackSlot{Pack: digest, Slot: uint32(i)})
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return catalog, nil
	}
	return catalog, err
}
