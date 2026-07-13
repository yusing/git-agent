package search

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"maps"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bytedance/sonic"
	"github.com/go-git/go-git/v6/plumbing/format/gitignore"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/openai"
	"golang.org/x/sync/errgroup"
)

const (
	DefaultMinRelatedness         = 0.70
	DefaultLimit                  = 20
	MaxLimit                      = 100
	MaxFileBytes                  = 1 << 20
	chunkLines                    = 80
	chunkOverlap                  = 20
	maxExcerptLines               = 40
	maxExcerptBytes               = 12 << 10
	indexVersion                  = 3
	legacyIndexVersion            = 2
	DefaultEmbeddingBatchInputs   = 32
	DefaultEmbeddingBatchMaxChars = 700_000
	DefaultEmbeddingMaxInputChars = 32_000
	maxEmbeddingLineChars         = 4_000
	ProgressStatusFetching        = "fetching"
)

var searchIgnoreFileNames = map[string]bool{
	".gitignore":      true,
	".gitagentignore": true,
}

var searchIgnoreFileOrder = []string{".gitignore", ".gitagentignore"}

var defaultSearchIgnorePatterns = []gitignore.Pattern{
	gitignore.ParsePattern("*.lock", nil),
	gitignore.ParsePattern("*.lockfile", nil),
	gitignore.ParsePattern("bun.lock", nil),
	gitignore.ParsePattern("bun.lockb", nil),
	gitignore.ParsePattern("Cartfile.resolved", nil),
	gitignore.ParsePattern("cabal.project.freeze", nil),
	gitignore.ParsePattern("Cargo.lock", nil),
	gitignore.ParsePattern("composer.lock", nil),
	gitignore.ParsePattern("conda-lock.yaml", nil),
	gitignore.ParsePattern("conda-lock.yml", nil),
	gitignore.ParsePattern("cpanfile.snapshot", nil),
	gitignore.ParsePattern("deno.lock", nil),
	gitignore.ParsePattern("flake.lock", nil),
	gitignore.ParsePattern("Gemfile.lock", nil),
	gitignore.ParsePattern("go.sum", nil),
	gitignore.ParsePattern("mix.lock", nil),
	gitignore.ParsePattern("npm-shrinkwrap.json", nil),
	gitignore.ParsePattern("package-lock.json", nil),
	gitignore.ParsePattern("Package.resolved", nil),
	gitignore.ParsePattern("packages.lock.json", nil),
	gitignore.ParsePattern("pdm.lock", nil),
	gitignore.ParsePattern("Pipfile.lock", nil),
	gitignore.ParsePattern("pixi.lock", nil),
	gitignore.ParsePattern("Podfile.lock", nil),
	gitignore.ParsePattern("poetry.lock", nil),
	gitignore.ParsePattern("pnpm-lock.yaml", nil),
	gitignore.ParsePattern("pubspec.lock", nil),
	gitignore.ParsePattern("renv.lock", nil),
	gitignore.ParsePattern("shard.lock", nil),
	gitignore.ParsePattern("stack.yaml.lock", nil),
	gitignore.ParsePattern("uv.lock", nil),
	gitignore.ParsePattern("yarn.lock", nil),
	gitignore.ParsePattern("*.bazel", nil),
	gitignore.ParsePattern("*.sha256", nil),
	gitignore.ParsePattern("LICENSE", nil),
	gitignore.ParsePattern("COPYING", nil),
	gitignore.ParsePattern("NOTICE", nil),
}

type Options struct {
	Root                   string
	Rev                    string
	Remote                 string
	IndexRemote            string
	MinRelatedness         float64
	Limit                  int
	IndexOnly              bool
	Reindex                bool
	CodeOnly               bool
	NoTests                bool
	Scope                  []string
	EmbeddingModel         string
	EmbeddingDimensions    int
	EmbeddingMaxInput      int
	EmbeddingBatchInputs   int
	EmbeddingBatchMaxChars int
	EmbeddingConcurrency   int
	APIKey                 string
	BaseURL                string
	Debug                  bool
	DebugLog               func(string, ...slog.Attr)
	ProgressLog            func(Progress) error
	skipIndexSync          bool
}

type Progress struct {
	Status  string
	Detail  string
	Done    int
	Total   int
	Reused  int
	Elapsed time.Duration
}

type Output struct {
	Query          string      `json:"query"`
	Source         Source      `json:"source"`
	MinRelatedness float64     `json:"min_relatedness"`
	Retrieval      Retrieval   `json:"retrieval"`
	Results        []Result    `json:"results"`
	Replay         Replay      `json:"replay"`
	Diagnostics    Diagnostics `json:"-"`
}

type Diagnostics struct {
	IndexDir       string
	Files          int
	Chunks         int
	ReusedChunks   int
	EmbeddedChunks int
	EmbeddedDone   int
	SkippedFiles   []SkippedFile
	Timings        []Timing
	Total          time.Duration
}

type SkippedFile struct {
	Path   string
	Reason string
}

type Timing struct {
	Step     string
	Duration time.Duration
}

type Source struct {
	Mode        string `json:"mode"`
	Root        string `json:"root,omitempty"`
	Remote      string `json:"remote,omitempty"`
	Rev         string `json:"rev,omitempty"`
	ResolvedRev string `json:"resolved_rev,omitempty"`
}

type Retrieval struct {
	Mode           string        `json:"mode"`
	EmbeddingModel string        `json:"embedding_model"`
	Index          string        `json:"index"`
	Dimensions     int           `json:"dimensions"`
	Filters        Filters       `json:"filters,omitzero"`
	Skipped        SkippedCounts `json:"skipped,omitzero"`
}

type Filters struct {
	Code    bool     `json:"code,omitempty"`
	NoTests bool     `json:"no_tests,omitempty"`
	Scope   []string `json:"scope,omitempty"`
}

type SkippedCounts struct {
	Dirs       int `json:"dirs,omitempty"`
	Binary     int `json:"binary,omitempty"`
	NonText    int `json:"non_text,omitempty"`
	Oversized  int `json:"oversized,omitempty"`
	Symlink    int `json:"symlink,omitempty"`
	Unreadable int `json:"unreadable,omitempty"`
}

type Result struct {
	Relatedness float64      `json:"relatedness"`
	Range       string       `json:"range"`
	Symbol      *Symbol      `json:"symbol"`
	Scores      ResultScores `json:"scores"`
	Excerpt     string       `json:"excerpt"`
	Path        string       `json:"-"`
	StartLine   int          `json:"-"`
}

type ResultScores struct {
	Cosine            float64 `json:"cosine"`
	VectorRelatedness float64 `json:"vector_relatedness"`
	Text              float64 `json:"text"`
	Path              float64 `json:"path"`
	Symbol            float64 `json:"symbol"`
	Lexical           float64 `json:"lexical"`
	Rank              float64 `json:"rank"`
}

type Replay struct {
	Mode string  `json:"mode"`
	From *string `json:"from"`
}

type Symbol struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

type Chunk struct {
	ID            string  `json:"id"`
	Path          string  `json:"path"`
	Source        string  `json:"source"`
	Blob          string  `json:"blob"`
	StartLine     int     `json:"start_line"`
	EndLine       int     `json:"end_line"`
	Symbol        *Symbol `json:"symbol"`
	ContentHash   string  `json:"content_hash"`
	EmbeddingText string  `json:"embedding_text"`
	Size          int64   `json:"size,omitempty"`
	MTimeUnixNano int64   `json:"mtime_unix_nano,omitempty"`

	text string
}

type fileContent struct {
	path   string
	blob   string
	source string
	text   string
	size   int64
	mtime  time.Time
}

type manifest struct {
	Version        int       `json:"version"`
	Mode           string    `json:"mode"`
	Root           string    `json:"root,omitempty"`
	Remote         string    `json:"remote,omitempty"`
	ResolvedRev    string    `json:"resolved_rev,omitempty"`
	EmbeddingModel string    `json:"embedding_model"`
	Dimensions     int       `json:"dimensions"`
	CreatedAt      time.Time `json:"created_at"`
	FileCount      int       `json:"file_count,omitempty"`
	ChunkCount     int       `json:"chunk_count,omitempty"`
	VectorStore    string    `json:"vector_store,omitempty"`
}

type vectorRecord struct {
	ChunkID            string    `json:"chunk_id"`
	Path               string    `json:"path"`
	Source             string    `json:"source"`
	Blob               string    `json:"blob,omitempty"`
	StartLine          int       `json:"start_line"`
	EndLine            int       `json:"end_line"`
	ContentHash        string    `json:"content_hash"`
	EmbeddingInputHash string    `json:"embedding_input_hash,omitempty"`
	EmbeddingModel     string    `json:"embedding_model"`
	Dimensions         int       `json:"dimensions"`
	Size               int64     `json:"size,omitempty"`
	MTimeUnixNano      int64     `json:"mtime_unix_nano,omitempty"`
	Vector             []float64 `json:"vector"`
}

type vectorIndexRecord struct {
	ChunkID            string `json:"chunk_id"`
	Path               string `json:"path"`
	Source             string `json:"source"`
	Blob               string `json:"blob,omitempty"`
	StartLine          int    `json:"start_line"`
	EndLine            int    `json:"end_line"`
	ContentHash        string `json:"content_hash"`
	EmbeddingInputHash string `json:"embedding_input_hash,omitempty"`
	EmbeddingModel     string `json:"embedding_model"`
	Dimensions         int    `json:"dimensions"`
	Size               int64  `json:"size,omitempty"`
	MTimeUnixNano      int64  `json:"mtime_unix_nano,omitempty"`
	Offset             int64  `json:"offset"`
	VectorKey          string `json:"vector_key,omitempty"`
	VectorChecksum     uint32 `json:"vector_checksum,omitempty"`
}

type historyEntry struct {
	Query          string    `json:"query"`
	Normalized     string    `json:"normalized"`
	QueryTextHash  string    `json:"query_text_hash,omitempty"`
	QueryEmbedding []float64 `json:"query_embedding"`
	EmbeddingOnly  bool      `json:"embedding_only,omitempty"`
	EmbeddingModel string    `json:"embedding_model"`
	Dimensions     int       `json:"dimensions"`
	SourceMode     string    `json:"source_mode"`
	Root           string    `json:"root,omitempty"`
	Remote         string    `json:"remote,omitempty"`
	ResolvedRev    string    `json:"resolved_rev,omitempty"`
	Filters        *Filters  `json:"filters,omitempty"`
	ResultChunkIDs []string  `json:"result_chunk_ids"`
	CreatedAt      time.Time `json:"created_at"`
}

func Run(ctx context.Context, client openai.EmbeddingClient, opts Options, query string) (Output, error) {
	started := time.Now()
	phaseStarted := started
	var diag Diagnostics
	debugLog := func(kind string, attrs ...slog.Attr) {
		if opts.DebugLog != nil {
			opts.DebugLog(kind, attrs...)
		}
	}
	mark := func(step string) {
		now := time.Now()
		duration := now.Sub(phaseStarted)
		diag.Timings = append(diag.Timings, Timing{Step: step, Duration: duration})
		phaseStarted = now
		debugLog("search_timing",
			slog.String("step", step),
			slog.Duration("duration", duration.Round(time.Millisecond)),
		)
	}
	resultWithDiagnostics := func(output Output) Output {
		diag.Total = time.Since(started)
		output.Diagnostics = diag
		return output
	}
	fail := func(err error) (Output, error) {
		return resultWithDiagnostics(Output{}), err
	}

	query = strings.TrimSpace(query)
	if query == "" && !opts.IndexOnly {
		return fail(errors.New("search query is empty"))
	}
	if opts.MinRelatedness <= 0 || opts.MinRelatedness > 1 {
		return fail(errors.New("--min-relatedness must be > 0 and <= 1"))
	}
	if opts.Limit < 1 || opts.Limit > MaxLimit {
		return fail(fmt.Errorf("--limit must be between 1 and %d", MaxLimit))
	}
	if strings.TrimSpace(opts.EmbeddingModel) == "" {
		return fail(errors.New("--embedding-model is required"))
	}
	if opts.EmbeddingDimensions < 1 {
		return fail(errors.New("--embedding-dimensions must be positive"))
	}
	if opts.EmbeddingMaxInput < 0 {
		return fail(errors.New("embedding max input chars must be positive"))
	}
	if opts.EmbeddingBatchInputs < 0 {
		return fail(errors.New("embedding batch inputs must be positive"))
	}
	if opts.EmbeddingBatchMaxChars < 0 {
		return fail(errors.New("embedding batch max chars must be positive"))
	}
	if opts.EmbeddingConcurrency < 0 {
		return fail(errors.New("embedding concurrency must be positive"))
	}
	scope, err := normalizeScopes(opts.Scope)
	if err != nil {
		return fail(err)
	}
	rootOpt := opts.Root
	if strings.TrimSpace(opts.Remote) == "" {
		rootOpt, scope, err = localSearchRootAndScope(rootOpt, scope)
		if err != nil {
			return fail(err)
		}
	}
	filters := Filters{Code: opts.CodeOnly, NoTests: opts.NoTests, Scope: scope}
	selection, err := resolveIndexSelection(ctx, rootOpt, opts.Remote, opts.Rev, filters, opts.Reindex, true, opts.ProgressLog)
	if err != nil {
		return fail(err)
	}
	root := selection.root
	source := selection.source
	resolvedRev := selection.resolvedRev
	sharedScope := len(scope) > 0 && !scopeUsesSkippedPath(scope)
	var headSync *indexSync
	if !opts.skipIndexSync && strings.TrimSpace(opts.IndexRemote) != "" && strings.TrimSpace(opts.Remote) == "" && selection.repo != nil && selection.repo.HeadSHA != "" {
		origin := repositoryOrigin(selection.repo)
		if origin != "" {
			headSync, err = prepareIndexSync(ctx, opts.IndexRemote, origin, selection.repo.HeadSHA, opts.EmbeddingModel, opts.EmbeddingDimensions, selection.metadataDir, selection.repo.RootPath)
			if err != nil {
				return fail(err)
			}
			syncSession := headSync
			defer syncSession.close()
			if source.Mode != "revision" || resolvedRev != selection.repo.HeadSHA {
				headOpts := opts
				headOpts.Root = selection.repo.RootPath
				headOpts.Rev = selection.repo.HeadSHA
				headOpts.Remote = ""
				headOpts.Scope = nil
				headOpts.IndexOnly = true
				headOpts.Reindex = false
				headOpts.CodeOnly = false
				headOpts.NoTests = false
				headOpts.skipIndexSync = true
				if _, err := Run(ctx, client, headOpts, ""); err != nil {
					return fail(fmt.Errorf("index current HEAD for sync: %w", err))
				}
				if err := headSync.exportAndPush(ctx, selection.metadataDir, selection.repo.RootPath); err != nil {
					return fail(err)
				}
				if err := headSync.close(); err != nil {
					return fail(err)
				}
				headSync = nil
			}
		}
	}

	var files []fileContent
	var discoveredFiles []fileContent
	var skipped SkippedCounts
	var skippedFiles []SkippedFile
	if source.Mode == "revision" || source.Mode == "remote" {
		files, skipped, skippedFiles, err = discoverRevisionFiles(selection.repo, resolvedRev, scope, debugLog)
		if err != nil {
			return fail(err)
		}
		discoveredFiles = files
	} else {
		files, skipped, skippedFiles, err = discoverFilesystemFiles(root, scope, debugLog)
		if err != nil {
			return fail(err)
		}
		discoveredFiles = files
	}
	diag.SkippedFiles = skippedFiles
	if filters.Code {
		files = filterCodeFiles(files)
	}
	diag.Files = len(files)
	mark("discover")

	chunks := buildChunks(files)
	diag.Chunks = len(chunks)
	mark("chunk")

	indexDir := selection.indexDir
	diag.IndexDir = indexDir
	indexLock, err := lockIndex(ctx, indexDir)
	if err != nil {
		return fail(err)
	}
	indexLocked := true
	unlockIndex := func() error {
		if !indexLocked {
			return nil
		}
		indexLocked = false
		return indexLock.Unlock()
	}
	defer func() {
		_ = unlockIndex()
	}()
	oldVectors, _ := loadVectors(indexDir)
	var seedVectors []vectorRecord
	if !opts.Reindex {
		missingKeys := missingVectorKeys(chunks, oldVectors, opts)
		if len(missingKeys) > 0 {
			if err := unlockIndex(); err != nil {
				return fail(err)
			}
			seedVectors, err = loadBestReuseCandidate(ctx, selection.metadataDir, indexDir, missingKeys, opts)
			if err != nil {
				return fail(err)
			}
			indexLock, err = lockIndex(ctx, indexDir)
			if err != nil {
				return fail(err)
			}
			indexLocked = true
			oldVectors, _ = loadVectors(indexDir)
		}
	}
	reuseOpts := opts
	if opts.Reindex && indexBuiltSince(indexDir, started) {
		reuseOpts.Reindex = false
	}
	reusePool := make([]vectorRecord, 0, len(seedVectors)+len(oldVectors))
	reusePool = append(reusePool, seedVectors...)
	reusePool = append(reusePool, oldVectors...)
	vectors, records, reused := reuseVectors(chunks, reusePool, reuseOpts)
	diag.ReusedChunks = reused
	mark("cache")

	var dimensions int
	if len(vectors) > 0 {
		for _, vector := range vectors {
			dimensions = len(vector)
			break
		}
	}
	missing := missingChunks(chunks, vectors)
	diag.EmbeddedChunks = len(missing)
	if len(missing) > 0 {
		batchInputs := embeddingBatchInputs(opts)
		batchMaxChars := embeddingBatchMaxChars(opts)
		concurrency := embeddingConcurrency(opts)
		texts := cappedEmbeddingInputs(missingEmbeddingTexts(missing), opts.EmbeddingMaxInput)
		type embeddingBatch struct {
			start int
			end   int
		}
		type embeddingBatchResult struct {
			embeddingBatch
			response      openai.EmbeddingResponse
			clientElapsed time.Duration
			err           error
		}
		var batches []embeddingBatch
		for start := 0; start < len(missing); {
			end := embeddingBatchEnd(texts, start, batchInputs, batchMaxChars)
			batches = append(batches, embeddingBatch{start: start, end: end})
			start = end
		}
		debugLog("search_embed_plan",
			slog.Int("missing_chunks", len(missing)),
			slog.Int("reused_chunks", reused),
			slog.Int("batches", len(batches)),
			slog.Int("batch_inputs", batchInputs),
			slog.Int("batch_max_chars", batchMaxChars),
			slog.Int("concurrency", concurrency),
			slog.String("index_dir", indexDir),
		)
		if opts.ProgressLog != nil {
			if err := opts.ProgressLog(Progress{Total: len(missing), Reused: reused}); err != nil {
				mark("embed_index")
				return fail(err)
			}
		}

		embedStarted := time.Now()
		embedCtx, cancelEmbeddings := context.WithCancel(ctx)
		group, groupCtx := errgroup.WithContext(embedCtx)
		group.SetLimit(concurrency)
		results := make(chan embeddingBatchResult, len(batches))
		waitErr := make(chan error, 1)
		go func() {
			for _, batch := range batches {
				if groupCtx.Err() != nil {
					break
				}
				group.Go(func() error {
					clientStarted := time.Now()
					response, err := embedBatch(groupCtx, client, opts, texts[batch.start:batch.end])
					clientElapsed := time.Since(clientStarted)
					if err != nil {
						err = fmt.Errorf("embedding request failed: %w", err)
					}
					results <- embeddingBatchResult{embeddingBatch: batch, response: response, clientElapsed: clientElapsed, err: err}
					return err
				})
			}
			waitErr <- group.Wait()
			close(results)
		}()

		var embedErr error
		for result := range results {
			if result.err != nil {
				if embedErr == nil {
					embedErr = result.err
				}
				cancelEmbeddings()
				continue
			}
			response := result.response
			start, end := result.start, result.end
			if len(response.Vectors) != end-start {
				if embedErr == nil {
					embedErr = fmt.Errorf("embedding response vectors = %d, want %d", len(response.Vectors), end-start)
				}
				cancelEmbeddings()
				continue
			}
			if dimensions == 0 {
				dimensions = response.Dimensions
			}
			if response.Dimensions != dimensions {
				if embedErr == nil {
					embedErr = fmt.Errorf("embedding dimensions mismatch: %d and %d", dimensions, response.Dimensions)
				}
				cancelEmbeddings()
				continue
			}
			for i, chunk := range missing[start:end] {
				vector := response.Vectors[i]
				vectors[chunk.ID] = vector
				records = append(records, vectorRecordForChunk(chunk, vector, opts))
				diag.EmbeddedDone++
			}
			elapsedRaw := time.Since(embedStarted)
			elapsed := elapsedRaw.Round(time.Millisecond)
			itemElapsed := (elapsedRaw / time.Duration(diag.EmbeddedDone)).Round(time.Millisecond)
			progress := float64(diag.EmbeddedDone) / float64(len(missing)) * 100
			debugLog("search_embed_progress",
				slog.String("progress", fmt.Sprintf("%d/%d (%.1f%%)", diag.EmbeddedDone, len(missing), progress)),
				slog.Duration("elapsed", elapsed),
				slog.Duration("item_elapsed", itemElapsed),
				slog.Duration("client_elapsed", result.clientElapsed.Round(time.Millisecond)),
			)
			if opts.ProgressLog != nil {
				if err := opts.ProgressLog(Progress{Done: diag.EmbeddedDone, Total: len(missing), Reused: reused, Elapsed: elapsedRaw}); err != nil {
					if embedErr == nil {
						embedErr = err
					}
					cancelEmbeddings()
					continue
				}
			}
		}
		cancelEmbeddings()
		if err := <-waitErr; embedErr == nil {
			embedErr = err
		}
		if embedErr != nil {
			mark("embed_index")
			return fail(embedErr)
		}
	}
	mark("embed_index")

	var forceVectorKeys map[string]bool
	if reuseOpts.Reindex {
		forceVectorKeys = make(map[string]bool, len(records))
		for _, record := range records {
			key := vectorStoreKey(record.EmbeddingInputHash, record.EmbeddingModel, record.Dimensions)
			forceVectorKeys[key] = true
		}
	}
	if filters.Code {
		records = preserveSharedFilteredRecords(records, oldVectors, discoveredFiles, opts)
	}
	shouldSave := len(missing) > 0 || reuseOpts.Reindex
	if !shouldSave {
		if sharedScope {
			shouldSave = scopedRecordsChanged(records, oldVectors, scope, opts)
		} else {
			shouldSave = len(records) != len(oldVectors)
		}
	}
	if !shouldSave && !indexUsesSharedVectors(indexDir) {
		shouldSave = true
	}
	if shouldSave && sharedScope {
		if opts.Rev != "" {
			records = preserveOutOfScopeRevisionRecords(records, oldVectors, scope, opts)
		} else {
			records = preserveOutOfScopeFilesystemRecords(records, oldVectors, root, scope, opts)
		}
	}
	if dimensions == 0 {
		dimensions = opts.EmbeddingDimensions
	}
	if shouldSave {
		if err := saveIndex(ctx, selection.metadataDir, indexDir, source, root, resolvedRev, opts.EmbeddingModel, dimensions, records, forceVectorKeys); err != nil {
			mark("persist")
			return fail(err)
		}
	}
	mark("persist")
	if err := unlockIndex(); err != nil {
		return fail(err)
	}
	if headSync != nil {
		if err := headSync.exportAndPush(ctx, selection.metadataDir, selection.repo.RootPath); err != nil {
			return fail(err)
		}
		if err := headSync.close(); err != nil {
			return fail(err)
		}
	}

	indexStatus := "miss"
	if reused > 0 {
		indexStatus = "hit"
	}
	if opts.IndexOnly {
		return resultWithDiagnostics(Output{
			Query:          query,
			Source:         source,
			MinRelatedness: opts.MinRelatedness,
			Retrieval: Retrieval{
				Mode:           "embeddings",
				EmbeddingModel: opts.EmbeddingModel,
				Index:          indexStatus,
				Dimensions:     dimensions,
				Filters:        filters,
				Skipped:        skipped,
			},
			Results: []Result{},
		}), nil
	}

	normalizedQuery := normalizeQuery(query)
	queryText := queryEmbeddingText(query, opts.EmbeddingMaxInput)
	sum := sha256.Sum256([]byte(queryText))
	queryTextHash := hex.EncodeToString(sum[:])
	var queryVector []float64
	var queryDimensions int
	var cachedQuery bool
	queryLock := queryLockDir(indexDir, normalizedQuery, queryTextHash, opts.EmbeddingModel, opts.EmbeddingDimensions, source, root, resolvedRev)
	if err := withIndexLock(ctx, queryLock, func() error {
		if err := withIndexLock(ctx, indexDir, func() error {
			queryVector, queryDimensions, cachedQuery = cachedQueryEmbedding(indexDir, normalizedQuery, queryTextHash, opts.EmbeddingModel, opts.EmbeddingDimensions, source, root, resolvedRev)
			return nil
		}); err != nil {
			return err
		}
		if cachedQuery {
			return nil
		}
		queryVectors, dim, err := embedTexts(ctx, client, opts, []string{queryText})
		if err != nil {
			return err
		}
		queryVector = queryVectors[0]
		queryDimensions = dim
		historyErr := withIndexLock(ctx, indexDir, func() error {
			return appendHistory(indexDir, historyEntry{
				Query:          query,
				Normalized:     normalizedQuery,
				QueryTextHash:  queryTextHash,
				QueryEmbedding: queryVector,
				EmbeddingOnly:  true,
				EmbeddingModel: opts.EmbeddingModel,
				Dimensions:     len(queryVector),
				SourceMode:     source.Mode,
				Root:           root,
				Remote:         source.Remote,
				ResolvedRev:    resolvedRev,
				CreatedAt:      time.Now().UTC(),
			})
		})
		if errors.Is(historyErr, context.Canceled) || errors.Is(historyErr, context.DeadlineExceeded) {
			return historyErr
		}
		if historyErr != nil {
			debugLog("search_history_error", slog.String("error", historyErr.Error()))
		}
		return nil
	}); err != nil {
		mark("embed_query")
		return fail(err)
	}
	if dimensions == 0 {
		dimensions = queryDimensions
	}
	mark("embed_query")

	var skipScoreChunk func(Chunk) bool
	if filters.NoTests {
		skipScoreChunk = func(chunk Chunk) bool { return isTestPath(chunk.Path) }
	}
	scored := scoreChunks(chunks, vectors, queryVector, query, opts.MinRelatedness, skipScoreChunk)
	sortResults(scored)
	if len(scored) > opts.Limit {
		scored = scored[:opts.Limit]
	}
	results := renderResults(scored)
	mark("score")

	replay := Replay{Mode: "none"}
	historyFilters := filters
	historyFilters.Scope = slices.Clone(filters.Scope)
	historyErr := withIndexLock(ctx, indexDir, func() error {
		replay = replayFor(indexDir, normalizedQuery, queryTextHash, queryVector, opts.EmbeddingModel, opts.EmbeddingDimensions, source, root, resolvedRev, filters)
		return appendHistory(indexDir, historyEntry{
			Query:          query,
			Normalized:     normalizedQuery,
			QueryTextHash:  queryTextHash,
			QueryEmbedding: queryVector,
			EmbeddingModel: opts.EmbeddingModel,
			Dimensions:     len(queryVector),
			SourceMode:     source.Mode,
			Root:           root,
			Remote:         source.Remote,
			ResolvedRev:    resolvedRev,
			Filters:        &historyFilters,
			ResultChunkIDs: resultIDs(scored),
			CreatedAt:      time.Now().UTC(),
		})
	})
	if errors.Is(historyErr, context.Canceled) || errors.Is(historyErr, context.DeadlineExceeded) {
		return fail(historyErr)
	}
	if historyErr != nil {
		debugLog("search_history_error", slog.String("error", historyErr.Error()))
	}
	mark("replay")

	return resultWithDiagnostics(Output{
		Query:          query,
		Source:         source,
		MinRelatedness: opts.MinRelatedness,
		Retrieval: Retrieval{
			Mode:           "embeddings",
			EmbeddingModel: opts.EmbeddingModel,
			Index:          indexStatus,
			Dimensions:     dimensions,
			Filters:        filters,
			Skipped:        skipped,
		},
		Results: results,
		Replay:  replay,
	}), nil
}

func discoverFilesystemFiles(root string, scope []string, debugLog func(string, ...slog.Attr)) ([]fileContent, SkippedCounts, []SkippedFile, error) {
	var files []fileContent
	var skipped SkippedCounts
	var skippedFiles []SkippedFile
	skip := func(path, reason string) {
		skippedFiles = append(skippedFiles, SkippedFile{Path: path, Reason: reason})
		debugLog("search_skip",
			slog.String("path", path),
			slog.String("reason", reason),
		)
	}
	ignoreMatcher := filesystemIgnoreMatcher(root, scope)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			skipped.Unreadable++
			skip(filepath.ToSlash(path), "unreadable")
			return nil
		}
		name := entry.Name()
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !pathInScope(rel, scope) {
			if entry.IsDir() && !scopeMayContainDir(rel, scope) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != root && shouldSkipDir(name) && shouldSkipPath(rel, scope) {
				skipped.Dirs++
				skip(rel, "dot_dir")
				return filepath.SkipDir
			}
			if path != root && ignoreMatcher.Match(pathParts(rel), true) {
				skipped.Dirs++
				return filepath.SkipDir
			}
			return nil
		}
		if shouldSkipFile(name) {
			if searchIgnoreFileNames[name] {
				return nil
			}
			if shouldSkipPath(rel, scope) {
				skip(rel, "dot_file")
				return nil
			}
		}
		if ignoreMatcher.Match(pathParts(rel), false) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			skipped.Unreadable++
			skip(rel, "unreadable")
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			skipped.Symlink++
			skip(rel, "symlink")
			return nil
		}
		if !info.Mode().IsRegular() {
			skip(rel, "non_regular")
			return nil
		}
		if info.Size() > MaxFileBytes {
			skipped.Oversized++
			skip(rel, "oversized")
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			skipped.Unreadable++
			skip(rel, "unreadable")
			return nil
		}
		if isBinary(data) {
			skipped.Binary++
			skip(rel, "binary")
			return nil
		}
		if !isIndexableText(rel, data) {
			skipped.NonText++
			skip(rel, "non_text")
			return nil
		}
		files = append(files, fileContent{
			path:   rel,
			source: "filesystem",
			text:   string(data),
			size:   info.Size(),
			mtime:  info.ModTime(),
		})
		return nil
	})
	slices.SortFunc(files, func(a, b fileContent) int { return strings.Compare(a.path, b.path) })
	return files, skipped, skippedFiles, err
}

func discoverRevisionFiles(repo *gitctx.Repository, rev string, scope []string, debugLog func(string, ...slog.Attr)) ([]fileContent, SkippedCounts, []SkippedFile, error) {
	var files []fileContent
	var skipped SkippedCounts
	var skippedFiles []SkippedFile
	skip := func(path, reason string) {
		skippedFiles = append(skippedFiles, SkippedFile{Path: path, Reason: reason})
		debugLog("search_skip",
			slog.String("path", path),
			slog.String("reason", reason),
		)
	}
	ignoreMatcher, err := revisionIgnoreMatcher(repo, rev, scope)
	if err != nil {
		return nil, skipped, nil, err
	}
	err = repo.WalkCommitTextFiles(rev, MaxFileBytes, func(path string) bool {
		return pathInScope(path, scope)
	}, func(file gitctx.CommitFile) error {
		if shouldSkipPath(file.Path, scope) {
			skipped.Dirs++
			skip(filepath.ToSlash(file.Path), "dot_path")
			return nil
		}
		if revisionPathIgnored(ignoreMatcher, file.Path) {
			return nil
		}
		if !isIndexableText(file.Path, []byte(file.Text)) {
			skipped.NonText++
			skip(filepath.ToSlash(file.Path), "non_text")
			return nil
		}
		files = append(files, fileContent{
			path:   filepath.ToSlash(file.Path),
			blob:   file.Blob,
			source: "revision",
			text:   file.Text,
			size:   file.Size,
		})
		return nil
	}, func(file gitctx.CommitFileSkip) error {
		if !pathInScope(file.Path, scope) {
			return nil
		}
		if shouldSkipPath(file.Path, scope) {
			skipped.Dirs++
			skip(filepath.ToSlash(file.Path), "dot_path")
			return nil
		}
		if revisionPathIgnored(ignoreMatcher, file.Path) {
			return nil
		}
		switch file.Reason {
		case "oversized":
			skipped.Oversized++
			skip(filepath.ToSlash(file.Path), "oversized")
		case "binary":
			skipped.Binary++
			skip(filepath.ToSlash(file.Path), "binary")
		}
		return nil
	})
	slices.SortFunc(files, func(a, b fileContent) int { return strings.Compare(a.path, b.path) })
	return files, skipped, skippedFiles, err
}

func filterCodeFiles(files []fileContent) []fileContent {
	filtered := make([]fileContent, 0, len(files))
	for _, file := range files {
		if isCodePath(file.path) {
			filtered = append(filtered, file)
		}
	}
	return filtered
}

func buildChunks(files []fileContent) []Chunk {
	var chunks []Chunk
	for _, file := range files {
		chunks = append(chunks, chunksForFile(file)...)
	}
	for i := range chunks {
		chunks[i].ID = fmt.Sprintf("c%06d", i+1)
	}
	return chunks
}

func chunksForFile(file fileContent) []Chunk {
	lines := splitLines(file.text)
	if strings.HasSuffix(file.path, ".go") {
		if chunks, ok := goChunks(file, lines); ok {
			return chunks
		}
	}
	return lineChunks(file, lines)
}

func lineChunks(file fileContent, lines []string) []Chunk {
	if len(lines) <= chunkLines {
		return []Chunk{newChunk(file, lines, 1, len(lines), nil)}
	}
	var chunks []Chunk
	step := chunkLines - chunkOverlap
	for start := 1; start <= len(lines); start += step {
		end := min(len(lines), start+chunkLines-1)
		chunks = append(chunks, newChunk(file, lines, start, end, nil))
		if end == len(lines) {
			break
		}
	}
	return chunks
}

func goChunks(file fileContent, lines []string) ([]Chunk, bool) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file.path, file.text, parser.ParseComments)
	if err != nil {
		return nil, false
	}
	if hasDoNotEditHeading(parsed) {
		return []Chunk{newPathOnlyChunk(file)}, true
	}
	var chunks []Chunk
	covered := make([]bool, len(lines)+1)
	add := func(start, end int, symbol *Symbol) {
		if start < 1 || end < start || start > len(lines) {
			return
		}
		end = min(end, len(lines))
		step := chunkLines - chunkOverlap
		for chunkStart := start; chunkStart <= end; chunkStart += step {
			chunkEnd := min(end, chunkStart+chunkLines-1)
			chunks = append(chunks, newChunk(file, lines, chunkStart, chunkEnd, symbol))
			for line := chunkStart; line <= chunkEnd; line++ {
				covered[line] = true
			}
			if chunkEnd == end {
				break
			}
		}
	}
	for _, decl := range parsed.Decls {
		start := fset.Position(decl.Pos()).Line
		end := fset.Position(decl.End()).Line
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind := "function"
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
			}
			add(start, end, &Symbol{Type: kind, Name: name})
		case *ast.GenDecl:
			switch d.Tok {
			case token.TYPE:
				add(start, end, &Symbol{Type: "type", Name: specNames(d.Specs)})
			case token.CONST:
				add(start, end, &Symbol{Type: "const", Name: specNames(d.Specs)})
			case token.VAR:
				add(start, end, &Symbol{Type: "var", Name: specNames(d.Specs)})
			}
		}
	}
	for start := 1; start <= len(lines); {
		for start <= len(lines) && (covered[start] || strings.TrimSpace(lines[start-1]) == "") {
			start++
		}
		if start > len(lines) {
			break
		}
		end := start
		for end <= len(lines) && !covered[end] {
			end++
		}
		add(start, end-1, nil)
		start = end
	}
	if len(chunks) == 0 {
		return nil, false
	}
	slices.SortFunc(chunks, func(a, b Chunk) int { return a.StartLine - b.StartLine })
	return chunks, true
}

func specNames(specs []ast.Spec) string {
	var names []string
	for _, spec := range specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			names = append(names, s.Name.Name)
		case *ast.ValueSpec:
			for _, name := range s.Names {
				names = append(names, name.Name)
			}
		}
	}
	return strings.Join(names, ", ")
}

func newChunk(file fileContent, lines []string, start, end int, symbol *Symbol) Chunk {
	if len(lines) == 0 {
		start, end = 1, 1
	}
	text := strings.Join(lines[start-1:end], "\n")
	hash := sha256.Sum256([]byte(text))
	chunk := Chunk{
		Path:          file.path,
		Source:        file.source,
		Blob:          file.blob,
		StartLine:     start,
		EndLine:       end,
		Symbol:        symbol,
		ContentHash:   hex.EncodeToString(hash[:]),
		Size:          file.size,
		MTimeUnixNano: file.mtime.UnixNano(),
		text:          text,
	}
	chunk.EmbeddingText = embeddingText(chunk, text)
	return chunk
}

func newPathOnlyChunk(file fileContent) Chunk {
	hash := sha256.Sum256([]byte("path-only:" + file.source + ":" + file.path))
	chunk := Chunk{
		Path:          file.path,
		Source:        file.source,
		StartLine:     1,
		EndLine:       1,
		ContentHash:   hex.EncodeToString(hash[:]),
		MTimeUnixNano: 0,
		text:          "",
	}
	chunk.EmbeddingText = embeddingText(chunk, "")
	return chunk
}

func hasDoNotEditHeading(file *ast.File) bool {
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if comment.Pos() > file.Package {
				return false
			}
			if strings.Contains(comment.Text, "DO NOT EDIT") {
				return true
			}
		}
	}
	return false
}

func embeddingText(chunk Chunk, text string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "path: %s\n", chunk.Path)
	if chunk.Symbol != nil && chunk.Symbol.Name != "" {
		fmt.Fprintf(&b, "symbol: %s %s\n", chunk.Symbol.Type, chunk.Symbol.Name)
	}
	if lang := languageForPath(chunk.Path); lang != "" {
		fmt.Fprintf(&b, "language: %s\n", lang)
	}
	b.WriteString("\n")
	b.WriteString(clampEmbeddingLines(text))
	return b.String()
}

func clampEmbeddingLines(text string) string {
	if text == "" {
		return ""
	}
	var b strings.Builder
	for i, line := range splitLines(text) {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(clampEmbeddingLine(line))
	}
	return b.String()
}

func clampEmbeddingLine(line string) string {
	chars := 0
	for end := range line {
		if chars == maxEmbeddingLineChars {
			return line[:end]
		}
		chars++
	}
	return line
}

func loadVectors(dir string) ([]vectorRecord, error) {
	found, err := loadManifest(dir)
	if err != nil {
		return nil, err
	}
	if found.VectorStore != "" {
		if found.VectorStore != sharedVectorStoreVersion {
			return nil, fmt.Errorf("unsupported vector store %q", found.VectorStore)
		}
		return loadSharedVectors(metadataDirForIndex(dir), dir)
	}
	if records, err := loadBinaryVectors(dir); err == nil {
		return records, nil
	}
	return loadLegacyVectors(dir)
}

func migrateSearchMetadata(ctx context.Context, legacyMetadataDir, targetMetadataDir string) error {
	legacySearch := filepath.Join(legacyMetadataDir, "search")
	if _, err := os.Stat(legacySearch); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var manifests []string
	if err := filepath.WalkDir(legacySearch, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && entry.Name() == "manifest.json" {
			manifests = append(manifests, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, manifestPath := range manifests {
		sourceDir := filepath.Dir(manifestPath)
		var found manifest
		var sourceRecords []vectorRecord
		if err := withIndexLock(ctx, sourceDir, func() error {
			var err error
			found, err = loadManifest(sourceDir)
			if err != nil {
				return err
			}
			sourceRecords, err = loadVectors(sourceDir)
			return err
		}); err != nil {
			return fmt.Errorf("load legacy search index %s: %w", sourceDir, err)
		}
		rel, err := filepath.Rel(legacyMetadataDir, sourceDir)
		if err != nil {
			return err
		}
		targetDir := filepath.Join(targetMetadataDir, rel)
		if existing, err := loadManifest(targetDir); err == nil && (existing.EmbeddingModel != found.EmbeddingModel || existing.Dimensions != found.Dimensions) {
			targetDir = filepath.Join(targetMetadataDir, "search", "migrated-"+pathHash(legacyMetadataDir), strings.TrimPrefix(rel, "search"+string(filepath.Separator)))
		}
		if err := withIndexLock(ctx, targetDir, func() error {
			targetRecords, _ := loadVectors(targetDir)
			records := mergeCompatibleRecords(targetRecords, sourceRecords, found.EmbeddingModel, found.Dimensions)
			source := Source{Mode: found.Mode, Root: found.Root, Remote: found.Remote, ResolvedRev: found.ResolvedRev}
			return saveIndex(ctx, targetMetadataDir, targetDir, source, found.Root, found.ResolvedRev, found.EmbeddingModel, found.Dimensions, records, nil)
		}); err != nil {
			return fmt.Errorf("migrate legacy search index %s: %w", sourceDir, err)
		}
	}
	if err := os.RemoveAll(legacySearch); err != nil {
		return fmt.Errorf("remove legacy search metadata: %w", err)
	}
	return nil
}

func pathHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

func indexUsesSharedVectors(dir string) bool {
	found, err := loadManifest(dir)
	return err == nil && found.VectorStore == sharedVectorStoreVersion
}

func metadataDirForIndex(dir string) string {
	for current := filepath.Clean(dir); ; current = filepath.Dir(current) {
		if filepath.Base(current) == "search" {
			return filepath.Dir(current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
	}
}

func loadManifest(dir string) (manifest, error) {
	var found manifest
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return manifest{}, err
	}
	if err := sonic.Unmarshal(data, &found); err != nil {
		return manifest{}, err
	}
	switch found.Version {
	case legacyIndexVersion:
		if found.VectorStore != "" {
			return manifest{}, fmt.Errorf("index version %d cannot use vector store %q", found.Version, found.VectorStore)
		}
	case indexVersion:
		if found.VectorStore != sharedVectorStoreVersion {
			return manifest{}, fmt.Errorf("index version %d vector store = %q, want %q", found.Version, found.VectorStore, sharedVectorStoreVersion)
		}
	default:
		return manifest{}, fmt.Errorf("index version = %d, want %d or %d", found.Version, legacyIndexVersion, indexVersion)
	}
	return found, nil
}

func indexBuiltSince(dir string, since time.Time) bool {
	found, err := loadManifest(dir)
	return err == nil && !found.CreatedAt.Before(since)
}

func withIndexLock(ctx context.Context, indexDir string, fn func() error) error {
	lock, err := lockIndex(ctx, indexDir)
	if err != nil {
		return err
	}
	fnErr := fn()
	return errors.Join(fnErr, lock.Unlock())
}

func queryLockDir(indexDir, normalized, queryTextHash, model string, dimensions int, source Source, root, resolvedRev string) string {
	key := strings.Join([]string{
		normalized,
		queryTextHash,
		model,
		fmt.Sprintf("%d", dimensions),
		source.Mode,
		root,
		source.Remote,
		resolvedRev,
	}, "\x00")
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(indexDir, "query-locks", hex.EncodeToString(sum[:])[:16])
}

func loadBinaryVectors(dir string) ([]vectorRecord, error) {
	index, err := loadVectorIndexRecords(dir)
	if err != nil {
		return nil, err
	}
	vectorData, err := os.ReadFile(filepath.Join(dir, "vectors.f32"))
	if err != nil {
		return nil, err
	}
	records := make([]vectorRecord, len(index))
	for i, entry := range index {
		if entry.VectorKey != "" || entry.VectorChecksum != 0 {
			return nil, fmt.Errorf("vectors.index.json entry %d contains a shared vector reference", i)
		}
		if entry.Dimensions < 1 {
			return nil, fmt.Errorf("vectors.index.json entry %d has invalid dimensions %d", i, entry.Dimensions)
		}
		byteLen := int64(entry.Dimensions * 4)
		if entry.Offset < 0 || entry.Offset+byteLen > int64(len(vectorData)) {
			return nil, fmt.Errorf("vectors.index.json entry %d offset out of range", i)
		}
		start := int(entry.Offset)
		vector := make([]float64, entry.Dimensions)
		for dim := range entry.Dimensions {
			bits := binary.LittleEndian.Uint32(vectorData[start+dim*4 : start+dim*4+4])
			vector[dim] = float64(math.Float32frombits(bits))
		}
		records[i] = vectorRecordFromIndex(entry, vector)
	}
	return records, nil
}

func loadLegacyVectors(dir string) ([]vectorRecord, error) {
	data, err := os.ReadFile(filepath.Join(dir, "embeddings.json"))
	if err != nil {
		return nil, err
	}
	var records []vectorRecord
	if err := sonic.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	return records, nil
}

type reuseCandidate struct {
	dir       string
	matches   int
	createdAt time.Time
}

func missingVectorKeys(chunks []Chunk, records []vectorRecord, opts Options) map[string]int {
	available := make(map[string]bool, len(records))
	for _, record := range records {
		if reusableVectorRecord(record, opts) {
			available[record.EmbeddingInputHash] = true
		}
	}
	missing := map[string]int{}
	for _, chunk := range chunks {
		key := embeddingInputHash(chunk.EmbeddingText, opts.EmbeddingMaxInput)
		if !available[key] {
			missing[key]++
		}
	}
	return missing
}

func loadBestReuseCandidate(ctx context.Context, metadataDir, targetDir string, targetKeys map[string]int, opts Options) ([]vectorRecord, error) {
	if opts.Reindex || len(targetKeys) == 0 {
		return nil, nil
	}
	searchRoot := filepath.Join(metadataDir, "search")

	var candidates []reuseCandidate
	err := filepath.WalkDir(searchRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == searchRoot {
				return walkErr
			}
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == "query-locks" {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != "manifest.json" {
			return nil
		}
		dir := filepath.Dir(path)
		if dir == targetDir {
			return nil
		}
		var candidate reuseCandidate
		candidate.dir = dir
		candidateErr := withIndexLock(ctx, dir, func() error {
			found, err := loadManifest(dir)
			if err != nil {
				return err
			}
			if found.EmbeddingModel != opts.EmbeddingModel || found.Dimensions != opts.EmbeddingDimensions {
				return nil
			}
			records, err := loadVectorIndexRecords(dir)
			if err != nil {
				return err
			}
			matched := map[string]bool{}
			for _, record := range records {
				if !reusableVectorIndexRecord(record, opts) {
					continue
				}
				key := record.EmbeddingInputHash
				if targetKeys[key] > 0 {
					matched[key] = true
				}
			}
			for key := range matched {
				candidate.matches += targetKeys[key]
			}
			candidate.createdAt = found.CreatedAt
			return nil
		})
		if candidateErr != nil {
			if errors.Is(candidateErr, context.Canceled) || errors.Is(candidateErr, context.DeadlineExceeded) {
				return candidateErr
			}
			return nil
		}
		if candidate.matches > 0 {
			candidates = append(candidates, candidate)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(candidates, func(a, b reuseCandidate) int {
		if diff := cmp.Compare(b.matches, a.matches); diff != 0 {
			return diff
		}
		return b.createdAt.Compare(a.createdAt)
	})
	for _, candidate := range candidates {
		var records []vectorRecord
		if err := withIndexLock(ctx, candidate.dir, func() error {
			var err error
			records, err = loadVectors(candidate.dir)
			return err
		}); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			continue
		}
		if opts.DebugLog != nil {
			opts.DebugLog("search_cache_seed",
				slog.String("index_dir", candidate.dir),
				slog.Int("matching_chunks", candidate.matches),
			)
		}
		return records, nil
	}
	return nil, nil
}

func reuseVectors(chunks []Chunk, old []vectorRecord, opts Options) (map[string][]float64, []vectorRecord, int) {
	vectors := map[string][]float64{}
	if opts.Reindex {
		return vectors, nil, 0
	}
	byKey := map[string]vectorRecord{}
	for _, record := range old {
		if !reusableVectorRecord(record, opts) {
			continue
		}
		byKey[record.EmbeddingInputHash] = record
	}
	var records []vectorRecord
	for _, chunk := range chunks {
		record, ok := byKey[embeddingInputHash(chunk.EmbeddingText, opts.EmbeddingMaxInput)]
		if !ok {
			continue
		}
		vectors[chunk.ID] = record.Vector
		records = append(records, vectorRecordForChunk(chunk, record.Vector, opts))
	}
	return vectors, records, len(records)
}

func reusableVectorRecord(record vectorRecord, opts Options) bool {
	return record.EmbeddingInputHash != "" && preservableVectorRecord(record, opts)
}

func preservableVectorRecord(record vectorRecord, opts Options) bool {
	return record.EmbeddingModel == opts.EmbeddingModel &&
		record.Dimensions == opts.EmbeddingDimensions &&
		len(record.Vector) == record.Dimensions
}

func reusableVectorIndexRecord(record vectorIndexRecord, opts Options) bool {
	return record.EmbeddingInputHash != "" &&
		record.EmbeddingModel == opts.EmbeddingModel &&
		record.Dimensions == opts.EmbeddingDimensions
}

func preserveSharedFilteredRecords(records, old []vectorRecord, files []fileContent, opts Options) []vectorRecord {
	if len(old) == 0 {
		return records
	}
	existing := make(map[string]bool, len(records))
	addRecordVectorKeys(existing, records)
	existingLocations := make(map[string]bool, len(records))
	for _, record := range records {
		existingLocations[cacheRecordLocationKey(record)] = true
	}
	var current map[string]bool
	for _, record := range old {
		if !preservableVectorRecord(record, opts) {
			continue
		}
		location := cacheRecordLocationKey(record)
		if existingLocations[location] {
			continue
		}
		key := cacheRecordKey(record)
		if current == nil {
			current = currentVectorKeys(files, opts)
		}
		if !current[key] {
			continue
		}
		if existing[key] {
			continue
		}
		existing[key] = true
		existingLocations[location] = true
		records = append(records, record)
	}
	return records
}

func scopedRecordsChanged(records, old []vectorRecord, scope []string, opts Options) bool {
	oldScoped := 0
	current := cacheRecordKeySet(records)
	for _, record := range old {
		if !pathInScope(record.Path, scope) || !preservableVectorRecord(record, opts) {
			continue
		}
		oldScoped++
		if !current[cacheRecordKey(record)] {
			return true
		}
	}
	return oldScoped != len(records)
}

func currentVectorKeys(files []fileContent, opts Options) map[string]bool {
	keys := map[string]bool{}
	for _, file := range files {
		for _, chunk := range chunksForFile(file) {
			keys[chunkCacheRecordKey(chunk, opts)] = true
			keys[legacyChunkCacheRecordKey(chunk, opts)] = true
		}
	}
	return keys
}

func preserveOutOfScopeRevisionRecords(records, old []vectorRecord, scope []string, opts Options) []vectorRecord {
	if len(old) == 0 {
		return records
	}
	existing := make(map[string]bool, len(records))
	addRecordVectorKeys(existing, records)
	for _, record := range old {
		if pathInScope(record.Path, scope) || !preservableVectorRecord(record, opts) {
			continue
		}
		key := cacheRecordKey(record)
		if existing[key] {
			continue
		}
		existing[key] = true
		records = append(records, record)
	}
	return records
}

func preserveOutOfScopeFilesystemRecords(records, old []vectorRecord, root string, scope []string, opts Options) []vectorRecord {
	if len(old) == 0 {
		return records
	}
	existing := make(map[string]bool, len(records))
	addRecordVectorKeys(existing, records)
	candidates := make([]vectorRecord, 0, len(old))
	var paths []string
	for _, record := range old {
		if pathInScope(record.Path, scope) || !preservableVectorRecord(record, opts) {
			continue
		}
		candidates = append(candidates, record)
		paths = append(paths, record.Path)
	}
	ignoreMatcher := filesystemIgnoreMatcherForPaths(root, paths)
	currentKeysByPath := map[string]map[string]bool{}
	for _, record := range candidates {
		key := cacheRecordKey(record)
		currentKeys, ok := currentKeysByPath[record.Path]
		if !ok {
			currentKeys = currentFilesystemVectorKeys(root, ignoreMatcher, record, opts)
			currentKeysByPath[record.Path] = currentKeys
		}
		if !currentKeys[key] {
			continue
		}
		if existing[key] {
			continue
		}
		existing[key] = true
		records = append(records, record)
	}
	return records
}

func cacheRecordKeySet(records []vectorRecord) map[string]bool {
	keys := make(map[string]bool, len(records))
	addRecordVectorKeys(keys, records)
	return keys
}

func addRecordVectorKeys(keys map[string]bool, records []vectorRecord) {
	for _, record := range records {
		keys[cacheRecordKey(record)] = true
	}
}

func currentFilesystemVectorKeys(root string, ignoreMatcher gitignore.Matcher, record vectorRecord, opts Options) map[string]bool {
	path := filepath.ToSlash(record.Path)
	if shouldSkipPath(path, nil) || revisionPathIgnored(ignoreMatcher, path) {
		return nil
	}
	abs := filepath.Join(root, filepath.FromSlash(path))
	info, err := os.Lstat(abs)
	if err != nil {
		return nil
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		return nil
	}
	if info.Size() != record.Size || info.ModTime().UnixNano() != record.MTimeUnixNano {
		return nil
	}
	data, err := os.ReadFile(abs)
	if err != nil || isBinary(data) || !isIndexableText(path, data) {
		return nil
	}
	return currentVectorKeys([]fileContent{{
		path:   path,
		source: "filesystem",
		text:   string(data),
		size:   info.Size(),
		mtime:  info.ModTime(),
	}}, opts)
}

func missingChunks(chunks []Chunk, vectors map[string][]float64) []Chunk {
	var missing []Chunk
	for _, chunk := range chunks {
		if vectors[chunk.ID] == nil {
			missing = append(missing, chunk)
		}
	}
	return missing
}

func missingEmbeddingTexts(chunks []Chunk) []string {
	texts := make([]string, len(chunks))
	for i, chunk := range chunks {
		texts[i] = chunk.EmbeddingText
	}
	return texts
}

func embedTexts(ctx context.Context, client openai.EmbeddingClient, opts Options, texts []string) ([][]float64, int, error) {
	texts = cappedEmbeddingInputs(texts, opts.EmbeddingMaxInput)
	type batch struct {
		start int
		end   int
	}
	var batches []batch
	batchInputs := embeddingBatchInputs(opts)
	batchMaxChars := embeddingBatchMaxChars(opts)
	for start := 0; start < len(texts); {
		end := embeddingBatchEnd(texts, start, batchInputs, batchMaxChars)
		batches = append(batches, batch{start: start, end: end})
		start = end
	}
	if len(batches) == 0 {
		return nil, 0, nil
	}

	batchVectors := make([][][]float64, len(batches))
	batchDimensions := make([]int, len(batches))
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(embeddingConcurrency(opts))
	for idx, batch := range batches {
		group.Go(func() error {
			response, err := embedBatch(ctx, client, opts, texts[batch.start:batch.end])
			if err != nil {
				return fmt.Errorf("embedding request failed: %w", err)
			}
			batchVectors[idx] = response.Vectors
			batchDimensions[idx] = response.Dimensions
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, 0, err
	}

	vectors := make([][]float64, 0, len(texts))
	dimensions := 0
	for i, vectorsForBatch := range batchVectors {
		if dimensions == 0 {
			dimensions = batchDimensions[i]
		}
		if batchDimensions[i] != dimensions {
			return nil, 0, fmt.Errorf("embedding dimensions mismatch: %d and %d", dimensions, batchDimensions[i])
		}
		vectors = append(vectors, vectorsForBatch...)
	}
	return vectors, dimensions, nil
}

func cappedEmbeddingInputs(texts []string, maxChars int) []string {
	inputs := make([]string, len(texts))
	for i, text := range texts {
		inputs[i] = cappedEmbeddingInput(text, maxChars)
	}
	return inputs
}

func cappedEmbeddingInput(text string, maxChars int) string {
	if maxChars == 0 {
		maxChars = DefaultEmbeddingMaxInputChars
	}
	chars := 0
	for i := range text {
		if chars == maxChars {
			return text[:i]
		}
		chars++
	}
	return text
}

func embedBatch(ctx context.Context, client openai.EmbeddingClient, opts Options, texts []string) (openai.EmbeddingResponse, error) {
	response, err := client.CreateEmbeddings(ctx, openai.EmbeddingRequest{
		Model:      opts.EmbeddingModel,
		Dimensions: opts.EmbeddingDimensions,
		BaseURL:    opts.BaseURL,
		APIKey:     opts.APIKey,
		Inputs:     texts,
	})
	if err == nil || len(texts) == 1 {
		return response, err
	}
	mid := len(texts) / 2
	left, leftErr := embedBatch(ctx, client, opts, texts[:mid])
	if leftErr != nil {
		return openai.EmbeddingResponse{}, leftErr
	}
	right, rightErr := embedBatch(ctx, client, opts, texts[mid:])
	if rightErr != nil {
		return openai.EmbeddingResponse{}, rightErr
	}
	if left.Dimensions != right.Dimensions {
		return openai.EmbeddingResponse{}, fmt.Errorf("embedding dimensions mismatch: %d and %d", left.Dimensions, right.Dimensions)
	}
	return openai.EmbeddingResponse{
		Model:      left.Model,
		Vectors:    append(left.Vectors, right.Vectors...),
		Dimensions: left.Dimensions,
	}, nil
}

func embeddingBatchInputs(opts Options) int {
	if opts.EmbeddingBatchInputs > 0 {
		return opts.EmbeddingBatchInputs
	}
	return DefaultEmbeddingBatchInputs
}

func embeddingBatchMaxChars(opts Options) int {
	if opts.EmbeddingBatchMaxChars > 0 {
		return opts.EmbeddingBatchMaxChars
	}
	return DefaultEmbeddingBatchMaxChars
}

func embeddingConcurrency(opts Options) int {
	if opts.EmbeddingConcurrency > 0 {
		return opts.EmbeddingConcurrency
	}
	return min(max(runtime.GOMAXPROCS(0), 1), 8)
}

func embeddingBatchEnd(texts []string, start, maxInputs, maxChars int) int {
	if maxInputs < 1 {
		maxInputs = DefaultEmbeddingBatchInputs
	}
	if maxChars < 1 {
		maxChars = DefaultEmbeddingBatchMaxChars
	}
	end := start
	chars := 0
	for end < len(texts) && end-start < maxInputs {
		nextChars := len(texts[end])
		if end > start && chars+nextChars > maxChars {
			break
		}
		chars += nextChars
		end++
	}
	if end == start {
		return start + 1
	}
	return end
}

func saveIndex(ctx context.Context, metadataDir, dir string, source Source, root, resolvedRev, model string, dimensions int, records []vectorRecord, forceVectorKeys map[string]bool) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.Remove(manifestPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("invalidate search index manifest: %w", err)
	} else if err == nil {
		if err := syncDirectory(dir); err != nil {
			return fmt.Errorf("sync invalidated search index: %w", err)
		}
	}
	if err := writeSharedVectorIndex(ctx, metadataDir, dir, records, forceVectorKeys); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync search index payloads: %w", err)
	}
	if err := writeJSONSync(manifestPath, manifest{
		Version:        indexVersion,
		Mode:           source.Mode,
		Root:           root,
		Remote:         source.Remote,
		ResolvedRev:    resolvedRev,
		EmbeddingModel: model,
		Dimensions:     dimensions,
		CreatedAt:      time.Now().UTC(),
		FileCount:      uniquePathCountFrom(records, func(record vectorRecord) string { return record.Path }),
		ChunkCount:     len(records),
		VectorStore:    sharedVectorStoreVersion,
	}); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		removeErr := os.Remove(manifestPath)
		if removeErr == nil {
			removeErr = syncDirectory(dir)
		}
		return errors.Join(fmt.Errorf("publish search index manifest: %w", err), removeErr)
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := sonic.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func writeJSONSync(path string, value any) (err error) {
	data, err := sonic.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

type scoredChunk struct {
	chunk             Chunk
	cosine            float64
	vectorRelatedness float64
	textScore         float64
	pathScore         float64
	symbolScore       float64
	lexicalScore      float64
	rank              float64
}

type scoreCandidate struct {
	item       scoredChunk
	textTerms  map[string]int
	textLength int
}

func scoreChunks(chunks []Chunk, vectors map[string][]float64, queryVector []float64, query string, minRelatedness float64, skipChunk func(Chunk) bool) []scoredChunk {
	queryTerms := uniqueSearchTerms(searchTerms(query))
	querySet := searchTermSet(queryTerms)
	candidates := make([]scoreCandidate, 0, len(chunks))
	var scored []scoredChunk
	for _, chunk := range chunks {
		if skipChunk != nil && skipChunk(chunk) {
			continue
		}
		vector := vectors[chunk.ID]
		if len(vector) == 0 {
			continue
		}
		cosine := cosineSimilarity(queryVector, vector)
		relatedness := math.Max(1e-9, min(1, max(0, (cosine+1)/2)))
		if relatedness < minRelatedness {
			continue
		}
		item := scoredChunk{
			chunk:             chunk,
			cosine:            cosine,
			vectorRelatedness: relatedness,
			rank:              relatedness,
		}
		if len(queryTerms) == 0 {
			scored = append(scored, item)
			continue
		}
		textTerms := searchTerms(chunk.text)
		candidates = append(candidates, scoreCandidate{
			item:       item,
			textTerms:  matchingTermCounts(textTerms, querySet),
			textLength: len(textTerms),
		})
	}
	if len(queryTerms) == 0 {
		return scored
	}
	scoreLexicalCandidates(candidates, queryTerms)
	scored = make([]scoredChunk, len(candidates))
	for i, candidate := range candidates {
		scored[i] = candidate.item
	}
	return scored
}

func scoreLexicalCandidates(candidates []scoreCandidate, queryTerms []string) {
	if len(candidates) == 0 || len(queryTerms) == 0 {
		return
	}
	df := make(map[string]int, len(queryTerms))
	totalTextLength := 0
	for _, candidate := range candidates {
		totalTextLength += candidate.textLength
		for _, term := range queryTerms {
			if candidate.textTerms[term] > 0 {
				df[term]++
			}
		}
	}
	avgTextLength := float64(totalTextLength) / float64(len(candidates))
	if avgTextLength == 0 {
		avgTextLength = 1
	}
	for i := range candidates {
		candidate := &candidates[i]
		candidate.item.textScore = bm25Score(candidate.textTerms, candidate.textLength, avgTextLength, df, len(candidates), queryTerms)
		candidate.item.pathScore = fieldMatchScore(searchTerms(candidate.item.chunk.Path), queryTerms)
		candidate.item.symbolScore = symbolMatchScore(candidate.item.chunk.Symbol, queryTerms)
		candidate.item.lexicalScore = min(1, candidate.item.textScore*0.45+candidate.item.pathScore*0.30+candidate.item.symbolScore*0.35)
		candidate.item.rank = candidate.item.vectorRelatedness + (1-candidate.item.vectorRelatedness)*candidate.item.lexicalScore
	}
}

func bm25Score(counts map[string]int, textLength int, avgTextLength float64, df map[string]int, docCount int, queryTerms []string) float64 {
	if textLength == 0 || docCount == 0 {
		return 0
	}
	const (
		k1 = 1.2
		b  = 0.75
	)
	var score float64
	for _, term := range queryTerms {
		freq := counts[term]
		if freq == 0 {
			continue
		}
		idf := math.Log(1 + (float64(docCount-df[term])+0.5)/(float64(df[term])+0.5))
		denom := float64(freq) + k1*(1-b+b*float64(textLength)/avgTextLength)
		score += idf * (float64(freq) * (k1 + 1) / denom)
	}
	return score / (score + 1)
}

func symbolMatchScore(symbol *Symbol, queryTerms []string) float64 {
	if symbol == nil {
		return 0
	}
	return fieldMatchScore(searchTerms(symbol.Type+" "+symbol.Name), queryTerms)
}

func fieldMatchScore(fieldTerms, queryTerms []string) float64 {
	if len(fieldTerms) == 0 || len(queryTerms) == 0 {
		return 0
	}
	fieldSet := make(map[string]bool, len(fieldTerms))
	for _, term := range fieldTerms {
		fieldSet[term] = true
	}
	matches := 0
	for _, term := range queryTerms {
		if fieldSet[term] {
			matches++
		}
	}
	return float64(matches) / float64(len(queryTerms))
}

func matchingTermCounts(terms []string, querySet map[string]bool) map[string]int {
	counts := make(map[string]int, len(querySet))
	for _, term := range terms {
		if querySet[term] {
			counts[term]++
		}
	}
	return counts
}

func sortResults(results []scoredChunk) {
	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.rank != b.rank {
			return a.rank > b.rank
		}
		aRange := a.chunk.EndLine - a.chunk.StartLine
		bRange := b.chunk.EndLine - b.chunk.StartLine
		if aRange != bRange {
			return aRange < bRange
		}
		if a.chunk.Path != b.chunk.Path {
			return a.chunk.Path < b.chunk.Path
		}
		return a.chunk.StartLine < b.chunk.StartLine
	})
}

func renderResults(scored []scoredChunk) []Result {
	results := make([]Result, len(scored))
	for i, item := range scored {
		chunk := item.chunk
		results[i] = Result{
			Relatedness: item.rank,
			Range:       fmt.Sprintf("%s:%d-%d", chunk.Path, chunk.StartLine, chunk.EndLine),
			Symbol:      chunk.Symbol,
			Scores: ResultScores{
				Cosine:            item.cosine,
				VectorRelatedness: item.vectorRelatedness,
				Text:              item.textScore,
				Path:              item.pathScore,
				Symbol:            item.symbolScore,
				Lexical:           item.lexicalScore,
				Rank:              item.rank,
			},
			Excerpt:   excerpt(chunk),
			Path:      chunk.Path,
			StartLine: chunk.StartLine,
		}
	}
	return results
}

func replayFor(dir, normalized, queryTextHash string, queryVector []float64, model string, dimensions int, source Source, root, resolvedRev string, filters Filters) Replay {
	entries, err := loadHistory(dir)
	if err != nil {
		return Replay{Mode: "none"}
	}
	var similar *historyEntry
	for i := range entries {
		entry := entries[i]
		if entry.EmbeddingModel != model || entry.Dimensions != dimensions || entry.SourceMode != source.Mode {
			continue
		}
		if source.Mode == "filesystem" && entry.Root != root {
			continue
		}
		if source.Mode == "revision" && entry.ResolvedRev != resolvedRev {
			continue
		}
		if source.Mode == "remote" && (entry.Remote != source.Remote || entry.ResolvedRev != resolvedRev) {
			continue
		}
		if !sameFilters(entry.Filters, filters) {
			continue
		}
		if entry.EmbeddingOnly {
			continue
		}
		if entry.Normalized == normalized && entry.QueryTextHash == queryTextHash {
			from := entry.Query
			return Replay{Mode: "hit", From: &from}
		}
		if similar == nil && cosineSimilarity(queryVector, entry.QueryEmbedding) >= 0.90 {
			similar = &entry
		}
	}
	if similar != nil {
		from := similar.Query
		return Replay{Mode: "similar", From: &from}
	}
	return Replay{Mode: "none"}
}

func sameFilters(stored *Filters, current Filters) bool {
	if stored == nil {
		return !current.Code
	}
	return stored.Code == current.Code && stored.NoTests == current.NoTests && slices.Equal(stored.Scope, current.Scope)
}

func cachedQueryEmbedding(dir, normalized, queryTextHash, model string, dimensions int, source Source, root, resolvedRev string) ([]float64, int, bool) {
	entries, err := loadHistory(dir)
	if err != nil {
		return nil, 0, false
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.Normalized != normalized || entry.QueryTextHash != queryTextHash || entry.EmbeddingModel != model || entry.Dimensions != dimensions || entry.SourceMode != source.Mode {
			continue
		}
		if source.Mode == "filesystem" && entry.Root != root {
			continue
		}
		if source.Mode == "revision" && entry.ResolvedRev != resolvedRev {
			continue
		}
		if source.Mode == "remote" && (entry.Remote != source.Remote || entry.ResolvedRev != resolvedRev) {
			continue
		}
		if len(entry.QueryEmbedding) == 0 {
			continue
		}
		return entry.QueryEmbedding, len(entry.QueryEmbedding), true
	}
	return nil, 0, false
}

func loadHistory(dir string) ([]historyEntry, error) {
	data, err := os.ReadFile(filepath.Join(dir, "history.json"))
	if err != nil {
		return nil, err
	}
	var entries []historyEntry
	return entries, sonic.Unmarshal(data, &entries)
}

func appendHistory(dir string, entry historyEntry) error {
	entries, _ := loadHistory(dir)
	entries = append(entries, entry)
	if len(entries) > 100 {
		entries = entries[len(entries)-100:]
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "history.json"), entries)
}

func resultIDs(results []scoredChunk) []string {
	ids := make([]string, len(results))
	for i, result := range results {
		ids[i] = result.chunk.ID
	}
	return ids
}

func excerpt(chunk Chunk) string {
	if chunk.text == "" {
		return ""
	}
	lines := splitLines(chunk.text)
	if len(lines) > maxExcerptLines {
		lines = lines[:maxExcerptLines]
	}
	var b strings.Builder
	for i, line := range lines {
		if b.Len() >= maxExcerptBytes {
			break
		}
		rendered := fmt.Sprintf("%d: %s", chunk.StartLine+i, line)
		if b.Len()+len(rendered)+1 > maxExcerptBytes {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(rendered)
	}
	return b.String()
}

func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return []string{""}
	}
	return strings.Split(text, "\n")
}

func isBinary(data []byte) bool {
	prefix := data
	if len(prefix) > 8000 {
		prefix = prefix[:8000]
	}
	return slices.Contains(prefix, 0)
}

func isIndexableText(path string, data []byte) bool {
	if isCodePath(path) {
		return true
	}
	mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	mediaType, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return true
	}
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/ecmascript",
		"application/javascript",
		"application/json",
		"application/sql",
		"application/toml",
		"application/yaml",
		"application/x-ndjson",
		"application/x-sh",
		"application/x-yaml",
		"application/xml":
		return true
	}
	return strings.HasPrefix(mediaType, "application/") &&
		(strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml"))
}

func shouldSkipDir(name string) bool {
	return strings.HasPrefix(name, ".")
}

func shouldSkipFile(name string) bool {
	return strings.HasPrefix(name, ".")
}

func shouldSkipPath(path string, scope []string) bool {
	parts := pathParts(path)
	for i, part := range parts {
		if shouldSkipDir(part) {
			prefix := strings.Join(parts[:i+1], "/")
			if scopeAllowsHiddenPathPrefix(prefix, scope) {
				continue
			}
			return true
		}
	}
	return false
}

func scopeAllowsHiddenPathPrefix(prefix string, scope []string) bool {
	for _, item := range scope {
		if item == prefix || strings.HasPrefix(item, prefix+"/") {
			return true
		}
		if pathHasSkippedDir(item) && strings.HasPrefix(prefix, item+"/") {
			return true
		}
	}
	return false
}

func filesystemIgnoreMatcher(root string, scope []string) gitignore.Matcher {
	patterns := slices.Clone(defaultSearchIgnorePatterns)
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			relDir := ""
			if path != root {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return nil
				}
				rel = filepath.ToSlash(rel)
				if !scopeMayContainDir(rel, scope) {
					return filepath.SkipDir
				}
				if shouldSkipDir(entry.Name()) && shouldSkipPath(rel, scope) {
					return filepath.SkipDir
				}
				if gitignore.NewMatcher(patterns).Match(pathParts(rel), true) {
					return filepath.SkipDir
				}
				relDir = rel
			}
			patterns = appendSearchIgnoreFilesFromDir(patterns, path, relDir)
			return nil
		}
		return nil
	})
	return gitignore.NewMatcher(patterns)
}

func filesystemIgnoreMatcherForPaths(root string, paths []string) gitignore.Matcher {
	if len(paths) == 0 {
		return gitignore.NewMatcher(nil)
	}
	dirs := map[string]bool{"": true}
	for _, path := range paths {
		dir := filepath.ToSlash(filepath.Dir(filepath.ToSlash(path)))
		if dir == "." {
			continue
		}
		parts := pathParts(dir)
		for i := range parts {
			dirs[strings.Join(parts[:i+1], "/")] = true
		}
	}
	patterns := slices.Clone(defaultSearchIgnorePatterns)
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		abs := root
		if dir != "" {
			abs = filepath.Join(root, filepath.FromSlash(dir))
		}
		patterns = appendSearchIgnoreFilesFromDir(patterns, abs, dir)
	}
	return gitignore.NewMatcher(patterns)
}

func appendSearchIgnoreFilesFromDir(patterns []gitignore.Pattern, dir, relDir string) []gitignore.Pattern {
	var base []string
	if relDir != "" {
		base = pathParts(relDir)
	}
	for _, name := range searchIgnoreFileOrder {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		patterns = appendSearchIgnorePatterns(patterns, string(data), base)
	}
	return patterns
}

func revisionIgnoreMatcher(repo *gitctx.Repository, rev string, scope []string) (gitignore.Matcher, error) {
	type ignoreFile struct {
		path string
		dir  string
		name string
		text string
	}
	var ignoreFiles []ignoreFile
	err := repo.WalkCommitTextFiles(rev, 0, func(path string) bool {
		if !searchIgnoreFileNames[filepath.Base(path)] {
			return false
		}
		dir := filepath.ToSlash(filepath.Dir(path))
		return scopeMayContainDir(dir, scope) && !shouldSkipPath(dir, scope)
	}, func(file gitctx.CommitFile) error {
		dir := ""
		if found := filepath.ToSlash(filepath.Dir(file.Path)); found != "." {
			dir = found
		}
		ignoreFiles = append(ignoreFiles, ignoreFile{
			path: filepath.ToSlash(file.Path),
			dir:  dir,
			name: filepath.Base(file.Path),
			text: file.Text,
		})
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(ignoreFiles, func(a, b ignoreFile) int {
		if order := strings.Compare(a.dir, b.dir); order != 0 {
			return order
		}
		if order := cmp.Compare(ignoreFileOrder(a.name), ignoreFileOrder(b.name)); order != 0 {
			return order
		}
		return strings.Compare(a.path, b.path)
	})

	patterns := slices.Clone(defaultSearchIgnorePatterns)
	for _, file := range ignoreFiles {
		if file.dir != "" && gitignore.NewMatcher(patterns).Match(pathParts(file.dir), true) {
			continue
		}
		var base []string
		if file.dir != "" {
			base = pathParts(file.dir)
		}
		patterns = appendSearchIgnorePatterns(patterns, file.text, base)
	}
	return gitignore.NewMatcher(patterns), nil
}

func ignoreFileOrder(name string) int {
	for i, candidate := range searchIgnoreFileOrder {
		if name == candidate {
			return i
		}
	}
	return len(searchIgnoreFileOrder)
}

func appendSearchIgnorePatterns(patterns []gitignore.Pattern, text string, base []string) []gitignore.Pattern {
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, gitignore.ParsePattern(line, base))
	}
	return patterns
}

func revisionPathIgnored(matcher gitignore.Matcher, path string) bool {
	parts := pathParts(path)
	for i := 1; i < len(parts); i++ {
		if matcher.Match(parts[:i], true) {
			return true
		}
	}
	return matcher.Match(parts, false)
}

func pathHasSkippedDir(path string) bool {
	for _, part := range pathParts(path) {
		if shouldSkipDir(part) {
			return true
		}
	}
	return false
}

func scopeUsesSkippedPath(scope []string) bool {
	for _, item := range scope {
		if pathHasSkippedDir(item) {
			return true
		}
	}
	return false
}

func pathParts(path string) []string {
	path = strings.Trim(filepath.ToSlash(path), "/")
	if path == "" || path == "." {
		return nil
	}
	return strings.Split(path, "/")
}

func normalizeScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	normalized := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if filepath.IsAbs(scope) {
			return nil, fmt.Errorf("--scope path %q must be relative", scope)
		}
		scope = filepath.ToSlash(filepath.Clean(scope))
		if scope == "." {
			return nil, nil
		}
		if strings.HasPrefix(scope, "../") || scope == ".." {
			return nil, fmt.Errorf("--scope path %q must stay under the search root", scope)
		}
		scope = strings.Trim(scope, "/")
		if scope == "" {
			return nil, nil
		}
		if seen[scope] {
			continue
		}
		seen[scope] = true
		normalized = append(normalized, scope)
	}
	if len(normalized) == 0 {
		return nil, errors.New("--scope requires at least one relative path")
	}
	slices.Sort(normalized)
	return normalized, nil
}

func pathInScope(path string, scope []string) bool {
	if len(scope) == 0 {
		return true
	}
	path = strings.Trim(filepath.ToSlash(path), "/")
	if path == "" || path == "." {
		return true
	}
	for _, item := range scope {
		if path == item || strings.HasPrefix(path, item+"/") {
			return true
		}
	}
	return false
}

func scopeMayContainDir(dir string, scope []string) bool {
	if len(scope) == 0 {
		return true
	}
	dir = strings.Trim(filepath.ToSlash(dir), "/")
	if dir == "" || dir == "." {
		return true
	}
	for _, item := range scope {
		if item == dir || strings.HasPrefix(item, dir+"/") || strings.HasPrefix(dir, item+"/") {
			return true
		}
	}
	return false
}

func indexDir(base, mode, root, resolvedRev string, filters Filters) string {
	var filter string
	if scopeUsesSkippedPath(filters.Scope) {
		sum := sha256.Sum256([]byte(strings.Join(filters.Scope, "\x00")))
		filter = "scope-" + hex.EncodeToString(sum[:])[:16]
	}
	if mode == "revision" || mode == "remote" {
		return filepath.Join(base, "search", "revs", resolvedRev, filter)
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(base, "search", "fs", hex.EncodeToString(sum[:])[:16], filter)
}

func isTestPath(path string) bool {
	parts := pathParts(path)
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts[:len(parts)-1] {
		if isTestDirName(strings.ToLower(part)) {
			return true
		}
	}
	name := parts[len(parts)-1]
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	for part := range strings.FieldsFuncSeq(strings.ToLower(stem), func(r rune) bool {
		return r == '.' || r == '-' || r == '_'
	}) {
		switch part {
		case "test", "tests", "spec", "specs", "unittest", "unittests":
			return true
		}
	}
	return isClassStyleTestName(name, stem)
}

func isTestDirName(name string) bool {
	switch name {
	case "test", "tests", "__tests__", "spec", "specs", "__specs__",
		"integration_test", "integration_tests", "integration-test", "integration-tests":
		return true
	default:
		return false
	}
}

func isClassStyleTestName(name, stem string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".cs", ".fs", ".fsx", ".groovy", ".java", ".kt", ".kts", ".m", ".mm", ".php", ".scala", ".swift", ".vb":
	default:
		return false
	}
	if strings.HasSuffix(stem, "Test") || strings.HasSuffix(stem, "Tests") ||
		strings.HasSuffix(stem, "TestCase") {
		return true
	}
	if !strings.HasPrefix(stem, "Test") {
		return false
	}
	next, _ := utf8.DecodeRuneInString(stem[len("Test"):])
	return unicode.IsUpper(next) || unicode.IsDigit(next)
}

func vectorRecordForChunk(chunk Chunk, vector []float64, opts Options) vectorRecord {
	return vectorRecord{
		ChunkID:            chunk.ID,
		Path:               chunk.Path,
		Source:             chunk.Source,
		Blob:               chunk.Blob,
		StartLine:          chunk.StartLine,
		EndLine:            chunk.EndLine,
		ContentHash:        chunk.ContentHash,
		EmbeddingInputHash: embeddingInputHash(chunk.EmbeddingText, opts.EmbeddingMaxInput),
		EmbeddingModel:     opts.EmbeddingModel,
		Dimensions:         len(vector),
		Size:               chunk.Size,
		MTimeUnixNano:      chunk.MTimeUnixNano,
		Vector:             vector,
	}
}

func embeddingInputHash(text string, maxChars int) string {
	sum := sha256.Sum256([]byte(cappedEmbeddingInput(text, maxChars)))
	return hex.EncodeToString(sum[:])
}

func chunkCacheRecordKey(chunk Chunk, opts Options) string {
	return chunkCacheRecordKeyWithHash(chunk, opts, embeddingInputHash(chunk.EmbeddingText, opts.EmbeddingMaxInput))
}

func legacyChunkCacheRecordKey(chunk Chunk, opts Options) string {
	return chunkCacheRecordKeyWithHash(chunk, opts, "")
}

func chunkCacheRecordKeyWithHash(chunk Chunk, opts Options, inputHash string) string {
	return fmt.Sprintf("%s:%s:%s:%d:%d:%d:%d:%s:%s:%s:%d",
		chunk.Source,
		chunk.Path,
		chunk.Blob,
		chunk.StartLine,
		chunk.EndLine,
		chunk.Size,
		chunk.MTimeUnixNano,
		chunk.ContentHash,
		inputHash,
		opts.EmbeddingModel,
		opts.EmbeddingDimensions,
	)
}

func cacheRecordLocationKey(record vectorRecord) string {
	return fmt.Sprintf("%s:%s:%s:%d:%d:%d:%d:%s:%s:%d",
		record.Source,
		record.Path,
		record.Blob,
		record.StartLine,
		record.EndLine,
		record.Size,
		record.MTimeUnixNano,
		record.ContentHash,
		record.EmbeddingModel,
		record.Dimensions,
	)
}

func cacheRecordKey(record vectorRecord) string {
	return fmt.Sprintf("%s:%s:%s:%d:%d:%d:%d:%s:%s:%s:%d",
		record.Source,
		record.Path,
		record.Blob,
		record.StartLine,
		record.EndLine,
		record.Size,
		record.MTimeUnixNano,
		record.ContentHash,
		record.EmbeddingInputHash,
		record.EmbeddingModel,
		record.Dimensions,
	)
}

func normalizeQuery(query string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(query)), " "))
}

func searchTerms(value string) []string {
	var terms []string
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		addSearchTerm(&terms, b.String())
		b.Reset()
	}
	runes := []rune(value)
	for i, r := range runes {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if shouldSplitSearchTerm(runes, i) {
				flush()
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return terms
}

func shouldSplitSearchTerm(runes []rune, index int) bool {
	if index == 0 || !unicode.IsUpper(runes[index]) {
		return false
	}
	previous := runes[index-1]
	if unicode.IsLower(previous) || unicode.IsDigit(previous) {
		return true
	}
	if !unicode.IsUpper(previous) || index+1 >= len(runes) {
		return false
	}
	return unicode.IsLower(runes[index+1])
}

func addSearchTerm(terms *[]string, term string) {
	if term == "" {
		return
	}
	*terms = append(*terms, term)
	if len(term) > 3 && strings.HasSuffix(term, "s") && !strings.HasSuffix(term, "ss") {
		singular := strings.TrimSuffix(term, "s")
		if singular != "" {
			*terms = append(*terms, singular)
		}
	}
}

func uniqueSearchTerms(terms []string) []string {
	seen := make(map[string]bool, len(terms))
	unique := make([]string, 0, len(terms))
	for _, term := range terms {
		if seen[term] {
			continue
		}
		seen[term] = true
		unique = append(unique, term)
	}
	return unique
}

func searchTermSet(terms []string) map[string]bool {
	set := make(map[string]bool, len(terms))
	for _, term := range terms {
		set[term] = true
	}
	return set
}

func queryEmbeddingText(query string, maxChars int) string {
	framed := "implementation entrypoint for " + query
	if maxChars == 0 {
		maxChars = DefaultEmbeddingMaxInputChars
	}
	if len([]rune(framed)) > maxChars {
		return cappedEmbeddingInput(query, maxChars)
	}
	return cappedEmbeddingInput(framed, maxChars)
}

func languageForPath(path string) string {
	switch filepath.Ext(path) {
	case ".go":
		return "go"
	case ".md":
		return "markdown"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	case ".js":
		return "javascript"
	case ".ts":
		return "typescript"
	default:
		return ""
	}
}

func isCodePath(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs",
		".py", ".rb", ".rs", ".java", ".kt", ".kts", ".c", ".h", ".cc", ".hh", ".cpp", ".hpp",
		".cs", ".php", ".swift", ".scala", ".sh", ".bash", ".zsh", ".fish", ".ps1",
		".sql", ".html", ".css", ".scss", ".sass", ".vue", ".svelte":
		return true
	default:
		return false
	}
}

func cosineSimilarity(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, aNorm, bNorm float64
	for i := range a {
		dot += a[i] * b[i]
		aNorm += a[i] * a[i]
		bNorm += b[i] * b[i]
	}
	if aNorm == 0 || bNorm == 0 {
		return 0
	}
	return dot / (math.Sqrt(aNorm) * math.Sqrt(bNorm))
}
