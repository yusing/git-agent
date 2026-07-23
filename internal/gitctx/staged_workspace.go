package gitctx

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/yusing/git-agent/internal/textutil"
)

type stagedReviewWorkspace struct {
	components []stagedReviewComponent
}

type stagedReviewComponent struct {
	prefix          string
	repo            *Repository
	baseTree        *object.Tree
	targetTree      *object.Tree
	diff            *treeDiff
	fingerprint     ChangeFingerprint
	expectedBase    string
	baseUnavailable bool
}

func (r *Repository) stagedReviewWorkspace(includeDiff bool) (*stagedReviewWorkspace, error) {
	baseTree, err := r.headTree()
	if err != nil {
		return nil, err
	}
	workspace := &stagedReviewWorkspace{}
	visited := map[string]bool{}
	if err := workspace.collect(r, "", baseTree, "", false, includeDiff, visited); err != nil {
		return nil, err
	}
	return workspace, nil
}

func (w *stagedReviewWorkspace) collect(
	repo *Repository,
	prefix string,
	baseTree *object.Tree,
	expectedBase string,
	baseUnavailable bool,
	includeDiff bool,
	visited map[string]bool,
) error {
	root, err := filepath.EvalSymlinks(repo.RootPath)
	if err != nil {
		return err
	}
	if visited[root] {
		return nil
	}
	visited[root] = true

	targetTree, err := repo.indexTree()
	if err != nil {
		return fmt.Errorf("snapshot staged repository %q: %w", prefix, err)
	}
	var diff *treeDiff
	if includeDiff {
		diff, err = newTreeDiff(baseTree, targetTree)
		if err != nil {
			return fmt.Errorf("diff staged repository %q: %w", prefix, err)
		}
	}
	w.components = append(w.components, stagedReviewComponent{
		prefix:          prefix,
		repo:            repo,
		baseTree:        baseTree,
		targetTree:      targetTree,
		diff:            diff,
		fingerprint:     changeFingerprint(baseTree, targetTree, nil),
		expectedBase:    expectedBase,
		baseUnavailable: baseUnavailable,
	})

	submodules, err := initializedSubmodules(repo, baseTree)
	if err != nil {
		return fmt.Errorf("discover staged nested repositories below %q: %w", prefix, err)
	}
	for _, submodule := range submodules {
		subPrefix := prefixRepoPath(prefix, submodule.path)
		if err := w.collect(
			submodule.repo,
			subPrefix,
			submodule.baseTree,
			submodule.expectedBase,
			submodule.baseUnavailable,
			includeDiff,
			visited,
		); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) StagedReviewSnapshot(maxBytes, maxLines int) (ChangeSnapshot, error) {
	workspace, err := r.stagedReviewWorkspace(true)
	if err != nil {
		return ChangeSnapshot{}, err
	}
	return workspace.snapshot(maxBytes, maxLines)
}

func (r *Repository) StagedReviewDiffForPaths(paths []string, maxBytes, maxLines int) (string, bool, error) {
	if paths != nil && len(paths) == 0 {
		return "", false, nil
	}
	workspace, err := r.stagedReviewWorkspace(true)
	if err != nil {
		return "", false, err
	}
	return workspace.diffForPaths(paths, maxBytes, maxLines)
}

func (r *Repository) StagedReviewFingerprint() (ChangeFingerprint, error) {
	workspace, err := r.stagedReviewWorkspace(false)
	if err != nil {
		return ChangeFingerprint{}, err
	}
	return workspace.fingerprint(), nil
}

func (r *Repository) CheckStagedReviewFingerprint(want ChangeFingerprint) error {
	got, err := r.StagedReviewFingerprint()
	return checkChangeFingerprint(want, got, err)
}

func (r *Repository) OpenStagedReviewFile(source FileSource, path string) (io.ReadCloser, error) {
	baseTree, err := r.headTree()
	if err != nil {
		return nil, err
	}
	componentRepo := r
	localPath := filepath.ToSlash(path)
	for {
		submodule, suffix, ok, err := resolveReviewSubmoduleForPath(componentRepo, baseTree, localPath)
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		componentRepo = submodule.repo
		baseTree = submodule.baseTree
		localPath = suffix
	}
	if source != FileSourceHead {
		return componentRepo.OpenFile(source, localPath)
	}
	if baseTree == nil {
		return nil, fmt.Errorf("path %q does not exist in the staged review base", path)
	}
	file, err := baseTree.File(localPath)
	if err != nil {
		return nil, err
	}
	return file.Blob.Reader()
}

func (r *Repository) StagedReviewFiles() ([]string, error) {
	workspace, err := r.stagedReviewWorkspace(false)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, component := range workspace.components {
		files := component.targetTree.Files()
		err := files.ForEach(func(file *object.File) error {
			paths = append(paths, prefixRepoPath(component.prefix, file.Name))
			return nil
		})
		files.Close()
		if err != nil {
			return nil, fmt.Errorf("enumerate staged component %q: %w", component.prefix, err)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

func (r *Repository) MaterializeStagedReview(destination string) error {
	workspace, err := r.stagedReviewWorkspace(false)
	if err != nil {
		return err
	}
	return workspace.materialize(destination)
}

func (w *stagedReviewWorkspace) snapshot(maxBytes, maxLines int) (ChangeSnapshot, error) {
	paths := map[string]bool{}
	statuses := map[string]PathChange{}
	stats := map[string]FileStat{}
	for _, component := range w.components {
		for _, path := range component.diff.Paths() {
			fullPath := prefixRepoPath(component.prefix, path)
			paths[fullPath] = true
		}
		for _, status := range component.diff.Status() {
			status.Path = prefixRepoPath(component.prefix, status.Path)
			statuses[status.Path] = status
		}
		for _, stat := range component.diff.Stats() {
			stat.Path = prefixRepoPath(component.prefix, stat.Path)
			stats[stat.Path] = stat
		}
	}

	orderedPaths := slices.Sorted(maps.Keys(paths))
	orderedStatus := make([]PathChange, 0, len(orderedPaths))
	orderedStats := make([]FileStat, 0, len(orderedPaths))
	for _, path := range orderedPaths {
		orderedStatus = append(orderedStatus, statuses[path])
		orderedStats = append(orderedStats, stats[path])
	}
	diff, truncated, err := w.diffForPaths(nil, maxBytes, maxLines)
	if err != nil {
		return ChangeSnapshot{}, err
	}
	return ChangeSnapshot{
		Paths: orderedPaths, Components: w.componentPaths(), Status: orderedStatus, Stats: orderedStats,
		Diff: diff, DiffTruncated: truncated, Fingerprint: w.fingerprint(),
	}, nil
}

func (w *stagedReviewWorkspace) componentPaths() []string {
	paths := make([]string, len(w.components))
	for index, component := range w.components {
		paths[index] = component.prefix
	}
	return paths
}

func (w *stagedReviewWorkspace) diffForPaths(paths []string, maxBytes, maxLines int) (string, bool, error) {
	var output strings.Builder
	for _, component := range w.components {
		selected, includeAll := componentPaths(component.prefix, paths)
		if paths != nil && !includeAll && len(selected) == 0 {
			continue
		}
		var text string
		if includeAll || paths == nil {
			text = component.repo.diffText(component.diff, nil)
		} else {
			filtered, _, err := component.repo.limitedTreeDiffForPaths(component.diff, selected, maxBytes, maxLines)
			if err != nil {
				return "", false, err
			}
			text = filtered
		}
		if text == "" {
			continue
		}
		if component.prefix != "" {
			if output.Len() > 0 {
				output.WriteByte('\n')
			}
			fmt.Fprintf(&output, "Repository %q (paths below are relative to %q):\n", component.prefix, component.prefix)
			if component.baseUnavailable {
				fmt.Fprintf(&output, "Base commit %s is unavailable locally; showing index-relative staged changes only.\n", component.expectedBase)
			}
		}
		output.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			output.WriteByte('\n')
		}
	}
	limited, truncated := textutil.Limit(output.String(), maxBytes, maxLines)
	return limited, truncated, nil
}

func (w *stagedReviewWorkspace) fingerprint() ChangeFingerprint {
	if len(w.components) == 0 {
		return ChangeFingerprint{}
	}
	result := w.components[0].fingerprint
	if len(w.components) == 1 {
		return result
	}
	hasher := sha256.New()
	for _, component := range w.components[1:] {
		writeWorkspaceHashField(hasher, component.prefix)
		writeWorkspaceHashField(hasher, component.expectedBase)
		writeWorkspaceHashField(hasher, component.fingerprint.BaseTree)
		writeWorkspaceHashField(hasher, component.fingerprint.TargetTree)
		writeWorkspaceHashField(hasher, fmt.Sprintf("%t", component.baseUnavailable))
	}
	result.NestedRepositories = fmt.Sprintf("%x", hasher.Sum(nil))
	return result
}

func (w *stagedReviewWorkspace) materialize(destination string) (returnErr error) {
	if !filepath.IsAbs(destination) {
		return fmt.Errorf("staged review destination must be absolute")
	}
	info, err := os.Lstat(destination)
	if err != nil {
		return fmt.Errorf("inspect staged review destination: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("staged review destination must be an owner-only directory")
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		return fmt.Errorf("inspect staged review destination contents: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("staged review destination must be empty")
	}

	root, err := os.OpenRoot(destination)
	if err != nil {
		return fmt.Errorf("open staged review destination: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, root.Close())
	}()
	for _, component := range w.components {
		if component.prefix != "" {
			if err := root.MkdirAll(filepath.FromSlash(component.prefix), 0o700); err != nil {
				return fmt.Errorf("create staged component %q: %w", component.prefix, err)
			}
		}
		if err := materializeTree(root, component.prefix, component.targetTree); err != nil {
			return fmt.Errorf("materialize staged component %q: %w", component.prefix, err)
		}
	}
	return nil
}

func materializeTree(root *os.Root, prefix string, tree *object.Tree) error {
	if tree == nil {
		return nil
	}
	files := tree.Files()
	defer files.Close()
	return files.ForEach(func(file *object.File) error {
		path := prefixRepoPath(prefix, file.Name)
		nativePath := filepath.FromSlash(path)
		if !filepath.IsLocal(nativePath) {
			return fmt.Errorf("unsafe index path %q", path)
		}
		if err := root.MkdirAll(filepath.Dir(nativePath), 0o700); err != nil {
			return err
		}
		switch file.Mode {
		case filemode.Regular, filemode.Deprecated, filemode.Executable:
			reader, err := file.Blob.Reader()
			if err != nil {
				return err
			}
			perm := os.FileMode(0o644)
			if file.Mode == filemode.Executable {
				perm = 0o755
			}
			output, openErr := root.OpenFile(nativePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
			if openErr != nil {
				return errors.Join(openErr, reader.Close())
			}
			_, copyErr := io.Copy(output, reader)
			return errors.Join(copyErr, output.Close(), reader.Close())
		case filemode.Symlink:
			target, err := file.Contents()
			if err != nil {
				return err
			}
			if filepath.IsAbs(target) || !filepath.IsLocal(filepath.Clean(filepath.Join(filepath.Dir(nativePath), target))) {
				return fmt.Errorf("unsafe symlink %q -> %q", path, target)
			}
			return root.Symlink(target, nativePath)
		default:
			return fmt.Errorf("unsupported index mode %s for %q", file.Mode, path)
		}
	})
}
