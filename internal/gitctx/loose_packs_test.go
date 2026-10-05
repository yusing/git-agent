package gitctx

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-git/go-billy/v6/osfs"
)

func TestLoosePacksRepositoryReads(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"worktree", "gitdir", "linked-worktree", "bare", "submodule"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			dir := initTempRepo(t)
			writeFile(t, filepath.Join(dir, "app.txt"), "old\n")
			runGit(t, dir, "add", "app.txt")
			runGit(t, dir, "commit", "-m", "base")
			gitDir := filepath.Join(dir, ".git")
			var parent *Repository
			switch kind {
			case "linked-worktree":
				linked := filepath.Join(t.TempDir(), "linked")
				runGit(t, dir, "worktree", "add", "-b", "linked", linked)
				dir = linked
			case "bare":
				bare := filepath.Join(t.TempDir(), "bare.git")
				runGit(t, dir, "clone", "--bare", dir, bare)
				dir, gitDir = bare, bare
			case "submodule":
				parentDir := initTempRepo(t)
				runGit(t, parentDir, "-c", "protocol.file.allow=always", "submodule", "add", dir, "nested")
				var err error
				parent, err = Open(parentDir)
				if err != nil {
					t.Fatal(err)
				}
				dir = filepath.Join(parentDir, "nested")
				gitDir = filepath.Join(parentDir, ".git", "modules", "nested")
			}
			if kind != "bare" {
				writeFile(t, filepath.Join(dir, "app.txt"), "new\n")
				runGit(t, dir, "add", "app.txt")
			}
			packLooseObjects(t, dir, gitDir)

			var repo *Repository
			var err error
			switch kind {
			case "bare", "gitdir":
				repo, err = OpenGitDir(gitDir)
			case "submodule":
				repo, err = parent.OpenSubmodule("nested")
			default:
				repo, err = Open(dir)
			}
			if err != nil {
				t.Fatal(err)
			}
			content, _, err := repo.ShowFileAtRev("HEAD", "app.txt", 4096, 100)
			if err != nil || content != "old\n" {
				t.Fatalf("HEAD file = %q, error = %v", content, err)
			}
			if kind != "bare" {
				paths, err := repo.StagedPaths()
				if err != nil || !slices.Equal(paths, []string{"app.txt"}) {
					t.Fatalf("staged paths = %v, error = %v", paths, err)
				}
				diff, _, err := repo.StagedDiff(4096, 100)
				if err != nil || !strings.Contains(diff, "-old") || !strings.Contains(diff, "+new") {
					t.Fatalf("staged diff = %q, error = %v", diff, err)
				}
				if _, err := repo.StagedFingerprint(); err != nil {
					t.Fatal(err)
				}
			}
			// Reading must not create canonical aliases in the repository.
			packs, err := filepath.Glob(filepath.Join(gitDir, "objects", "pack", "loose-*.pack"))
			if err != nil {
				t.Fatal(err)
			}
			for _, pack := range packs {
				alias := filepath.Join(filepath.Dir(pack), strings.Replace(filepath.Base(pack), "loose-", "pack-", 1))
				if _, err := os.Stat(alias); !os.IsNotExist(err) {
					t.Fatalf("read created alias %s: %v", alias, err)
				}
			}
		})
	}
}

func packLooseObjects(t *testing.T, dir, gitDir string) {
	t.Helper()
	// Use Git's real maintenance producer, then remove duplicate loose objects
	// so a reader cannot pass by ignoring the maintenance pack.
	runGit(t, dir, "maintenance", "run", "--task=loose-objects")
	runGit(t, dir, "prune-packed")
	packs, err := filepath.Glob(filepath.Join(gitDir, "objects", "pack", "loose-*.pack"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("maintenance packs = %v, error = %v", packs, err)
	}
}

func TestLoosePackFilesystemAliases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hash := strings.Repeat("a", 40)
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		writeFile(t, filepath.Join(dir, "objects", "pack", "loose-"+hash+ext), "loose")
	}
	writeFile(t, filepath.Join(dir, "objects", "pack", "pack-"+hash+".idx"), "canonical")
	writeFile(t, filepath.Join(dir, "objects", "pack", "loose-invalid.pack"), "invalid")
	writeFile(t, filepath.Join(dir, "elsewhere", "loose-"+hash+".pack"), "outside")
	f := &loosePackFilesystem{osfs.New(dir)}
	entries, err := f.ReadDir(filepath.Join("objects", "pack"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}
	if !slices.IsSorted(names) || len(names) != 7 || slices.Contains(names, "pack-invalid.pack") {
		t.Fatalf("pack directory entries = %v", names)
	}
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		path := filepath.Join("objects", "pack", "pack-"+hash+ext)
		file, err := f.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		file.Close()
		info, err := f.Stat(path)
		wantSize := int64(len("loose"))
		if ext == ".idx" {
			wantSize = int64(len("canonical"))
		}
		if err != nil || info.Size() != wantSize {
			t.Fatalf("Stat(%s) = %v, error = %v", path, info, err)
		}
	}
	if _, err := f.Open(filepath.Join("elsewhere", "pack-"+hash+".pack")); !os.IsNotExist(err) {
		t.Fatalf("aliased outside pack directory: %v", err)
	}
}
