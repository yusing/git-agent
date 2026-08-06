package gitctx

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v6"
	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/utils/merkletrie"
	filesystemnode "github.com/go-git/go-git/v6/utils/merkletrie/filesystem"
	indexnode "github.com/go-git/go-git/v6/utils/merkletrie/index"
	"github.com/go-git/go-git/v6/utils/merkletrie/noder"
	"github.com/go-git/go-git/v6/x/plugin"
	ignorectx "github.com/yusing/git-agent/internal/ignore"
)

var emptyStatusNodeHash = make([]byte, 24)

func (r *Repository) status() (git.Status, error) {
	index, err := r.ReadIndex()
	if err != nil {
		return nil, err
	}
	config, err := r.Repo.Config()
	if err != nil {
		return nil, err
	}
	matcher, err := r.WorktreeIgnoreMatcher()
	if err != nil {
		return nil, err
	}
	worktree, err := r.Repo.Worktree()
	if err != nil {
		return nil, err
	}
	submodules, err := r.worktreeSubmoduleHashes()
	if err != nil {
		return nil, err
	}

	from := indexnode.NewRootNodeWithOptions(index, indexnode.RootNodeOptions{
		UpholdExecutableBit: config.Core.FileMode,
	})
	to := filesystemnode.NewRootNodeWithOptions(worktree.Filesystem(), submodules, filesystemnode.Options{
		AutoCRLF:      config.Core.AutoCRLF == "true" || config.Core.AutoCRLF == "input",
		Index:         index,
		IgnoreMatcher: matcher,
	})
	worktreeChanges, err := merkletrie.DiffTree(from, to, statusNodesEqual)
	if err != nil {
		return nil, err
	}

	status := make(git.Status)
	staged, err := r.stagedTreeDiff()
	if err != nil {
		return nil, err
	}
	for _, change := range staged.Status() {
		file := status.File(change.Path)
		file.Staging = git.StatusCode(change.Staging[0])
		file.Worktree = git.Unmodified
	}

	// Source: github.com/go-git/go-git/v6@v6.0.0-alpha.4/worktree_status.go:91:117 Worktree.status
	for _, change := range worktreeChanges {
		action, err := change.Action()
		if err != nil {
			return nil, err
		}
		path := change.To.String()
		if path == "" {
			path = change.From.String()
		}
		file := status.File(path)
		if file.Staging == git.Untracked {
			file.Staging = git.Unmodified
		}
		switch action {
		case merkletrie.Delete:
			file.Worktree = git.Deleted
		case merkletrie.Insert:
			file.Worktree = git.Untracked
			file.Staging = git.Untracked
		case merkletrie.Modify:
			file.Worktree = git.Modified
		}
	}
	return status, nil
}

// WorktreeIgnoreMatcher returns the effective Git ignore rules for the worktree.
func (r *Repository) WorktreeIgnoreMatcher() (ignorectx.Matcher, error) {
	matcher := ignorectx.New()
	globalExclude, err := r.globalExclude()
	if err != nil {
		return ignorectx.Matcher{}, err
	}
	matcher = matcher.Append(globalExclude, nil)

	exclude, err := r.repositoryExclude()
	if err != nil {
		return ignorectx.Matcher{}, err
	}
	matcher = matcher.Append(exclude, nil)

	err = filepath.WalkDir(r.RootPath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(r.RootPath, path)
		if err != nil {
			return err
		}
		if rel != "." {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			if matcher.Match(ignorectx.PathParts(rel), true) {
				return filepath.SkipDir
			}
			if _, gitErr := os.Lstat(filepath.Join(path, ".git")); gitErr == nil {
				return filepath.SkipDir
			} else if !errors.Is(gitErr, fs.ErrNotExist) {
				return gitErr
			}
		}

		data, err := os.ReadFile(filepath.Join(path, ".gitignore"))
		if err == nil {
			base := ignorectx.PathParts(rel)
			matcher = matcher.Append(string(data), base)
			return nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	})
	if err != nil {
		return ignorectx.Matcher{}, err
	}
	return matcher.Append(".git-agent/\n.omx/\n", nil), nil
}

func (r *Repository) globalExclude() (string, error) {
	path, configured, err := r.configuredExcludesFile()
	if err != nil {
		return "", err
	}
	if !configured {
		path, err = defaultGlobalExcludePath()
		if err != nil {
			return "", err
		}
	}
	if path == "" {
		return "", nil
	}
	path, err = expandGlobalExcludePath(path, r.RootPath)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(data), err
}

func (r *Repository) configuredExcludesFile() (string, bool, error) {
	source, err := plugin.Get(plugin.ConfigLoader())
	if err != nil {
		return "", false, err
	}

	var path string
	var configured bool
	for _, scope := range []gitconfig.Scope{gitconfig.SystemScope, gitconfig.GlobalScope} {
		storer, err := source.Load(scope)
		if err != nil {
			return "", false, err
		}
		cfg, err := storer.Config()
		if err != nil {
			return "", false, err
		}
		if value, ok := excludesFileOption(cfg); ok {
			path, configured = value, true
		}
	}

	local, err := r.Repo.Config()
	if err != nil {
		return "", false, err
	}
	if value, ok := excludesFileOption(local); ok {
		path, configured = value, true
	}
	return path, configured, nil
}

func excludesFileOption(cfg *gitconfig.Config) (string, bool) {
	if cfg == nil || cfg.Raw == nil || !cfg.Raw.HasSection("core") {
		return "", false
	}
	section := cfg.Raw.Section("core")
	if !section.HasOption("excludesFile") {
		return "", false
	}
	return section.Option("excludesFile"), true
}

func defaultGlobalExcludePath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "git", "ignore"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "git", "ignore"), nil
}

func expandGlobalExcludePath(path, root string) (string, error) {
	if !strings.HasPrefix(path, "~") {
		if filepath.IsAbs(path) {
			return filepath.Clean(path), nil
		}
		return filepath.Join(root, path), nil
	}

	name, rest, _ := strings.Cut(strings.TrimPrefix(filepath.ToSlash(path), "~"), "/")
	var home string
	if name == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", err
		}
	} else {
		account, err := user.Lookup(name)
		if err != nil {
			return "", err
		}
		home = account.HomeDir
	}
	return filepath.Join(home, filepath.FromSlash(rest)), nil
}

type repositoryFilesystemProvider interface {
	Filesystem() billy.Filesystem
}

func (r *Repository) repositoryExclude() (text string, returnErr error) {
	provider, ok := r.Repo.Storer.(repositoryFilesystemProvider)
	if !ok {
		return "", nil
	}
	filesystem := provider.Filesystem()
	file, err := filesystem.Open(filesystem.Join("info", "exclude"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	data, err := io.ReadAll(file)
	return string(data), err
}

// Source: github.com/go-git/go-git/v6@v6.0.0-alpha.4/worktree_status.go:268:282 diffTreeIsEquals
func statusNodesEqual(a, b noder.Hasher) bool {
	hashA := a.Hash()
	hashB := b.Hash()
	if bytes.Equal(hashA, emptyStatusNodeHash) || bytes.Equal(hashB, emptyStatusNodeHash) {
		return false
	}
	return bytes.Equal(hashA, hashB)
}
