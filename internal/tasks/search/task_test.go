package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yusing/git-agent/internal/openai"
)

type fakeEmbedder struct{}

func (fakeEmbedder) CreateEmbeddings(_ context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	vectors := make([][]float64, len(request.Inputs))
	for i, input := range request.Inputs {
		vectors[i] = vectorFor(input)
	}
	return openai.EmbeddingResponse{Model: request.Model, Vectors: vectors, Dimensions: 3}, nil
}

type countingEmbedder struct {
	calls    atomic.Int64
	maxBatch atomic.Int64
}

type splittingEmbedder struct {
	calls atomic.Int64
}

func (e *splittingEmbedder) CreateEmbeddings(_ context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	e.calls.Add(1)
	if len(request.Inputs) > 1 {
		return openai.EmbeddingResponse{}, errors.New("batch too large")
	}
	return fakeEmbedder{}.CreateEmbeddings(context.Background(), request)
}

func (e *countingEmbedder) CreateEmbeddings(_ context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	e.calls.Add(1)
	for {
		current := e.maxBatch.Load()
		if int64(len(request.Inputs)) <= current || e.maxBatch.CompareAndSwap(current, int64(len(request.Inputs))) {
			break
		}
	}
	vectors := make([][]float64, len(request.Inputs))
	for i, input := range request.Inputs {
		vectors[i] = vectorFor(input)
	}
	return openai.EmbeddingResponse{Model: request.Model, Vectors: vectors, Dimensions: 3}, nil
}

func (e *countingEmbedder) callCount() int64 {
	return e.calls.Load()
}

func (e *countingEmbedder) maxBatchSize() int64 {
	return e.maxBatch.Load()
}

func TestFilesystemSearchDoesNotRequireGitAndIndexesCurrentFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "node_modules/\n")
	writeFile(t, root, "notes.txt", "release notes live here\n")
	writeFile(t, root, "node_modules/ignored.txt", "release notes live here\n")
	writeFile(t, root, ".omx/ignored.txt", "release notes live here\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		MinRelatedness:      0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "release notes")
	if err != nil {
		t.Fatal(err)
	}
	if out.Source.Mode != "filesystem" || out.Source.Root != root {
		t.Fatalf("source = %#v", out.Source)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %#v", out.Results)
	}
	if got := out.Results[0].Range; got != "notes.txt:1-1" {
		t.Fatalf("range = %q", got)
	}
	if strings.Contains(out.Results[0].Excerpt, "ignored") {
		t.Fatalf("excerpt includes skipped dependency dir: %s", out.Results[0].Excerpt)
	}
}

func TestSearchFiltersAndSortsByVectorOnly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "b.txt", "alpha\n")
	writeFile(t, root, "a.txt", "alpha\n")
	writeFile(t, root, "c.txt", "opposite\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		MinRelatedness:      0.99,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results = %#v", out.Results)
	}
	if out.Results[0].Range != "a.txt:1-1" || out.Results[1].Range != "b.txt:1-1" {
		t.Fatalf("unexpected sort order: %#v", out.Results)
	}
}

func TestSearchCodeOnlyFiltersDocs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "release notes live here\n")
	writeFile(t, root, "main.go", "package main\n\nfunc releaseNotes() {}\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		MinRelatedness:      0.70,
		Limit:               10,
		CodeOnly:            true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "release notes")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Retrieval.Filters.Code {
		t.Fatalf("filters = %#v", out.Retrieval.Filters)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %#v", out.Results)
	}
	if got := out.Results[0].Range; !strings.HasPrefix(got, "main.go:") {
		t.Fatalf("range = %q", got)
	}
}

func TestDenseHandwrittenGoFilesKeepSymbolChunks(t *testing.T) {
	var b strings.Builder
	b.WriteString("package handwritten\n\n")
	for i := range 60 {
		fmt.Fprintf(&b, "func F%d() {}\n", i)
	}

	chunks := chunksForFile(fileContent{
		path:   "handwritten.go",
		source: "filesystem",
		text:   b.String(),
	})
	if len(chunks) < 60 {
		t.Fatalf("chunks = %d, want symbol chunks retained", len(chunks))
	}
	hasFunction := false
	for _, chunk := range chunks {
		if chunk.Symbol != nil && chunk.Symbol.Type == "function" {
			hasFunction = true
			break
		}
	}
	if !hasFunction {
		t.Fatalf("chunks have no function symbols: %#v", chunks)
	}
}

func TestLargeGoDeclarationsAreSplit(t *testing.T) {
	var b strings.Builder
	b.WriteString("package handwritten\n\nfunc Large() {\n")
	for i := range chunkLines + 20 {
		fmt.Fprintf(&b, "\t_ = %d\n", i)
	}
	b.WriteString("}\n")

	chunks := chunksForFile(fileContent{
		path:   "large.go",
		source: "filesystem",
		text:   b.String(),
	})
	foundLarge := false
	for _, chunk := range chunks {
		if chunk.EndLine-chunk.StartLine+1 > chunkLines {
			t.Fatalf("chunk range = %d-%d, want at most %d lines", chunk.StartLine, chunk.EndLine, chunkLines)
		}
		if chunk.Symbol != nil && chunk.Symbol.Name == "Large" {
			foundLarge = true
		}
	}
	if !foundLarge {
		t.Fatalf("large function symbol missing: %#v", chunks)
	}
}

func TestGeneratedGoFilesUsePathOnlyChunks(t *testing.T) {
	file := fileContent{
		path:   "internal/web/uc/types/user_profile.go",
		source: "filesystem",
		text: strings.Join([]string{
			"// database exporter output. DO NOT EDIT.",
			"package types",
			"",
			"type UserProfile struct {",
			"    SecretGeneratedField string",
			"}",
			"",
		}, "\n"),
		size: 1234,
	}
	chunks := chunksForFile(file)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want one path-only chunk", len(chunks))
	}
	chunk := chunks[0]
	if chunk.text != "" || excerpt(chunk) != "" {
		t.Fatalf("generated content leaked into chunk text/excerpt: text=%q excerpt=%q", chunk.text, excerpt(chunk))
	}
	if !strings.Contains(chunk.EmbeddingText, "path: internal/web/uc/types/user_profile.go") {
		t.Fatalf("embedding text missing path: %q", chunk.EmbeddingText)
	}
	if strings.Contains(chunk.EmbeddingText, "SecretGeneratedField") || strings.Contains(chunk.EmbeddingText, "UserProfile struct") {
		t.Fatalf("generated content leaked into embedding text: %q", chunk.EmbeddingText)
	}

	changed := file
	changed.text = strings.ReplaceAll(changed.text, "SecretGeneratedField", "DifferentGeneratedField")
	if got := chunksForFile(changed)[0].ContentHash; got != chunk.ContentHash {
		t.Fatalf("path-only content hash changed with generated body: %s != %s", got, chunk.ContentHash)
	}
}

func TestDoNotEditAfterPackageDoesNotMarkGenerated(t *testing.T) {
	chunks := chunksForFile(fileContent{
		path:   "handwritten.go",
		source: "filesystem",
		text: strings.Join([]string{
			"package handwritten",
			"",
			"// DO NOT EDIT this constant without checking callers.",
			"const Value = 1",
			"",
		}, "\n"),
	})
	if chunks[0].text == "" {
		t.Fatal("post-package DO NOT EDIT comment incorrectly marked file generated")
	}
}

func TestReindexIgnoresExistingVectors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")

	opts := Options{
		Root:                root,
		MinRelatedness:      0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), fakeEmbedder{}, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if first.Retrieval.Index != "miss" {
		t.Fatalf("first index = %q", first.Retrieval.Index)
	}
	second, err := Run(t.Context(), fakeEmbedder{}, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if second.Retrieval.Index != "hit" || second.Replay.Mode != "hit" {
		t.Fatalf("second retrieval = %#v replay = %#v", second.Retrieval, second.Replay)
	}
	opts.Reindex = true
	third, err := Run(t.Context(), fakeEmbedder{}, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if third.Retrieval.Index != "miss" {
		t.Fatalf("reindex index = %q", third.Retrieval.Index)
	}
}

func TestSearchBatchesIndexEmbeddingsAndCachesExactQueryEmbedding(t *testing.T) {
	root := t.TempDir()
	for i := range 130 {
		writeFile(t, root, filepath.Join("pkg", fmt.Sprintf("file_%03d.go", i)), "package pkg\n\nfunc alpha() {}\n")
	}

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		MinRelatedness:      0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if first.Retrieval.Index != "miss" {
		t.Fatalf("first index = %q", first.Retrieval.Index)
	}
	for _, name := range []string{"vectors.f32", "vectors.index.json"} {
		if _, err := os.Stat(filepath.Join(first.Diagnostics.IndexDir, name)); err != nil {
			t.Fatalf("missing binary vector cache %s: %v", name, err)
		}
	}
	firstCalls := embedder.callCount()
	if firstCalls <= 2 {
		t.Fatalf("embedding calls after first run = %d, want multiple bounded batches + query", firstCalls)
	}
	if embedder.maxBatchSize() > batchMaxInputs {
		t.Fatalf("max embedding batch = %d, want <= %d", embedder.maxBatchSize(), batchMaxInputs)
	}

	second, err := Run(t.Context(), embedder, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if second.Retrieval.Index != "hit" || second.Replay.Mode != "hit" {
		t.Fatalf("second retrieval = %#v replay = %#v", second.Retrieval, second.Replay)
	}
	if embedder.callCount() != firstCalls {
		t.Fatalf("embedding calls after exact replay = %d, want unchanged from %d", embedder.callCount(), firstCalls)
	}
}

func TestSearchSplitsRejectedEmbeddingBatches(t *testing.T) {
	root := t.TempDir()
	for i := range 12 {
		writeFile(t, root, filepath.Join("pkg", fmt.Sprintf("file_%03d.go", i)), "package pkg\n\nfunc alpha() {}\n")
	}

	embedder := &splittingEmbedder{}
	out, err := Run(t.Context(), embedder, Options{
		Root:                root,
		MinRelatedness:      0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if out.Retrieval.Index != "miss" || len(out.Results) == 0 {
		t.Fatalf("output = %#v", out)
	}
	if embedder.calls.Load() <= 2 {
		t.Fatalf("embedding calls = %d, want split retries", embedder.calls.Load())
	}
}

func TestEmbeddingConcurrencyUsesHalfGOMAXPROCSCappedAtEight(t *testing.T) {
	old := runtime.GOMAXPROCS(20)
	defer runtime.GOMAXPROCS(old)

	if got := embeddingConcurrency(); got != 8 {
		t.Fatalf("embedding concurrency = %d, want cap 8", got)
	}

	runtime.GOMAXPROCS(2)
	if got := embeddingConcurrency(); got != 1 {
		t.Fatalf("embedding concurrency = %d, want half GOMAXPROCS floor 1", got)
	}
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func vectorFor(input string) []float64 {
	input = strings.ToLower(input)
	switch {
	case strings.Contains(input, "opposite"):
		return []float64{-1, 0, 0}
	case strings.Contains(input, "release"):
		return []float64{0, 1, 0}
	case strings.Contains(input, "alpha"):
		return []float64{1, 0, 0}
	default:
		return []float64{0, 0, 1}
	}
}
