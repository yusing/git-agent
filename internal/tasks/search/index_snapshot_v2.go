package search

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

type syncedIndexV2 struct {
	Version    int              `json:"version"`
	Origin     string           `json:"origin"`
	Revision   string           `json:"revision"`
	Model      string           `json:"model"`
	Dimensions int              `json:"dimensions"`
	Packs      []string         `json:"packs"`
	Records    []syncedRecordV2 `json:"records"`
}

type syncedRecordV2 struct {
	ChunkID            string `json:"chunk_id"`
	Path               string `json:"path"`
	Source             string `json:"source"`
	Blob               string `json:"blob"`
	StartLine          int    `json:"start_line"`
	EndLine            int    `json:"end_line"`
	ContentHash        string `json:"content_hash"`
	EmbeddingInputHash string `json:"embedding_input_hash"`
	Size               int64  `json:"size,omitzero"`
	MTimeUnixNano      int64  `json:"mtime_unix_nano,omitzero"`
	Pack               int    `json:"pack"`
	Slot               uint32 `json:"slot"`
}

func (sync *indexSync) loadV2Snapshot(path string, target indexSyncTarget) ([]vectorRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return sync.decodeV2Snapshot(data, target)
}

func (sync *indexSync) decodeV2Snapshot(data []byte, target indexSyncTarget) ([]vectorRecord, error) {
	var snapshot syncedIndexV2
	if err := decodeStrictJSON(data, &snapshot); err != nil {
		return nil, fmt.Errorf("parse synced index v2: %w", err)
	}
	if err := validateV2SnapshotMetadata(snapshot, target); err != nil {
		return nil, err
	}
	modelKey := syncModelKey(snapshot.Model, snapshot.Dimensions)
	for i, digest := range snapshot.Packs {
		if !canonicalLowerHex(digest, 64) {
			return nil, fmt.Errorf("synced index v2 pack %d has invalid digest", i)
		}
		if i > 0 && snapshot.Packs[i-1] >= digest {
			return nil, errors.New("synced index v2 pack table is not strictly sorted")
		}
	}
	packCache := make(map[string]vectorPack, len(snapshot.Packs))
	usedPacks := make([]bool, len(snapshot.Packs))
	records := make([]vectorRecord, len(snapshot.Records))
	seenKeys := make(map[string][32]byte, len(snapshot.Records))
	for i, found := range snapshot.Records {
		if found.Pack < 0 || found.Pack >= len(snapshot.Packs) {
			return nil, fmt.Errorf("synced index v2 record %d pack index is out of range", i)
		}
		if found.EmbeddingInputHash == "" {
			return nil, fmt.Errorf("synced index v2 record %d has no embedding input hash", i)
		}
		packDigest := snapshot.Packs[found.Pack]
		pack, ok := packCache[packDigest]
		if !ok {
			path := filepath.Join(sync.dir, "packs", digestHex(modelKey), packDigest+".pack")
			var err error
			pack, err = readVectorPack(path, packDigest)
			if err != nil {
				return nil, err
			}
			if pack.Dimensions != snapshot.Dimensions || pack.ModelKey != modelKey {
				return nil, fmt.Errorf("vector pack %s is incompatible with snapshot model", packDigest)
			}
			packCache[packDigest] = pack
		}
		if uint64(found.Slot) >= uint64(len(pack.Entries)) {
			return nil, fmt.Errorf("synced index v2 record %d slot is out of range", i)
		}
		entry := pack.Entries[found.Slot]
		expectedEmbeddingKey, err := decodeDigest(vectorStoreKey(found.EmbeddingInputHash, snapshot.Model, snapshot.Dimensions))
		if err != nil {
			return nil, err
		}
		if entry.EmbeddingKey != expectedEmbeddingKey {
			return nil, fmt.Errorf("synced index v2 record %d embedding key mismatch", i)
		}
		if previous, ok := seenKeys[digestHex(expectedEmbeddingKey)]; ok && previous != entry.VectorDigest {
			return nil, fmt.Errorf("synced index v2 contains conflicting payloads for embedding key %s", digestHex(expectedEmbeddingKey))
		}
		seenKeys[digestHex(expectedEmbeddingKey)] = entry.VectorDigest
		vector, err := pack.vector(found.Slot)
		if err != nil {
			return nil, fmt.Errorf("synced index v2 record %d: %w", i, err)
		}
		records[i] = vectorRecord{
			ChunkID:            found.ChunkID,
			Path:               found.Path,
			Source:             found.Source,
			Blob:               found.Blob,
			StartLine:          found.StartLine,
			EndLine:            found.EndLine,
			ContentHash:        found.ContentHash,
			EmbeddingInputHash: found.EmbeddingInputHash,
			EmbeddingModel:     snapshot.Model,
			Dimensions:         snapshot.Dimensions,
			Size:               found.Size,
			MTimeUnixNano:      found.MTimeUnixNano,
			Vector:             vector,
		}
		usedPacks[found.Pack] = true
	}
	for i, used := range usedPacks {
		if !used {
			return nil, fmt.Errorf("synced index v2 pack %d is not referenced", i)
		}
	}
	for i := 1; i < len(records); i++ {
		if compareRecordKeys(records[i-1], records[i]) >= 0 {
			return nil, errors.New("synced index v2 records are not strictly sorted")
		}
	}
	return records, nil
}

func validateV2SnapshotMetadata(snapshot syncedIndexV2, target indexSyncTarget) error {
	if snapshot.Version != indexSyncSchemaV2 || snapshot.Origin != target.origin || snapshot.Revision != target.revision ||
		snapshot.Model != target.model || snapshot.Dimensions != target.dimensions {
		return errors.New("synced index metadata is incompatible with selected revision")
	}
	return nil
}

func compareRecordKeys(a, b vectorRecord) int {
	return bytes.Compare([]byte(cacheRecordKey(a)), []byte(cacheRecordKey(b)))
}

func mergeCompatibleRecordsStrict(base, incoming []vectorRecord, model string, dimensions int) ([]vectorRecord, error) {
	byKey := make(map[string]vectorRecord, len(base)+len(incoming))
	for _, records := range [][]vectorRecord{base, incoming} {
		for _, record := range records {
			if record.EmbeddingModel != model || record.Dimensions != dimensions || len(record.Vector) != dimensions || record.EmbeddingInputHash == "" {
				return nil, errors.New("revision index contains incompatible records")
			}
			key := cacheRecordKey(record)
			if previous, ok := byKey[key]; ok {
				if !bytes.Equal(encodeVector(previous.Vector), encodeVector(record.Vector)) {
					return nil, fmt.Errorf("synced index record %s has conflicting vector payloads", key)
				}
				continue
			}
			byKey[key] = record
		}
	}
	return sortedRecordValues(byKey), nil
}

func (sync *indexSync) writeSnapshotV2(target indexSyncTarget, compatible []vectorRecord) (int, error) {
	if err := validateSyncTreeForSchema(sync.dir, indexSyncSchemaV2); err != nil {
		return 0, err
	}
	path, err := sync.snapshotPath(target)
	if err != nil {
		return 0, err
	}
	records := compatible
	if _, err := os.Stat(path); err == nil {
		existing, err := sync.loadV2Snapshot(path, target)
		if err != nil {
			return 0, err
		}
		records, err = mergeCompatibleRecordsStrict(existing, compatible, target.model, target.dimensions)
		if err != nil {
			return 0, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	snapshot, err := sync.buildV2Snapshot(target, records)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	if err := writeJSONSync(path, snapshot); err != nil {
		return 0, err
	}
	return len(compatible), nil
}

func (sync *indexSync) buildV2Snapshot(target indexSyncTarget, records []vectorRecord) (syncedIndexV2, error) {
	if sync.packCatalog == nil {
		catalog, err := sync.loadVectorPackCatalog()
		if err != nil {
			return syncedIndexV2{}, err
		}
		sync.packCatalog = catalog
	}
	modelKey := syncModelKey(target.model, target.dimensions)
	type identity struct {
		embedding string
		vector    string
	}
	recordIdentity := make([]identity, len(records))
	missing := map[identity]vectorPackItem{}
	seenEmbedding := map[string]string{}
	for i, record := range records {
		if record.EmbeddingModel != target.model || record.Dimensions != target.dimensions || len(record.Vector) != target.dimensions || record.EmbeddingInputHash == "" {
			return syncedIndexV2{}, errors.New("revision index contains incompatible records")
		}
		embeddingKey, err := decodeDigest(vectorStoreKey(record.EmbeddingInputHash, target.model, target.dimensions))
		if err != nil {
			return syncedIndexV2{}, err
		}
		data := encodeVector(record.Vector)
		vectorDigest := vectorPayloadDigest(data)
		id := identity{embedding: digestHex(embeddingKey), vector: digestHex(vectorDigest)}
		if previous, ok := seenEmbedding[id.embedding]; ok && previous != id.vector {
			return syncedIndexV2{}, fmt.Errorf("revision index contains conflicting payloads for embedding key %s", id.embedding)
		}
		seenEmbedding[id.embedding] = id.vector
		recordIdentity[i] = id
		if sync.packCatalog[id.embedding] == nil {
			missing[id] = vectorPackItem{EmbeddingKey: embeddingKey, VectorDigest: vectorDigest, Data: data}
		} else if _, ok := sync.packCatalog[id.embedding][id.vector]; !ok {
			missing[id] = vectorPackItem{EmbeddingKey: embeddingKey, VectorDigest: vectorDigest, Data: data}
		}
	}
	missingItems := make([]vectorPackItem, 0, len(missing))
	for _, item := range missing {
		missingItems = append(missingItems, item)
	}
	for _, batch := range vectorPackBatches(missingItems, target.dimensions) {
		data, err := encodeVectorPack(target.dimensions, modelKey, batch)
		if err != nil {
			return syncedIndexV2{}, err
		}
		packDigest, err := sync.publishVectorPack(modelKey, data)
		if err != nil {
			return syncedIndexV2{}, err
		}
		slices.SortFunc(batch, compareVectorPackItems)
		for slot, item := range batch {
			sync.packCatalog.add(digestHex(item.EmbeddingKey), digestHex(item.VectorDigest), vectorPackSlot{Pack: packDigest, Slot: uint32(slot)})
		}
	}
	packSet := map[string]bool{}
	for _, id := range recordIdentity {
		packSet[sync.packCatalog[id.embedding][id.vector].Pack] = true
	}
	packs := make([]string, 0, len(packSet))
	for digest := range packSet {
		packs = append(packs, digest)
	}
	slices.Sort(packs)
	packIndexes := make(map[string]int, len(packs))
	for i, digest := range packs {
		packIndexes[digest] = i
	}
	slices.SortFunc(records, compareRecordKeys)
	result := syncedIndexV2{
		Version:    indexSyncSchemaV2,
		Origin:     target.origin,
		Revision:   target.revision,
		Model:      target.model,
		Dimensions: target.dimensions,
		Packs:      packs,
		Records:    make([]syncedRecordV2, len(records)),
	}
	for i, record := range records {
		embeddingKey := vectorStoreKey(record.EmbeddingInputHash, target.model, target.dimensions)
		vectorDigest := digestHex(vectorPayloadDigest(encodeVector(record.Vector)))
		slot := sync.packCatalog[embeddingKey][vectorDigest]
		result.Records[i] = syncedRecordV2{
			ChunkID:            record.ChunkID,
			Path:               record.Path,
			Source:             record.Source,
			Blob:               record.Blob,
			StartLine:          record.StartLine,
			EndLine:            record.EndLine,
			ContentHash:        record.ContentHash,
			EmbeddingInputHash: record.EmbeddingInputHash,
			Size:               record.Size,
			MTimeUnixNano:      record.MTimeUnixNano,
			Pack:               packIndexes[slot.Pack],
			Slot:               slot.Slot,
		}
	}
	return result, nil
}
