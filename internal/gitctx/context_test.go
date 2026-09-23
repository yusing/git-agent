package gitctx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestOpenIdentifiesDirectoryOutsideRepository(t *testing.T) {
	t.Parallel()

	_, err := Open(t.TempDir())
	if !errors.Is(err, ErrNotRepository) {
		t.Fatalf("Open error = %v, want ErrNotRepository", err)
	}
}

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

func TestStagedInspectionDoesNotRequireWorktreeAccess(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "staged.txt"), "base\n")
	runGit(t, repoDir, "add", "staged.txt")
	runGit(t, repoDir, "commit", "-m", "base")
	writeFile(t, filepath.Join(repoDir, "staged.txt"), "staged\n")
	runGit(t, repoDir, "add", "staged.txt")

	repo, err := OpenGitDir(filepath.Join(repoDir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	paths, err := repo.StagedPaths()
	if err != nil {
		t.Fatalf("StagedPaths required worktree access: %v", err)
	}
	if !slices.Equal(paths, []string{"staged.txt"}) {
		t.Fatalf("paths = %#v, want [staged.txt]", paths)
	}
	status, err := repo.StagedStatus()
	if err != nil {
		t.Fatalf("StagedStatus required worktree access: %v", err)
	}
	if len(status) != 1 || status[0].Path != "staged.txt" {
		t.Fatalf("status = %#v, want staged.txt only", status)
	}
}

func TestStagedStatusComesFromHeadAndIndex(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	for _, path := range []string{"deleted.txt", "modified.txt", "old-name.txt"} {
		writeFile(t, filepath.Join(repoDir, path), "base\n")
	}
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "base")
	writeFile(t, filepath.Join(repoDir, "added.txt"), "added\n")
	writeFile(t, filepath.Join(repoDir, "modified.txt"), "modified\n")
	runGit(t, repoDir, "add", "added.txt", "modified.txt")
	runGit(t, repoDir, "rm", "deleted.txt")
	runGit(t, repoDir, "mv", "old-name.txt", "new-name.txt")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	status, err := repo.StagedStatus()
	if err != nil {
		t.Fatal(err)
	}
	want := []PathChange{
		{Path: "added.txt", Staging: "A"},
		{Path: "deleted.txt", Staging: "D"},
		{Path: "modified.txt", Staging: "M"},
		{Path: "new-name.txt", Staging: "A"},
		{Path: "old-name.txt", Staging: "D"},
	}
	if !slices.Equal(status, want) {
		t.Fatalf("status = %#v, want %#v", status, want)
	}
}

func TestStagedInspectionIgnoresUnrelatedWorktreeCollisionsAndUnknownIgnoreFiles(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "left", "shared.txt"), "base\n")
	writeFile(t, filepath.Join(repoDir, "right", "shared.txt"), "base\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "base")
	writeFile(t, filepath.Join(repoDir, "left", "shared.txt"), "staged\n")
	runGit(t, repoDir, "add", "left/shared.txt")
	writeFile(t, filepath.Join(repoDir, "right", "shared.txt"), "unstaged collision\n")
	writeBinaryFile(t, filepath.Join(repoDir, ".gitignore.future"), []byte{'[', 0, ']'})
	writeFile(t, filepath.Join(repoDir, "unknown-ignore-format", "untracked.txt"), "unrelated\n")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := repo.StagedPaths()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{"left/shared.txt"}) {
		t.Fatalf("paths = %#v, want [left/shared.txt]", paths)
	}
	status, err := repo.StagedStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(status, []PathChange{{Path: "left/shared.txt", Staging: "M"}}) {
		t.Fatalf("status = %#v, want only staged collision path", status)
	}
}

func TestStagedInspectionRejectsMalformedIndex(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "staged.txt"), "staged\n")
	runGit(t, repoDir, "add", "staged.txt")
	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".git", "index"), []byte("not a git index"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.StagedPaths(); err == nil {
		t.Fatal("StagedPaths accepted malformed index")
	}
}

func TestLogMessagesFromIncludesFullCommitMessage(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "app.txt"), "base\n")
	runGit(t, repoDir, "add", "app.txt")
	runGit(t, repoDir, "commit", "-m", "base")
	baseSHA := gitHead(t, repoDir)

	writeFile(t, filepath.Join(repoDir, "app.txt"), "release\n")
	runGit(t, repoDir, "add", "app.txt")
	runGit(t, repoDir, "commit",
		"-m", "feat(webui): add command option highlighting",
		"-m", "Adds completion support for RuleDo option blocks.",
		"-m", "Highlights pass and bypass variants.",
	)

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	commits, err := repo.LogMessagesFrom(baseSHA, "HEAD", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 {
		t.Fatalf("commits = %d", len(commits))
	}
	if commits[0].Summary != "feat(webui): add command option highlighting" {
		t.Fatalf("summary = %q", commits[0].Summary)
	}
	for _, want := range []string{
		"feat(webui): add command option highlighting",
		"Adds completion support for RuleDo option blocks.",
		"Highlights pass and bypass variants.",
	} {
		if !strings.Contains(commits[0].Message, want) {
			t.Fatalf("message missing %q:\n%s", want, commits[0].Message)
		}
	}
	if commits[0].Diffstat.FilesChanged != 1 || commits[0].Diffstat.Additions == 0 {
		t.Fatalf("diffstat = %#v", commits[0].Diffstat)
	}
	if len(commits[0].Files) != 1 || commits[0].Files[0].Path != "app.txt" || commits[0].Files[0].Status != "modified" {
		t.Fatalf("files = %#v", commits[0].Files)
	}
	if !strings.Contains(commits[0].PatchExcerpt, "--- app.txt") || !strings.Contains(commits[0].PatchExcerpt, "+ release") {
		t.Fatalf("patch excerpt missing expected change:\n%s", commits[0].PatchExcerpt)
	}

	show, truncated, err := repo.ShowCommit("HEAD", 16*1024, 400)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("show commit marked truncated")
	}
	for _, want := range []string{
		"feat(webui): add command option highlighting",
		"Adds completion support for RuleDo option blocks.",
		"+release",
	} {
		if !strings.Contains(show, want) {
			t.Fatalf("show commit missing %q:\n%s", want, show)
		}
	}
	filtered, _, err := repo.ShowCommitForPaths("HEAD", []string{"missing.txt"}, 16*1024, 400)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(filtered, "+release") {
		t.Fatalf("path-filtered show leaked unrelated patch:\n%s", filtered)
	}
}

func TestOpenSubmoduleRejectsEscapingPaths(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "app.txt"), "base\n")
	runGit(t, repoDir, "add", "app.txt")
	runGit(t, repoDir, "commit", "-m", "base")
	nested := filepath.Join(repoDir, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(nested, "inner.txt"), "inner\n")
	runGit(t, nested, "init")
	runGit(t, nested, "config", "user.name", "Test User")
	runGit(t, nested, "config", "user.email", "test@example.com")
	runGit(t, nested, "add", "inner.txt")
	runGit(t, nested, "commit", "-m", "nested base")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := repo.OpenSubmodule("nested")
	if err != nil {
		t.Fatal(err)
	}
	show, _, err := sub.ShowCommit("HEAD", 4096, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(show, "nested base") {
		t.Fatalf("submodule show used parent evidence:\n%s", show)
	}
	if _, err := repo.OpenSubmodule("../secret"); err == nil {
		t.Fatal("escaped parent directory")
	}

	plain := filepath.Join(repoDir, "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.OpenSubmodule("plain"); !errors.Is(err, ErrNotRepository) {
		t.Fatalf("plain directory error = %v, want ErrNotRepository", err)
	}
}

func TestStagedStatusExcludesUnstagedOnlyChanges(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "staged.txt"), "old\n")
	writeFile(t, filepath.Join(repoDir, "unstaged_deleted.txt"), "delete me\n")
	writeFile(t, filepath.Join(repoDir, "unstaged_modified.txt"), "old\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "Initial commit")

	writeFile(t, filepath.Join(repoDir, "staged.txt"), "new\n")
	runGit(t, repoDir, "add", "staged.txt")
	if err := os.Remove(filepath.Join(repoDir, "unstaged_deleted.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repoDir, "unstaged_modified.txt"), "new\n")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	status, err := repo.StagedStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(status) != 1 {
		t.Fatalf("status = %#v, want only staged path", status)
	}
	if status[0].Path != "staged.txt" {
		t.Fatalf("status = %#v, want staged.txt only", status)
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

func TestPullRequestDiffComparesHeadToOriginHead(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "app.txt"), "base\n")
	runGit(t, repoDir, "add", "app.txt")
	runGit(t, repoDir, "commit", "-m", "Initial commit")
	baseSHA := gitHead(t, repoDir)
	runGit(t, repoDir, "update-ref", "refs/remotes/origin/HEAD", baseSHA)

	writeFile(t, filepath.Join(repoDir, "app.txt"), "branch\n")
	writeFile(t, filepath.Join(repoDir, "docs", "note.md"), "new doc\n")
	runGit(t, repoDir, "add", "app.txt", "docs/note.md")
	runGit(t, repoDir, "commit", "-m", "feat: update branch files")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	base, err := repo.PullRequestBase()
	if err != nil {
		t.Fatal(err)
	}
	if base.SHA != baseSHA {
		t.Fatalf("base SHA = %s, want %s", base.SHA, baseSHA)
	}
	paths, err := repo.PullRequestPaths()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "app.txt,docs/note.md" {
		t.Fatalf("paths = %#v", paths)
	}
	diff, truncated, err := repo.PullRequestDiff(16*1024, 400)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("unexpected truncation")
	}
	for _, want := range []string{
		"diff --git a/app.txt b/app.txt",
		"-base",
		"+branch",
		"diff --git a/docs/note.md b/docs/note.md",
		"+new doc",
	} {
		if !strings.Contains(diff, want) {
			t.Fatalf("pr diff missing %q:\n%s", want, diff)
		}
	}
	commits, err := repo.PullRequestCommits(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || commits[0].Summary != "feat: update branch files" {
		t.Fatalf("commits = %#v", commits)
	}
}

func TestPullRequestCommitsExcludeOriginHeadReachableHistoryWhenBaseAdvanced(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "app.txt"), "base\n")
	runGit(t, repoDir, "add", "app.txt")
	runGit(t, repoDir, "commit", "-m", "Initial commit")
	baseSHA := gitHead(t, repoDir)

	runGit(t, repoDir, "checkout", "-b", "feature")
	writeFile(t, filepath.Join(repoDir, "feature.txt"), "feature\n")
	runGit(t, repoDir, "add", "feature.txt")
	runGit(t, repoDir, "commit", "-m", "feat: branch change")

	runGit(t, repoDir, "checkout", "-B", "main", baseSHA)
	writeFile(t, filepath.Join(repoDir, "main.txt"), "main\n")
	runGit(t, repoDir, "add", "main.txt")
	runGit(t, repoDir, "commit", "-m", "chore: advance default branch")
	advancedMainSHA := gitHead(t, repoDir)
	runGit(t, repoDir, "update-ref", "refs/remotes/origin/HEAD", advancedMainSHA)

	runGit(t, repoDir, "checkout", "feature")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	commits, err := repo.PullRequestCommits(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 {
		t.Fatalf("commits = %#v, want only feature branch commit", commits)
	}
	if commits[0].Summary != "feat: branch change" {
		t.Fatalf("commit summary = %q, want feature branch commit", commits[0].Summary)
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
	stats, err := repo.StagedStat()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Path != "new.txt" {
		t.Fatalf("rename staged stats = %#v", stats)
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
	stats, err := repo.StagedStat()
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Path != "bin.dat" || !stats[0].IsBinary {
		t.Fatalf("binary staged stats = %#v", stats)
	}
}

func TestStagedStatAlignsAroundBinaryPatch(t *testing.T) {
	t.Parallel()

	repoDir := initTempRepo(t)
	writeFile(t, filepath.Join(repoDir, "a.txt"), "first\n")
	writeBinaryFile(t, filepath.Join(repoDir, "b.bin"), []byte{0x00, 0x01, 0x02, 0x03})
	writeFile(t, filepath.Join(repoDir, "c.txt"), "third\nline\n")
	runGit(t, repoDir, "add", "a.txt", "b.bin", "c.txt")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := repo.StagedStat()
	if err != nil {
		t.Fatal(err)
	}
	want := []FileStat{
		{Path: "a.txt", Adds: 1},
		{Path: "b.bin", IsBinary: true},
		{Path: "c.txt", Adds: 2},
	}
	if !slices.Equal(stats, want) {
		t.Fatalf("mixed staged stats = %#v, want %#v", stats, want)
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

func TestStagedSubmoduleChangesDetectsMovedIndexPointers(t *testing.T) {
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
	runGit(t, subDir, "commit", "-m", "fix(webui): refresh login")
	releaseSHA := gitHead(t, subDir)

	repoDir := initTempRepo(t)
	runGit(t, repoDir, "-c", "protocol.file.allow=always", "submodule", "add", subDir, "webui")
	runGit(t, filepath.Join(repoDir, "webui"), "checkout", baseSHA)
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "feat: add webui submodule")

	runGit(t, filepath.Join(repoDir, "webui"), "checkout", releaseSHA)
	runGit(t, repoDir, "add", "webui")

	repo, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := repo.StagedSubmoduleChanges()
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
	diff, _, err := repo.StagedDiff(16*1024, 400)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, baseSHA) || !strings.Contains(diff, releaseSHA) {
		t.Fatalf("staged submodule diff missing gitlink revisions:\n%s", diff)
	}
	if !strings.Contains(diff, "Submodule commits webui") || !strings.Contains(diff, "fix(webui): refresh login") {
		t.Fatalf("staged submodule diff missing local commit summaries:\n%s", diff)
	}
	filtered, _, err := repo.StagedDiffForPaths([]string{"webui"}, 16*1024, 400)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filtered, "fix(webui): refresh login") {
		t.Fatalf("filtered staged submodule diff missing local commit summaries:\n%s", filtered)
	}
	paths, err := repo.StagedPaths()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{"webui"}) {
		t.Fatalf("staged paths = %#v, want [webui]", paths)
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
	gitArgs := append([]string{"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "tag.forceSignAnnotated=false"}, args...)
	cmd := exec.Command("git", gitArgs...)
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
