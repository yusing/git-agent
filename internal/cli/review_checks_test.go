package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yusing/git-agent/internal/gitctx"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
)

func TestCodebaseReviewCheckSessionPrunesRepositoryIgnoredModules(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions required")
	}

	root := initRepo(t)
	runGit(t, root, "commit", "-m", "base")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(root, "ignored")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "go.mod"), []byte("module ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	repo, err := gitctx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	session, err := newReviewCheckSession(repo, reviewtask.ModeCodebase, reviewtask.PreparedContext{
		Components: []string{""},
	})
	if err != nil {
		t.Fatalf("ignored unreadable module blocked codebase check planning: %v", err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	if !session.scope.Ignored("ignored", true) {
		t.Fatal("codebase checker scope lost repository ignore rules")
	}
}
