package gitctx

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"

	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/yusing/git-agent/internal/textutil"
)

type uncommittedWorkspace struct {
	components []uncommittedComponent
}

type uncommittedComponent struct {
	prefix          string
	repo            *Repository
	status          git.Status
	baseTree        *object.Tree
	diff            *treeDiff
	fingerprint     ChangeFingerprint
	expectedBase    string
	baseUnavailable bool
}

func (r *Repository) uncommittedWorkspace(includeDiff bool) (*uncommittedWorkspace, error) {
	baseTree, err := r.headTree()
	if err != nil {
		return nil, err
	}
	workspace := &uncommittedWorkspace{}
	visited := map[string]bool{}
	if err := workspace.collect(r, "", baseTree, "", false, includeDiff, visited); err != nil {
		return nil, err
	}
	return workspace, nil
}

func (w *uncommittedWorkspace) collect(repo *Repository, prefix string, baseTree *object.Tree, expectedBase string, baseUnavailable, includeDiff bool, visited map[string]bool) error {
	root, err := filepath.EvalSymlinks(repo.RootPath)
	if err != nil {
		return err
	}
	if visited[root] {
		return nil
	}
	visited[root] = true

	status, err := repo.status()
	if err != nil {
		return fmt.Errorf("inspect repository %q: %w", prefix, err)
	}
	targetTree, err := repo.worktreeTree(status)
	if err != nil {
		return fmt.Errorf("snapshot repository %q: %w", prefix, err)
	}
	dirtySubmodules, err := repo.dirtySubmoduleChanges(baseTree)
	if err != nil {
		return fmt.Errorf("inspect nested repositories below %q: %w", prefix, err)
	}
	var diff *treeDiff
	if includeDiff {
		diff, err = newTreeDiff(baseTree, targetTree)
		if err != nil {
			return fmt.Errorf("diff repository %q: %w", prefix, err)
		}
		mergeDirtySubmodules(diff, dirtySubmodules)
	}
	w.components = append(w.components, uncommittedComponent{
		prefix:          prefix,
		repo:            repo,
		status:          status,
		baseTree:        baseTree,
		diff:            diff,
		fingerprint:     changeFingerprint(baseTree, targetTree, dirtySubmodules),
		expectedBase:    expectedBase,
		baseUnavailable: baseUnavailable,
	})

	submodules, err := initializedSubmodules(repo, baseTree)
	if err != nil {
		return fmt.Errorf("discover nested repositories below %q: %w", prefix, err)
	}
	for _, submodule := range submodules {
		subPrefix := prefixRepoPath(prefix, submodule.path)
		if err := w.collect(submodule.repo, subPrefix, submodule.baseTree, submodule.expectedBase, submodule.baseUnavailable, includeDiff, visited); err != nil {
			return err
		}
	}
	return nil
}

type initializedSubmodule struct {
	path            string
	repo            *Repository
	baseTree        *object.Tree
	expectedBase    string
	baseUnavailable bool
}

func initializedSubmodules(repo *Repository, baseTree *object.Tree) ([]initializedSubmodule, error) {
	worktree, err := repo.Repo.Worktree()
	if err != nil {
		return nil, err
	}
	submodules, err := worktree.Submodules()
	if err != nil {
		return nil, err
	}
	baseHashes, err := collectSubmoduleEntries(baseTree, "")
	if err != nil {
		return nil, err
	}
	registrations, err := reviewSubmoduleRegistrations(repo, baseHashes)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(repo.RootPath)
	if err != nil {
		return nil, err
	}

	result := make([]initializedSubmodule, 0, len(submodules))
	for _, submodule := range submodules {
		path, ok := normalizedNestedRepositoryPath(submodule.Config().Path)
		if !ok || !registrations.allows(submodule.Config(), path) {
			continue
		}
		expectedBase := baseHashes[path]
		opened, ok, err := openInitializedSubmodule(root, path, expectedBase)
		if err != nil {
			return nil, fmt.Errorf("resolve base for submodule %q: %w", path, err)
		}
		if ok {
			result = append(result, opened)
		}
	}
	slices.SortFunc(result, func(a, b initializedSubmodule) int {
		return strings.Compare(a.path, b.path)
	})
	return result, nil
}

func openInitializedSubmodule(parentRoot, path, expectedBase string) (initializedSubmodule, bool, error) {
	subRoot, err := filepath.EvalSymlinks(filepath.Join(parentRoot, filepath.FromSlash(path)))
	if err != nil {
		return initializedSubmodule{}, false, nil
	}
	rel, err := filepath.Rel(parentRoot, subRoot)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return initializedSubmodule{}, false, nil
	}
	subRepo, err := Open(subRoot)
	if err != nil || subRepo.RootPath != subRoot {
		return initializedSubmodule{}, false, nil
	}
	subBaseTree, unavailable, err := submoduleBaseTree(subRepo, expectedBase)
	if err != nil {
		return initializedSubmodule{}, false, err
	}
	return initializedSubmodule{
		path: path, repo: subRepo, baseTree: subBaseTree,
		expectedBase: expectedBase, baseUnavailable: unavailable,
	}, true, nil
}

type submoduleRegistrations struct {
	byName   map[string]*gitconfig.Submodule
	gitlinks map[string]bool
}

func reviewSubmoduleRegistrations(repo *Repository, baseHashes map[string]string) (submoduleRegistrations, error) {
	config, err := repo.Repo.Config()
	if err != nil {
		return submoduleRegistrations{}, err
	}
	index, err := repo.Repo.Storer.Index()
	if err != nil {
		return submoduleRegistrations{}, err
	}
	gitlinks := make(map[string]bool, len(baseHashes)+len(index.Entries))
	for path := range baseHashes {
		gitlinks[path] = true
	}
	for _, entry := range index.Entries {
		if entry.Mode == filemode.Submodule {
			gitlinks[filepath.ToSlash(entry.Name)] = true
		}
	}
	return submoduleRegistrations{byName: config.Submodules, gitlinks: gitlinks}, nil
}

func (r submoduleRegistrations) allows(module *gitconfig.Submodule, path string) bool {
	if module == nil || module.Name == "" || !r.gitlinks[path] {
		return false
	}
	registered, ok := r.byName[module.Name]
	if !ok || registered == nil || registered.Name != module.Name {
		return false
	}
	if registered.Path == "" {
		return true
	}
	registeredPath, ok := normalizedNestedRepositoryPath(registered.Path)
	return ok && registeredPath == path
}

func normalizedNestedRepositoryPath(raw string) (string, bool) {
	path := filepath.ToSlash(raw)
	clean := pathpkg.Clean(path)
	nativePath := filepath.FromSlash(clean)
	return clean, path != "" && clean == path && clean != "." && filepath.IsLocal(nativePath) &&
		!slices.ContainsFunc(strings.Split(clean, "/"), func(part string) bool {
			return strings.EqualFold(part, ".git")
		})
}

func submoduleBaseTree(repo *Repository, expectedBase string) (*object.Tree, bool, error) {
	if expectedBase == "" {
		return nil, false, nil
	}
	commit, err := object.GetCommit(repo.Repo.Storer, plumbing.NewHash(expectedBase))
	if err == nil {
		tree, treeErr := commit.Tree()
		return tree, false, treeErr
	}
	if !errors.Is(err, plumbing.ErrObjectNotFound) {
		return nil, false, err
	}
	baseTree, headErr := repo.headTree()
	return baseTree, true, headErr
}

func (w *uncommittedWorkspace) snapshot(maxBytes, maxLines int) (ChangeSnapshot, error) {
	paths := map[string]bool{}
	statuses := map[string]PathChange{}
	stats := map[string]FileStat{}
	for _, component := range w.components {
		for _, path := range component.diff.Paths() {
			fullPath := prefixRepoPath(component.prefix, path)
			paths[fullPath] = true
			file := component.status[path]
			if file != nil {
				statuses[fullPath] = PathChange{Path: fullPath, Staging: string(file.Staging), Worktree: string(file.Worktree)}
			} else {
				statuses[fullPath] = PathChange{Path: fullPath}
			}
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
	diff, truncated, err := w.diff(nil, maxBytes, maxLines)
	if err != nil {
		return ChangeSnapshot{}, err
	}
	return ChangeSnapshot{
		Paths: orderedPaths, Components: w.componentPaths(), Status: orderedStatus, Stats: orderedStats,
		Diff: diff, DiffTruncated: truncated, Fingerprint: w.fingerprint(),
	}, nil
}

func (w *uncommittedWorkspace) componentPaths() []string {
	paths := make([]string, len(w.components))
	for index, component := range w.components {
		paths[index] = component.prefix
	}
	return paths
}

func (w *uncommittedWorkspace) diff(paths []string, maxBytes, maxLines int) (string, bool, error) {
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
			filtered, _, filterErr := component.repo.limitedTreeDiffForPaths(component.diff, selected, maxBytes, maxLines)
			if filterErr != nil {
				return "", false, filterErr
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
				fmt.Fprintf(&output, "Base commit %s is unavailable locally; showing checkout-relative dirty changes only.\n", component.expectedBase)
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

func componentPaths(prefix string, paths []string) ([]string, bool) {
	if paths == nil {
		return nil, true
	}
	selected := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.ToSlash(path)
		if prefix == "" {
			selected = append(selected, path)
			continue
		}
		if path == prefix {
			return nil, true
		}
		if suffix, ok := strings.CutPrefix(path, prefix+"/"); ok {
			selected = append(selected, suffix)
		}
	}
	return selected, false
}

func (w *uncommittedWorkspace) fingerprint() ChangeFingerprint {
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
		writeWorkspaceHashField(hasher, component.fingerprint.DirtySubmodules)
		writeWorkspaceHashField(hasher, fmt.Sprintf("%t", component.baseUnavailable))
	}
	result.NestedRepositories = fmt.Sprintf("%x", hasher.Sum(nil))
	return result
}

// OpenUncommittedReviewFile opens a root-relative file from the repository
// component that owns it in the recursive uncommitted review snapshot.
func (r *Repository) OpenUncommittedReviewFile(source FileSource, path string) (io.ReadCloser, error) {
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
		return nil, fmt.Errorf("path %q does not exist in the review base", path)
	}
	file, err := baseTree.File(localPath)
	if err != nil {
		return nil, err
	}
	return file.Blob.Reader()
}

func resolveReviewSubmoduleForPath(repo *Repository, baseTree *object.Tree, requested string) (initializedSubmodule, string, bool, error) {
	worktree, err := repo.Repo.Worktree()
	if err != nil {
		return initializedSubmodule{}, "", false, err
	}
	submodules, err := worktree.Submodules()
	if err != nil {
		return initializedSubmodule{}, "", false, err
	}
	baseHashes, err := collectSubmoduleEntries(baseTree, "")
	if err != nil {
		return initializedSubmodule{}, "", false, err
	}
	registrations, err := reviewSubmoduleRegistrations(repo, baseHashes)
	if err != nil {
		return initializedSubmodule{}, "", false, err
	}
	root, err := filepath.EvalSymlinks(repo.RootPath)
	if err != nil {
		return initializedSubmodule{}, "", false, err
	}

	var selectedPath string
	var selectedSuffix string
	for _, submodule := range submodules {
		path, ok := normalizedNestedRepositoryPath(submodule.Config().Path)
		if !ok || !registrations.allows(submodule.Config(), path) || len(path) <= len(selectedPath) {
			continue
		}
		if suffix, ok := strings.CutPrefix(requested, path+"/"); ok {
			selectedPath = path
			selectedSuffix = suffix
		}
	}
	if selectedPath == "" {
		return initializedSubmodule{}, "", false, nil
	}
	opened, ok, err := openInitializedSubmodule(root, selectedPath, baseHashes[selectedPath])
	if err != nil || !ok {
		return initializedSubmodule{}, "", false, err
	}
	return opened, selectedSuffix, true, nil
}

func writeWorkspaceHashField(writer io.Writer, value string) {
	_, _ = fmt.Fprintf(writer, "%d:", len(value))
	_, _ = io.WriteString(writer, value)
}

func prefixRepoPath(prefix, path string) string {
	if prefix == "" {
		return filepath.ToSlash(path)
	}
	if path == "" {
		return prefix
	}
	return prefix + "/" + filepath.ToSlash(path)
}

func (r *Repository) ReviewComponentPaths() ([]string, error) {
	baseTree, err := r.headTree()
	if err != nil {
		return nil, err
	}
	var paths []string
	visited := map[string]bool{}
	var collect func(*Repository, string, *object.Tree) error
	collect = func(repo *Repository, prefix string, base *object.Tree) error {
		root, err := filepath.EvalSymlinks(repo.RootPath)
		if err != nil {
			return err
		}
		if visited[root] {
			return nil
		}
		visited[root] = true
		paths = append(paths, prefix)
		submodules, err := initializedSubmodules(repo, base)
		if err != nil {
			return err
		}
		for _, submodule := range submodules {
			if err := collect(submodule.repo, prefixRepoPath(prefix, submodule.path), submodule.baseTree); err != nil {
				return err
			}
		}
		return nil
	}
	if err := collect(r, "", baseTree); err != nil {
		return nil, err
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}
