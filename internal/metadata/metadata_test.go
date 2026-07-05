package metadata

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirUsesHomePathSHAAndMigratesLegacyDirectory(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)

	if err := os.MkdirAll(filepath.Join(root, dirName, "sessions", "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, dirName, "sessions", "old", "session.json"), []byte("{}\n"), 0o644); err != nil {
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
	if _, err := os.Stat(filepath.Join(dir, "sessions", "old", "session.json")); err != nil {
		t.Fatalf("migrated session missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, dirName)); !os.IsNotExist(err) {
		t.Fatalf("legacy dir stat err = %v, want not exist", err)
	}
}

func TestDirPreservesConflictingNewFilesDuringMigration(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, dirName, PathSHA(root))

	if err := os.MkdirAll(filepath.Join(root, dirName, "sessions", "same"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, dirName, "sessions", "same", "session.json"), []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions", "same"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions", "same", "session.json"), []byte("current\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Dir(root); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(dir, "sessions", "same", "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "current\n" {
		t.Fatalf("current file = %q, want preserved", current)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "sessions", "same", "session.legacy-*.json"))
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
