package search

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yusing/git-agent/internal/openai"
)

type benchmarkEmbedder struct {
	dimensions int
	calls      atomic.Int64
}

func (e *benchmarkEmbedder) CreateEmbeddings(_ context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	e.calls.Add(1)
	vectors := make([][]float64, len(request.Inputs))
	for i := range request.Inputs {
		vector := make([]float64, e.dimensions)
		vector[0] = 1
		vectors[i] = vector
	}
	return openai.EmbeddingResponse{Model: request.Model, Vectors: vectors, Dimensions: e.dimensions}, nil
}

func BenchmarkWarmIndexedSearchRun(b *testing.B) {
	b.Setenv("HOME", b.TempDir())
	root := b.TempDir()
	const (
		fileCount        = 200
		functionsPerFile = 16
		dimensions       = 1024
	)
	for fileIndex := range fileCount {
		var content strings.Builder
		content.WriteString("package benchmark\n\n")
		for functionIndex := range functionsPerFile {
			fmt.Fprintf(&content, "func SearchOwner%03d_%03d() string { return \"semantic search owner %03d %03d\" }\n\n",
				fileIndex, functionIndex, fileIndex, functionIndex)
		}
		writeFile(b, root, filepath.Join("pkg", fmt.Sprintf("file_%03d.go", fileIndex)), content.String())
	}

	embedder := &benchmarkEmbedder{dimensions: dimensions}
	opts := Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "benchmark-model",
		EmbeddingDimensions: dimensions,
	}
	indexed, err := Run(b.Context(), embedder, opts, "")
	if err != nil {
		b.Fatal(err)
	}
	if indexed.Diagnostics.EmbeddedChunks == 0 {
		b.Fatal("benchmark setup did not build an index")
	}
	metadataDir := metadataDirForIndex(indexed.Diagnostics.IndexDir)
	for historicalIndex := range 100 {
		dir := filepath.Join(metadataDir, "search", fmt.Sprintf("historical-%03d", historicalIndex))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			b.Fatal(err)
		}
		if err := writeJSON(filepath.Join(dir, "manifest.json"), manifest{
			Version:        legacyIndexVersion,
			EmbeddingModel: opts.EmbeddingModel,
			Dimensions:     opts.EmbeddingDimensions,
		}); err != nil {
			b.Fatal(err)
		}
	}

	opts.IndexOnly = false
	query := "where is the semantic search owner implemented"
	primed, err := Run(b.Context(), embedder, opts, query)
	if err != nil {
		b.Fatal(err)
	}
	if primed.Retrieval.Index != "hit" || len(primed.Results) == 0 {
		b.Fatalf("benchmark setup retrieval = %#v, results = %d", primed.Retrieval, len(primed.Results))
	}
	primedCalls := embedder.calls.Load()
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		output, err := Run(b.Context(), embedder, opts, query)
		if err != nil {
			b.Fatal(err)
		}
		if output.Retrieval.Index != "hit" || output.Replay.Mode != "hit" {
			b.Fatalf("warm retrieval = %#v, replay = %#v", output.Retrieval, output.Replay)
		}
		if calls := embedder.calls.Load(); calls != primedCalls {
			b.Fatalf("warm retrieval embedding calls = %d, want %d", calls, primedCalls)
		}
	}
}
