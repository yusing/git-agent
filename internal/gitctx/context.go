package gitctx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/utils/merkletrie"
	"github.com/go-git/go-git/v6/utils/merkletrie/noder"
	"github.com/yusing/git-agent/internal/textutil"
)

type Repository struct {
	RootPath   string
	WorkPath   string
	Branch     string
	HeadSHA    string
	IsDetached bool
	Repo       *git.Repository
}

type CommitInfo struct {
	SHA     string `json:"sha"`
	Summary string `json:"summary"`
	Author  string `json:"author,omitempty"`
	Date    string `json:"date,omitempty"`
}

type PathChange struct {
	Path     string `json:"path"`
	Staging  string `json:"staging"`
	Worktree string `json:"worktree,omitempty"`
}

type FileStat struct {
	Path     string `json:"path"`
	Adds     int    `json:"adds"`
	Deletes  int    `json:"deletes"`
	IsBinary bool   `json:"is_binary,omitempty"`
}

type SubmoduleChange struct {
	Path string `json:"path"`
	Old  string `json:"old,omitempty"`
	New  string `json:"new,omitempty"`
}

const PullRequestBaseRef = "origin/HEAD"

func Open(start string) (*Repository, error) {
	root, err := discoverRoot(start)
	if err != nil {
		return nil, err
	}
	repo, err := git.PlainOpenWithOptions(root, &git.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, err
	}
	head, err := repo.Head()
	if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, err
	}

	result := &Repository{RootPath: root, WorkPath: root, Repo: repo, IsDetached: true}
	if head != nil {
		result.HeadSHA = head.Hash().String()
		result.IsDetached = !head.Name().IsBranch()
		if head.Name().IsBranch() {
			result.Branch = head.Name().Short()
		}
	}
	return result, nil
}

func (r *Repository) Summary() map[string]any {
	remotes := map[string][]string{}
	if cfg, err := r.Repo.Config(); err == nil {
		for name, remote := range cfg.Remotes {
			remotes[name] = remote.URLs
		}
	}
	return map[string]any{
		"root_path":   r.RootPath,
		"work_path":   r.WorkPath,
		"branch":      r.Branch,
		"head_sha":    r.HeadSHA,
		"is_detached": r.IsDetached,
		"remotes":     remotes,
	}
}

func (r *Repository) StagedPaths() ([]string, error) {
	status, err := r.status()
	if err != nil {
		return nil, err
	}

	var paths []string
	for path, file := range status {
		if file.Staging != git.Unmodified && file.Staging != git.Untracked {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	return paths, nil
}

func (r *Repository) StagedStatus() ([]PathChange, error) {
	status, err := r.status()
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(status))
	for path := range status {
		paths = append(paths, path)
	}
	slices.Sort(paths)

	changes := make([]PathChange, 0, len(paths))
	for _, path := range paths {
		file := status[path]
		if file.Staging == git.Unmodified || file.Staging == git.Untracked {
			continue
		}
		changes = append(changes, PathChange{
			Path:     path,
			Staging:  string(file.Staging),
			Worktree: string(file.Worktree),
		})
	}
	return changes, nil
}

func (r *Repository) StagedDiff(maxBytes, maxLines int) (string, bool, error) {
	diff, err := r.diffIndexAgainstHead()
	if err != nil {
		return "", false, err
	}
	limited, truncated := textutil.Limit(diff, maxBytes, maxLines)
	return limited, truncated, nil
}

func (r *Repository) StagedStat() ([]FileStat, error) {
	patch, err := r.patchIndexAgainstHead()
	if err != nil {
		return nil, err
	}
	return fileStatsFromPatch(patch), nil
}

func (r *Repository) StagedSubmoduleChanges() ([]SubmoduleChange, error) {
	headTree, err := r.headTree()
	if err != nil {
		return nil, err
	}
	indexTree, err := r.indexTree()
	if err != nil {
		return nil, err
	}
	return submoduleChangesBetweenTrees(headTree, indexTree)
}

func (r *Repository) RecentCommits(limit int) ([]CommitInfo, error) {
	return r.LogFrom("", "", limit)
}

func (r *Repository) HeadShow(maxBytes, maxLines int) (string, bool, error) {
	head, err := r.headCommit()
	if err != nil {
		return "", false, err
	}
	text := formatCommit(head)
	if head.NumParents() > 0 {
		parent, err := head.Parent(0)
		if err != nil {
			return "", false, err
		}
		patch, err := parent.Patch(head)
		if err != nil {
			return "", false, err
		}
		text += "\n" + patch.String()
	}
	limited, truncated := textutil.Limit(text, maxBytes, maxLines)
	return limited, truncated, nil
}

func (r *Repository) DiffAgainstParent(maxBytes, maxLines int) (string, bool, error) {
	patch, err := r.patchHeadAgainstParent()
	if err != nil {
		return "", false, err
	}
	limited, truncated := textutil.Limit(patch.String(), maxBytes, maxLines)
	return limited, truncated, nil
}

func (r *Repository) DiffAgainstParentPaths() ([]string, error) {
	patch, err := r.patchHeadAgainstParent()
	if err != nil {
		return nil, err
	}
	return filePathsFromPatch(patch), nil
}

func (r *Repository) DiffAgainstParentStat() ([]FileStat, error) {
	patch, err := r.patchHeadAgainstParent()
	if err != nil {
		return nil, err
	}
	return fileStatsFromPatch(patch), nil
}

func (r *Repository) AmendDelta(maxBytes, maxLines int) (string, bool, error) {
	diff, err := r.diffIndexAgainstHead()
	if err != nil {
		return "", false, err
	}
	limited, truncated := textutil.Limit(diff, maxBytes, maxLines)
	return limited, truncated, nil
}

func (r *Repository) FinalAmendedDiff(maxBytes, maxLines int) (string, bool, error) {
	head, err := r.headCommit()
	if err != nil {
		return "", false, err
	}
	parentTree, finalTree, err := r.finalAmendedTrees(head)
	if err != nil {
		return "", false, err
	}
	patch, err := patchBetweenTrees(parentTree, finalTree)
	if err != nil {
		return "", false, err
	}
	limited, truncated := textutil.Limit(patch.String(), maxBytes, maxLines)
	return limited, truncated, nil
}

func (r *Repository) ShowFileAtRev(rev, path string, maxBytes, maxLines int) (string, bool, error) {
	commit, err := r.ResolveCommit(rev)
	if err != nil {
		return "", false, err
	}
	file, err := commit.File(path)
	if err != nil {
		return "", false, err
	}
	reader, err := file.Reader()
	if err != nil {
		return "", false, err
	}
	defer reader.Close()
	var b bytes.Buffer
	if _, err := io.Copy(&b, reader); err != nil {
		return "", false, err
	}
	limited, truncated := textutil.Limit(b.String(), maxBytes, maxLines)
	return limited, truncated, nil
}

func (r *Repository) ResolveRef(ref string) (string, error) {
	hash, err := r.resolveRevision(ref)
	if err != nil {
		return "", err
	}
	return hash.String(), nil
}

func (r *Repository) ResolveCommit(ref string) (*object.Commit, error) {
	hash, err := r.resolveRevision(ref)
	if err != nil {
		return nil, err
	}
	return r.Repo.CommitObject(*hash)
}

func (r *Repository) PullRequestBase() (CommitInfo, error) {
	commit, err := r.ResolveCommit(PullRequestBaseRef)
	if err != nil {
		return CommitInfo{}, err
	}
	return commitInfo(commit), nil
}

func (r *Repository) PullRequestPaths() ([]string, error) {
	patch, err := r.patchAgainstRef(PullRequestBaseRef)
	if err != nil {
		return nil, err
	}
	return filePathsFromPatch(patch), nil
}

func (r *Repository) PullRequestStat() ([]FileStat, error) {
	patch, err := r.patchAgainstRef(PullRequestBaseRef)
	if err != nil {
		return nil, err
	}
	return fileStatsFromPatch(patch), nil
}

func (r *Repository) PullRequestDiff(maxBytes, maxLines int) (string, bool, error) {
	patch, err := r.patchAgainstRef(PullRequestBaseRef)
	if err != nil {
		return "", false, err
	}
	limited, truncated := textutil.Limit(patch.String(), maxBytes, maxLines)
	return limited, truncated, nil
}

func (r *Repository) PullRequestCommits(limit int) ([]CommitInfo, error) {
	return r.LogFrom(PullRequestBaseRef, "HEAD", limit)
}

func (r *Repository) LogFrom(base, release string, limit int) ([]CommitInfo, error) {
	from, err := r.resolveLogStart(release)
	if err != nil {
		return nil, err
	}

	excluded, err := r.reachableCommitSet(base)
	if err != nil {
		return nil, err
	}

	iter := object.NewCommitPreorderIter(from, excluded, nil)
	defer iter.Close()

	var commits []CommitInfo
	err = iter.ForEach(func(commit *object.Commit) error {
		commits = append(commits, commitInfo(commit))
		if limit > 0 && len(commits) >= limit {
			return stopeach{}
		}
		return nil
	})
	if errors.As(err, new(stopeach)) {
		return commits, nil
	}
	return commits, err
}

func (r *Repository) resolveLogStart(ref string) (*object.Commit, error) {
	if ref == "" {
		head, err := r.Repo.Head()
		if err != nil {
			return nil, err
		}
		return r.Repo.CommitObject(head.Hash())
	}
	return r.ResolveCommit(ref)
}

func (r *Repository) reachableCommitSet(ref string) (map[plumbing.Hash]bool, error) {
	if ref == "" {
		return nil, nil
	}

	start, err := r.ResolveCommit(ref)
	if err != nil {
		return nil, err
	}

	reachable := map[plumbing.Hash]bool{}
	iter := object.NewCommitPreorderIter(start, nil, nil)
	defer iter.Close()

	err = iter.ForEach(func(commit *object.Commit) error {
		reachable[commit.Hash] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reachable, nil
}

func (r *Repository) SubmoduleGitlinkRange(base, release string) ([]SubmoduleChange, error) {
	baseCommit, err := r.ResolveCommit(base)
	if err != nil {
		return nil, err
	}
	releaseCommit, err := r.ResolveCommit(release)
	if err != nil {
		return nil, err
	}
	baseTree, err := baseCommit.Tree()
	if err != nil {
		return nil, err
	}
	releaseTree, err := releaseCommit.Tree()
	if err != nil {
		return nil, err
	}
	return submoduleChangesBetweenTrees(baseTree, releaseTree)
}

func submoduleChangesBetweenTrees(baseTree, releaseTree *object.Tree) ([]SubmoduleChange, error) {
	baseSubs, err := collectSubmoduleEntries(baseTree, "")
	if err != nil {
		return nil, err
	}
	releaseSubs, err := collectSubmoduleEntries(releaseTree, "")
	if err != nil {
		return nil, err
	}

	paths := make([]string, 0, len(baseSubs)+len(releaseSubs))
	seen := map[string]bool{}
	for path := range baseSubs {
		paths = append(paths, path)
		seen[path] = true
	}
	for path := range releaseSubs {
		if seen[path] {
			continue
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)

	changes := make([]SubmoduleChange, 0, len(paths))
	for _, path := range paths {
		baseHash, baseOK := baseSubs[path]
		releaseHash, releaseOK := releaseSubs[path]
		if baseOK && releaseOK && baseHash == releaseHash {
			continue
		}
		change := SubmoduleChange{Path: path}
		if baseOK {
			change.Old = baseHash
		}
		if releaseOK {
			change.New = releaseHash
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func (r *Repository) RepoKind() string {
	if _, err := os.Stat(filepath.Join(r.RootPath, ".gitmodules")); err == nil {
		return "parent-with-submodules"
	}
	return "repository"
}

func (r *Repository) status() (git.Status, error) {
	worktree, err := r.Repo.Worktree()
	if err != nil {
		return nil, err
	}
	return worktree.Status()
}

func (r *Repository) headCommit() (*object.Commit, error) {
	head, err := r.Repo.Head()
	if err != nil {
		return nil, err
	}
	return r.Repo.CommitObject(head.Hash())
}

func (r *Repository) resolveRevision(ref string) (*plumbing.Hash, error) {
	hash, err := r.Repo.ResolveRevision(plumbing.Revision(ref))
	if err == nil {
		return hash, nil
	}
	if ref != PullRequestBaseRef {
		return nil, err
	}
	remoteHead, refErr := r.Repo.Reference(plumbing.NewRemoteHEADReferenceName("origin"), true)
	if refErr != nil {
		return nil, err
	}
	resolved := remoteHead.Hash()
	return &resolved, nil
}

func (r *Repository) diffIndexAgainstHead() (string, error) {
	patch, err := r.patchIndexAgainstHead()
	if err != nil {
		return "", err
	}
	return patch.String(), nil
}

func (r *Repository) indexFileContent(path string) (string, error) {
	idx, err := r.Repo.Storer.Index()
	if err != nil {
		return "", err
	}
	entry, err := idx.Entry(path)
	if err != nil {
		return "", err
	}
	obj, err := r.Repo.BlobObject(entry.Hash)
	if err != nil {
		return "", err
	}
	reader, err := obj.Reader()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	var b bytes.Buffer
	if _, err := io.Copy(&b, reader); err != nil {
		return "", err
	}
	return b.String(), nil
}

func fileContentAtCommit(commit *object.Commit, path string) string {
	file, err := commit.File(path)
	if err != nil {
		return ""
	}
	reader, err := file.Reader()
	if err != nil {
		return ""
	}
	defer reader.Close()
	var b bytes.Buffer
	if _, err := io.Copy(&b, reader); err != nil {
		return ""
	}
	return b.String()
}

func discoverRoot(start string) (string, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err == nil && !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	for {
		if _, err := os.Stat(filepath.Join(abs, ".git")); err == nil {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("not inside a git repository: %s", start)
		}
		abs = parent
	}
}

func (r *Repository) patchIndexAgainstHead() (*object.Patch, error) {
	headTree, err := r.headTree()
	if err != nil {
		return nil, err
	}
	indexTree, err := r.indexTree()
	if err != nil {
		return nil, err
	}
	return patchBetweenTrees(headTree, indexTree)
}

func (r *Repository) patchHeadAgainstParent() (*object.Patch, error) {
	head, err := r.headCommit()
	if err != nil {
		return nil, err
	}
	if head.NumParents() == 0 {
		tree, err := head.Tree()
		if err != nil {
			return nil, err
		}
		return (&object.Tree{}).Patch(tree)
	}
	parent, err := head.Parent(0)
	if err != nil {
		return nil, err
	}
	return parent.Patch(head)
}

func (r *Repository) patchAgainstRef(baseRef string) (*object.Patch, error) {
	base, err := r.ResolveCommit(baseRef)
	if err != nil {
		return nil, err
	}
	head, err := r.headCommit()
	if err != nil {
		return nil, err
	}
	baseTree, err := base.Tree()
	if err != nil {
		return nil, err
	}
	headTree, err := head.Tree()
	if err != nil {
		return nil, err
	}
	return patchBetweenTrees(baseTree, headTree)
}

func (r *Repository) finalAmendedTrees(head *object.Commit) (*object.Tree, *object.Tree, error) {
	finalTree, err := r.indexTree()
	if err != nil {
		return nil, nil, err
	}
	if head.NumParents() == 0 {
		return nil, finalTree, nil
	}
	parent, err := head.Parent(0)
	if err != nil {
		return nil, nil, err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, nil, err
	}
	return parentTree, finalTree, nil
}

func (r *Repository) headTree() (*object.Tree, error) {
	head, err := r.Repo.Head()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil, nil
		}
		return nil, err
	}
	commit, err := r.Repo.CommitObject(head.Hash())
	if err != nil {
		return nil, err
	}
	return commit.Tree()
}

func (r *Repository) indexTree() (*object.Tree, error) {
	idx, err := r.Repo.Storer.Index()
	if err != nil {
		return nil, err
	}
	if idx == nil {
		return nil, nil
	}
	st := newOverlayObjectStorer(r.Repo.Storer)
	hash, err := buildIndexTree(st, idx)
	if err != nil {
		return nil, err
	}
	if hash.IsZero() {
		return nil, nil
	}
	return object.GetTree(st, hash)
}

func patchBetweenTrees(from, to *object.Tree) (*object.Patch, error) {
	changes, err := diffTreesWithRenames(from, to)
	if err != nil {
		return nil, err
	}
	return changes.PatchContext(context.Background())
}

func diffTreesWithRenames(from, to *object.Tree) (object.Changes, error) {
	fromNode := object.NewTreeRootNode(from)
	toNode := object.NewTreeRootNode(to)
	hashEqual := func(left, right noder.Hasher) bool {
		return bytes.Equal(left.Hash(), right.Hash())
	}
	mtChanges, err := merkletrie.DiffTreeContext(context.Background(), fromNode, toNode, hashEqual)
	if err != nil {
		return nil, err
	}
	changes, err := treeChangesFromMerkle(mtChanges, from, to)
	if err != nil {
		return nil, err
	}
	return object.DetectRenames(changes, object.DefaultDiffTreeOptions)
}

func treeChangesFromMerkle(src merkletrie.Changes, fromRoot, toRoot *object.Tree) (object.Changes, error) {
	changes := make(object.Changes, 0, len(src))
	for i, change := range src {
		fromEntry, err := treeChangeEntry(change.From, fromRoot)
		if err != nil {
			return nil, fmt.Errorf("from entry %d: %w", i, err)
		}
		toEntry, err := treeChangeEntry(change.To, toRoot)
		if err != nil {
			return nil, fmt.Errorf("to entry %d: %w", i, err)
		}
		changes = append(changes, &object.Change{From: fromEntry, To: toEntry})
	}
	return changes, nil
}

func treeChangeEntry(path noder.Path, root *object.Tree) (object.ChangeEntry, error) {
	if path == nil {
		return object.ChangeEntry{}, nil
	}
	pathText := path.String()
	if pathText == "" {
		return object.ChangeEntry{}, nil
	}
	tree, entry, err := lookupTreeEntry(root, pathText)
	if err != nil {
		return object.ChangeEntry{}, err
	}
	return object.ChangeEntry{
		Name: pathText,
		Tree: tree,
		TreeEntry: object.TreeEntry{
			Name: entry.Name,
			Mode: entry.Mode,
			Hash: entry.Hash,
		},
	}, nil
}

func lookupTreeEntry(root *object.Tree, path string) (*object.Tree, *object.TreeEntry, error) {
	if root == nil {
		return nil, nil, fmt.Errorf("tree entry %q not found", path)
	}
	parts := strings.Split(path, "/")
	tree := root
	for _, part := range parts[:len(parts)-1] {
		next, err := tree.Tree(part)
		if err != nil {
			return nil, nil, err
		}
		tree = next
	}
	entry, err := tree.FindEntry(parts[len(parts)-1])
	if err != nil {
		return nil, nil, err
	}
	return tree, entry, nil
}

func collectSubmoduleEntries(tree *object.Tree, prefix string) (map[string]string, error) {
	if tree == nil {
		return nil, nil
	}
	submodules := map[string]string{}
	for _, entry := range tree.Entries {
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		switch entry.Mode {
		case filemode.Submodule:
			submodules[path] = entry.Hash.String()
		case filemode.Dir:
			child, err := tree.Tree(entry.Name)
			if err != nil {
				return nil, err
			}
			childEntries, err := collectSubmoduleEntries(child, path)
			if err != nil {
				return nil, err
			}
			maps.Copy(submodules, childEntries)
		}
	}
	return submodules, nil
}

func buildIndexTree(st storer.EncodedObjectStorer, idx *index.Index) (plumbing.Hash, error) {
	const root = ""
	trees := map[string]*object.Tree{
		root: {Hash: plumbing.ZeroHash},
	}
	for _, entry := range idx.Entries {
		if entry.Hash.IsZero() {
			continue
		}
		buildIndexTreeEntry(trees, entry)
	}
	return persistTreeRecursive(st, trees, root)
}

func buildIndexTreeEntry(trees map[string]*object.Tree, entry *index.Entry) {
	parts := strings.Split(entry.Name, "/")
	var fullPath string
	for i, part := range parts {
		parentPath := fullPath
		fullPath = filepath.ToSlash(filepath.Join(fullPath, part))
		parent := trees[parentPath]
		if i == len(parts)-1 {
			parent.Entries = append(parent.Entries, object.TreeEntry{
				Name: part,
				Mode: entry.Mode,
				Hash: entry.Hash,
			})
			continue
		}
		if _, ok := trees[fullPath]; ok {
			continue
		}
		trees[fullPath] = &object.Tree{Hash: plumbing.ZeroHash}
		parent.Entries = append(parent.Entries, object.TreeEntry{
			Name: part,
			Mode: filemode.Dir,
		})
	}
}

func persistTreeRecursive(st storer.EncodedObjectStorer, trees map[string]*object.Tree, path string) (plumbing.Hash, error) {
	tree, ok := trees[path]
	if !ok {
		return plumbing.ZeroHash, nil
	}
	sort.Slice(tree.Entries, func(i, j int) bool {
		return treeEntrySortName(tree.Entries[i]) < treeEntrySortName(tree.Entries[j])
	})
	for i, entry := range tree.Entries {
		if entry.Mode != filemode.Dir {
			continue
		}
		childPath := entry.Name
		if path != "" {
			childPath = path + "/" + entry.Name
		}
		hash, err := persistTreeRecursive(st, trees, childPath)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		tree.Entries[i].Hash = hash
	}
	if len(tree.Entries) == 0 && path == "" {
		return plumbing.ZeroHash, nil
	}
	obj := st.NewEncodedObject()
	if err := tree.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	hash, err := st.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	tree.Hash = hash
	return hash, nil
}

type overlayObjectStorer struct {
	base    storer.EncodedObjectStorer
	objects map[plumbing.Hash]plumbing.EncodedObject
}

func newOverlayObjectStorer(base storer.EncodedObjectStorer) *overlayObjectStorer {
	return &overlayObjectStorer{
		base:    base,
		objects: map[plumbing.Hash]plumbing.EncodedObject{},
	}
}

func (s *overlayObjectStorer) RawObjectWriter(plumbing.ObjectType, int64) (io.WriteCloser, error) {
	return nil, errors.New("raw object writer is not supported")
}

func (s *overlayObjectStorer) NewEncodedObject() plumbing.EncodedObject {
	return s.base.NewEncodedObject()
}

func (s *overlayObjectStorer) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	hash := obj.Hash()
	s.objects[hash] = obj
	return hash, nil
}

func (s *overlayObjectStorer) EncodedObject(typ plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	if obj, ok := s.objects[hash]; ok && (typ == plumbing.AnyObject || obj.Type() == typ) {
		return obj, nil
	}
	return s.base.EncodedObject(typ, hash)
}

func (s *overlayObjectStorer) IterEncodedObjects(typ plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	return s.base.IterEncodedObjects(typ)
}

func (s *overlayObjectStorer) HasEncodedObject(hash plumbing.Hash) error {
	if _, ok := s.objects[hash]; ok {
		return nil
	}
	return s.base.HasEncodedObject(hash)
}

func (s *overlayObjectStorer) EncodedObjectSize(hash plumbing.Hash) (int64, error) {
	if obj, ok := s.objects[hash]; ok {
		return obj.Size(), nil
	}
	return s.base.EncodedObjectSize(hash)
}

func (s *overlayObjectStorer) AddAlternate(remote string) error {
	return s.base.AddAlternate(remote)
}

func treeEntrySortName(entry object.TreeEntry) string {
	if entry.Mode == filemode.Dir {
		return entry.Name + "/"
	}
	return entry.Name
}

func fileStatsFromPatch(patch *object.Patch) []FileStat {
	if patch == nil {
		return nil
	}
	stats := patch.Stats()
	out := make([]FileStat, 0, len(stats))
	for _, stat := range stats {
		out = append(out, FileStat{
			Path:    stat.Name,
			Adds:    stat.Addition,
			Deletes: stat.Deletion,
		})
	}
	return out
}

func filePathsFromPatch(patch *object.Patch) []string {
	if patch == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, filePatch := range patch.FilePatches() {
		from, to := filePatch.Files()
		if from != nil {
			seen[from.Path()] = true
		}
		if to != nil {
			seen[to.Path()] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

func commitInfo(commit *object.Commit) CommitInfo {
	return CommitInfo{
		SHA:     commit.Hash.String(),
		Summary: strings.Split(strings.TrimSpace(commit.Message), "\n")[0],
		Author:  commit.Author.Name,
		Date:    commit.Author.When.Format(time.RFC3339),
	}
}

func formatCommit(commit *object.Commit) string {
	return fmt.Sprintf("commit %s\nAuthor: %s <%s>\nDate: %s\n\n%s\n",
		commit.Hash,
		commit.Author.Name,
		commit.Author.Email,
		commit.Author.When.Format(time.RFC3339),
		strings.TrimSpace(commit.Message),
	)
}

type stopeach struct{}

func (stopeach) Error() string { return "stop" }
