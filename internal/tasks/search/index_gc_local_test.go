package search

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLocalGCDryRunCompactsAndIsIdempotent(t *testing.T) {
	metadataRoot := t.TempDir()
	fixture := newLocalGCFixture(t, metadataRoot, 'a')
	before := snapshotLocalGCTree(t, metadataRoot)

	drySummary, err := GCAllLocal(t.Context(), LocalGCOptions{
		DryRun:       true,
		metadataRoot: metadataRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	afterDryRun := snapshotLocalGCTree(t, metadataRoot)
	if !reflect.DeepEqual(afterDryRun, before) {
		t.Fatal("dry-run changed local metadata")
	}

	summary, err := GCAllLocal(t.Context(), LocalGCOptions{metadataRoot: metadataRoot})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary, drySummary) {
		t.Fatalf("normal summary = %#v, dry-run summary = %#v", summary, drySummary)
	}
	if summary.Stores != 1 || summary.Compacted != 1 || summary.Vectors != 2 || summary.RemovedVectors != 1 {
		t.Fatalf("summary = %#v", summary)
	}

	loaded, err := loadVectors(fixture.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalGCVectors(t, loaded, fixture.live)
	index, err := loadVectorIndexRecords(fixture.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	store := newVectorStore(fixture.root)
	catalog, _, err := store.loadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	remapped := false
	for _, record := range index {
		if catalog.Entries[record.VectorKey].Offset != record.Offset {
			remapped = true
			break
		}
	}
	if catalog.Payload == "" || !remapped {
		t.Fatalf("compacted catalog did not remap stale offsets: %#v", catalog)
	}

	var catalogs, payloads int
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, ok := vectorStoreCatalogGeneration(entry.Name()); ok {
			catalogs++
		}
		if recognizedVectorStorePayloadName(entry.Name()) {
			payloads++
		}
	}
	if catalogs != 2 || payloads != 1 {
		t.Fatalf("retained store files: catalogs=%d payloads=%d", catalogs, payloads)
	}
	for _, path := range []string{
		filepath.Join(store.dir, "keep.txt"),
		filepath.Join(fixture.incompleteDir, "keep.txt"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unknown file %s was not preserved: %v", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(fixture.indexDir, "embeddings.json"),
		filepath.Join(fixture.indexDir, "vectors.f32"),
		filepath.Join(fixture.incompleteDir, "vectors.index.json"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("recognized stale payload %s remains: %v", path, err)
		}
	}

	repeated, err := GCAllLocal(t.Context(), LocalGCOptions{metadataRoot: metadataRoot})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Stores != 1 || repeated.Compacted != 0 || repeated.Vectors != 2 ||
		repeated.RemovedVectors != 0 || repeated.CurrentBytes != repeated.ProjectedBytes {
		t.Fatalf("repeated summary = %#v", repeated)
	}
}

func TestLocalGCInterruptedPublicationKeepsReadableGeneration(t *testing.T) {
	for _, stage := range []string{
		localGCAfterPayload,
		localGCAfterRecovery,
		localGCAfterCurrent,
	} {
		t.Run(stage, func(t *testing.T) {
			metadataRoot := t.TempDir()
			fixture := newLocalGCFixture(t, metadataRoot, 'a')
			interrupted := errors.New("interrupt publication")
			_, err := GCAllLocal(t.Context(), LocalGCOptions{
				metadataRoot: metadataRoot,
				afterPublish: func(found string) error {
					if found == stage {
						return interrupted
					}
					return nil
				},
			})
			if !errors.Is(err, interrupted) {
				t.Fatalf("GC error = %v, want interruption", err)
			}
			loaded, err := loadVectors(fixture.indexDir)
			if err != nil {
				t.Fatalf("load after interruption: %v", err)
			}
			assertLocalGCVectors(t, loaded, fixture.live)
			if _, err := GCAllLocal(t.Context(), LocalGCOptions{metadataRoot: metadataRoot}); err != nil {
				t.Fatalf("resume GC: %v", err)
			}
			loaded, err = loadVectors(fixture.indexDir)
			if err != nil {
				t.Fatalf("load after resume: %v", err)
			}
			assertLocalGCVectors(t, loaded, fixture.live)
		})
	}
}

func TestLocalGCPreflightsEveryRootBeforePublishing(t *testing.T) {
	metadataRoot := t.TempDir()
	fixture := newLocalGCFixture(t, metadataRoot, 'a')
	badRoot := filepath.Join(metadataRoot, strings.Repeat("b", 64))
	badDir := filepath.Join(badRoot, "search", "revs", "bad")
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "manifest.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := GCAllLocal(t.Context(), LocalGCOptions{metadataRoot: metadataRoot}); err == nil {
		t.Fatal("GC accepted malformed completed metadata")
	}
	if _, err := os.Stat(sharedVectorPayloadPath(fixture.root)); err != nil {
		t.Fatalf("first store published before preflight failed: %v", err)
	}
	generated, err := filepath.Glob(filepath.Join(newVectorStore(fixture.root).dir, "vectors-*.f32"))
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) != 0 {
		t.Fatalf("first store published generation payloads before preflight failed: %v", generated)
	}
}

func TestLocalGCSerializesWithConcurrentWriter(t *testing.T) {
	metadataRoot := t.TempDir()
	fixture := newLocalGCFixture(t, metadataRoot, 'a')
	paused := make(chan struct{})
	resume := make(chan struct{})
	gcDone := make(chan error, 1)
	go func() {
		_, err := GCAllLocal(context.Background(), LocalGCOptions{
			metadataRoot: metadataRoot,
			afterPublish: func(stage string) error {
				if stage == localGCAfterPayload {
					close(paused)
					<-resume
				}
				return nil
			},
		})
		gcDone <- err
	}()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not reach publication pause")
	}

	writerLocked := make(chan struct{})
	writerDone := make(chan error, 1)
	added := localGCTestRecord("live-c", "c.txt", []float64{0, 0, 1})
	go func() {
		lock, err := lockIndex(context.Background(), fixture.indexDir)
		if err != nil {
			writerDone <- err
			return
		}
		close(writerLocked)
		records := append(slices.Clone(fixture.live), added)
		err = saveIndex(
			context.Background(),
			fixture.root,
			fixture.indexDir,
			Source{Mode: "revision"},
			"",
			strings.Repeat("1", 40),
			"test-model",
			3,
			records,
			nil,
		)
		writerDone <- errors.Join(err, lock.Unlock())
	}()
	select {
	case <-writerLocked:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not acquire index lock")
	}
	close(resume)
	select {
	case err := <-gcDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GC deadlocked with writer")
	}
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer deadlocked with GC")
	}

	loaded, err := loadVectors(fixture.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalGCVectors(t, loaded, append(slices.Clone(fixture.live), added))
	if _, err := GCAllLocal(t.Context(), LocalGCOptions{metadataRoot: metadataRoot}); err != nil {
		t.Fatal(err)
	}
	loaded, err = loadVectors(fixture.indexDir)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalGCVectors(t, loaded, append(slices.Clone(fixture.live), added))
}

func TestLocalGCWaitsForActiveReader(t *testing.T) {
	metadataRoot := t.TempDir()
	fixture := newLocalGCFixture(t, metadataRoot, 'a')
	if _, err := GCAllLocal(t.Context(), LocalGCOptions{metadataRoot: metadataRoot}); err != nil {
		t.Fatal(err)
	}
	store := newVectorStore(fixture.root)
	if _, err := store.put(t.Context(), []vectorRecord{
		localGCTestRecord("orphan-after-gc", "orphan-after.txt", []float64{1, 1, 0}),
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sharedVectorPayloadPath(fixture.root)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("normal append recreated legacy payload: %v", err)
	}

	readerOpened := make(chan struct{})
	releaseReader := make(chan struct{})
	type readerResult struct {
		records []vectorRecord
		err     error
	}
	readerDone := make(chan readerResult, 1)
	go func() {
		records, err := loadSharedVectorsWithHook(
			context.Background(),
			fixture.root,
			fixture.indexDir,
			func() {
				close(readerOpened)
				<-releaseReader
			},
		)
		readerDone <- readerResult{records: records, err: err}
	}()
	select {
	case <-readerOpened:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not open shared payload")
	}

	gcWaiting := make(chan struct{})
	gcDone := make(chan error, 1)
	go func() {
		_, err := GCAllLocal(context.Background(), LocalGCOptions{
			metadataRoot: metadataRoot,
			onLifecycleWait: func() error {
				close(gcWaiting)
				return nil
			},
		})
		gcDone <- err
	}()
	select {
	case <-gcWaiting:
	case err := <-gcDone:
		t.Fatalf("GC completed before waiting for the lifecycle lock: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not wait for the lifecycle lock")
	}
	select {
	case err := <-gcDone:
		t.Fatalf("GC completed while reader held lifecycle lock: %v", err)
	default:
	}
	close(releaseReader)
	select {
	case result := <-readerDone:
		if result.err != nil {
			t.Fatal(result.err)
		}
		assertLocalGCVectors(t, result.records, fixture.live)
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not complete")
	}
	select {
	case err := <-gcDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not resume after reader")
	}
}

func TestLocalGCPreservesIndexLocalVectors(t *testing.T) {
	metadataRoot := t.TempDir()
	root := filepath.Join(metadataRoot, strings.Repeat("a", 64))
	indexDir := filepath.Join(root, "search", "fs", "local")
	records := []vectorRecord{
		localGCTestRecord("shared", "shared.txt", []float64{1, 0, 0}),
		{
			ChunkID:        "local",
			Path:           "local.txt",
			Source:         "filesystem",
			EmbeddingModel: "test-model",
			Dimensions:     3,
			Vector:         []float64{0, 1, 0},
		},
	}
	if err := withIndexLock(t.Context(), indexDir, func() error {
		return saveIndex(
			t.Context(),
			root,
			indexDir,
			Source{Mode: "filesystem"},
			"",
			"",
			"test-model",
			3,
			records,
			nil,
		)
	}); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(indexDir, "embeddings.json")
	if err := os.WriteFile(legacyPath, []byte("preserve\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := GCAllLocal(t.Context(), LocalGCOptions{metadataRoot: metadataRoot}); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadVectors(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	assertLocalGCVectors(t, loaded, records)
	for _, path := range []string{filepath.Join(indexDir, "vectors.f32"), legacyPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("index-local payload %s was removed: %v", path, err)
		}
	}
}

func TestDiscoverLocalGCRootsUsesExactLayout(t *testing.T) {
	metadataRoot := t.TempDir()
	direct := filepath.Join(metadataRoot, strings.Repeat("a", 64))
	remote := filepath.Join(metadataRoot, "remotes", strings.Repeat("b", 64))
	for _, path := range []string{
		direct,
		remote,
		filepath.Join(metadataRoot, "not-an-id"),
		filepath.Join(metadataRoot, "index-sync", strings.Repeat("c", 64)),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	roots, err := discoverLocalGCRoots(metadataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{direct, remote}; !reflect.DeepEqual(roots, want) {
		t.Fatalf("roots = %v, want %v", roots, want)
	}

	symlink := filepath.Join(metadataRoot, strings.Repeat("d", 64))
	if err := os.Symlink(direct, symlink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := discoverLocalGCRoots(metadataRoot); err == nil {
		t.Fatal("discovery accepted a symlink metadata root")
	}
}

func TestLocalGCRejectsInvalidRecognizedCatalog(t *testing.T) {
	metadataRoot := t.TempDir()
	fixture := newLocalGCFixture(t, metadataRoot, 'a')
	store := newVectorStore(fixture.root)
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	var catalogs []string
	for _, entry := range entries {
		if _, ok := vectorStoreCatalogGeneration(entry.Name()); ok {
			catalogs = append(catalogs, entry.Name())
		}
	}
	slices.Sort(catalogs)
	if len(catalogs) < 2 {
		t.Fatalf("recognized catalogs = %v, want at least two", catalogs)
	}
	if err := os.WriteFile(filepath.Join(store.dir, catalogs[0]), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotLocalGCTree(t, metadataRoot)
	if _, err := GCAllLocal(t.Context(), LocalGCOptions{metadataRoot: metadataRoot}); err == nil {
		t.Fatal("GC accepted an invalid recognized catalog")
	}
	after := snapshotLocalGCTree(t, metadataRoot)
	if !reflect.DeepEqual(after, before) {
		t.Fatal("GC published after invalid recognized catalog preflight")
	}
}

func TestLocalGCRejectsMalformedGenerationName(t *testing.T) {
	metadataRoot := t.TempDir()
	fixture := newLocalGCFixture(t, metadataRoot, 'a')
	path := filepath.Join(newVectorStore(fixture.root).dir, "catalog-1.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := GCAllLocal(t.Context(), LocalGCOptions{metadataRoot: metadataRoot}); err == nil {
		t.Fatal("GC accepted a malformed generation filename")
	}
}

type localGCTestFixture struct {
	root          string
	indexDir      string
	incompleteDir string
	live          []vectorRecord
}

func newLocalGCFixture(t *testing.T, metadataRoot string, id byte) localGCTestFixture {
	t.Helper()
	root := filepath.Join(metadataRoot, strings.Repeat(string(id), 64))
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store := newVectorStore(root)
	orphan := localGCTestRecord("orphan", "orphan.txt", []float64{1, 1, 1})
	if _, err := store.put(t.Context(), []vectorRecord{orphan}, nil); err != nil {
		t.Fatal(err)
	}
	live := []vectorRecord{
		localGCTestRecord("live-a", "a.txt", []float64{1, 0, 0}),
		localGCTestRecord("live-b", "b.txt", []float64{0, 1, 0}),
	}
	indexDir := filepath.Join(root, "search", "revs", strings.Repeat("1", 40))
	if err := withIndexLock(t.Context(), indexDir, func() error {
		return saveIndex(
			t.Context(),
			root,
			indexDir,
			Source{Mode: "revision"},
			"",
			strings.Repeat("1", 40),
			"test-model",
			3,
			live,
			nil,
		)
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(indexDir, "embeddings.json"), []byte("legacy payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, "keep.txt"), []byte("unknown\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	incompleteDir := filepath.Join(root, "search", "revs", "incomplete")
	if err := withIndexLock(t.Context(), incompleteDir, func() error {
		if err := os.MkdirAll(incompleteDir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(incompleteDir, "vectors.index.json"), []byte("incomplete\n"), 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(incompleteDir, "keep.txt"), []byte("unknown\n"), 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	return localGCTestFixture{
		root:          root,
		indexDir:      indexDir,
		incompleteDir: incompleteDir,
		live:          live,
	}
}

func localGCTestRecord(inputHash, path string, vector []float64) vectorRecord {
	return vectorRecord{
		ChunkID:            inputHash,
		Path:               path,
		Source:             "revision",
		EmbeddingInputHash: inputHash,
		EmbeddingModel:     "test-model",
		Dimensions:         len(vector),
		Vector:             vector,
	}
}

func assertLocalGCVectors(t *testing.T, got, want []vectorRecord) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("vectors length = %d, want %d", len(got), len(want))
	}
	byPath := map[string][]float64{}
	for _, record := range got {
		byPath[record.Path] = record.Vector
	}
	for _, record := range want {
		if !reflect.DeepEqual(byPath[record.Path], record.Vector) {
			t.Fatalf("vector for %s = %v, want %v", record.Path, byPath[record.Path], record.Vector)
		}
	}
}

type localGCTreeEntry struct {
	mode    fs.FileMode
	size    int64
	modTime int64
	sum     [32]byte
}

func snapshotLocalGCTree(t *testing.T, root string) map[string]localGCTreeEntry {
	t.Helper()
	snapshot := map[string]localGCTreeEntry{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		item := localGCTreeEntry{
			mode:    info.Mode(),
			size:    info.Size(),
			modTime: info.ModTime().UnixNano(),
		}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			item.sum = sha256.Sum256(data)
		}
		snapshot[relative] = item
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
