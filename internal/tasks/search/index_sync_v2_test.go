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
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/yusing/git-agent/internal/metadata"
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
	assertRemoteHasNoLegacyV1Manifests(t, remote)

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

func TestIndexMigrationRepairsLegacyV1ManifestInV2Repository(t *testing.T) {
	fixture := newMixedV2Remote(t, true)
	before := remoteHead(t, fixture.remote)
	var progress []Progress
	summary, err := MigrateIndex(t.Context(), fixture.remote, IndexMigrationOptions{
		ProgressLog: func(update Progress) error {
			progress = append(progress, update)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.From != indexSyncSchemaV2 || summary.To != indexSyncSchemaV2 || summary.Indexes != 1 || summary.Records != 1 {
		t.Fatalf("repair summary = %#v", summary)
	}
	if got, want := compactProgressStatuses(progress), []string{ProgressStatusFetching, ProgressStatusScanning, ProgressStatusBuilding, ProgressStatusInstalling, ProgressStatusPushing}; !slices.Equal(got, want) {
		t.Fatalf("repair progress statuses = %q, want %q", got, want)
	}
	after := remoteHead(t, fixture.remote)
	if after == before {
		t.Fatal("repair did not advance the remote")
	}

	t.Setenv("HOME", t.TempDir())
	repaired, err := openIndexSync(t.Context(), fixture.remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repaired.dir, fixture.legacyRel)); !os.IsNotExist(err) {
		t.Fatalf("legacy v1 manifest remains after repair: %v", err)
	}
	fullPath, err := repaired.snapshotPath(fixture.target)
	if err != nil {
		t.Fatal(err)
	}
	records, err := repaired.loadV2Snapshot(fullPath, fixture.target)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !bytes.Equal(encodeVector(records[0].Vector), encodeVector(fixture.record.Vector)) {
		t.Fatalf("repaired records = %#v", records)
	}
	if err := repaired.close(); err != nil {
		t.Fatal(err)
	}

	repeated, err := MigrateIndex(t.Context(), fixture.remote, IndexMigrationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.From != indexSyncSchemaV2 || repeated.To != indexSyncSchemaV2 || repeated.Indexes != 0 || remoteHead(t, fixture.remote) != after {
		t.Fatalf("repeated repair summary = %#v", repeated)
	}
}

func TestIndexMigrationDryRunReportsLegacyV1ManifestRepairWithoutMutation(t *testing.T) {
	fixture := newMixedV2Remote(t, true)
	before := remoteHead(t, fixture.remote)
	var progress []Progress
	summary, err := MigrateIndex(t.Context(), fixture.remote, IndexMigrationOptions{
		DryRun: true,
		ProgressLog: func(update Progress) error {
			progress = append(progress, update)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.From != indexSyncSchemaV2 || summary.To != indexSyncSchemaV2 || summary.Indexes != 1 || summary.Records != 1 {
		t.Fatalf("dry-run repair summary = %#v", summary)
	}
	if got, want := compactProgressStatuses(progress), []string{ProgressStatusFetching, ProgressStatusScanning, ProgressStatusBuilding}; !slices.Equal(got, want) {
		t.Fatalf("dry-run repair progress statuses = %q, want %q", got, want)
	}
	if after := remoteHead(t, fixture.remote); after != before {
		t.Fatalf("dry-run repair advanced remote from %s to %s", before, after)
	}
}

func TestIndexMigrationRemovesRedundantLegacyV1ManifestFromV2Remote(t *testing.T) {
	fixture := newMixedV2Remote(t, false)
	summary, err := MigrateIndex(t.Context(), fixture.remote, IndexMigrationOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Indexes != 1 || summary.Records != 1 {
		t.Fatalf("redundant repair summary = %#v", summary)
	}
	assertRemoteHasNoLegacyV1Manifests(t, fixture.remote)
}

func TestIndexMigrationMixedV2RejectsInvalidStateWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		want   string
		mutate func(*testing.T, string, mixedV2Fixture)
	}{
		{
			name: "malformed legacy manifest",
			want: "parse synced index v1",
			mutate: func(t *testing.T, root string, fixture mixedV2Fixture) {
				writeFile(t, root, fixture.legacyRel, "{")
			},
		},
		{
			name: "metadata path mismatch",
			want: "metadata does not match path",
			mutate: func(t *testing.T, root string, fixture mixedV2Fixture) {
				mismatch := filepath.Join(filepath.Dir(fixture.legacyRel), strings.Repeat("a", 16)+".json")
				if err := os.Rename(filepath.Join(root, fixture.legacyRel), filepath.Join(root, mismatch)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "conflicting legacy payload",
			want: "conflicting vector payloads",
			mutate: func(t *testing.T, root string, fixture mixedV2Fixture) {
				conflict := fixture.record
				conflict.Vector = []float64{0, 1, 0}
				snapshot := syncedIndex{
					Version:    indexSyncSchemaV1,
					Origin:     fixture.target.origin,
					Revision:   fixture.target.revision,
					Model:      fixture.target.model,
					Dimensions: fixture.target.dimensions,
					Records:    []vectorRecord{conflict},
				}
				if err := writeJSONSync(filepath.Join(root, fixture.legacyRel), snapshot); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "malformed existing v2 manifest",
			want: "parse synced index v2",
			mutate: func(t *testing.T, root string, fixture mixedV2Fixture) {
				writeFile(t, root, fixture.fullRel, "{")
			},
		},
		{
			name: "malformed unreferenced v2 pack",
			want: "vector pack",
			mutate: func(t *testing.T, root string, _ mixedV2Fixture) {
				path := filepath.Join("packs", strings.Repeat("a", 64), strings.Repeat("b", 64)+".pack")
				writeFile(t, root, path, "not a vector pack")
			},
		},
		{
			name: "unrelated path collision",
			want: "unsafe path",
			mutate: func(t *testing.T, root string, _ mixedV2Fixture) {
				writeFile(t, root, "README.md", "not index data\n")
			},
		},
		{
			name: "future schema",
			want: "unsupported index sync schema version 3",
			mutate: func(t *testing.T, root string, _ mixedV2Fixture) {
				writeFile(t, root, "schema.json", "{\"version\":3}\n")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMixedV2Remote(t, false)
			mutateRemoteTree(t, fixture.remote, "inject invalid mixed-v2 state", func(root string) {
				test.mutate(t, root, fixture)
			})
			before := remoteHead(t, fixture.remote)
			_, err := MigrateIndex(t.Context(), fixture.remote, IndexMigrationOptions{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("migration error = %v, want %q", err, test.want)
			}
			if after := remoteHead(t, fixture.remote); after != before {
				t.Fatalf("failed migration advanced remote from %s to %s", before, after)
			}
		})
	}
}

func TestIndexSyncStillRejectsLegacyV1ManifestInV2Repository(t *testing.T) {
	fixture := newMixedV2Remote(t, false)
	t.Setenv("HOME", t.TempDir())
	sync, err := openIndexSync(t.Context(), fixture.remote, nil)
	if sync != nil {
		_ = sync.close()
	}
	if err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("normal index sync error = %v", err)
	}
}

func TestIndexMigrationMixedV2ProgressFailureDoesNotMutateRemote(t *testing.T) {
	fixture := newMixedV2Remote(t, true)
	before := remoteHead(t, fixture.remote)
	wantErr := errors.New("stop mixed-v2 repair")
	_, err := MigrateIndex(t.Context(), fixture.remote, IndexMigrationOptions{
		ProgressLog: func(update Progress) error {
			if update.Status == ProgressStatusBuilding {
				return wantErr
			}
			return nil
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("migration error = %v, want %v", err, wantErr)
	}
	if after := remoteHead(t, fixture.remote); after != before {
		t.Fatalf("failed migration advanced remote from %s to %s", before, after)
	}
}

func TestIndexMigrationRecoversInterruptedV1InstallationBoundaries(t *testing.T) {
	for _, publishCandidate := range []bool{false, true} {
		name := "after schema switch"
		if publishCandidate {
			name = "after v2 publish"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := writeIndexSyncSchema(root, indexSyncSchemaV1); err != nil {
				t.Fatal(err)
			}
			target := indexSyncTarget{
				origin:     "https://example.test/acme/interrupted",
				revision:   strings.Repeat("7", 40),
				model:      "test-model",
				dimensions: 3,
			}
			record := testSyncedVectorRecord("interrupted", []float64{1, 0, 0})
			v1 := &indexSync{dir: root, schema: indexSyncSchemaV1}
			if _, err := v1.writeSnapshot(target, []vectorRecord{record}); err != nil {
				t.Fatal(err)
			}
			legacyPath, err := v1.snapshotPath(target)
			if err != nil {
				t.Fatal(err)
			}

			candidate := t.TempDir()
			if err := writeIndexSyncSchema(candidate, indexSyncSchemaV2); err != nil {
				t.Fatal(err)
			}
			v2 := &indexSync{dir: candidate, schema: indexSyncSchemaV2}
			if _, err := v2.writeSnapshotV2(target, []vectorRecord{record}); err != nil {
				t.Fatal(err)
			}
			if err := writeIndexSyncSchema(root, indexSyncSchemaV2); err != nil {
				t.Fatal(err)
			}
			if publishCandidate {
				if err := publishV2CandidateFiles(root, candidate); err != nil {
					t.Fatal(err)
				}
			}

			repair := newIndexMigrationRepair(t.Context(), false, nil)
			interrupted := &indexSync{dir: root, schema: indexSyncSchemaV2, migrationRepair: repair}
			if err := interrupted.ensureSchema(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
				t.Fatalf("legacy manifest remains after restart recovery: %v", err)
			}
			if err := interrupted.validateV2TreeContents(); err != nil {
				t.Fatalf("recovered tree is invalid: %v", err)
			}
		})
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

type mixedV2Fixture struct {
	remote    string
	target    indexSyncTarget
	record    vectorRecord
	legacyRel string
	fullRel   string
}

func newMixedV2Remote(t *testing.T, removeV2Manifest bool) mixedV2Fixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	remote := newEmptySyncRemote(t)
	sync, err := openIndexSync(t.Context(), remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := indexSyncTarget{
		origin:     "https://example.test/acme/mixed-schema",
		revision:   strings.Repeat("9", 40),
		model:      "test-model",
		dimensions: 3,
	}
	record := testSyncedVectorRecord("mixed-schema", []float64{1, 0, 0})
	if _, err := sync.writeSnapshot(target, []vectorRecord{record}); err != nil {
		t.Fatal(err)
	}
	legacyPath, err := sync.snapshotPath(target)
	if err != nil {
		t.Fatal(err)
	}
	legacyData, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	legacyRel, err := filepath.Rel(sync.dir, legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := sync.commitPending("seed mixed-schema migration test"); err != nil {
		t.Fatal(err)
	}
	if err := sync.push(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := sync.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := MigrateIndex(t.Context(), remote, IndexMigrationOptions{}); err != nil {
		t.Fatal(err)
	}

	cloneDir := filepath.Join(t.TempDir(), "mixed")
	repo, err := git.PlainClone(cloneDir, &git.CloneOptions{URL: remote})
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	legacyClonePath := filepath.Join(cloneDir, legacyRel)
	if err := os.MkdirAll(filepath.Dir(legacyClonePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyClonePath, legacyData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add(legacyRel); err != nil {
		t.Fatal(err)
	}
	modelKey := digestHex(syncModelKey(target.model, target.dimensions))
	fullRel := filepath.Join("indexes", metadata.IdentitySHA(target.origin), target.revision, modelKey+".json")
	if removeV2Manifest {
		if _, err := worktree.Remove(fullRel); err != nil {
			t.Fatal(err)
		}
	}
	signature := &object.Signature{Name: "Search Test", Email: "search@example.test", When: time.Unix(1, 0)}
	if _, err := worktree.Commit("inject legacy v1 manifest into schema v2", &git.CommitOptions{Author: signature, Committer: signature}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	return mixedV2Fixture{remote: remote, target: target, record: record, legacyRel: legacyRel, fullRel: fullRel}
}

func remoteHead(t *testing.T, remote string) plumbing.Hash {
	t.Helper()
	repo, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	return head.Hash()
}

func assertRemoteHasNoLegacyV1Manifests(t *testing.T, remote string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "strict-v2")
	if _, err := git.PlainClone(root, &git.CloneOptions{URL: remote}); err != nil {
		t.Fatal(err)
	}
	legacy, err := legacyV1ManifestPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 0 {
		t.Fatalf("remote retains legacy v1 manifests: %q", legacy)
	}
	if err := validateSyncTreeForSchema(root, indexSyncSchemaV2); err != nil {
		t.Fatalf("remote is not strict schema v2: %v", err)
	}
}

func mutateRemoteTree(t *testing.T, remote, message string, mutate func(root string)) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "remote-mutation")
	repo, err := git.PlainClone(root, &git.CloneOptions{URL: remote})
	if err != nil {
		t.Fatal(err)
	}
	mutate(root)
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	signature := &object.Signature{Name: "Search Test", Email: "search@example.test", When: time.Unix(2, 0)}
	if _, err := worktree.Commit(message, &git.CommitOptions{Author: signature, Committer: signature}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&git.PushOptions{}); err != nil {
		t.Fatal(err)
	}
}
