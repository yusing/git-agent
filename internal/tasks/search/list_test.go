package search

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestListIndexesAndFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "cmd/app.go", "package main\n\nfunc main() {}\n")
	writeFile(t, root, "internal/cli/app.go", "package cli\n\nfunc Run() {}\n")
	writeFile(t, root, "README.md", "# demo\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		IndexOnly:           true,
		MinRelatedness:      DefaultMinRelatedness,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Diagnostics.IndexDir == "" {
		t.Fatal("expected index dir")
	}

	indexes, err := ListIndexes(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 1 {
		t.Fatalf("indexes = %d, want 1: %#v", len(indexes), indexes)
	}
	index := indexes[0]
	if index.Mode != "filesystem" {
		t.Fatalf("mode = %q, want filesystem", index.Mode)
	}
	if index.Files < 3 {
		t.Fatalf("files = %d, want >= 3", index.Files)
	}
	if index.Chunks < 3 {
		t.Fatalf("chunks = %d, want >= 3", index.Chunks)
	}
	if index.EmbeddingModel != "test-model" {
		t.Fatalf("model = %q, want test-model", index.EmbeddingModel)
	}
	if index.Dimensions != 3 {
		t.Fatalf("dims = %d, want 3", index.Dimensions)
	}
	if index.Dir != out.Diagnostics.IndexDir {
		t.Fatalf("dir = %q, want %q", index.Dir, out.Diagnostics.IndexDir)
	}
	if index.CreatedAt.IsZero() || index.CreatedAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("created_at = %v", index.CreatedAt)
	}

	text := FormatIndexes(indexes)
	if !strings.Contains(text, "filesystem") || !strings.Contains(text, "files=") {
		t.Fatalf("format indexes missing summary:\n%s", text)
	}
	if !strings.Contains(text, index.Dir) {
		t.Fatalf("format indexes missing dir:\n%s", text)
	}

	listed, err := ListIndexFiles(t.Context(), ListFilesOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Index.Dir != index.Dir {
		t.Fatalf("ls-files dir = %q, want %q", listed.Index.Dir, index.Dir)
	}
	wantPaths := []string{"README.md", "cmd/app.go", "internal/cli/app.go"}
	for _, want := range wantPaths {
		if !slices.Contains(listed.Files, want) {
			t.Fatalf("files = %v, missing %q", listed.Files, want)
		}
	}

	tree := FormatFileTree(listed.Files)
	for _, want := range []string{
		".\n",
		"├── README.md\n",
		"├── cmd/\n",
		"│   └── app.go\n",
		"└── internal/\n",
		"    └── cli/\n",
		"        └── app.go\n",
	} {
		if !strings.Contains(tree, want) {
			t.Fatalf("tree missing %q:\n%s", want, tree)
		}
	}
}

func TestListIndexesEmpty(t *testing.T) {
	root := t.TempDir()
	indexes, err := ListIndexes(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 0 {
		t.Fatalf("indexes = %#v, want empty", indexes)
	}
	if got := FormatIndexes(indexes); got != "no search indexes\n" {
		t.Fatalf("format = %q", got)
	}
}

func TestListIndexFilesMissing(t *testing.T) {
	root := t.TempDir()
	_, err := ListIndexFiles(t.Context(), ListFilesOptions{Root: root})
	if err == nil || !strings.Contains(err.Error(), "no search index") {
		t.Fatalf("err = %v, want missing index error", err)
	}
}

func TestListIndexFilesNoTestsSharesIndex(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.go", "package main\n")
	writeFile(t, root, "app_test.go", "package main\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		IndexOnly:           true,
		NoTests:             true,
		MinRelatedness:      DefaultMinRelatedness,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	indexes, err := ListIndexes(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 1 {
		t.Fatalf("indexes = %#v, want 1", indexes)
	}
	if indexes[0].Dir != out.Diagnostics.IndexDir {
		t.Fatalf("index dir = %q, want %q", indexes[0].Dir, out.Diagnostics.IndexDir)
	}
	if len(indexes[0].Filters) != 0 {
		t.Fatalf("filters = %v, want shared unfiltered index", indexes[0].Filters)
	}

	allFiles, err := ListIndexFiles(t.Context(), ListFilesOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(allFiles.Files, "app_test.go") {
		t.Fatalf("shared index should retain tests: %v", allFiles.Files)
	}

	listed, err := ListIndexFiles(t.Context(), ListFilesOptions{Root: root, NoTests: true})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(listed.Files, "app_test.go") {
		t.Fatalf("no-tests listing should exclude tests: %v", listed.Files)
	}
	if !slices.Contains(listed.Files, "app.go") {
		t.Fatalf("files = %v, missing app.go", listed.Files)
	}
}

func TestListIndexFilesNoTestsFiltersSharedIndex(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.go", "package main\n")
	writeFile(t, root, "app_test.go", "package main\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		IndexOnly:           true,
		MinRelatedness:      DefaultMinRelatedness,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	listed, err := ListIndexFiles(t.Context(), ListFilesOptions{Root: root, NoTests: true})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Index.Dir != out.Diagnostics.IndexDir {
		t.Fatalf("index dir = %q, want shared %q", listed.Index.Dir, out.Diagnostics.IndexDir)
	}
	if slices.Contains(listed.Files, "app_test.go") {
		t.Fatalf("no-tests listing should exclude tests: %v", listed.Files)
	}
	if !slices.Contains(listed.Files, "app.go") {
		t.Fatalf("files = %v, missing app.go", listed.Files)
	}
}

func TestListIndexFilesSelectsScopedRevisionIndex(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "pkg/app.go", "package pkg\n")
	writeFile(t, root, "README.md", "# demo\n")
	rev := commitSearchRepo(t, root)

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Rev:                 rev,
		IndexOnly:           true,
		Scope:               []string{"pkg"},
		MinRelatedness:      DefaultMinRelatedness,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	listed, err := ListIndexFiles(t.Context(), ListFilesOptions{Root: root, Rev: rev, Scope: []string{"pkg"}})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Index.Dir != out.Diagnostics.IndexDir {
		t.Fatalf("index dir = %q, want %q", listed.Index.Dir, out.Diagnostics.IndexDir)
	}
	if listed.Index.Mode != "revision" || listed.Index.ResolvedRev == "" {
		t.Fatalf("index = %#v, want revision with resolved rev", listed.Index)
	}
	if !slices.Contains(listed.Files, "pkg/app.go") {
		t.Fatalf("files = %v, missing scoped file", listed.Files)
	}
	if slices.Contains(listed.Files, "README.md") {
		t.Fatalf("files = %v, included out-of-scope file", listed.Files)
	}
}

func TestListIndexFilesWaitsForIndexLock(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.go", "package main\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		IndexOnly:           true,
		MinRelatedness:      DefaultMinRelatedness,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockIndex(t.Context(), out.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		listed, err := ListIndexFiles(t.Context(), ListFilesOptions{Root: root})
		if err == nil && !slices.Contains(listed.Files, "app.go") {
			err = errors.New("listed files missing app.go")
		}
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("ListIndexFiles returned while lock held: %v", err)
	case <-time.After(2 * indexLockPollInterval):
	}

	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFormatFileTreeEmpty(t *testing.T) {
	if got := FormatFileTree(nil); got != ".\n" {
		t.Fatalf("empty tree = %q", got)
	}
}
