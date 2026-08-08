package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yusing/git-agent/internal/metadata"
)

const (
	ProgressStatusGCScanningLocal   = "gc_scanning_local"
	ProgressStatusGCCompactingLocal = "gc_compacting_local"

	localGCAfterPayload  = "after_payload"
	localGCAfterRecovery = "after_recovery_catalog"
	localGCAfterCurrent  = "after_current_catalog"
)

type LocalGCSummary struct {
	Stores         int
	Compacted      int
	Vectors        int
	RemovedVectors int
	CurrentBytes   int64
	ProjectedBytes int64
}

type LocalGCOptions struct {
	DryRun      bool
	ProgressLog func(Progress) error

	metadataRoot    string
	afterPublish    func(string) error
	onLifecycleWait func() error
}

type localGCRootPlan struct {
	storePlan *localGCStorePlan
	indexDirs []string
}

type localGCStorePlan struct {
	store             vectorStore
	currentPayload    string
	liveSourceEntries map[string]vectorStoreEntry
	projectedEntries  map[string]vectorStoreEntry
	sortedKeys        []string
	firstGeneration   uint64
	secondGeneration  uint64
	firstCatalog      vectorStoreCatalog
	secondCatalog     vectorStoreCatalog
	currentBytes      int64
	projectedBytes    int64
	removedVectors    int
	changed           bool
}

type localGCStoreState struct {
	current            vectorStoreCatalog
	currentPayload     string
	catalogs           []vectorStoreCatalog
	recognizedPayloads []string
	payloadSizes       map[string]int64
	currentBytes       int64
	maxGeneration      uint64
}

func GCAllLocal(ctx context.Context, opts LocalGCOptions) (LocalGCSummary, error) {
	root := opts.metadataRoot
	if root == "" {
		var err error
		root, err = metadata.Root()
		if err != nil {
			return LocalGCSummary{}, err
		}
	}
	return gcAllLocalAt(ctx, root, opts)
}

func gcAllLocalAt(ctx context.Context, metadataRoot string, opts LocalGCOptions) (LocalGCSummary, error) {
	if err := reportProgress(opts.ProgressLog, Progress{Status: ProgressStatusGCScanningLocal}); err != nil {
		return LocalGCSummary{}, err
	}
	roots, err := discoverLocalGCRoots(metadataRoot)
	if err != nil {
		return LocalGCSummary{}, err
	}

	preflight := make([]localGCRootPlan, 0, len(roots))
	for _, root := range roots {
		plan, err := planLocalGCRoot(ctx, root, false, nil, opts.onLifecycleWait)
		if err != nil {
			return LocalGCSummary{}, err
		}
		preflight = append(preflight, plan)
	}

	var summary LocalGCSummary
	totalStores := 0
	for _, plan := range preflight {
		if plan.storePlan != nil {
			totalStores++
		}
	}
	if err := reportProgress(opts.ProgressLog, Progress{
		Status: ProgressStatusGCCompactingLocal,
		Total:  totalStores,
	}); err != nil {
		return LocalGCSummary{}, err
	}

	started := time.Now()
	done := 0
	activePlans := preflight
	if !opts.DryRun {
		activePlans = activePlans[:0]
		for _, root := range roots {
			plan, err := planLocalGCRoot(ctx, root, true, opts.afterPublish, opts.onLifecycleWait)
			if err != nil {
				return LocalGCSummary{}, err
			}
			if plan.storePlan != nil {
				done++
				if err := reportProgress(opts.ProgressLog, Progress{
					Status:  ProgressStatusGCCompactingLocal,
					Done:    done,
					Total:   totalStores,
					Elapsed: time.Since(started),
				}); err != nil {
					return LocalGCSummary{}, err
				}
			}
			activePlans = append(activePlans, plan)
		}
	}

	for _, plan := range activePlans {
		if plan.storePlan != nil {
			summary.Stores++
			summary.Vectors += len(plan.storePlan.projectedEntries)
			summary.RemovedVectors += plan.storePlan.removedVectors
			summary.CurrentBytes += plan.storePlan.currentBytes
			summary.ProjectedBytes += plan.storePlan.projectedBytes
			if plan.storePlan.changed {
				summary.Compacted++
			}
			if opts.DryRun {
				done++
				if err := reportProgress(opts.ProgressLog, Progress{
					Status:  ProgressStatusGCCompactingLocal,
					Done:    done,
					Total:   totalStores,
					Elapsed: time.Since(started),
				}); err != nil {
					return LocalGCSummary{}, err
				}
			}
		}
		for _, indexDir := range plan.indexDirs {
			currentBytes, err := cleanLocalGCIndex(ctx, indexDir, opts.DryRun)
			if err != nil {
				return LocalGCSummary{}, err
			}
			summary.CurrentBytes += currentBytes
		}
	}
	return summary, nil
}

func discoverLocalGCRoots(metadataRoot string) ([]string, error) {
	entries, err := os.ReadDir(metadataRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, entry := range entries {
		path := filepath.Join(metadataRoot, entry.Name())
		if entry.Name() == "remotes" {
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				return nil, fmt.Errorf("local GC metadata path %s is not a regular directory", path)
			}
			remoteEntries, err := os.ReadDir(path)
			if err != nil {
				return nil, err
			}
			for _, remote := range remoteEntries {
				if !canonicalLowerHex(remote.Name(), 64) {
					continue
				}
				remotePath := filepath.Join(path, remote.Name())
				if remote.Type()&os.ModeSymlink != 0 || !remote.IsDir() {
					return nil, fmt.Errorf("local GC metadata root %s is not a regular directory", remotePath)
				}
				roots = append(roots, remotePath)
			}
			continue
		}
		if !canonicalLowerHex(entry.Name(), 64) {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return nil, fmt.Errorf("local GC metadata root %s is not a regular directory", path)
		}
		roots = append(roots, path)
	}
	slices.Sort(roots)
	return roots, nil
}

func planLocalGCRoot(
	ctx context.Context,
	root string,
	publish bool,
	afterPublish func(string) error,
	onLifecycleWait func() error,
) (plan localGCRootPlan, err error) {
	storeExists, _, err := inspectLocalGCRoot(root)
	if err != nil {
		return plan, err
	}
	store := newVectorStore(root)
	var lifecycle *indexLock
	if storeExists {
		lifecycle, err = lockIndex(ctx, store.dir, onLifecycleWait)
		if err != nil {
			return plan, err
		}
		defer func() { err = errors.Join(err, lifecycle.Unlock()) }()
	}

	storeExists, indexDirs, err := inspectLocalGCRoot(root)
	if err != nil {
		return plan, err
	}
	if storeExists && lifecycle == nil {
		return planLocalGCRoot(ctx, root, publish, afterPublish, onLifecycleWait)
	}
	plan = localGCRootPlan{indexDirs: indexDirs}
	if storeExists {
		storePlan, err := buildLocalGCStorePlan(ctx, root, indexDirs)
		if err != nil {
			return plan, err
		}
		plan.storePlan = storePlan
		if publish && storePlan.changed {
			if err := publishLocalGCStore(storePlan, afterPublish); err != nil {
				return plan, err
			}
		}
	} else {
		if _, err := validateLocalGCIndexes(ctx, root, indexDirs, nil); err != nil {
			return plan, err
		}
	}
	return plan, nil
}

func inspectLocalGCRoot(root string) (bool, []string, error) {
	searchRoot := filepath.Join(root, "search")
	info, err := os.Lstat(searchRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil, fmt.Errorf("local GC search path %s is not a regular directory", searchRoot)
	}

	storePath := filepath.Join(searchRoot, vectorStoreDirName)
	storeExists := false
	indexSet := map[string]bool{}
	err = filepath.WalkDir(searchRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("local GC path %s escapes metadata root %s", path, root)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("local GC path %s is a symlink", path)
		}
		if path == storePath {
			if !entry.IsDir() {
				return fmt.Errorf("local GC vector store %s is not a directory", path)
			}
			storeExists = true
			return fs.SkipDir
		}
		if entry.IsDir() {
			if path != searchRoot &&
				(entry.Name() == ".git" || entry.Name() == "repo.git" || entry.Name() == "query-locks") {
				return fs.SkipDir
			}
			if recognizedLocalGCIndexPayload(entry.Name()) {
				return fmt.Errorf("local GC owned payload %s is not a regular file", path)
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("local GC path %s is not a regular file", path)
		}
		if recognizedLocalGCIndexPayload(entry.Name()) {
			indexSet[filepath.Dir(path)] = true
		}
		return nil
	})
	if err != nil {
		return false, nil, err
	}
	return storeExists, slices.Sorted(maps.Keys(indexSet)), nil
}

func recognizedLocalGCIndexPayload(name string) bool {
	switch name {
	case "manifest.json", "vectors.index.json", "vectors.f32", "embeddings.json":
		return true
	default:
		return false
	}
}

func buildLocalGCStorePlan(ctx context.Context, root string, indexDirs []string) (*localGCStorePlan, error) {
	store := newVectorStore(root)
	state, err := loadLocalGCStoreState(store)
	if err != nil {
		return nil, err
	}
	live, err := validateLocalGCIndexes(ctx, root, indexDirs, &state)
	if err != nil {
		return nil, err
	}

	keys := slices.Sorted(maps.Keys(live))
	projectedEntries := make(map[string]vectorStoreEntry, len(keys))
	maxPayloadBytes := int64(^uint64(0) >> 1)
	var payloadBytes int64
	for _, key := range keys {
		source := live[key]
		vectorBytes := int64(source.Dimensions) * 4
		if vectorBytes < 0 || payloadBytes > maxPayloadBytes-vectorBytes {
			return nil, errors.New("shared vector payload size overflows")
		}
		projectedEntries[key] = vectorStoreEntry{
			Offset:     payloadBytes,
			Dimensions: source.Dimensions,
			Checksum:   source.Checksum,
		}
		payloadBytes += vectorBytes
	}
	removedVectors := 0
	for key := range state.current.Entries {
		if _, ok := live[key]; !ok {
			removedVectors++
		}
	}

	plan := &localGCStorePlan{
		store:             store,
		currentPayload:    state.currentPayload,
		liveSourceEntries: live,
		projectedEntries:  projectedEntries,
		sortedKeys:        keys,
		currentBytes:      state.currentBytes,
		removedVectors:    removedVectors,
	}
	if localGCStoreAlreadyCompact(state, projectedEntries, payloadBytes) {
		plan.projectedBytes = state.currentBytes
		return plan, nil
	}
	if len(keys) == 0 && state.currentBytes == 0 {
		return plan, nil
	}
	if state.maxGeneration > ^uint64(0)-2 {
		return nil, errors.New("shared vector catalog generation overflows")
	}
	plan.firstGeneration = state.maxGeneration + 1
	plan.secondGeneration = state.maxGeneration + 2
	payloadName := vectorStoreGenerationPayloadName(plan.firstGeneration)
	plan.firstCatalog = vectorStoreCatalog{
		Version:    vectorStoreCatalogVersion,
		Generation: plan.firstGeneration,
		Payload:    payloadName,
		Entries:    maps.Clone(projectedEntries),
	}
	plan.secondCatalog = vectorStoreCatalog{
		Version:    vectorStoreCatalogVersion,
		Generation: plan.secondGeneration,
		Payload:    payloadName,
		Entries:    maps.Clone(projectedEntries),
	}
	firstData, err := marshalVectorStoreCatalog(plan.firstCatalog)
	if err != nil {
		return nil, err
	}
	secondData, err := marshalVectorStoreCatalog(plan.secondCatalog)
	if err != nil {
		return nil, err
	}
	plan.projectedBytes = payloadBytes + int64(len(firstData)+len(secondData))
	plan.changed = true
	return plan, nil
}

func loadLocalGCStoreState(store vectorStore) (localGCStoreState, error) {
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		return localGCStoreState{}, err
	}
	state := localGCStoreState{
		payloadSizes: map[string]int64{},
	}
	for _, entry := range entries {
		path := filepath.Join(store.dir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			return state, fmt.Errorf("local GC vector-store path %s is a symlink", path)
		}
		if entry.IsDir() {
			continue
		}
		if !entry.Type().IsRegular() {
			return state, fmt.Errorf("local GC vector-store path %s is not a regular file", path)
		}
		info, err := entry.Info()
		if err != nil {
			return state, err
		}
		name := entry.Name()
		if generation, ok := vectorStoreCatalogGeneration(name); ok {
			state.maxGeneration = max(state.maxGeneration, generation)
			state.currentBytes += info.Size()
			data, err := os.ReadFile(path)
			if err != nil {
				return state, err
			}
			var catalog vectorStoreCatalog
			if err := decodeStrictJSON(data, &catalog); err != nil {
				return state, fmt.Errorf("validate local GC vector-store catalog %s: %w", path, err)
			}
			if err := validateLocalGCCatalog(store, catalog, generation); err != nil {
				return state, fmt.Errorf("validate local GC vector-store catalog %s: %w", path, err)
			}
			state.catalogs = append(state.catalogs, catalog)
			continue
		}
		if malformedVectorStoreGenerationName(name) {
			return state, fmt.Errorf("local GC vector-store generation name %q is malformed", name)
		}
		if recognizedVectorStorePayloadName(name) {
			if generation, ok := vectorStorePayloadGeneration(name); ok {
				state.maxGeneration = max(state.maxGeneration, generation)
			}
			state.recognizedPayloads = append(state.recognizedPayloads, name)
			state.currentBytes += info.Size()
			state.payloadSizes[name] = info.Size()
		}
	}
	slices.SortFunc(state.catalogs, func(a, b vectorStoreCatalog) int {
		switch {
		case a.Generation > b.Generation:
			return -1
		case a.Generation < b.Generation:
			return 1
		default:
			return 0
		}
	})
	if len(state.catalogs) > 0 {
		state.current = state.catalogs[0]
		state.currentPayload, err = vectorStoreCatalogPayload(state.current)
		if err != nil {
			return state, err
		}
	} else {
		state.current = vectorStoreCatalog{
			Version: vectorStoreCatalogVersion,
			Entries: map[string]vectorStoreEntry{},
		}
		state.currentPayload = vectorStorePayloadName
	}
	return state, nil
}

func validateLocalGCCatalog(store vectorStore, catalog vectorStoreCatalog, generation uint64) (err error) {
	if catalog.Version != vectorStoreCatalogVersion {
		return fmt.Errorf("version = %d, want %d", catalog.Version, vectorStoreCatalogVersion)
	}
	if catalog.Generation != generation {
		return fmt.Errorf("generation = %d, want %d", catalog.Generation, generation)
	}
	if catalog.Entries == nil {
		return errors.New("entries are missing")
	}
	payloadName, err := vectorStoreCatalogPayload(catalog)
	if err != nil {
		return err
	}
	payloadPath := filepath.Join(store.dir, payloadName)
	info, err := os.Lstat(payloadPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("payload %s is not a regular file", payloadPath)
	}
	payload, err := os.Open(payloadPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, payload.Close()) }()
	for key, entry := range catalog.Entries {
		if !canonicalLowerHex(key, 64) {
			return fmt.Errorf("vector key %q is malformed", key)
		}
		if _, err := readStoredVectorData(payload, entry); err != nil {
			return fmt.Errorf("vector key %s: %w", key, err)
		}
	}
	return nil
}

func malformedVectorStoreGenerationName(name string) bool {
	if strings.HasPrefix(name, "catalog-") && strings.HasSuffix(name, ".json") {
		_, ok := vectorStoreCatalogGeneration(name)
		return !ok
	}
	if strings.HasPrefix(name, "vectors-") && strings.HasSuffix(name, ".f32") {
		_, ok := vectorStorePayloadGeneration(name)
		return !ok
	}
	return false
}

func validateLocalGCIndexes(
	ctx context.Context,
	root string,
	indexDirs []string,
	state *localGCStoreState,
) (map[string]vectorStoreEntry, error) {
	live := map[string]vectorStoreEntry{}
	var sharedPayload *os.File
	var sharedPayloadErr error
	if state != nil && len(state.current.Entries) > 0 {
		sharedPayload, sharedPayloadErr = os.Open(filepath.Join(newVectorStore(root).dir, state.currentPayload))
		if sharedPayloadErr != nil {
			return nil, sharedPayloadErr
		}
		defer sharedPayload.Close()
	}
	for _, indexDir := range indexDirs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		found, err := loadManifest(indexDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("validate local index %s: %w", indexDir, err)
		}
		if found.Version == legacyIndexVersion {
			if _, err := loadVectorsContext(ctx, indexDir); err != nil {
				return nil, fmt.Errorf("validate local index %s: %w", indexDir, err)
			}
			continue
		}
		records, err := loadVectorIndexRecords(indexDir)
		if err != nil {
			return nil, fmt.Errorf("validate local index %s: %w", indexDir, err)
		}
		var localPayload *os.File
		for i, record := range records {
			if record.EmbeddingModel != found.EmbeddingModel || record.Dimensions != found.Dimensions {
				if localPayload != nil {
					_ = localPayload.Close()
				}
				return nil, fmt.Errorf("validate local index %s: vectors.index.json entry %d model or dimensions mismatch", indexDir, i)
			}
			if record.VectorKey == "" {
				if localPayload == nil {
					localPayload, err = os.Open(filepath.Join(indexDir, "vectors.f32"))
					if err != nil {
						return nil, fmt.Errorf("validate local index %s: %w", indexDir, err)
					}
				}
				if _, err := readStoredVectorData(localPayload, vectorStoreEntry{
					Offset:     record.Offset,
					Dimensions: record.Dimensions,
					Checksum:   record.VectorChecksum,
				}); err != nil {
					_ = localPayload.Close()
					return nil, fmt.Errorf("validate local index %s: vectors.index.json entry %d: %w", indexDir, i, err)
				}
				continue
			}
			if state == nil || record.EmbeddingInputHash == "" {
				if localPayload != nil {
					_ = localPayload.Close()
				}
				return nil, fmt.Errorf("validate local index %s: vectors.index.json entry %d has no shared vector store", indexDir, i)
			}
			expectedKey := vectorStoreKey(record.EmbeddingInputHash, record.EmbeddingModel, record.Dimensions)
			if record.VectorKey != expectedKey {
				if localPayload != nil {
					_ = localPayload.Close()
				}
				return nil, fmt.Errorf("validate local index %s: vectors.index.json entry %d has invalid shared vector key", indexDir, i)
			}
			entry, ok := state.current.Entries[record.VectorKey]
			if !ok || entry.Dimensions != record.Dimensions || entry.Checksum != record.VectorChecksum {
				if localPayload != nil {
					_ = localPayload.Close()
				}
				return nil, fmt.Errorf("validate local index %s: vectors.index.json entry %d does not match the shared catalog", indexDir, i)
			}
			if _, seen := live[record.VectorKey]; !seen {
				if sharedPayload == nil {
					if localPayload != nil {
						_ = localPayload.Close()
					}
					if sharedPayloadErr != nil {
						return nil, sharedPayloadErr
					}
					return nil, fmt.Errorf("validate local index %s: shared vector payload is missing", indexDir)
				}
				if _, err := readStoredVectorData(sharedPayload, entry); err != nil {
					if localPayload != nil {
						_ = localPayload.Close()
					}
					return nil, fmt.Errorf("validate local index %s: vectors.index.json entry %d: %w", indexDir, i, err)
				}
				live[record.VectorKey] = entry
			}
		}
		if localPayload != nil {
			if err := localPayload.Close(); err != nil {
				return nil, err
			}
		}
	}
	return live, nil
}

func localGCStoreAlreadyCompact(
	state localGCStoreState,
	projected map[string]vectorStoreEntry,
	payloadBytes int64,
) bool {
	if len(state.catalogs) != 2 || len(state.recognizedPayloads) != 1 {
		return false
	}
	first := state.catalogs[0]
	second := state.catalogs[1]
	firstPayload, err := vectorStoreCatalogPayload(first)
	if err != nil {
		return false
	}
	secondPayload, err := vectorStoreCatalogPayload(second)
	if err != nil || firstPayload != secondPayload || firstPayload != state.recognizedPayloads[0] {
		return false
	}
	if !maps.Equal(first.Entries, projected) || !maps.Equal(second.Entries, projected) {
		return false
	}
	return state.payloadSizes[firstPayload] == payloadBytes
}

func publishLocalGCStore(plan *localGCStorePlan, afterPublish func(string) error) error {
	payloadName := vectorStoreGenerationPayloadName(plan.firstGeneration)
	source, err := os.Open(filepath.Join(plan.store.dir, plan.currentPayload))
	if err != nil && len(plan.sortedKeys) > 0 {
		return err
	}
	if source != nil {
		defer source.Close()
	}
	if err := publishLocalGCPayload(plan, source, payloadName); err != nil {
		return err
	}
	if afterPublish != nil {
		if err := afterPublish(localGCAfterPayload); err != nil {
			return err
		}
	}
	if err := plan.store.publishCatalog(plan.firstCatalog); err != nil {
		return err
	}
	if afterPublish != nil {
		if err := afterPublish(localGCAfterRecovery); err != nil {
			return err
		}
	}
	if err := plan.store.publishCatalog(plan.secondCatalog); err != nil {
		return err
	}
	if afterPublish != nil {
		if err := afterPublish(localGCAfterCurrent); err != nil {
			return err
		}
	}
	removed, err := plan.store.removeOldGenerations(plan.firstGeneration, plan.secondGeneration)
	if err != nil {
		return err
	}
	if removed {
		return syncDirectory(plan.store.dir)
	}
	return nil
}

func publishLocalGCPayload(plan *localGCStorePlan, source io.ReaderAt, payloadName string) error {
	temporary, err := os.CreateTemp(plan.store.dir, ".vectors-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	for _, key := range plan.sortedKeys {
		data, err := readStoredVectorData(source, plan.liveSourceEntries[key])
		if err != nil {
			_ = temporary.Close()
			return err
		}
		if _, err := temporary.Write(data); err != nil {
			_ = temporary.Close()
			return err
		}
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, filepath.Join(plan.store.dir, payloadName)); err != nil {
		return err
	}
	return syncDirectory(plan.store.dir)
}

func cleanLocalGCIndex(ctx context.Context, indexDir string, dryRun bool) (currentBytes int64, err error) {
	lock, err := lockIndex(ctx, indexDir)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	entries, err := os.ReadDir(indexDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	byName := map[string]fs.DirEntry{}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return 0, fmt.Errorf("local GC index path %s is a symlink", filepath.Join(indexDir, entry.Name()))
		}
		if recognizedLocalGCIndexPayload(entry.Name()) && !entry.Type().IsRegular() {
			return 0, fmt.Errorf("local GC index payload %s is not a regular file", filepath.Join(indexDir, entry.Name()))
		}
		byName[entry.Name()] = entry
	}
	found, manifestErr := loadManifest(indexDir)
	incomplete := errors.Is(manifestErr, fs.ErrNotExist)
	if manifestErr != nil && !incomplete {
		return 0, manifestErr
	}
	removeNames := map[string]bool{}
	if incomplete {
		for name := range byName {
			if recognizedLocalGCIndexPayload(name) {
				removeNames[name] = true
			}
		}
	} else if found.Version == indexVersion {
		records, err := loadVectorIndexRecords(indexDir)
		if err != nil {
			return 0, err
		}
		allShared := true
		for _, record := range records {
			if record.VectorKey == "" {
				allShared = false
				break
			}
		}
		if allShared {
			if _, err := loadVectorsContext(ctx, indexDir); err != nil {
				return 0, err
			}
			removeNames["embeddings.json"] = true
			removeNames["vectors.f32"] = true
		}
	}
	removed := false
	for name := range removeNames {
		entry := byName[name]
		if entry == nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return currentBytes, err
		}
		currentBytes += info.Size()
		if !dryRun {
			if err := os.Remove(filepath.Join(indexDir, name)); err != nil {
				return currentBytes, err
			}
			removed = true
		}
	}
	if !dryRun && removed {
		if err := syncDirectory(indexDir); err != nil {
			return currentBytes, err
		}
	}
	if !dryRun && incomplete {
		if err := os.Remove(indexDir); err == nil {
			if err := syncDirectory(filepath.Dir(indexDir)); err != nil {
				return currentBytes, err
			}
		} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrExist) {
			return currentBytes, err
		}
	}
	return currentBytes, nil
}
