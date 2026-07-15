package metadata

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirUsesHomePathSHAAndMigratesLegacyDirectory(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)

	if err := os.MkdirAll(filepath.Join(root, dirName, "state", "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, dirName, "state", "old", "record.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, err := Dir(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, dirName, PathSHA(root))
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "state", "old", "record.json")); err != nil {
		t.Fatalf("migrated session missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, dirName)); !os.IsNotExist(err) {
		t.Fatalf("legacy dir stat err = %v, want not exist", err)
	}
	assertPrivateTree(t, dir)
}

func TestDirTightensExistingMetadataPermissions(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, dirName, PathSHA(root))
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "cache.json"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Dir(root); err != nil {
		t.Fatal(err)
	}
	assertPrivateTree(t, dir)
}

func TestSearchDirMigratesOnlySearchToOriginKey(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	legacy := filepath.Join(home, dirName, PathSHA(root))
	if err := os.MkdirAll(filepath.Join(legacy, "search", "fs", "index"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(legacy, "state", "record"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "search", "fs", "index", "manifest.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir, err := SearchDir(root, "github.com/acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(home, dirName, IdentitySHA("github.com/acme/repo")) {
		t.Fatalf("dir = %q", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "search", "fs", "index", "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "search")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy search still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "state", "record")); err != nil {
		t.Fatalf("legacy state removed: %v", err)
	}
}

func assertPrivateTree(t *testing.T, root string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if entry.IsDir() {
			want = 0o700
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", path, got, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDirPreservesConflictingNewFilesDuringMigration(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, dirName, PathSHA(root))

	if err := os.MkdirAll(filepath.Join(root, dirName, "state", "same"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, dirName, "state", "same", "record.json"), []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "state", "same"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "same", "record.json"), []byte("current\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Dir(root); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(dir, "state", "same", "record.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "current\n" {
		t.Fatalf("current file = %q, want preserved", current)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "state", "same", "record.legacy-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("legacy conflicts = %#v, want one", matches)
	}
	legacy, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(legacy) != "legacy\n" || !strings.Contains(filepath.Base(matches[0]), ".legacy-") {
		t.Fatalf("legacy file = %q at %s", legacy, matches[0])
	}
}

func TestDirSkipsMigrationWhenHomeMetadataContainsDestination(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)

	metadataRoot := filepath.Join(root, dirName)
	if err := os.MkdirAll(metadataRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadataRoot, "marker.txt"), []byte("home metadata\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, err := Dir(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(metadataRoot, PathSHA(root))
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	if _, err := os.Stat(filepath.Join(metadataRoot, "marker.txt")); err != nil {
		t.Fatalf("home metadata marker missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(want, PathSHA(root))); !os.IsNotExist(err) {
		t.Fatalf("nested metadata stat err = %v, want not exist", err)
	}
}

func TestDirSkipsMigrationWhenHomeSymlinkContainsDestination(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(root, home); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("HOME", home)

	metadataRoot := filepath.Join(root, dirName)
	if err := os.MkdirAll(metadataRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadataRoot, "marker.txt"), []byte("home metadata\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir, err := Dir(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, dirName, PathSHA(root))
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	if _, err := os.Stat(filepath.Join(metadataRoot, "marker.txt")); err != nil {
		t.Fatalf("home metadata marker missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(want, PathSHA(root))); !os.IsNotExist(err) {
		t.Fatalf("nested metadata stat err = %v, want not exist", err)
	}
}
