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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

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
	indexVersion                  = 2
	batchMaxInputs                = 10
	batchMaxChars                 = 700_000
	DefaultEmbeddingMaxInputChars = 32_000
	maxEmbeddingLineChars         = 4_000
)

type Options struct {
	Root                string
	Rev                 string
	MinRelatedness      float64
	Limit               int
	IndexOnly           bool
	Reindex             bool
	CodeOnly            bool
	EmbeddingModel      string
	EmbeddingDimensions int
	EmbeddingMaxInput   int
	APIKey              string
	BaseURL             string
	Debug               bool
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
	Timings        []Timing
	Total          time.Duration
}

type Timing struct {
	Step     string
	Duration time.Duration
}

type Source struct {
	Mode        string `json:"mode"`
	Root        string `json:"root,omitempty"`
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
	Code bool `json:"code,omitempty"`
}

type SkippedCounts struct {
	Dirs       int `json:"dirs,omitempty"`
	Binary     int `json:"binary,omitempty"`
	Oversized  int `json:"oversized,omitempty"`
	Symlink    int `json:"symlink,omitempty"`
	Unreadable int `json:"unreadable,omitempty"`
}

type Result struct {
	Relatedness float64        `json:"relatedness"`
	Description string         `json:"description"`
	Range       string         `json:"range"`
	Symbol      *Symbol        `json:"symbol"`
	Scores      map[string]any `json:"scores"`
	Excerpt     string         `json:"excerpt"`
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
	ResolvedRev    string    `json:"resolved_rev,omitempty"`
	EmbeddingModel string    `json:"embedding_model"`
	Dimensions     int       `json:"dimensions"`
	CreatedAt      time.Time `json:"created_at"`
}

type vectorRecord struct {
	ChunkID        string    `json:"chunk_id"`
	Path           string    `json:"path"`
	Source         string    `json:"source"`
	Blob           string    `json:"blob,omitempty"`
	StartLine      int       `json:"start_line"`
	EndLine        int       `json:"end_line"`
	ContentHash    string    `json:"content_hash"`
	EmbeddingModel string    `json:"embedding_model"`
	Dimensions     int       `json:"dimensions"`
	Size           int64     `json:"size,omitempty"`
	MTimeUnixNano  int64     `json:"mtime_unix_nano,omitempty"`
	Vector         []float64 `json:"vector"`
}

type vectorIndexRecord struct {
	ChunkID        string `json:"chunk_id"`
	Path           string `json:"path"`
	Source         string `json:"source"`
	Blob           string `json:"blob,omitempty"`
	StartLine      int    `json:"start_line"`
	EndLine        int    `json:"end_line"`
	ContentHash    string `json:"content_hash"`
	EmbeddingModel string `json:"embedding_model"`
	Dimensions     int    `json:"dimensions"`
	Size           int64  `json:"size,omitempty"`
	MTimeUnixNano  int64  `json:"mtime_unix_nano,omitempty"`
	Offset         int64  `json:"offset"`
}

type historyEntry struct {
	Query          string    `json:"query"`
	Normalized     string    `json:"normalized"`
	QueryEmbedding []float64 `json:"query_embedding"`
	EmbeddingModel string    `json:"embedding_model"`
	Dimensions     int       `json:"dimensions"`
	SourceMode     string    `json:"source_mode"`
	Root           string    `json:"root,omitempty"`
	ResolvedRev    string    `json:"resolved_rev,omitempty"`
	ResultChunkIDs []string  `json:"result_chunk_ids"`
	CreatedAt      time.Time `json:"created_at"`
}

func Run(ctx context.Context, client openai.EmbeddingClient, opts Options, query string) (Output, error) {
	started := time.Now()
	phaseStarted := started
	var diag Diagnostics
	mark := func(step string) {
		now := time.Now()
		diag.Timings = append(diag.Timings, Timing{Step: step, Duration: now.Sub(phaseStarted)})
		phaseStarted = now
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

	root, err := filepath.Abs(cmp.Or(opts.Root, "."))
	if err != nil {
		return fail(err)
	}

	source := Source{Mode: "filesystem", Root: root}
	indexRoot := root
	var resolvedRev string
	var files []fileContent
	var skipped SkippedCounts
	if opts.Rev != "" {
		repo, err := gitctx.Open(root)
		if err != nil {
			return fail(fmt.Errorf("--rev requires a Git repository: %w", err))
		}
		resolvedRev, err = repo.ResolveRef(opts.Rev)
		if err != nil {
			return fail(fmt.Errorf("resolve --rev %q: %w", opts.Rev, err))
		}
		indexRoot = repo.RootPath
		source = Source{Mode: "revision", Rev: opts.Rev, ResolvedRev: resolvedRev}
		files, skipped, err = discoverRevisionFiles(repo, resolvedRev)
		if err != nil {
			return fail(err)
		}
	} else {
		if repo, err := gitctx.Open(root); err == nil {
			indexRoot = repo.RootPath
		}
		files, skipped, err = discoverFilesystemFiles(root)
		if err != nil {
			return fail(err)
		}
	}
	if opts.CodeOnly {
		files = filterCodeFiles(files)
	}
	diag.Files = len(files)
	mark("discover")

	chunks := buildChunks(files)
	diag.Chunks = len(chunks)
	mark("chunk")

	indexDir := indexDir(indexRoot, source.Mode, root, resolvedRev, opts.CodeOnly)
	diag.IndexDir = indexDir
	oldVectors, _ := loadVectors(indexDir)
	vectors, records, reused := reuseVectors(chunks, oldVectors, opts)
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
		embedded, dim, err := embedTexts(ctx, client, opts, missingEmbeddingTexts(missing))
		if err != nil {
			mark("embed_index")
			return fail(err)
		}
		dimensions = dim
		for i, chunk := range missing {
			vector := embedded[i]
			vectors[chunk.ID] = vector
			records = append(records, vectorRecord{
				ChunkID:        chunk.ID,
				Path:           chunk.Path,
				Source:         chunk.Source,
				Blob:           chunk.Blob,
				StartLine:      chunk.StartLine,
				EndLine:        chunk.EndLine,
				ContentHash:    chunk.ContentHash,
				EmbeddingModel: opts.EmbeddingModel,
				Dimensions:     len(vector),
				Size:           chunk.Size,
				MTimeUnixNano:  chunk.MTimeUnixNano,
				Vector:         vector,
			})
		}
	}
	mark("embed_index")

	if len(chunks) > 0 && (len(missing) > 0 || opts.Reindex) {
		if err := saveIndex(indexDir, source, root, resolvedRev, opts.EmbeddingModel, dimensions, chunks, records); err != nil {
			mark("persist")
			return fail(err)
		}
	}
	mark("persist")

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
				Filters:        Filters{Code: opts.CodeOnly},
				Skipped:        skipped,
			},
			Results: []Result{},
		}), nil
	}

	normalizedQuery := normalizeQuery(query)
	queryVector, queryDimensions, cachedQuery := cachedQueryEmbedding(indexDir, normalizedQuery, opts.EmbeddingModel, opts.EmbeddingDimensions, source, root, resolvedRev)
	if !cachedQuery {
		queryVectors, dim, err := embedTexts(ctx, client, opts, []string{query})
		if err != nil {
			mark("embed_query")
			return fail(err)
		}
		queryVector = queryVectors[0]
		queryDimensions = dim
	}
	if dimensions == 0 {
		dimensions = queryDimensions
	}
	mark("embed_query")

	scored := scoreChunks(chunks, vectors, queryVector, opts.MinRelatedness)
	sortResults(scored)
	if len(scored) > opts.Limit {
		scored = scored[:opts.Limit]
	}
	results := renderResults(scored)
	mark("score")

	replay := replayFor(indexDir, query, normalizedQuery, queryVector, opts.EmbeddingModel, opts.EmbeddingDimensions, source, root, resolvedRev)
	_ = appendHistory(indexDir, historyEntry{
		Query:          query,
		Normalized:     normalizedQuery,
		QueryEmbedding: queryVector,
		EmbeddingModel: opts.EmbeddingModel,
		Dimensions:     len(queryVector),
		SourceMode:     source.Mode,
		Root:           root,
		ResolvedRev:    resolvedRev,
		ResultChunkIDs: resultIDs(scored),
		CreatedAt:      time.Now().UTC(),
	})
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
			Filters:        Filters{Code: opts.CodeOnly},
			Skipped:        skipped,
		},
		Results: results,
		Replay:  replay,
	}), nil
}

func discoverFilesystemFiles(root string) ([]fileContent, SkippedCounts, error) {
	var files []fileContent
	var skipped SkippedCounts
	ignoreMatcher := filesystemIgnoreMatcher(root)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			skipped.Unreadable++
			return nil
		}
		name := entry.Name()
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if path != root && (shouldSkipDir(name) || ignoreMatcher.Match(pathParts(rel), true)) {
				skipped.Dirs++
				return filepath.SkipDir
			}
			return nil
		}
		if shouldSkipFile(name) || ignoreMatcher.Match(pathParts(rel), false) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			skipped.Unreadable++
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			skipped.Symlink++
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > MaxFileBytes {
			skipped.Oversized++
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			skipped.Unreadable++
			return nil
		}
		if isBinary(data) {
			skipped.Binary++
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
	return files, skipped, err
}

func discoverRevisionFiles(repo *gitctx.Repository, rev string) ([]fileContent, SkippedCounts, error) {
	var files []fileContent
	var skipped SkippedCounts
	err := repo.WalkCommitTextFiles(rev, MaxFileBytes, func(file gitctx.CommitFile) error {
		if shouldSkipPath(file.Path) {
			skipped.Dirs++
			return nil
		}
		if file.Size > MaxFileBytes {
			skipped.Oversized++
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
	})
	slices.SortFunc(files, func(a, b fileContent) int { return strings.Compare(a.path, b.path) })
	return files, skipped, err
}

func filterCodeFiles(files []fileContent) []fileContent {
	filtered := files[:0]
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
	var found manifest
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	if err := sonic.Unmarshal(data, &found); err != nil {
		return nil, err
	}
	if found.Version != indexVersion {
		return nil, fmt.Errorf("index version = %d, want %d", found.Version, indexVersion)
	}
	if records, err := loadBinaryVectors(dir); err == nil {
		return records, nil
	}
	return loadLegacyVectors(dir)
}

func loadBinaryVectors(dir string) ([]vectorRecord, error) {
	indexData, err := os.ReadFile(filepath.Join(dir, "vectors.index.json"))
	if err != nil {
		return nil, err
	}
	var index []vectorIndexRecord
	if err := sonic.Unmarshal(indexData, &index); err != nil {
		return nil, err
	}
	vectorData, err := os.ReadFile(filepath.Join(dir, "vectors.f32"))
	if err != nil {
		return nil, err
	}
	records := make([]vectorRecord, len(index))
	for i, entry := range index {
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
		records[i] = vectorRecord{
			ChunkID:        entry.ChunkID,
			Path:           entry.Path,
			Source:         entry.Source,
			Blob:           entry.Blob,
			StartLine:      entry.StartLine,
			EndLine:        entry.EndLine,
			ContentHash:    entry.ContentHash,
			EmbeddingModel: entry.EmbeddingModel,
			Dimensions:     entry.Dimensions,
			Size:           entry.Size,
			MTimeUnixNano:  entry.MTimeUnixNano,
			Vector:         vector,
		}
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

func reuseVectors(chunks []Chunk, old []vectorRecord, opts Options) (map[string][]float64, []vectorRecord, int) {
	vectors := map[string][]float64{}
	if opts.Reindex {
		return vectors, nil, 0
	}
	byKey := map[string]vectorRecord{}
	for _, record := range old {
		if record.EmbeddingModel != opts.EmbeddingModel ||
			record.Dimensions != opts.EmbeddingDimensions ||
			len(record.Vector) != record.Dimensions {
			continue
		}
		byKey[recordVectorKey(record)] = record
	}
	var records []vectorRecord
	for _, chunk := range chunks {
		record, ok := byKey[chunkVectorKey(chunk, opts.EmbeddingModel, opts.EmbeddingDimensions)]
		if !ok {
			continue
		}
		record.ChunkID = chunk.ID
		vectors[chunk.ID] = record.Vector
		records = append(records, record)
	}
	return vectors, records, len(records)
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
	for start := 0; start < len(texts); {
		end := embeddingBatchEnd(texts, start)
		batches = append(batches, batch{start: start, end: end})
		start = end
	}
	if len(batches) == 0 {
		return nil, 0, nil
	}

	batchVectors := make([][][]float64, len(batches))
	batchDimensions := make([]int, len(batches))
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(embeddingConcurrency())
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
	if maxChars == 0 {
		maxChars = DefaultEmbeddingMaxInputChars
	}
	inputs := make([]string, len(texts))
	for i, text := range texts {
		chars := 0
		for j := range text {
			if chars == maxChars {
				text = text[:j]
				break
			}
			chars++
		}
		inputs[i] = text
	}
	return inputs
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

func embeddingConcurrency() int {
	return min(max(runtime.GOMAXPROCS(0)/2, 1), 8)
}

func embeddingBatchEnd(texts []string, start int) int {
	end := start
	chars := 0
	for end < len(texts) && end-start < batchMaxInputs {
		nextChars := len(texts[end])
		if end > start && chars+nextChars > batchMaxChars {
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

func saveIndex(dir string, source Source, root, resolvedRev, model string, dimensions int, chunks []Chunk, vectors []vectorRecord) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(dir, "chunks.json"), chunks); err != nil {
		return err
	}
	if err := writeBinaryVectors(dir, vectors); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "manifest.json"), manifest{
		Version:        indexVersion,
		Mode:           source.Mode,
		Root:           root,
		ResolvedRev:    resolvedRev,
		EmbeddingModel: model,
		Dimensions:     dimensions,
		CreatedAt:      time.Now().UTC(),
	})
}

func writeBinaryVectors(dir string, records []vectorRecord) error {
	index := make([]vectorIndexRecord, len(records))
	var data bytes.Buffer
	for i, record := range records {
		if record.Dimensions < 1 || len(record.Vector) != record.Dimensions {
			return fmt.Errorf("vector record %s dimensions mismatch", record.ChunkID)
		}
		index[i] = vectorIndexRecord{
			ChunkID:        record.ChunkID,
			Path:           record.Path,
			Source:         record.Source,
			Blob:           record.Blob,
			StartLine:      record.StartLine,
			EndLine:        record.EndLine,
			ContentHash:    record.ContentHash,
			EmbeddingModel: record.EmbeddingModel,
			Dimensions:     record.Dimensions,
			Size:           record.Size,
			MTimeUnixNano:  record.MTimeUnixNano,
			Offset:         int64(data.Len()),
		}
		var buf [4]byte
		for _, value := range record.Vector {
			binary.LittleEndian.PutUint32(buf[:], math.Float32bits(float32(value)))
			data.Write(buf[:])
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "vectors.f32"), data.Bytes(), 0o644); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, "vectors.index.json"), index)
}

func writeJSON(path string, value any) error {
	data, err := sonic.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

type scoredChunk struct {
	chunk       Chunk
	cosine      float64
	relatedness float64
}

func scoreChunks(chunks []Chunk, vectors map[string][]float64, query []float64, minRelatedness float64) []scoredChunk {
	var scored []scoredChunk
	for _, chunk := range chunks {
		vector := vectors[chunk.ID]
		if len(vector) == 0 {
			continue
		}
		cosine := cosineSimilarity(query, vector)
		relatedness := math.Max(1e-9, min(1, max(0, (cosine+1)/2)))
		if relatedness < minRelatedness || relatedness <= 0 {
			continue
		}
		scored = append(scored, scoredChunk{chunk: chunk, cosine: cosine, relatedness: relatedness})
	}
	return scored
}

func sortResults(results []scoredChunk) {
	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.relatedness != b.relatedness {
			return a.relatedness > b.relatedness
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
			Relatedness: item.relatedness,
			Description: description(chunk),
			Range:       fmt.Sprintf("%s:%d-%d", chunk.Path, chunk.StartLine, chunk.EndLine),
			Symbol:      chunk.Symbol,
			Scores:      map[string]any{"cosine": item.cosine},
			Excerpt:     excerpt(chunk),
		}
	}
	return results
}

func replayFor(dir, query, normalized string, queryVector []float64, model string, dimensions int, source Source, root, resolvedRev string) Replay {
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
		if entry.Normalized == normalized {
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

func cachedQueryEmbedding(dir, normalized, model string, dimensions int, source Source, root, resolvedRev string) ([]float64, int, bool) {
	entries, err := loadHistory(dir)
	if err != nil {
		return nil, 0, false
	}
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if entry.Normalized != normalized || entry.EmbeddingModel != model || entry.Dimensions != dimensions || entry.SourceMode != source.Mode {
			continue
		}
		if source.Mode == "filesystem" && entry.Root != root {
			continue
		}
		if source.Mode == "revision" && entry.ResolvedRev != resolvedRev {
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

func description(chunk Chunk) string {
	if chunk.Symbol != nil && chunk.Symbol.Name != "" {
		return fmt.Sprintf("Semantic match in %s %s.", chunk.Symbol.Type, chunk.Symbol.Name)
	}
	return fmt.Sprintf("Semantic match in %s.", chunk.Path)
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

func shouldSkipDir(name string) bool {
	return strings.HasPrefix(name, ".")
}

func shouldSkipFile(name string) bool {
	return strings.HasPrefix(name, ".")
}

func shouldSkipPath(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if shouldSkipDir(part) {
			return true
		}
	}
	return false
}

func filesystemIgnoreMatcher(root string) gitignore.Matcher {
	var patterns []gitignore.Pattern
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			if path != root {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return nil
				}
				if shouldSkipDir(entry.Name()) || gitignore.NewMatcher(patterns).Match(pathParts(rel), true) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Name() != ".gitignore" {
			return nil
		}
		relDir := ""
		if dir := filepath.Dir(path); dir != root {
			rel, err := filepath.Rel(root, dir)
			if err != nil {
				return nil
			}
			relDir = filepath.ToSlash(rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var base []string
		if relDir != "" {
			base = pathParts(relDir)
		}
		for line := range strings.Lines(string(data)) {
			line = strings.TrimRight(line, "\r\n")
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
				continue
			}
			patterns = append(patterns, gitignore.ParsePattern(line, base))
		}
		return nil
	})
	return gitignore.NewMatcher(patterns)
}

func pathParts(path string) []string {
	path = strings.Trim(filepath.ToSlash(path), "/")
	if path == "" || path == "." {
		return nil
	}
	return strings.Split(path, "/")
}

func indexDir(base, mode, root, resolvedRev string, codeOnly bool) string {
	filter := ""
	if codeOnly {
		filter = "code"
	}
	if mode == "revision" {
		return filepath.Join(base, ".git-agent", "search", "revs", resolvedRev, filter)
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(base, ".git-agent", "search", "fs", hex.EncodeToString(sum[:])[:16], filter)
}

func chunkVectorKey(chunk Chunk, model string, dimensions int) string {
	return fmt.Sprintf("%s:%s:%s:%d:%d:%d:%d:%s:%s:%d",
		chunk.Source,
		chunk.Path,
		chunk.Blob,
		chunk.StartLine,
		chunk.EndLine,
		chunk.Size,
		chunk.MTimeUnixNano,
		chunk.ContentHash,
		model,
		dimensions,
	)
}

func recordVectorKey(record vectorRecord) string {
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

func normalizeQuery(query string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(query)), " "))
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
