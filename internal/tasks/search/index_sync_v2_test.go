package search

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
)

func TestIndexSyncSchemaRejectsMalformedAndFutureData(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		want string
	}{
		{name: "malformed", data: "{", want: "parse index sync schema"},
		{name: "missing version", data: "{}\n", want: "unsupported index sync schema version 0"},
		{name: "wrong type", data: "{\"version\":\"1\"}\n", want: "parse index sync schema"},
		{name: "unknown field", data: "{\"version\":1,\"mode\":\"future\"}\n", want: "unknown field"},
		{name: "duplicate version", data: "{\"version\":1,\"version\":2}\n", want: "duplicate JSON object key"},
		{name: "trailing value", data: "{\"version\":1}\n{}\n", want: "unexpected trailing JSON value"},
		{name: "future", data: "{\"version\":3}\n", want: "unsupported index sync schema version 3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "schema.json", test.data)
			sync := &indexSync{dir: root}
			err := sync.ensureSchema()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ensureSchema() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestIndexSyncSchemaRejectsMissingSchemaWithData(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "indexes/data", "not a schema\n")
	err := (&indexSync{dir: root}).ensureSchema()
	if err == nil || !strings.Contains(err.Error(), "has data but no schema.json") {
		t.Fatalf("ensureSchema() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "schema.json")); !os.IsNotExist(err) {
		t.Fatalf("schema was published after invalid tree: %v", err)
	}
}

func TestVectorPackRoundTripAndValidation(t *testing.T) {
	modelKey := syncModelKey("test-model", 3)
	firstData := encodeVector([]float64{1, 0, 0})
	secondData := encodeVector([]float64{0, 1, 0})
	firstKey, err := decodeDigest(vectorStoreKey("first", "test-model", 3))
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := decodeDigest(vectorStoreKey("second", "test-model", 3))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeVectorPack(3, modelKey, []vectorPackItem{
		{EmbeddingKey: secondKey, VectorDigest: vectorPayloadDigest(secondData), Data: secondData},
		{EmbeddingKey: firstKey, VectorDigest: vectorPayloadDigest(firstData), Data: firstData},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := digestHex(vectorPayloadDigest(encoded))
	pack, err := decodeVectorPack(encoded, digest)
	if err != nil {
		t.Fatal(err)
	}
	if pack.Dimensions != 3 || pack.ModelKey != modelKey || len(pack.Entries) != 2 {
		t.Fatalf("decoded pack = %#v", pack)
	}
	for slot := range pack.Entries {
		vector, err := pack.vector(uint32(slot))
		if err != nil || len(vector) != 3 {
			t.Fatalf("vector slot %d = %#v, %v", slot, vector, err)
		}
	}
	if _, err := pack.vector(2); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("out-of-range vector error = %v", err)
	}
	if _, err := decodeVectorPack(encoded[:len(encoded)-1], ""); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("truncated pack error = %v", err)
	}
	future := bytes.Clone(encoded)
	binary.LittleEndian.PutUint32(future[8:12], vectorPackVersion+1)
	if _, err := decodeVectorPack(future, ""); err == nil || !strings.Contains(err.Error(), "unsupported vector pack version") {
		t.Fatalf("future pack error = %v", err)
	}
	corrupt := bytes.Clone(encoded)
	corrupt[len(corrupt)-1] ^= 0xff
	if _, err := decodeVectorPack(corrupt, ""); err == nil || (!strings.Contains(err.Error(), "checksum") && !strings.Contains(err.Error(), "digest")) {
		t.Fatalf("corrupt pack error = %v", err)
	}
	if _, err := decodeVectorPack(encoded, strings.Repeat("0", 64)); err == nil || !strings.Contains(err.Error(), "content digest") {
		t.Fatalf("filename digest error = %v", err)
	}
	overflow := make([]byte, vectorPackHeaderSize)
	copy(overflow[:8], vectorPackMagic[:])
	binary.LittleEndian.PutUint32(overflow[8:12], vectorPackVersion)
	binary.LittleEndian.PutUint32(overflow[12:16], 1<<31-17)
	binary.LittleEndian.PutUint32(overflow[16:20], 1<<31)
	if _, err := decodeVectorPack(overflow, ""); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("overflowing pack size error = %v", err)
	}
}

func FuzzDecodeVectorPack(f *testing.F) {
	modelKey := syncModelKey("fuzz-model", 2)
	data := encodeVector([]float64{0.25, -0.5})
	embeddingKey, err := decodeDigest(vectorStoreKey("fuzz-input", "fuzz-model", 2))
	if err != nil {
		f.Fatal(err)
	}
	valid, err := encodeVectorPack(2, modelKey, []vectorPackItem{{
		EmbeddingKey: embeddingKey,
		VectorDigest: vectorPayloadDigest(data),
		Data:         data,
	}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	for i := range min(len(valid), vectorPackHeaderSize+vectorPackEntrySize+1) {
		f.Add(bytes.Clone(valid[:i]))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		_, _ = decodeVectorPack(input, "")
	})
}

func TestIndexMigrationDeduplicatesAndImportsV2(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	remote := newEmptySyncRemote(t)
	sync, err := openIndexSync(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	record := testSyncedVectorRecord("shared-input", []float64{1, 0, 0})
	origin := "https://example.test/acme/migration"
	for _, revision := range []string{strings.Repeat("1", 40), strings.Repeat("2", 40)} {
		target := indexSyncTarget{origin: origin, revision: revision, model: "test-model", dimensions: 3}
		if _, err := sync.writeSnapshot(target, []vectorRecord{record}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sync.commitPending("seed v1 migration test"); err != nil {
		t.Fatal(err)
	}
	if err := sync.push(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := sync.close(); err != nil {
		t.Fatal(err)
	}
	remoteRepo, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	before, err := remoteRepo.Head()
	if err != nil {
		t.Fatal(err)
	}
	var dryProgress []Progress
	dryRun, err := MigrateIndex(t.Context(), remote, IndexMigrationOptions{
		DryRun: true,
		ProgressLog: func(progress Progress) error {
			dryProgress = append(dryProgress, progress)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	afterDryRun, err := remoteRepo.Head()
	if err != nil {
		t.Fatal(err)
	}
	if afterDryRun.Hash() != before.Hash() {
		t.Fatal("dry-run changed the remote")
	}
	if dryRun.Indexes != 2 || dryRun.Records != 2 || dryRun.UniqueVectors != 1 || dryRun.Packs != 1 {
		t.Fatalf("dry-run summary = %#v", dryRun)
	}
	if got, want := compactProgressStatuses(dryProgress), []string{ProgressStatusFetching, ProgressStatusScanning, ProgressStatusBuilding}; !slices.Equal(got, want) {
		t.Fatalf("dry-run progress statuses = %q, want %q", got, want)
	}
	var migrationProgress []Progress
	summary, err := MigrateIndex(t.Context(), remote, IndexMigrationOptions{
		ProgressLog: func(progress Progress) error {
			migrationProgress = append(migrationProgress, progress)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Indexes != 2 || summary.Records != 2 || summary.UniqueVectors != 1 || summary.Packs != 1 {
		t.Fatalf("migration summary = %#v", summary)
	}
	if got, want := compactProgressStatuses(migrationProgress), []string{ProgressStatusFetching, ProgressStatusScanning, ProgressStatusBuilding, ProgressStatusInstalling, ProgressStatusPushing}; !slices.Equal(got, want) {
		t.Fatalf("migration progress statuses = %q, want %q", got, want)
	}

	t.Setenv("HOME", t.TempDir())
	metadataDir := t.TempDir()
	indexDir := filepath.Join(metadataDir, "search", "revs", strings.Repeat("1", 40))
	target := indexSyncTarget{
		origin:      origin,
		revision:    strings.Repeat("1", 40),
		model:       "test-model",
		dimensions:  3,
		metadataDir: metadataDir,
		indexDir:    indexDir,
		root:        t.TempDir(),
		source:      Source{Mode: "revision", ResolvedRev: strings.Repeat("1", 40), OriginIdentity: origin},
	}
	importSync, err := prepareIndexSync(t.Context(), remote, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := importSync.close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadVectors(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || !bytes.Equal(encodeVector(loaded[0].Vector), encodeVector(record.Vector)) {
		t.Fatalf("imported records = %#v", loaded)
	}
}

func TestV2SnapshotRejectsConflictingPayloadForEmbeddingIdentity(t *testing.T) {
	root := t.TempDir()
	if err := writeIndexSyncSchema(root, indexSyncSchemaV2); err != nil {
		t.Fatal(err)
	}
	sync := &indexSync{dir: root, schema: indexSyncSchemaV2}
	first := testSyncedVectorRecord("same-input", []float64{1, 0, 0})
	second := testSyncedVectorRecord("same-input", []float64{0, 1, 0})
	second.Path = "other.go"
	target := indexSyncTarget{origin: "https://example.test/acme/conflict", revision: strings.Repeat("a", 40), model: "test-model", dimensions: 3}
	_, err := sync.writeSnapshotV2(target, []vectorRecord{first, second})
	if err == nil || !strings.Contains(err.Error(), "conflicting payloads for embedding key") {
		t.Fatalf("writeSnapshotV2() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "indexes")); !os.IsNotExist(err) {
		t.Fatalf("conflicting snapshot published indexes: %v", err)
	}
}

func TestV2SnapshotRejectsUnknownAndFutureJSON(t *testing.T) {
	root := t.TempDir()
	if err := writeIndexSyncSchema(root, indexSyncSchemaV2); err != nil {
		t.Fatal(err)
	}
	sync := &indexSync{dir: root, schema: indexSyncSchemaV2}
	target := indexSyncTarget{origin: "https://example.test/acme/future", revision: strings.Repeat("d", 40), model: "test-model", dimensions: 3}
	if _, err := sync.writeSnapshotV2(target, []vectorRecord{testSyncedVectorRecord("future", []float64{1, 0, 0})}); err != nil {
		t.Fatal(err)
	}
	path, err := sync.snapshotPath(target)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	unknown := bytes.Replace(valid, []byte("{\"version\":2,"), []byte("{\"version\":2,\"future\":true,"), 1)
	if _, err := sync.decodeV2Snapshot(unknown, target); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown snapshot field error = %v", err)
	}
	future := bytes.Replace(valid, []byte("\"version\":2"), []byte("\"version\":3"), 1)
	if _, err := sync.decodeV2Snapshot(future, target); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("future snapshot version error = %v", err)
	}
}

func TestVectorStoreDoesNotAliasDifferentPayloadWithSameEmbeddingKey(t *testing.T) {
	metadataDir := t.TempDir()
	store := newVectorStore(metadataDir)
	first := testSyncedVectorRecord("same-input", []float64{1, 0, 0})
	second := testSyncedVectorRecord("same-input", []float64{0, 1, 0})
	firstEntries, err := store.put(t.Context(), []vectorRecord{first}, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondEntries, err := store.put(t.Context(), []vectorRecord{second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	key := vectorStoreKey("same-input", "test-model", 3)
	if firstEntries[key].Offset == secondEntries[key].Offset {
		t.Fatal("different payload reused the prior shared-vector offset")
	}
	_, err = store.put(t.Context(), []vectorRecord{first, second}, nil)
	if err == nil || !strings.Contains(err.Error(), "conflicting payloads") {
		t.Fatalf("same-write conflict error = %v", err)
	}
}

func TestV2ReconcileUnionsPacksAndSnapshots(t *testing.T) {
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
	first, err := openIndexSync(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	second, err := openIndexSync(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstTarget := indexSyncTarget{origin: "https://example.test/acme/first", revision: strings.Repeat("3", 40), model: "test-model", dimensions: 3}
	secondTarget := indexSyncTarget{origin: "https://example.test/acme/second", revision: strings.Repeat("4", 40), model: "test-model", dimensions: 3}
	if _, err := first.writeSnapshot(firstTarget, []vectorRecord{testSyncedVectorRecord("first", []float64{1, 0, 0})}); err != nil {
		t.Fatal(err)
	}
	if err := first.commitPending("publish first v2 snapshot"); err != nil {
		t.Fatal(err)
	}
	if err := first.push(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.writeSnapshot(secondTarget, []vectorRecord{testSyncedVectorRecord("second", []float64{0, 1, 0})}); err != nil {
		t.Fatal(err)
	}
	if err := second.commitPending("publish second v2 snapshot"); err != nil {
		t.Fatal(err)
	}
	if err := second.pushWithRetry(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", t.TempDir())
	merged, err := openIndexSync(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer merged.close()
	for _, target := range []indexSyncTarget{firstTarget, secondTarget} {
		path, err := merged.snapshotPath(target)
		if err != nil {
			t.Fatal(err)
		}
		records, err := merged.loadV2Snapshot(path, target)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 {
			t.Fatalf("records for %#v = %#v", target, records)
		}
	}
	stats, err := readTrackedTreeStats(merged.dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Packs != 2 {
		t.Fatalf("merged pack count = %d, want 2", stats.Packs)
	}
}

func TestMigrationCloneErrorSanitizesRemoteCredentials(t *testing.T) {
	raw := "https://user:secret@example.test/repo.git?token=abc#private-fragment"
	err := sanitizeMigrationCloneError(raw, errors.New("clone "+raw+" failed for user secret abc private-fragment"))
	message := err.Error()
	for _, secret := range []string{"user", "secret", "abc", "private-fragment", "token="} {
		if strings.Contains(message, secret) {
			t.Fatalf("sanitized migration error contains %q: %s", secret, message)
		}
	}
}

func TestIndexMigrationPropagatesProgressError(t *testing.T) {
	remote := newEmptySyncRemote(t)
	wantErr := errors.New("stop migration progress")
	_, err := MigrateIndex(t.Context(), remote, IndexMigrationOptions{
		DryRun: true,
		ProgressLog: func(progress Progress) error {
			if progress.Status == ProgressStatusScanning {
				return wantErr
			}
			return nil
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("migration error = %v, want %v", err, wantErr)
	}
	repo, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Head(); err == nil {
		t.Fatal("dry-run progress failure changed the empty remote")
	}
}

func TestV2ReconcileRejectsSameRecordPayloadConflict(t *testing.T) {
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
	first, err := openIndexSync(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	second, err := openIndexSync(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	target := indexSyncTarget{origin: "https://example.test/acme/conflict", revision: strings.Repeat("5", 40), model: "test-model", dimensions: 3}
	if _, err := first.writeSnapshot(target, []vectorRecord{testSyncedVectorRecord("same", []float64{1, 0, 0})}); err != nil {
		t.Fatal(err)
	}
	if err := first.commitPending("publish first conflict value"); err != nil {
		t.Fatal(err)
	}
	if err := first.push(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.writeSnapshot(target, []vectorRecord{testSyncedVectorRecord("same", []float64{0, 1, 0})}); err != nil {
		t.Fatal(err)
	}
	if err := second.commitPending("publish conflicting value"); err != nil {
		t.Fatal(err)
	}
	err = second.pushWithRetry(t.Context())
	if err == nil || !strings.Contains(err.Error(), "conflicting vector payloads") {
		t.Fatalf("conflicting reconcile error = %v", err)
	}
}

func testSyncedVectorRecord(inputHash string, vector []float64) vectorRecord {
	return vectorRecord{
		ChunkID:            "c000001",
		Path:               "app.go",
		Source:             "revision",
		Blob:               strings.Repeat("b", 40),
		StartLine:          1,
		EndLine:            3,
		ContentHash:        strings.Repeat("c", 64),
		EmbeddingInputHash: inputHash,
		EmbeddingModel:     "test-model",
		Dimensions:         3,
		Vector:             vector,
	}
}

func compactProgressStatuses(progress []Progress) []string {
	var result []string
	for _, update := range progress {
		if len(result) == 0 || result[len(result)-1] != update.Status {
			result = append(result, update.Status)
		}
	}
	return result
}
