package gitctx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenAndInspectStagedChanges(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "app.txt"), "old\n")
	runGit(t, repoDir, "add", "app.txt")
	runGit(t, repoDir, "commit", "-m", "Initial commit")
	writeFile(t, filepath.Join(repoDir, "app.txt"), "new\n")
	runGit(t, repoDir, "add", "app.txt")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if repo.RootPath != repoDir {
		t.Fatalf("RootPath = %q, want %q", repo.RootPath, repoDir)
	}
	paths, err := repo.StagedPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "app.txt" {
		t.Fatalf("paths = %#v", paths)
	}
	diff, truncated, err := repo.StagedDiff(4096, 200)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if !strings.Contains(diff, "-old") || !strings.Contains(diff, "+new") {
		t.Fatalf("diff missing expected content:\n%s", diff)
	}
}

func TestFinalAmendedDiffOverlaysStagedChangesOnHead(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "app.txt"), "base\n")
	runGit(t, repoDir, "add", "app.txt")
	runGit(t, repoDir, "commit", "-m", "Initial commit")
	writeFile(t, filepath.Join(repoDir, "app.txt"), "head\n")
	runGit(t, repoDir, "add", "app.txt")
	runGit(t, repoDir, "commit", "-m", "Update app")
	writeFile(t, filepath.Join(repoDir, "app.txt"), "amended\n")
	runGit(t, repoDir, "add", "app.txt")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	diff, truncated, err := repo.FinalAmendedDiff(4096, 200)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if !strings.Contains(diff, "-base") || !strings.Contains(diff, "+amended") || strings.Contains(diff, "+head") {
		t.Fatalf("final amended diff did not compare parent to staged result:\n%s", diff)
	}
}

func TestStagedDiffPreservesMultipleHunksAndBlankLines(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	original := strings.Join([]string{
		"one",
		"two",
		"",
		"four",
		"five",
		"six",
		"seven",
		"eight",
		"nine",
		"ten",
		"",
	}, "\n")
	updated := strings.Join([]string{
		"one",
		"TWO",
		"three",
		"four",
		"five",
		"six",
		"seven",
		"eight",
		"NINE",
		"ten",
		"",
	}, "\n")

	writeFile(t, filepath.Join(repoDir, "dir", "nested.txt"), original)
	runGit(t, repoDir, "add", "dir/nested.txt")
	runGit(t, repoDir, "commit", "-m", "Initial commit")
	writeFile(t, filepath.Join(repoDir, "dir", "nested.txt"), updated)
	runGit(t, repoDir, "add", "dir/nested.txt")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	diff, truncated, err := repo.StagedDiff(16*1024, 400)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if !strings.Contains(diff, "diff --git a/dir/nested.txt b/dir/nested.txt") {
		t.Fatalf("diff missing nested path header:\n%s", diff)
	}
	if strings.Count(diff, "@@") < 2 {
		t.Fatalf("diff should preserve multiple hunks:\n%s", diff)
	}
	if !strings.Contains(diff, "\n-\n") || !strings.Contains(diff, "\n+three\n") {
		t.Fatalf("diff should preserve blank-line deletion and nearby additions:\n%s", diff)
	}
}

func TestFinalAmendedDiffForRootCommitIncludesUnchangedNestedFiles(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "app.txt"), "one\n")
	writeFile(t, filepath.Join(repoDir, "dir", "keep.txt"), "keep\n")
	runGit(t, repoDir, "add", "app.txt", "dir/keep.txt")
	runGit(t, repoDir, "commit", "-m", "Initial commit")
	writeFile(t, filepath.Join(repoDir, "app.txt"), "two\n")
	runGit(t, repoDir, "add", "app.txt")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	diff, truncated, err := repo.FinalAmendedDiff(16*1024, 400)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	for _, want := range []string{
		"diff --git a/app.txt b/app.txt",
		"diff --git a/dir/keep.txt b/dir/keep.txt",
		"+keep",
		"+two",
	} {
		if !strings.Contains(diff, want) {
			t.Fatalf("final amended diff missing %q:\n%s", want, diff)
		}
	}
}

func TestStagedDiffWithoutHeadUsesInitialCommitStylePatch(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "first.txt"), "hello\n")
	runGit(t, repoDir, "add", "first.txt")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	diff, truncated, err := repo.StagedDiff(16*1024, 400)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	for _, want := range []string{
		"diff --git a/first.txt b/first.txt",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/first.txt",
		"+hello",
	} {
		if !strings.Contains(diff, want) {
			t.Fatalf("initial staged diff missing %q:\n%s", want, diff)
		}
	}
}

func TestStagedDiffPreservesRenameHeaders(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "old.txt"), "same\n")
	runGit(t, repoDir, "add", "old.txt")
	runGit(t, repoDir, "commit", "-m", "Initial commit")
	runGit(t, repoDir, "mv", "old.txt", "new.txt")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	diff, truncated, err := repo.StagedDiff(16*1024, 400)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	for _, want := range []string{
		"rename from old.txt",
		"rename to new.txt",
	} {
		if !strings.Contains(diff, want) {
			t.Fatalf("rename diff missing %q:\n%s", want, diff)
		}
	}
}

func TestStagedDiffPreservesBinaryMarkers(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeBinaryFile(t, filepath.Join(repoDir, "bin.dat"), []byte{0x00, 0x01, 0x02, 0x03})
	runGit(t, repoDir, "add", "bin.dat")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	diff, truncated, err := repo.StagedDiff(16*1024, 400)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	if !strings.Contains(diff, "Binary files /dev/null and b/bin.dat differ") {
		t.Fatalf("binary diff missing marker:\n%s", diff)
	}
}

func TestSubmoduleGitlinkRangeDetectsMovedSubmodulePointers(t *testing.T) {
	t.Parallel()

	subDir := filepath.Join(t.TempDir(), "webui")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, subDir, "init")
	runGit(t, subDir, "config", "user.name", "Test User")
	runGit(t, subDir, "config", "user.email", "test@example.com")
	writeFile(t, filepath.Join(subDir, "ui.txt"), "v1\n")
	runGit(t, subDir, "add", "ui.txt")
	runGit(t, subDir, "commit", "-m", "feat(webui): initial")
	baseSHA := gitHead(t, subDir)
	writeFile(t, filepath.Join(subDir, "ui.txt"), "v2\n")
	runGit(t, subDir, "add", "ui.txt")
	runGit(t, subDir, "commit", "-m", "docs(webui): refresh")
	releaseSHA := gitHead(t, subDir)

	repoDir := initTempRepo(t)
	runGit(t, repoDir, "-c", "protocol.file.allow=always", "submodule", "add", subDir, "webui")
	runGit(t, filepath.Join(repoDir, "webui"), "checkout", baseSHA)
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "feat: add webui submodule")
	runGit(t, repoDir, "tag", "-m", "v1.0.0", "v1.0.0")

	runGit(t, filepath.Join(repoDir, "webui"), "checkout", releaseSHA)
	runGit(t, repoDir, "add", "webui")
	runGit(t, repoDir, "commit", "-m", "chore: update webui")
	runGit(t, repoDir, "tag", "-m", "v1.1.0", "v1.1.0")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := repo.SubmoduleGitlinkRange("v1.0.0", "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %#v", changes)
	}
	change := changes[0]
	if change.Path != "webui" || change.Old != baseSHA || change.New != releaseSHA {
		t.Fatalf("change = %#v", change)
	}
}

func initTempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func gitHead(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeBinaryFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}
