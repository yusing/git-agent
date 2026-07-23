package gitctx

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStagedReviewSnapshotAndMaterializationUseRecursiveIndexes(t *testing.T) {
	wikiSource := initTempRepo(t)
	writeFile(t, filepath.Join(wikiSource, "go.mod"), "module example/wiki\n")
	writeFile(t, filepath.Join(wikiSource, "wiki.go"), "package wiki\n\nconst value = \"base\"\n")
	runGit(t, wikiSource, "add", "go.mod", "wiki.go")
	runGit(t, wikiSource, "commit", "-m", "wiki base")

	webSource := initTempRepo(t)
	writeFile(t, filepath.Join(webSource, "go.mod"), "module example/web\n")
	writeFile(t, filepath.Join(webSource, "ui.go"), "package web\n\nconst value = \"base\"\n")
	runGit(t, webSource, "add", "go.mod", "ui.go")
	runGit(t, webSource, "commit", "-m", "web base")
	runGit(t, webSource, "-c", "protocol.file.allow=always", "submodule", "add", wikiSource, "wiki")
	runGit(t, webSource, "commit", "-m", "add wiki")

	root := initTempRepo(t)
	writeFile(t, filepath.Join(root, "go.mod"), "module example/root\n")
	writeFile(t, filepath.Join(root, "root.go"), "package root\n\nconst value = \"base\"\n")
	runGit(t, root, "add", "go.mod", "root.go")
	runGit(t, root, "commit", "-m", "root base")
	runGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", webSource, "web")
	runGit(t, root, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")
	runGit(t, root, "commit", "-m", "add web")

	stageThenRewrite(t, root, "root.go", "package root\n\nconst value = \"index\"\n", "package root\n\nconst value = \"worktree\"\n")
	stageThenRewrite(t, filepath.Join(root, "web"), "ui.go", "package web\n\nconst value = \"index\"\n", "package web\n\nconst value = \"worktree\"\n")
	stageThenRewrite(t, filepath.Join(root, "web", "wiki"), "wiki.go", "package wiki\n\nconst value = \"index\"\n", "package wiki\n\nconst value = \"worktree\"\n")
	writeFile(t, filepath.Join(root, "untracked.go"), "package untracked\n")

	repo, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := repo.StagedReviewSnapshot(64*1024, 2000)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"root.go", "web/ui.go", "web/wiki/wiki.go"} {
		if !slices.Contains(snapshot.Paths, path) {
			t.Errorf("snapshot paths = %#v, missing %q", snapshot.Paths, path)
		}
	}
	if !slices.Equal(snapshot.Components, []string{"", "web", "web/wiki"}) {
		t.Fatalf("snapshot components = %#v", snapshot.Components)
	}
	for _, marker := range []string{`Repository "web"`, `Repository "web/wiki"`} {
		if !strings.Contains(snapshot.Diff, marker) {
			t.Errorf("snapshot diff missing %q:\n%s", marker, snapshot.Diff)
		}
	}

	reader, err := repo.OpenStagedReviewFile(FileSourceIndex, "web/wiki/wiki.go")
	if err != nil {
		t.Fatal(err)
	}
	nestedIndex, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if strings.Contains(string(nestedIndex), "worktree") || !strings.Contains(string(nestedIndex), "index") {
		t.Fatalf("nested index content = %q", nestedIndex)
	}

	destination := t.TempDir()
	if err := os.Chmod(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := repo.MaterializeStagedReview(destination); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"root.go", "web/ui.go", "web/wiki/wiki.go"} {
		content, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "worktree") || !strings.Contains(string(content), "index") {
			t.Errorf("materialized %s = %q", path, content)
		}
	}
	if _, err := os.Stat(filepath.Join(destination, "untracked.go")); !os.IsNotExist(err) {
		t.Fatalf("untracked file materialized: %v", err)
	}

	wantFingerprint := snapshot.Fingerprint
	writeFile(t, filepath.Join(root, "web", "wiki", "wiki.go"), "package wiki\n\nconst value = \"later index\"\n")
	runGit(t, filepath.Join(root, "web", "wiki"), "add", "wiki.go")
	if err := repo.CheckStagedReviewFingerprint(wantFingerprint); err != ErrChangeSnapshotStale {
		t.Fatalf("nested drift error = %v, want %v", err, ErrChangeSnapshotStale)
	}
}

func TestMaterializeStagedReviewRejectsUnsafeDestinations(t *testing.T) {
	root := initTempRepo(t)
	writeFile(t, filepath.Join(root, "go.mod"), "module example/root\n")
	runGit(t, root, "add", "go.mod")
	runGit(t, root, "commit", "-m", "base")
	repo, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}

	broad := t.TempDir()
	if err := os.Chmod(broad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := repo.MaterializeStagedReview(broad); err == nil {
		t.Fatal("broad destination permissions accepted")
	}
	nonempty := t.TempDir()
	if err := os.Chmod(nonempty, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(nonempty, "collision"), "user data")
	if err := repo.MaterializeStagedReview(nonempty); err == nil {
		t.Fatal("nonempty destination accepted")
	}
}

func stageThenRewrite(t *testing.T, repository, path, staged, worktree string) {
	t.Helper()
	absolutePath := filepath.Join(repository, filepath.FromSlash(path))
	writeFile(t, absolutePath, staged)
	runGit(t, repository, "add", path)
	writeFile(t, absolutePath, worktree)
}
