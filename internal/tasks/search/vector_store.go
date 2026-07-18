package search

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
)

const (
	sharedVectorStoreVersion  = "shared-v1"
	vectorStoreCatalogVersion = 1
	vectorStoreDirName        = "vector-store"
	vectorStorePayloadName    = "vectors.f32"
)

type vectorStore struct {
	dir string
}

type vectorStoreCatalog struct {
	Version    int                         `json:"version"`
	Generation uint64                      `json:"generation"`
	Entries    map[string]vectorStoreEntry `json:"entries"`
}

type vectorStoreEntry struct {
	Offset     int64  `json:"offset"`
	Dimensions int    `json:"dimensions"`
	Checksum   uint32 `json:"checksum"`
}

func newVectorStore(metadataDir string) vectorStore {
	return vectorStore{dir: filepath.Join(metadataDir, "search", vectorStoreDirName)}
}

func sharedVectorPayloadPath(metadataDir string) string {
	return filepath.Join(newVectorStore(metadataDir).dir, vectorStorePayloadName)
}

func vectorStoreKey(inputHash, model string, dimensions int) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{inputHash, model, strconv.Itoa(dimensions)}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (store vectorStore) put(ctx context.Context, records []vectorRecord, forceKeys map[string]bool) (keys map[string]vectorStoreEntry, err error) {
	keys = make(map[string]vectorStoreEntry, len(records))
	keyData := make(map[string][]byte, len(records))
	if err := os.MkdirAll(store.dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := lockIndex(ctx, store.dir)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()

	catalog, nextGeneration, err := store.loadCatalog()
	if err != nil {
		return nil, err
	}
	payloadPath := filepath.Join(store.dir, vectorStorePayloadName)
	payload, err := os.OpenFile(payloadPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, payload.Close()) }()
	info, err := payload.Stat()
	if err != nil {
		return nil, err
	}
	offset := info.Size()
	changed := false

	for _, record := range records {
		if record.EmbeddingInputHash == "" {
			continue
		}
		if record.Dimensions < 1 || len(record.Vector) != record.Dimensions {
			return nil, fmt.Errorf("vector record %s dimensions mismatch", record.ChunkID)
		}
		key := vectorStoreKey(record.EmbeddingInputHash, record.EmbeddingModel, record.Dimensions)
		data := encodeVector(record.Vector)
		if previous, ok := keyData[key]; ok {
			if !bytes.Equal(previous, data) {
				return nil, fmt.Errorf("vector records for embedding key %s contain conflicting payloads", key)
			}
			continue
		}
		keyData[key] = data
		if !forceKeys[key] {
			if entry, ok := catalog.Entries[key]; ok && entry.Dimensions == record.Dimensions {
				stored, readErr := readStoredVectorData(payload, entry)
				if readErr == nil && bytes.Equal(stored, data) {
					keys[key] = entry
					continue
				}
			}
		}

		entry := vectorStoreEntry{
			Offset:     offset,
			Dimensions: record.Dimensions,
			Checksum:   crc32.ChecksumIEEE(data),
		}
		if _, err := payload.WriteAt(data, offset); err != nil {
			return nil, fmt.Errorf("append shared vector: %w", err)
		}
		offset += int64(len(data))
		keys[key] = entry
		catalog.Entries[key] = entry
		changed = true
	}
	if !changed {
		return keys, nil
	}
	if err := payload.Sync(); err != nil {
		return nil, fmt.Errorf("sync shared vectors: %w", err)
	}
	previousGeneration := catalog.Generation
	catalog.Generation = nextGeneration
	if err := store.publishCatalog(catalog); err != nil {
		return nil, err
	}
	if store.removeOldCatalogs(previousGeneration, catalog.Generation) {
		_ = syncDirectory(store.dir)
	}
	return keys, nil
}

func (store vectorStore) loadCatalog() (vectorStoreCatalog, uint64, error) {
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		return vectorStoreCatalog{}, 0, err
	}
	type candidate struct {
		name       string
		generation uint64
	}
	var candidates []candidate
	var maxGeneration uint64
	for _, entry := range entries {
		generation, ok := vectorStoreCatalogGeneration(entry.Name())
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{name: entry.Name(), generation: generation})
		maxGeneration = max(maxGeneration, generation)
	}
	slices.SortFunc(candidates, func(a, b candidate) int {
		return cmp.Compare(b.generation, a.generation)
	})
	for _, candidate := range candidates {
		data, err := os.ReadFile(filepath.Join(store.dir, candidate.name))
		if err != nil {
			continue
		}
		var catalog vectorStoreCatalog
		if sonic.Unmarshal(data, &catalog) != nil || catalog.Version != vectorStoreCatalogVersion ||
			catalog.Generation != candidate.generation || catalog.Entries == nil {
			continue
		}
		return catalog, maxGeneration + 1, nil
	}
	return vectorStoreCatalog{
		Version: vectorStoreCatalogVersion,
		Entries: map[string]vectorStoreEntry{},
	}, maxGeneration + 1, nil
}

func (store vectorStore) publishCatalog(catalog vectorStoreCatalog) error {
	data, err := sonic.Marshal(catalog)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(store.dir, ".catalog-*.tmp")
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
	path := filepath.Join(store.dir, vectorStoreCatalogName(catalog.Generation))
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish shared vector catalog: %w", err)
	}
	return syncDirectory(store.dir)
}

func (store vectorStore) removeOldCatalogs(previousGeneration, currentGeneration uint64) bool {
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		return false
	}
	removed := false
	for _, entry := range entries {
		generation, ok := vectorStoreCatalogGeneration(entry.Name())
		if !ok || generation == previousGeneration || generation == currentGeneration {
			continue
		}
		if os.Remove(filepath.Join(store.dir, entry.Name())) == nil {
			removed = true
		}
	}
	return removed
}

func vectorStoreCatalogName(generation uint64) string {
	return fmt.Sprintf("catalog-%020d.json", generation)
}

func vectorStoreCatalogGeneration(name string) (uint64, bool) {
	value, ok := strings.CutPrefix(name, "catalog-")
	if !ok {
		return 0, false
	}
	value, ok = strings.CutSuffix(value, ".json")
	if !ok {
		return 0, false
	}
	generation, err := strconv.ParseUint(value, 10, 64)
	return generation, err == nil
}

func readStoredVector(payload io.ReaderAt, entry vectorStoreEntry) ([]float64, error) {
	data, err := readStoredVectorData(payload, entry)
	if err != nil {
		return nil, err
	}
	vector := make([]float64, entry.Dimensions)
	for dimension := range entry.Dimensions {
		bits := binary.LittleEndian.Uint32(data[dimension*4 : dimension*4+4])
		vector[dimension] = float64(math.Float32frombits(bits))
	}
	return vector, nil
}

func readStoredVectorData(payload io.ReaderAt, entry vectorStoreEntry) ([]byte, error) {
	if entry.Dimensions < 1 || entry.Dimensions > int(^uint(0)>>1)/4 || entry.Offset < 0 {
		return nil, errors.New("invalid shared vector reference")
	}
	byteLength := entry.Dimensions * 4
	data := make([]byte, byteLength)
	if _, err := payload.ReadAt(data, entry.Offset); err != nil {
		return nil, fmt.Errorf("read shared vector: %w", err)
	}
	if crc32.ChecksumIEEE(data) != entry.Checksum {
		return nil, errors.New("shared vector checksum mismatch")
	}
	return data, nil
}

func writeSharedVectorIndex(ctx context.Context, metadataDir, indexDir string, records []vectorRecord, forceKeys map[string]bool) error {
	storeEntries, err := newVectorStore(metadataDir).put(ctx, records, forceKeys)
	if err != nil {
		return err
	}
	index := make([]vectorIndexRecord, len(records))
	localPath := filepath.Join(indexDir, "vectors.f32")
	local, err := os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	var localOffset int64
	for i, record := range records {
		if record.Dimensions < 1 || len(record.Vector) != record.Dimensions {
			_ = local.Close()
			return fmt.Errorf("vector record %s dimensions mismatch", record.ChunkID)
		}
		entry := vectorIndexRecordFor(record)
		if record.EmbeddingInputHash != "" {
			entry.VectorKey = vectorStoreKey(record.EmbeddingInputHash, record.EmbeddingModel, record.Dimensions)
			stored, ok := storeEntries[entry.VectorKey]
			if !ok {
				_ = local.Close()
				return fmt.Errorf("shared vector %s was not stored", record.ChunkID)
			}
			entry.Offset = stored.Offset
			entry.VectorChecksum = stored.Checksum
		} else {
			data := encodeVector(record.Vector)
			entry.Offset = localOffset
			entry.VectorChecksum = crc32.ChecksumIEEE(data)
			if _, err := local.Write(data); err != nil {
				_ = local.Close()
				return err
			}
			localOffset += int64(len(data))
		}
		index[i] = entry
	}
	if err := local.Sync(); err != nil {
		_ = local.Close()
		return err
	}
	if err := local.Close(); err != nil {
		return err
	}
	return writeJSONSync(filepath.Join(indexDir, "vectors.index.json"), index)
}

func loadSharedVectors(metadataDir, indexDir string) ([]vectorRecord, error) {
	index, err := loadVectorIndexRecords(indexDir)
	if err != nil {
		return nil, err
	}
	var shared *os.File
	var local *os.File
	defer func() {
		if shared != nil {
			_ = shared.Close()
		}
		if local != nil {
			_ = local.Close()
		}
	}()
	records := make([]vectorRecord, len(index))
	for i, entry := range index {
		payload := local
		if entry.VectorKey != "" {
			if entry.EmbeddingInputHash != "" {
				expectedKey := vectorStoreKey(entry.EmbeddingInputHash, entry.EmbeddingModel, entry.Dimensions)
				if entry.VectorKey != expectedKey {
					return nil, fmt.Errorf("vectors.index.json entry %d has invalid shared vector key", i)
				}
			}
			if shared == nil {
				shared, err = os.Open(sharedVectorPayloadPath(metadataDir))
				if err != nil {
					return nil, err
				}
			}
			payload = shared
		} else if local == nil {
			local, err = os.Open(filepath.Join(indexDir, "vectors.f32"))
			if err != nil {
				return nil, err
			}
			payload = local
		}
		vector, err := readStoredVector(payload, vectorStoreEntry{
			Offset:     entry.Offset,
			Dimensions: entry.Dimensions,
			Checksum:   entry.VectorChecksum,
		})
		if err != nil {
			return nil, fmt.Errorf("vectors.index.json entry %d: %w", i, err)
		}
		records[i] = vectorRecordFromIndex(entry, vector)
	}
	return records, nil
}

func vectorIndexRecordFor(record vectorRecord) vectorIndexRecord {
	return vectorIndexRecord{
		ChunkID:            record.ChunkID,
		Path:               record.Path,
		Source:             record.Source,
		Blob:               record.Blob,
		StartLine:          record.StartLine,
		EndLine:            record.EndLine,
		ContentHash:        record.ContentHash,
		EmbeddingInputHash: record.EmbeddingInputHash,
		EmbeddingModel:     record.EmbeddingModel,
		Dimensions:         record.Dimensions,
		Size:               record.Size,
		MTimeUnixNano:      record.MTimeUnixNano,
	}
}

func vectorRecordFromIndex(entry vectorIndexRecord, vector []float64) vectorRecord {
	return vectorRecord{
		ChunkID:            entry.ChunkID,
		Path:               entry.Path,
		Source:             entry.Source,
		Blob:               entry.Blob,
		StartLine:          entry.StartLine,
		EndLine:            entry.EndLine,
		ContentHash:        entry.ContentHash,
		EmbeddingInputHash: entry.EmbeddingInputHash,
		EmbeddingModel:     entry.EmbeddingModel,
		Dimensions:         entry.Dimensions,
		Size:               entry.Size,
		MTimeUnixNano:      entry.MTimeUnixNano,
		Vector:             vector,
	}
}

func encodeVector(vector []float64) []byte {
	data := make([]byte, len(vector)*4)
	for dimension, value := range vector {
		binary.LittleEndian.PutUint32(data[dimension*4:dimension*4+4], math.Float32bits(float32(value)))
	}
	return data
}
