package gitctx

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-billy/v6"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/filesystem"
)

func openRepository(path string, detect bool) (*git.Repository, error) {
	repo, err := git.PlainOpenWithOptions(path, &git.PlainOpenOptions{DetectDotGit: detect})
	if err != nil {
		return nil, err
	}
	// PlainOpen always returns filesystem storage. Preserve its common-directory,
	// index, reference, and worktree handling; adapt only object reads.
	storage := repo.Storer.(*filesystem.Storage)
	adapted := filesystem.NewStorage(&loosePackFilesystem{storage.Filesystem()}, nil)
	storage.ObjectStorage = adapted.ObjectStorage
	return repo, nil
}

// Git maintenance writes loose-<hash>.pack, but go-git only discovers pack-*.pack.
// Present read aliases without renaming files or redirecting filesystem writes.
type loosePackFilesystem struct {
	billy.Filesystem
}

func (f *loosePackFilesystem) ReadDir(path string) ([]fs.DirEntry, error) {
	entries, err := f.Filesystem.ReadDir(path)
	if err != nil || filepath.Clean(path) != filepath.Join("objects", "pack") {
		return entries, err
	}
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := packAlias(entry.Name(), "loose-", "pack-")
		if name != entry.Name() && !names[name] {
			entries = append(entries, packAliasEntry{DirEntry: entry, name: name})
		}
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, nil
}

func (f *loosePackFilesystem) Open(path string) (billy.File, error) {
	file, err := f.Filesystem.Open(path)
	if os.IsNotExist(err) {
		if alias := loosePackPath(path); alias != path {
			return f.Filesystem.Open(alias)
		}
	}
	return file, err
}

func (f *loosePackFilesystem) Stat(path string) (fs.FileInfo, error) {
	info, err := f.Filesystem.Stat(path)
	if os.IsNotExist(err) {
		if alias := loosePackPath(path); alias != path {
			return f.Filesystem.Stat(alias)
		}
	}
	return info, err
}

func (f *loosePackFilesystem) Chroot(path string) (billy.Filesystem, error) {
	root, err := f.Filesystem.Chroot(path)
	if err != nil {
		return nil, err
	}
	return &loosePackFilesystem{root}, nil
}

func loosePackPath(path string) string {
	if filepath.Dir(path) != filepath.Join("objects", "pack") {
		return path
	}
	return filepath.Join(filepath.Dir(path), packAlias(filepath.Base(path), "pack-", "loose-"))
}

func packAlias(name, from, to string) string {
	ext := filepath.Ext(name)
	if !strings.HasPrefix(name, from) || (ext != ".pack" && ext != ".idx" && ext != ".rev") {
		return name
	}
	hash := strings.TrimSuffix(strings.TrimPrefix(name, from), ext)
	parsed := plumbing.NewHash(hash)
	if parsed.IsZero() || parsed.String() != hash {
		return name
	}
	return to + hash + ext
}

type packAliasEntry struct {
	fs.DirEntry
	name string
}

func (e packAliasEntry) Name() string { return e.name }
