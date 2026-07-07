package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/yusing/git-agent/internal/openai"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "git-agent-search-home-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("HOME", home); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

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

type failOnPathEmbedder string

func (e failOnPathEmbedder) CreateEmbeddings(ctx context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	for _, input := range request.Inputs {
		if strings.Contains(input, string(e)) {
			return openai.EmbeddingResponse{}, errors.New("boom")
		}
	}
	return fakeEmbedder{}.CreateEmbeddings(ctx, request)
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
	writeFile(t, root, ".gitagentignore", "search-only.txt\n")
	writeFile(t, root, "notes.txt", "release notes live here\n")
	writeFile(t, root, "icon.svg", `<svg xmlns="http://www.w3.org/2000/svg"><title>release notes live here</title></svg>`)
	writeFile(t, root, "search-only.txt", "release notes live here\n")
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
	if strings.Contains(out.Results[0].Range, "search-only.txt") {
		t.Fatalf("range includes .gitagentignore file: %s", out.Results[0].Range)
	}
	if out.Retrieval.Skipped.NonText != 1 {
		t.Fatalf("non-text skipped = %d, want 1", out.Retrieval.Skipped.NonText)
	}
}

func TestFilesystemSearchScopeKeepsRootIgnoreRules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitagentignore", "ignored.txt\n")
	writeFile(t, root, "foo/keep.txt", "alpha\n")
	writeFile(t, root, "foo/ignored.txt", "alpha\n")
	writeFile(t, root, "bar/keep.txt", "alpha\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Scope:               []string{"foo/"},
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
	if got := out.Retrieval.Filters.Scope; !slices.Equal(got, []string{"foo"}) {
		t.Fatalf("scope filter = %#v", got)
	}
	if len(out.Results) != 1 || out.Results[0].Range != "foo/keep.txt:1-1" {
		t.Fatalf("results = %#v", out.Results)
	}
	if !strings.Contains(out.Diagnostics.IndexDir, "scope-") {
		t.Fatalf("index dir = %q, want scoped cache", out.Diagnostics.IndexDir)
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

func TestSearchNoTestsFiltersCommonTestPaths(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"main.go":              "alpha\n",
		"main_test.go":         "alpha\n",
		"button.test.ts":       "alpha\n",
		"button.spec.ts":       "alpha\n",
		"test.js":              "alpha\n",
		"spec.ts":              "alpha\n",
		"tests/helper.go":      "alpha\n",
		"test/helper.py":       "alpha\n",
		"__tests__/view.ts":    "alpha\n",
		"spec/model.rb":        "alpha\n",
		"testdata/sample.json": "alpha\n",
	} {
		writeFile(t, root, name, content)
	}

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		MinRelatedness:      0.70,
		Limit:               20,
		NoTests:             true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Retrieval.Filters.NoTests {
		t.Fatalf("filters = %#v", out.Retrieval.Filters)
	}
	got := resultRanges(out.Results)
	want := []string{"main.go:1-1", "testdata/sample.json:1-1"}
	if !slices.Equal(got, want) {
		t.Fatalf("result ranges = %#v, want %#v", got, want)
	}
	if !strings.Contains(out.Diagnostics.IndexDir, "no-tests") {
		t.Fatalf("index dir = %q, want no-tests filter", out.Diagnostics.IndexDir)
	}
}

func TestRevisionSearchNoTestsFiltersCommittedTestPaths(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "alpha\n")
	writeFile(t, root, "main_test.go", "alpha\n")
	writeFile(t, root, "tests/helper.go", "alpha\n")
	rev := commitSearchRepo(t, root)

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Rev:                 rev,
		MinRelatedness:      0.70,
		Limit:               10,
		NoTests:             true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	got := resultRanges(out.Results)
	want := []string{"main.go:1-1"}
	if !slices.Equal(got, want) {
		t.Fatalf("result ranges = %#v, want %#v", got, want)
	}
}

func TestRevisionSearchUsesIgnoreFilesFromResolvedCommit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "ignored-by-gitignore.txt\n")
	writeFile(t, root, ".gitagentignore", "ignored-by-rev.txt\nignored-dir/\nignored-binary.dat\n")
	writeFile(t, root, "notes.txt", "release notes live here\n")
	writeFile(t, root, "ignored-by-gitignore.txt", "release notes live here\n")
	writeFile(t, root, "ignored-by-rev.txt", "release notes live here\n")
	writeFile(t, root, "ignored-binary.dat", "release\x00notes\n")
	writeFile(t, root, "ignored-dir/file.txt", "release notes live here\n")
	writeFile(t, root, "icon.svg", `<svg xmlns="http://www.w3.org/2000/svg"><title>release notes live here</title></svg>`)
	writeFile(t, root, "binary.dat", "release\x00notes\n")
	writeFile(t, root, "large.txt", strings.Repeat("x", int(MaxFileBytes)+1))
	rev := commitSearchRepo(t, root)

	writeFile(t, root, ".gitagentignore", "notes.txt\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Rev:                 rev,
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
	if len(out.Results) != 1 {
		t.Fatalf("results = %#v", out.Results)
	}
	if got := out.Results[0].Range; got != "notes.txt:1-1" {
		t.Fatalf("range = %q", got)
	}
	if out.Retrieval.Skipped.NonText != 1 {
		t.Fatalf("non-text skipped = %d, want 1", out.Retrieval.Skipped.NonText)
	}
	if out.Retrieval.Skipped.Binary != 1 {
		t.Fatalf("binary skipped = %d, want 1", out.Retrieval.Skipped.Binary)
	}
	if out.Retrieval.Skipped.Oversized != 1 {
		t.Fatalf("oversized skipped = %d, want 1", out.Retrieval.Skipped.Oversized)
	}
	for _, want := range []SkippedFile{
		{Path: "binary.dat", Reason: "binary"},
		{Path: "icon.svg", Reason: "non_text"},
		{Path: "large.txt", Reason: "oversized"},
	} {
		if !slices.Contains(out.Diagnostics.SkippedFiles, want) {
			t.Fatalf("skipped files missing %#v: %#v", want, out.Diagnostics.SkippedFiles)
		}
	}
	if slices.ContainsFunc(out.Diagnostics.SkippedFiles, func(file SkippedFile) bool {
		return file.Path == "ignored-binary.dat"
	}) {
		t.Fatalf("skipped files include ignored binary: %#v", out.Diagnostics.SkippedFiles)
	}
}

func TestRevisionSearchScopeKeepsRootIgnoreRules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitagentignore", "ignored.txt\n")
	writeFile(t, root, "foo/keep.txt", "alpha\n")
	writeFile(t, root, "foo/ignored.txt", "alpha\n")
	writeFile(t, root, "bar/keep.txt", "alpha\n")
	rev := commitSearchRepo(t, root)

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Rev:                 rev,
		Scope:               []string{"foo"},
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
	if len(out.Results) != 1 || out.Results[0].Range != "foo/keep.txt:1-1" {
		t.Fatalf("results = %#v", out.Results)
	}
}

func TestRevisionSearchScopeSkipsOutOfScopeBlobsBeforeLoading(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "foo/keep.txt", "alpha\n")
	rev := commitSearchRepo(t, root)
	rev = addMissingBlobToCommittedTree(t, root, rev, "bar.txt")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Rev:                 rev,
		Scope:               []string{"foo"},
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
	if len(out.Results) != 1 || out.Results[0].Range != "foo/keep.txt:1-1" {
		t.Fatalf("results = %#v", out.Results)
	}
}

func TestIsIndexableTextContentTypes(t *testing.T) {
	tests := []struct {
		name string
		path string
		data string
		want bool
	}{
		{
			name: "go source bypasses mime table",
			path: "main.go",
			data: "package main\nfunc main() {}\n",
			want: true,
		},
		{
			name: "tsx source bypasses non-code mime mapping",
			path: "component.tsx",
			data: "export function Component() { return <div /> }\n",
			want: true,
		},
		{
			name: "markdown text",
			path: "README.md",
			data: "# title\n",
			want: true,
		},
		{
			name: "json application text",
			path: "data.json",
			data: `{"release":"notes"}`,
			want: true,
		},
		{
			name: "yaml application text",
			path: "config.yaml",
			data: "release: notes\n",
			want: true,
		},
		{
			name: "toml application text",
			path: "config.toml",
			data: "release = \"notes\"\n",
			want: true,
		},
		{
			name: "sql application text",
			path: "schema.sql",
			data: "select 1;\n",
			want: true,
		},
		{
			name: "xml text",
			path: "feed.xml",
			data: "<feed />\n",
			want: true,
		},
		{
			name: "unknown plain text",
			path: "LOCKFILE",
			data: "release notes\n",
			want: true,
		},
		{
			name: "svg image xml",
			path: "icon.svg",
			data: `<svg xmlns="http://www.w3.org/2000/svg"><title>release notes</title></svg>`,
			want: false,
		},
		{
			name: "pdf by extension",
			path: "doc.pdf",
			data: "%PDF-1.7\nrelease notes\n",
			want: false,
		},
		{
			name: "png by extension",
			path: "image.png",
			data: "release notes\n",
			want: false,
		},
		{
			name: "octet stream by extension",
			path: "archive.bin",
			data: "release notes\n",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isIndexableText(tt.path, []byte(tt.data)); got != tt.want {
				t.Fatalf("isIndexableText(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestDiscoverFilesystemFilesClassifiesSkipReasons(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "keep.txt", "release notes\n")
	writeFile(t, root, "config.yaml", "release: notes\n")
	writeFile(t, root, "component.tsx", "export const releaseNotes = true\n")
	writeFile(t, root, "icon.svg", `<svg xmlns="http://www.w3.org/2000/svg"><title>release notes</title></svg>`)
	writeFile(t, root, "manual.pdf", "%PDF-1.7\nrelease notes\n")
	writeFile(t, root, "binary.dat", "release\x00notes\n")

	files, skipped, skippedFiles, err := discoverFilesystemFiles(root, nil, false, func(string, ...slog.Attr) {})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, file := range files {
		paths = append(paths, file.path)
	}
	wantPaths := []string{"component.tsx", "config.yaml", "keep.txt"}
	if strings.Join(paths, ",") != strings.Join(wantPaths, ",") {
		t.Fatalf("paths = %#v, want %#v", paths, wantPaths)
	}
	if skipped.Binary != 1 {
		t.Fatalf("binary skipped = %d, want 1", skipped.Binary)
	}
	if skipped.NonText != 2 {
		t.Fatalf("non-text skipped = %d, want 2", skipped.NonText)
	}
	wantSkipped := []SkippedFile{
		{Path: "binary.dat", Reason: "binary"},
		{Path: "icon.svg", Reason: "non_text"},
		{Path: "manual.pdf", Reason: "non_text"},
	}
	if fmt.Sprint(skippedFiles) != fmt.Sprint(wantSkipped) {
		t.Fatalf("skipped files = %#v, want %#v", skippedFiles, wantSkipped)
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

func TestEmbeddingTextClampsLongLines(t *testing.T) {
	longLine := strings.Repeat("x", maxEmbeddingLineChars+100)
	chunks := chunksForFile(fileContent{
		path:   "bundle.js",
		source: "filesystem",
		text:   longLine,
		size:   int64(len(longLine)),
	})
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want one chunk", len(chunks))
	}
	if got := chunks[0].text; got != longLine {
		t.Fatal("chunk text was clamped")
	}
	_, body, ok := strings.Cut(chunks[0].EmbeddingText, "\n\n")
	if !ok {
		t.Fatalf("embedding text missing metadata separator: %q", chunks[0].EmbeddingText)
	}
	if got := len([]rune(body)); got != maxEmbeddingLineChars {
		t.Fatalf("embedding body chars = %d, want %d", got, maxEmbeddingLineChars)
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

func TestSearchIgnoresStaleIndexVersion(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")

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
	firstCalls := embedder.callCount()

	manifestPath := filepath.Join(first.Diagnostics.IndexDir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), fmt.Sprintf(`"version":%d`, indexVersion), fmt.Sprintf(`"version":%d`, indexVersion-1), 1))
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := Run(t.Context(), embedder, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if second.Retrieval.Index != "miss" {
		t.Fatalf("second index = %q, want miss", second.Retrieval.Index)
	}
	if embedder.callCount() <= firstCalls {
		t.Fatalf("embedding calls after stale manifest = %d, want > %d", embedder.callCount(), firstCalls)
	}
}

func TestSearchPersistsIndexAfterAllEmbeddingsSucceed(t *testing.T) {
	root := t.TempDir()
	for i := range DefaultEmbeddingBatchInputs + 1 {
		writeFile(t, root, filepath.Join("pkg", fmt.Sprintf("file_%03d.txt", i)), "alpha\n")
	}

	opts := Options{
		Root:                root,
		MinRelatedness:      0.70,
		Limit:               10,
		IndexOnly:           true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	firstEmbedder := failOnPathEmbedder("file_010.txt")
	if _, err := Run(t.Context(), firstEmbedder, opts, ""); err == nil {
		t.Fatal("expected embedding failure")
	}

	secondEmbedder := &countingEmbedder{}
	second, err := Run(t.Context(), secondEmbedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.ReusedChunks != 0 {
		t.Fatalf("reused chunks = %d, want 0", second.Diagnostics.ReusedChunks)
	}
	if second.Diagnostics.EmbeddedChunks != DefaultEmbeddingBatchInputs+1 || second.Diagnostics.EmbeddedDone != DefaultEmbeddingBatchInputs+1 {
		t.Fatalf("embedding diagnostics = %#v", second.Diagnostics)
	}
	if secondEmbedder.callCount() != 2 {
		t.Fatalf("embedding calls = %d, want 2", secondEmbedder.callCount())
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
	if embedder.maxBatchSize() > DefaultEmbeddingBatchInputs {
		t.Fatalf("max embedding batch = %d, want <= %d", embedder.maxBatchSize(), DefaultEmbeddingBatchInputs)
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

func TestSearchTruncatesEmbeddingInputs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "alpha.txt", strings.Repeat("alpha ", 100))

	out, err := Run(t.Context(), lengthLimitEmbedder{max: 32}, Options{
		Root:                root,
		MinRelatedness:      0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		EmbeddingMaxInput:   32,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, strings.Repeat("alpha ", 100))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) == 0 {
		t.Fatalf("results = %#v", out.Results)
	}
}

func TestEmbeddingConcurrencyUsesGOMAXPROCSCappedAtEight(t *testing.T) {
	old := runtime.GOMAXPROCS(20)
	defer runtime.GOMAXPROCS(old)

	if got := embeddingConcurrency(Options{}); got != 8 {
		t.Fatalf("embedding concurrency = %d, want cap 8", got)
	}

	runtime.GOMAXPROCS(6)
	if got := embeddingConcurrency(Options{}); got != 6 {
		t.Fatalf("embedding concurrency = %d, want GOMAXPROCS", got)
	}

	if got := embeddingConcurrency(Options{EmbeddingConcurrency: 12}); got != 12 {
		t.Fatalf("embedding concurrency override = %d, want 12", got)
	}
}

func TestEmbeddingBatchTuning(t *testing.T) {
	embedder := &countingEmbedder{}
	texts := []string{"aaaa", "bbbb", "cccc", "dddd", "eeee", "fffff", "ggggg"}

	_, _, err := embedTexts(t.Context(), embedder, Options{
		EmbeddingModel:         "text-embedding-3-small",
		EmbeddingDimensions:    3,
		EmbeddingBatchInputs:   3,
		EmbeddingBatchMaxChars: 10,
		APIKey:                 "test-key",
		BaseURL:                "http://example.test",
	}, texts)
	if err != nil {
		t.Fatal(err)
	}
	if embedder.callCount() != 4 {
		t.Fatalf("embedding calls = %d, want 4", embedder.callCount())
	}
	if embedder.maxBatchSize() > 3 {
		t.Fatalf("max embedding batch = %d, want <= 3", embedder.maxBatchSize())
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

func commitSearchRepo(t *testing.T, root string) string {
	t.Helper()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Commit.GpgSign = config.OptBoolFalse
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := worktree.AddGlob("."); err != nil {
		t.Fatal(err)
	}
	hash, err := worktree.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Search Test",
			Email: "search@example.test",
			When:  time.Unix(0, 0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return hash.String()
}

func addMissingBlobToCommittedTree(t *testing.T, root, rev, path string) string {
	t.Helper()
	repo, err := git.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(plumbing.NewHash(rev))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	tree.Entries = append(tree.Entries, object.TreeEntry{
		Name: path,
		Mode: filemode.Regular,
		Hash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	})
	slices.SortFunc(tree.Entries, func(a, b object.TreeEntry) int {
		return strings.Compare(a.Name, b.Name)
	})
	treeObj := repo.Storer.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		t.Fatal(err)
	}
	treeHash, err := repo.Storer.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatal(err)
	}
	commit.TreeHash = treeHash
	commitObj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		t.Fatal(err)
	}
	hash, err := repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatal(err)
	}
	return hash.String()
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

func resultRanges(results []Result) []string {
	ranges := make([]string, 0, len(results))
	for _, result := range results {
		ranges = append(ranges, result.Range)
	}
	slices.Sort(ranges)
	return ranges
}

type lengthLimitEmbedder struct {
	max int
}

func (e lengthLimitEmbedder) CreateEmbeddings(_ context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	for _, input := range request.Inputs {
		if len([]rune(input)) > e.max {
			return openai.EmbeddingResponse{}, fmt.Errorf("input length = %d, want <= %d", len([]rune(input)), e.max)
		}
	}
	return fakeEmbedder{}.CreateEmbeddings(context.Background(), request)
}
