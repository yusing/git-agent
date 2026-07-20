package gitctx

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	fdiff "github.com/go-git/go-git/v6/plumbing/format/diff"
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

type CommitMessageInfo struct {
	SHA           string             `json:"sha"`
	Summary       string             `json:"summary"`
	Message       string             `json:"message,omitempty"`
	Author        string             `json:"author,omitempty"`
	Date          string             `json:"date,omitempty"`
	Files         []CommitFileChange `json:"files,omitempty"`
	Diffstat      CommitDiffstat     `json:"diffstat,omitzero"`
	PatchExcerpt  string             `json:"patch_excerpt,omitempty"`
	EvidenceError string             `json:"evidence_error,omitempty"`
}

type CommitFileChange struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions,omitempty"`
	Deletions int    `json:"deletions,omitempty"`
	Binary    bool   `json:"binary,omitempty"`
	Submodule bool   `json:"submodule,omitempty"`
}

type CommitDiffstat struct {
	FilesChanged int `json:"files_changed,omitempty"`
	Additions    int `json:"additions,omitempty"`
	Deletions    int `json:"deletions,omitempty"`
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

type ChangeSnapshot struct {
	Paths         []string
	Status        []PathChange
	Stats         []FileStat
	Diff          string
	DiffTruncated bool
	Fingerprint   ChangeFingerprint
}

type ChangeFingerprint struct {
	BaseTree           string `json:"base_tree"`
	TargetTree         string `json:"target_tree"`
	DirtySubmodules    string `json:"dirty_submodules,omitempty"`
	NestedRepositories string `json:"nested_repositories,omitempty"`
}

var ErrChangeSnapshotStale = errors.New("authoritative repository state changed since launch; rerun command")

type CommitFile struct {
	Path string
	Blob string
	Text string
	Size int64
}

type CommitFileSkip struct {
	Path   string
	Blob   string
	Size   int64
	Reason string
}

type FileSource string

const (
	FileSourceWorktree FileSource = "worktree"
	FileSourceIndex    FileSource = "index"
	FileSourceHead     FileSource = "head"
)

type SubmoduleChange struct {
	Path       string `json:"path"`
	Old        string `json:"old,omitempty"`
	New        string `json:"new,omitempty"`
	Dirty      bool   `json:"dirty,omitempty"`
	dirtyState string
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
	return repositoryFromHead(root, root, repo)
}

func OpenGitDir(path string) (*Repository, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	repo, err := git.PlainOpen(root)
	if err != nil {
		return nil, err
	}
	return repositoryFromHead(root, "", repo)
}

func repositoryFromHead(root, workPath string, repo *git.Repository) (*Repository, error) {
	head, err := repo.Head()
	if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, err
	}

	result := &Repository{RootPath: root, WorkPath: workPath, Repo: repo, IsDetached: true}
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
	diff, err := r.stagedTreeDiff()
	if err != nil {
		return nil, err
	}
	return diff.Paths(), nil
}

func (r *Repository) StagedStatus() ([]PathChange, error) {
	diff, err := r.stagedTreeDiff()
	if err != nil {
		return nil, err
	}
	return diff.Status(), nil
}

func (r *Repository) StagedDiff(maxBytes, maxLines int) (string, bool, error) {
	diff, err := r.stagedTreeDiff()
	if err != nil {
		return "", false, err
	}
	limited, truncated := textutil.Limit(r.diffText(diff, nil), maxBytes, maxLines)
	return limited, truncated, nil
}

func (r *Repository) StagedDiffForPaths(paths []string, maxBytes, maxLines int) (string, bool, error) {
	if len(paths) == 0 {
		return "", false, nil
	}
	diff, err := r.stagedTreeDiff()
	if err != nil {
		return "", false, err
	}
	return r.limitedTreeDiffForPaths(diff, paths, maxBytes, maxLines)
}

func (r *Repository) UncommittedDiff(maxBytes, maxLines int) (string, bool, error) {
	workspace, err := r.uncommittedWorkspace(true)
	if err != nil {
		return "", false, err
	}
	return workspace.diff(nil, maxBytes, maxLines)
}

func (r *Repository) UncommittedDiffForPaths(paths []string, maxBytes, maxLines int) (string, bool, error) {
	if len(paths) == 0 {
		return "", false, nil
	}
	workspace, err := r.uncommittedWorkspace(true)
	if err != nil {
		return "", false, err
	}
	return workspace.diff(paths, maxBytes, maxLines)
}

func (r *Repository) StagedStat() ([]FileStat, error) {
	diff, err := r.stagedTreeDiff()
	if err != nil {
		return nil, err
	}
	return diff.Stats(), nil
}

func (r *Repository) StagedSnapshot(maxBytes, maxLines int) (ChangeSnapshot, error) {
	baseTree, targetTree, diff, err := r.stagedChangeState()
	if err != nil {
		return ChangeSnapshot{}, err
	}
	diffText, truncated := textutil.Limit(r.diffText(diff, nil), maxBytes, maxLines)
	return ChangeSnapshot{
		Paths:         diff.Paths(),
		Status:        diff.Status(),
		Stats:         diff.Stats(),
		Diff:          diffText,
		DiffTruncated: truncated,
		Fingerprint:   changeFingerprint(baseTree, targetTree, diff.submodules),
	}, nil
}

func (r *Repository) UncommittedSnapshot(maxBytes, maxLines int) (ChangeSnapshot, error) {
	workspace, err := r.uncommittedWorkspace(true)
	if err != nil {
		return ChangeSnapshot{}, err
	}
	return workspace.snapshot(maxBytes, maxLines)
}

func (r *Repository) StagedFingerprint() (ChangeFingerprint, error) {
	baseTree, err := r.headTree()
	if err != nil {
		return ChangeFingerprint{}, err
	}
	targetTree, err := r.indexTree()
	if err != nil {
		return ChangeFingerprint{}, err
	}
	return changeFingerprint(baseTree, targetTree, nil), nil
}

func (r *Repository) UncommittedFingerprint() (ChangeFingerprint, error) {
	workspace, err := r.uncommittedWorkspace(false)
	if err != nil {
		return ChangeFingerprint{}, err
	}
	return workspace.fingerprint(), nil
}

func (r *Repository) CheckStagedFingerprint(want ChangeFingerprint) error {
	got, err := r.StagedFingerprint()
	return checkChangeFingerprint(want, got, err)
}

func (r *Repository) CheckUncommittedFingerprint(want ChangeFingerprint) error {
	got, err := r.UncommittedFingerprint()
	return checkChangeFingerprint(want, got, err)
}

func checkChangeFingerprint(want, got ChangeFingerprint, err error) error {
	if err != nil {
		return fmt.Errorf("refresh authoritative repository state: %w", err)
	}
	if got != want {
		return ErrChangeSnapshotStale
	}
	return nil
}

func changeFingerprint(baseTree, targetTree *object.Tree, submodules []SubmoduleChange) ChangeFingerprint {
	fingerprint := ChangeFingerprint{
		BaseTree:   treeHash(baseTree),
		TargetTree: treeHash(targetTree),
	}
	hasher := sha256.New()
	dirty := 0
	for _, change := range submodules {
		if !change.Dirty {
			continue
		}
		dirty++
		writeHashField(hasher, change.Path)
		writeHashField(hasher, change.Old)
		writeHashField(hasher, change.New)
		writeHashField(hasher, change.dirtyState)
	}
	if dirty > 0 {
		fingerprint.DirtySubmodules = fmt.Sprintf("%x", hasher.Sum(nil))
	}
	return fingerprint
}

func treeHash(tree *object.Tree) string {
	if tree == nil {
		return plumbing.ZeroHash.String()
	}
	return tree.Hash.String()
}

func writeHashField(hasher hash.Hash, value string) {
	_, _ = fmt.Fprintf(hasher, "%d:", len(value))
	_, _ = io.WriteString(hasher, value)
}

func (r *Repository) StagedSubmoduleChanges() ([]SubmoduleChange, error) {
	diff, err := r.stagedTreeDiff()
	if err != nil {
		return nil, err
	}
	return diff.submodules, nil
}

type filePatchSet struct {
	patches []fdiff.FilePatch
}

type treeDiff struct {
	patch      *object.Patch
	submodules []SubmoduleChange
}

func newTreeDiff(from, to *object.Tree) (*treeDiff, error) {
	patch, err := patchBetweenTrees(from, to)
	if err != nil {
		return nil, err
	}
	submodules, err := submoduleChangesBetweenTrees(from, to)
	if err != nil {
		return nil, err
	}
	return &treeDiff{patch: patch, submodules: submodules}, nil
}

func (d *treeDiff) String() string {
	if d == nil {
		return ""
	}
	base := d.patch.String()
	supplement := submoduleDiffText(d.unrepresentedSubmodules(nil))
	if base == "" || supplement == "" {
		return base + supplement
	}
	return strings.TrimRight(base, "\n") + "\n" + supplement
}

func (d *treeDiff) Paths() []string {
	if d == nil {
		return nil
	}
	paths := map[string]bool{}
	for _, path := range filePathsFromPatch(d.patch) {
		paths[path] = true
	}
	for _, change := range d.submodules {
		paths[change.Path] = true
	}
	return slices.Sorted(maps.Keys(paths))
}

func (d *treeDiff) Stats() []FileStat {
	if d == nil {
		return nil
	}
	stats := fileStatsFromPatch(d.patch)
	seen := make(map[string]bool, len(stats))
	for _, stat := range stats {
		seen[stat.Path] = true
	}
	for _, change := range d.submodules {
		if seen[change.Path] {
			continue
		}
		stats = append(stats, FileStat{
			Path:    change.Path,
			Adds:    boolInt(change.New != ""),
			Deletes: boolInt(change.Old != ""),
		})
	}
	slices.SortFunc(stats, func(a, b FileStat) int { return cmp.Compare(a.Path, b.Path) })
	return stats
}

func (d *treeDiff) Status() []PathChange {
	if d == nil {
		return nil
	}
	status := make(map[string]git.StatusCode)
	for _, patch := range d.patch.FilePatches() {
		from, to := patch.Files()
		switch {
		case from == nil && to != nil:
			status[to.Path()] = git.Added
		case from != nil && to == nil:
			status[from.Path()] = git.Deleted
		case from != nil && to != nil && from.Path() != to.Path():
			status[from.Path()] = git.Deleted
			status[to.Path()] = git.Added
		case to != nil:
			status[to.Path()] = git.Modified
		}
	}
	for _, change := range d.submodules {
		if _, ok := status[change.Path]; ok {
			continue
		}
		switch {
		case change.Old == "":
			status[change.Path] = git.Added
		case change.New == "":
			status[change.Path] = git.Deleted
		default:
			status[change.Path] = git.Modified
		}
	}
	paths := slices.Sorted(maps.Keys(status))
	changes := make([]PathChange, 0, len(paths))
	for _, path := range paths {
		changes = append(changes, PathChange{Path: path, Staging: string(status[path])})
	}
	return changes
}

func (d *treeDiff) unrepresentedSubmodules(selected map[string]bool) []SubmoduleChange {
	if d == nil {
		return nil
	}
	represented := map[string]bool{}
	for _, path := range filePathsFromPatch(d.patch) {
		represented[path] = true
	}
	changes := make([]SubmoduleChange, 0, len(d.submodules))
	for _, change := range d.submodules {
		if represented[change.Path] || selected != nil && !selected[change.Path] {
			continue
		}
		changes = append(changes, change)
	}
	return changes
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func submoduleDiffText(changes []SubmoduleChange) string {
	var b strings.Builder
	for _, change := range changes {
		oldPath := strconv.Quote("a/" + change.Path)
		newPath := strconv.Quote("b/" + change.Path)
		oldHash := strings.Repeat("0", 40)
		newHash := strings.Repeat("0", 40)
		if change.Old != "" {
			oldHash = change.Old
		} else {
			oldPath = "/dev/null"
		}
		if change.New != "" {
			newHash = change.New
		} else {
			newPath = "/dev/null"
		}
		fmt.Fprintf(&b, "diff --git %q %q\nindex %.7s..%.7s 160000\n--- %s\n+++ %s\n", "a/"+change.Path, "b/"+change.Path, oldHash, newHash, oldPath, newPath)
		switch {
		case change.Old == "":
			newCommit := change.New
			if change.Dirty {
				newCommit += "-dirty"
			}
			fmt.Fprintf(&b, "@@ -0,0 +1 @@\n+Subproject commit %s\n", newCommit)
		case change.New == "":
			fmt.Fprintf(&b, "@@ -1 +0,0 @@\n-Subproject commit %s\n", change.Old)
		default:
			newCommit := change.New
			if change.Dirty {
				newCommit += "-dirty"
			}
			fmt.Fprintf(&b, "@@ -1 +1 @@\n-Subproject commit %s\n+Subproject commit %s\n", change.Old, newCommit)
		}
	}
	return b.String()
}

func (r *Repository) diffText(diff *treeDiff, selected map[string]bool) string {
	return r.prependSubmoduleHistory(diff.String(), diff.submodules, selected)
}

func (r *Repository) prependSubmoduleHistory(text string, changes []SubmoduleChange, selected map[string]bool) string {
	summary := r.submoduleHistoryText(changes, selected)
	if summary == "" {
		return text
	}
	if text == "" {
		return summary
	}
	return summary + strings.TrimLeft(text, "\n")
}

func (r *Repository) submoduleHistoryText(changes []SubmoduleChange, selected map[string]bool) string {
	var summaries strings.Builder
	for _, change := range changes {
		if selected != nil && !selected[change.Path] || change.Old == "" || change.New == "" || change.Old == change.New {
			continue
		}
		commits, err := r.SubmoduleCommits(change.Path, change.Old, change.New, 50)
		if err != nil || len(commits) == 0 {
			continue
		}
		fmt.Fprintf(&summaries, "Submodule commits %s (%s..%s):\n", change.Path, change.Old[:7], change.New[:7])
		for _, commit := range commits {
			fmt.Fprintf(&summaries, "  %.7s %s\n", commit.SHA, commit.Summary)
		}
	}
	return summaries.String()
}

// SubmoduleCommits returns locally available commits between two gitlinks.
func (r *Repository) SubmoduleCommits(path, base, head string, limit int) ([]CommitInfo, error) {
	subRepo, err := Open(filepath.Join(r.RootPath, path))
	if err != nil {
		return nil, err
	}
	return subRepo.LogFrom(base, head, limit)
}

func (p filePatchSet) FilePatches() []fdiff.FilePatch {
	return p.patches
}

func (p filePatchSet) Message() string {
	return ""
}

func filePatchMatchesAnyPath(patch fdiff.FilePatch, paths map[string]bool) bool {
	from, to := patch.Files()
	for _, file := range []fdiff.File{from, to} {
		if file != nil && paths[filepath.ToSlash(file.Path())] {
			return true
		}
	}
	return false
}

func patchForPaths(patch *object.Patch, paths []string) (string, error) {
	pathSet := make(map[string]bool, len(paths))
	for _, path := range paths {
		pathSet[filepath.ToSlash(path)] = true
	}
	selected := make([]fdiff.FilePatch, 0, len(paths))
	for _, filePatch := range patch.FilePatches() {
		if filePatchMatchesAnyPath(filePatch, pathSet) {
			selected = append(selected, filePatch)
		}
	}
	if len(selected) == 0 {
		return "", nil
	}
	var b bytes.Buffer
	if err := fdiff.NewUnifiedEncoder(&b, fdiff.DefaultContextLines).Encode(filePatchSet{patches: selected}); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (r *Repository) limitedTreeDiffForPaths(diff *treeDiff, paths []string, maxBytes, maxLines int) (string, bool, error) {
	pathSet := make(map[string]bool, len(paths))
	for _, path := range paths {
		pathSet[filepath.ToSlash(path)] = true
	}
	base, err := patchForPaths(diff.patch, paths)
	if err != nil {
		return "", false, err
	}
	supplement := submoduleDiffText(diff.unrepresentedSubmodules(pathSet))
	if base != "" && supplement != "" {
		base = strings.TrimRight(base, "\n") + "\n"
	}
	text := r.prependSubmoduleHistory(base+supplement, diff.submodules, pathSet)
	limited, truncated := textutil.Limit(text, maxBytes, maxLines)
	return limited, truncated, nil
}

func (r *Repository) LastVersionTag() (string, error) {
	tagsByCommit, err := r.versionTagsByCommit()
	if err != nil {
		return "", err
	}
	if len(tagsByCommit) == 0 {
		return "", errors.New("no semantic version tags found")
	}

	from, err := r.resolveLogStart("HEAD")
	if err != nil {
		return "", err
	}
	iter := object.NewCommitPreorderIter(from, nil, nil)
	defer iter.Close()

	var tag string
	err = iter.ForEach(func(commit *object.Commit) error {
		tags := tagsByCommit[commit.Hash]
		if len(tags) == 0 {
			return nil
		}
		slices.SortFunc(tags, compareVersionTags)
		tag = tags[0].Name
		return stopeach{}
	})
	if errors.As(err, new(stopeach)) {
		return tag, nil
	}
	if err != nil {
		return "", err
	}
	return "", errors.New("no semantic version tags reachable from HEAD")
}

type versionTag struct {
	Name  string
	Major int
	Minor int
	Patch int
}

func (r *Repository) versionTagsByCommit() (map[plumbing.Hash][]versionTag, error) {
	iter, err := r.Repo.Tags()
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	tags := map[plumbing.Hash][]versionTag{}
	for {
		ref, err := iter.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		tag, ok := parseVersionTag(ref.Name().Short())
		if !ok {
			continue
		}
		commit, err := r.ResolveCommit(tag.Name)
		if err != nil {
			continue
		}
		tags[commit.Hash] = append(tags[commit.Hash], tag)
	}
	return tags, nil
}

func parseVersionTag(name string) (versionTag, bool) {
	trimmed := strings.TrimPrefix(name, "v")
	parts := strings.Split(trimmed, ".")
	if len(parts) != 3 {
		return versionTag{}, false
	}
	major, ok := parseVersionPart(parts[0])
	if !ok {
		return versionTag{}, false
	}
	minor, ok := parseVersionPart(parts[1])
	if !ok {
		return versionTag{}, false
	}
	patch, ok := parseVersionPart(parts[2])
	if !ok {
		return versionTag{}, false
	}
	return versionTag{Name: name, Major: major, Minor: minor, Patch: patch}, true
}

func parseVersionPart(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	if len(value) > 1 && value[0] == '0' {
		return 0, false
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func compareVersionTags(a, b versionTag) int {
	if diff := cmp.Compare(b.Major, a.Major); diff != 0 {
		return diff
	}
	if diff := cmp.Compare(b.Minor, a.Minor); diff != 0 {
		return diff
	}
	if diff := cmp.Compare(b.Patch, a.Patch); diff != 0 {
		return diff
	}
	return strings.Compare(a.Name, b.Name)
}

func (r *Repository) RecentCommits(limit int) ([]CommitInfo, error) {
	return r.LogFrom("", "", limit)
}

func (r *Repository) HeadInfo() (CommitInfo, error) {
	head, err := r.headCommit()
	if err != nil {
		return CommitInfo{}, err
	}
	return commitInfo(head), nil
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

func (r *Repository) HeadMessage() (string, error) {
	head, err := r.headCommit()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(head.Message), nil
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

func (r *Repository) DiffAgainstParentChanges() ([]CommitFileChange, error) {
	patch, err := r.patchHeadAgainstParent()
	if err != nil {
		return nil, err
	}
	return commitFileChangesFromPatch(patch), nil
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
	patch, err := r.patchFinalAmended()
	if err != nil {
		return "", false, err
	}
	limited, truncated := textutil.Limit(patch.String(), maxBytes, maxLines)
	return limited, truncated, nil
}

func (r *Repository) FinalAmendedPaths() ([]string, error) {
	patch, err := r.patchFinalAmended()
	if err != nil {
		return nil, err
	}
	return filePathsFromPatch(patch), nil
}

func (r *Repository) FinalAmendedStat() ([]FileStat, error) {
	patch, err := r.patchFinalAmended()
	if err != nil {
		return nil, err
	}
	return fileStatsFromPatch(patch), nil
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
	input := io.Reader(reader)
	if maxBytes > 0 {
		input = io.LimitReader(reader, int64(maxBytes)+1)
	}
	var b bytes.Buffer
	if _, err := io.Copy(&b, input); err != nil {
		return "", false, err
	}
	limited, truncated := textutil.Limit(b.String(), maxBytes, maxLines)
	return limited, truncated, nil
}

func (r *Repository) StagedFilePrefix(path string, maxBytes int) (string, bool, error) {
	return r.FilePrefix(FileSourceIndex, path, maxBytes)
}

func (r *Repository) FilePrefix(source FileSource, path string, maxBytes int) (string, bool, error) {
	reader, err := r.OpenFile(source, path)
	if err != nil {
		return "", false, err
	}
	defer reader.Close()
	if maxBytes <= 0 {
		content, err := io.ReadAll(reader)
		return string(content), false, err
	}
	var b bytes.Buffer
	if _, err := io.Copy(&b, io.LimitReader(reader, int64(maxBytes)+1)); err != nil {
		return "", false, err
	}
	limited, truncated := textutil.Limit(b.String(), maxBytes, 0)
	return limited, truncated, nil
}

func (r *Repository) IndexFile(path string) (string, bool, error) {
	content, err := r.indexFileContent(path)
	if errors.Is(err, index.ErrEntryNotFound) {
		return "", false, nil
	}
	return content, err == nil, err
}

func (r *Repository) OpenFile(source FileSource, path string) (io.ReadCloser, error) {
	switch source {
	case FileSourceWorktree:
		root, err := os.OpenRoot(r.RootPath)
		if err != nil {
			return nil, err
		}
		info, err := root.Lstat(filepath.FromSlash(path))
		if err != nil || !info.Mode().IsRegular() {
			closeErr := root.Close()
			if err == nil {
				err = fmt.Errorf("worktree path %q is not a regular file", path)
			}
			return nil, errors.Join(err, closeErr)
		}
		file, err := root.Open(filepath.FromSlash(path))
		closeErr := root.Close()
		if err != nil || closeErr != nil {
			if file != nil {
				_ = file.Close()
			}
			return nil, errors.Join(err, closeErr)
		}
		return file, nil
	case FileSourceIndex:
		idx, err := r.Repo.Storer.Index()
		if err != nil {
			return nil, err
		}
		entry, err := idx.Entry(path)
		if err != nil {
			return nil, err
		}
		blob, err := r.Repo.BlobObject(entry.Hash)
		if err != nil {
			return nil, err
		}
		return blob.Reader()
	case FileSourceHead:
		commit, err := r.ResolveCommit("HEAD")
		if err != nil {
			return nil, err
		}
		file, err := commit.File(path)
		if err != nil {
			return nil, err
		}
		return file.Reader()
	default:
		return nil, fmt.Errorf("unknown repository file source %q", source)
	}
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

func (r *Repository) WalkCommitTextFiles(ref string, maxBytes int64, include func(string) bool, visit func(CommitFile) error, skip func(CommitFileSkip) error) error {
	commit, err := r.ResolveCommit(ref)
	if err != nil {
		return err
	}
	tree, err := commit.Tree()
	if err != nil {
		return err
	}
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()
	for {
		name, entry, err := walker.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Mode == filemode.Dir || entry.Mode == filemode.Submodule {
			continue
		}
		path := filepath.ToSlash(name)
		if include != nil && !include(path) {
			continue
		}
		blob, err := object.GetBlob(r.Repo.Storer, entry.Hash)
		if err != nil {
			return err
		}
		file := object.NewFile(path, entry.Mode, blob)
		if maxBytes > 0 && file.Size > maxBytes {
			if skip == nil {
				continue
			}
			if err := skip(CommitFileSkip{
				Path:   path,
				Blob:   file.Hash.String(),
				Size:   file.Size,
				Reason: "oversized",
			}); err != nil {
				return err
			}
			continue
		}
		binary, err := file.IsBinary()
		if err != nil {
			return err
		}
		if binary {
			if skip == nil {
				continue
			}
			if err := skip(CommitFileSkip{
				Path:   path,
				Blob:   file.Hash.String(),
				Size:   file.Size,
				Reason: "binary",
			}); err != nil {
				return err
			}
			continue
		}
		text, err := file.Contents()
		if err != nil {
			return err
		}
		if err := visit(CommitFile{
			Path: path,
			Blob: file.Hash.String(),
			Text: text,
			Size: file.Size,
		}); err != nil {
			return err
		}
	}
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

func (r *Repository) LogMessagesFrom(base, release string, limit int) ([]CommitMessageInfo, error) {
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

	var commits []CommitMessageInfo
	err = iter.ForEach(func(commit *object.Commit) error {
		commits = append(commits, commitMessageInfo(commit))
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
	diff, err := r.stagedTreeDiff()
	if err != nil {
		return "", err
	}
	return diff.String(), nil
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

func (r *Repository) stagedTreeDiff() (*treeDiff, error) {
	_, _, diff, err := r.stagedChangeState()
	return diff, err
}

func (r *Repository) stagedChangeState() (*object.Tree, *object.Tree, *treeDiff, error) {
	headTree, err := r.headTree()
	if err != nil {
		return nil, nil, nil, err
	}
	indexTree, err := r.indexTree()
	if err != nil {
		return nil, nil, nil, err
	}
	diff, err := newTreeDiff(headTree, indexTree)
	return headTree, indexTree, diff, err
}

func mergeDirtySubmodules(diff *treeDiff, dirtySubmodules []SubmoduleChange) {
	byPath := make(map[string]int, len(diff.submodules))
	for i, change := range diff.submodules {
		byPath[change.Path] = i
	}
	for _, change := range dirtySubmodules {
		if index, ok := byPath[change.Path]; ok {
			diff.submodules[index].Dirty = true
			diff.submodules[index].dirtyState = change.dirtyState
			continue
		}
		diff.submodules = append(diff.submodules, change)
	}
	slices.SortFunc(diff.submodules, func(a, b SubmoduleChange) int { return cmp.Compare(a.Path, b.Path) })
}

func (r *Repository) dirtySubmoduleChanges(headTree *object.Tree) ([]SubmoduleChange, error) {
	worktree, err := r.Repo.Worktree()
	if err != nil {
		return nil, err
	}
	submodules, err := worktree.Submodules()
	if err != nil {
		return nil, err
	}
	baseHashes, err := collectSubmoduleEntries(headTree, "")
	if err != nil {
		return nil, err
	}
	rootPath, err := filepath.EvalSymlinks(r.RootPath)
	if err != nil {
		return nil, err
	}
	dirtySubmodules := make([]SubmoduleChange, 0, len(submodules))
	for _, submodule := range submodules {
		path := filepath.ToSlash(submodule.Config().Path)
		nativePath := filepath.FromSlash(path)
		if !filepath.IsLocal(nativePath) || slices.ContainsFunc(strings.Split(path, "/"), func(part string) bool {
			return strings.EqualFold(part, ".git")
		}) {
			continue
		}
		subRoot, err := filepath.EvalSymlinks(filepath.Join(rootPath, nativePath))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootPath, subRoot)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			continue
		}
		subRepo, err := git.PlainOpen(subRoot)
		if err != nil {
			continue
		}
		subStatus, statusErr := (&Repository{RootPath: subRoot, WorkPath: subRoot, Repo: subRepo}).status()
		head, headErr := subRepo.Head()
		dirtyState := ""
		if statusErr == nil && !subStatus.IsClean() {
			dirtyState, err = worktreeStatusFingerprint(subRoot, subStatus)
		}
		closeErr := subRepo.Close()
		if err != nil {
			return nil, fmt.Errorf("fingerprint dirty submodule %q: %w", path, err)
		}
		if err := errors.Join(statusErr, headErr, closeErr); err != nil {
			return nil, fmt.Errorf("inspect dirty submodule %q: %w", path, err)
		}
		if subStatus.IsClean() {
			continue
		}
		change := SubmoduleChange{Path: path, Old: baseHashes[path], New: head.Hash().String(), Dirty: true, dirtyState: dirtyState}
		dirtySubmodules = append(dirtySubmodules, change)
	}
	slices.SortFunc(dirtySubmodules, func(a, b SubmoduleChange) int { return cmp.Compare(a.Path, b.Path) })
	return dirtySubmodules, nil
}

func worktreeStatusFingerprint(rootPath string, status git.Status) (string, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()

	paths := slices.Sorted(maps.Keys(status))
	hasher := sha256.New()
	for _, path := range paths {
		file := status[path]
		writeHashField(hasher, filepath.ToSlash(path))
		writeHashField(hasher, string(file.Staging))
		writeHashField(hasher, string(file.Worktree))
		if file.Worktree == git.Deleted {
			writeHashField(hasher, "deleted")
			continue
		}
		if err := hashWorktreePath(hasher, root, path); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

func hashWorktreePath(hasher hash.Hash, root *os.Root, path string) error {
	info, err := root.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeHashField(hasher, "missing")
			return nil
		}
		return fmt.Errorf("stat path %q: %w", path, err)
	}
	writeHashField(hasher, info.Mode().String())
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := root.Readlink(path)
		if err != nil {
			return fmt.Errorf("read symlink %q: %w", path, err)
		}
		writeHashField(hasher, target)
	case info.Mode().IsRegular():
		writeHashField(hasher, strconv.FormatInt(info.Size(), 10))
		file, err := root.Open(path)
		if err != nil {
			return fmt.Errorf("open path %q: %w", path, err)
		}
		_, copyErr := io.Copy(hasher, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return fmt.Errorf("hash path %q: %w", path, err)
		}
	}
	return nil
}

func (r *Repository) worktreeTree(status git.Status) (*object.Tree, error) {
	idx, err := r.Repo.Storer.Index()
	if err != nil {
		return nil, err
	}
	worktreeIndex := cloneIndex(idx)
	submoduleHashes, err := r.worktreeSubmoduleHashes()
	if err != nil {
		return nil, err
	}
	st := newOverlayObjectStorer(r.Repo.Storer)
	root, err := os.OpenRoot(r.RootPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()

	const maxFileBytes = 8 * 1024 * 1024
	const maxSnapshotBytes = 64 * 1024 * 1024
	loadedBytes := int64(0)
	for path, file := range status {
		if file.Worktree == git.Unmodified {
			continue
		}
		path = filepath.ToSlash(path)
		if file.Worktree == git.Deleted {
			_, _ = worktreeIndex.Remove(path)
			continue
		}

		entry, entryErr := worktreeIndex.Entry(path)
		if entryErr == nil && entry.Mode == filemode.Submodule {
			if hash := submoduleHashes[path]; !hash.IsZero() {
				entry.Hash = hash
			}
			continue
		}
		info, err := root.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("stat worktree path %q: %w", path, err)
		}
		var content []byte
		mode := filemode.Regular
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := root.Readlink(path)
			if err != nil {
				return nil, fmt.Errorf("read worktree symlink %q: %w", path, err)
			}
			content = []byte(target)
			mode = filemode.Symlink
		case info.Mode().IsRegular():
			remaining := int64(maxSnapshotBytes) - loadedBytes
			limit := min(int64(maxFileBytes), remaining)
			if limit <= 0 || info.Size() > limit {
				digest, err := fileSHA256(root, path)
				if err != nil {
					return nil, err
				}
				content = omittedWorktreeContent(info.Size(), limit, digest)
			} else {
				file, err := root.Open(path)
				if err != nil {
					return nil, fmt.Errorf("open worktree path %q: %w", path, err)
				}
				content, err = io.ReadAll(io.LimitReader(file, limit+1))
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					return nil, fmt.Errorf("read worktree path %q: %w", path, errors.Join(err, closeErr))
				}
				if int64(len(content)) > limit {
					digest, err := fileSHA256(root, path)
					if err != nil {
						return nil, err
					}
					content = omittedWorktreeContent(info.Size(), limit, digest)
				} else {
					loadedBytes += int64(len(content))
				}
			}
			if info.Mode().Perm()&0o111 != 0 {
				mode = filemode.Executable
			}
		default:
			continue
		}

		hash, err := storeBlob(st, content)
		if err != nil {
			return nil, fmt.Errorf("store worktree path %q: %w", path, err)
		}
		if entryErr != nil {
			entry, err = worktreeIndex.Add(path)
			if err != nil {
				return nil, err
			}
		}
		entry.Hash = hash
		entry.Mode = mode
		entry.Size = uint32(min(int64(len(content)), int64(^uint32(0))))
	}

	hash, err := buildIndexTree(st, worktreeIndex)
	if err != nil {
		return nil, err
	}
	if hash.IsZero() {
		return nil, nil
	}
	return object.GetTree(st, hash)
}

func (r *Repository) worktreeSubmoduleHashes() (map[string]plumbing.Hash, error) {
	worktree, err := r.Repo.Worktree()
	if err != nil {
		return nil, err
	}
	submodules, err := worktree.Submodules()
	if err != nil {
		return nil, err
	}
	statuses, err := submodules.Status()
	if err != nil {
		return nil, err
	}
	hashes := make(map[string]plumbing.Hash, len(statuses))
	for _, status := range statuses {
		hash := status.Current
		if hash.IsZero() {
			hash = status.Expected
		}
		hashes[filepath.ToSlash(status.Path)] = hash
	}
	return hashes, nil
}

func fileSHA256(root *os.Root, path string) (string, error) {
	file, err := root.Open(path)
	if err != nil {
		return "", fmt.Errorf("open worktree path %q for fingerprint: %w", path, err)
	}
	hasher := sha256.New()
	_, copyErr := io.Copy(hasher, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", fmt.Errorf("hash worktree path %q: %w", path, err)
	}
	return fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

func omittedWorktreeContent(size, limit int64, digest string) []byte {
	return fmt.Appendf(nil, "[git-agent: worktree file omitted; size=%d bytes exceeds %d-byte review cap; sha256=%s]\n", size, max(0, limit), digest)
}

func cloneIndex(src *index.Index) *index.Index {
	if src == nil {
		return &index.Index{Version: 2}
	}
	dst := *src
	dst.Entries = make([]*index.Entry, len(src.Entries))
	for i, entry := range src.Entries {
		cloned := *entry
		dst.Entries[i] = &cloned
	}
	dst.Cache = nil
	return &dst
}

func storeBlob(st storer.EncodedObjectStorer, content []byte) (plumbing.Hash, error) {
	obj := st.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	writer, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := writer.Write(content); err != nil {
		_ = writer.Close()
		return plumbing.ZeroHash, err
	}
	if err := writer.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return st.SetEncodedObject(obj)
}

func (r *Repository) patchHeadAgainstParent() (*object.Patch, error) {
	head, err := r.headCommit()
	if err != nil {
		return nil, err
	}
	return patchCommitAgainstFirstParent(head)
}

func (r *Repository) patchFinalAmended() (*object.Patch, error) {
	head, err := r.headCommit()
	if err != nil {
		return nil, err
	}
	parentTree, finalTree, err := r.finalAmendedTrees(head)
	if err != nil {
		return nil, err
	}
	return patchBetweenTrees(parentTree, finalTree)
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
	filePatches := patch.FilePatches()
	stats := patch.Stats()
	out := make([]FileStat, 0, len(filePatches))
	statsIndex := 0
	for _, filePatch := range filePatches {
		binary := filePatch.IsBinary()
		// go-git Patch.Stats omits empty-chunk patches, including binary
		// files and submodule pointer updates, while FilePatches retains them.
		if len(filePatch.Chunks()) == 0 && !binary {
			continue
		}
		from, to := filePatch.Files()
		path, _ := patchPath(from, to)
		stat := FileStat{Path: path, IsBinary: binary}
		if len(filePatch.Chunks()) > 0 {
			if statsIndex < len(stats) {
				stat.Adds = stats[statsIndex].Addition
				stat.Deletes = stats[statsIndex].Deletion
			}
			statsIndex++
		}
		out = append(out, stat)
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
	message := strings.TrimSpace(commit.Message)
	return CommitInfo{
		SHA:     commit.Hash.String(),
		Summary: strings.Split(message, "\n")[0],
		Author:  commit.Author.Name,
		Date:    commit.Author.When.Format(time.RFC3339),
	}
}

func commitMessageInfo(commit *object.Commit) CommitMessageInfo {
	info := commitInfo(commit)
	message := CommitMessageInfo{
		SHA:     info.SHA,
		Summary: info.Summary,
		Message: strings.TrimSpace(commit.Message),
		Author:  info.Author,
		Date:    info.Date,
	}
	patch, err := patchCommitAgainstFirstParent(commit)
	if err != nil {
		message.EvidenceError = err.Error()
		return message
	}
	message.Files = commitFileChangesFromPatch(patch)
	message.Diffstat = commitDiffstat(message.Files)
	message.PatchExcerpt = commitPatchExcerpt(patch, 12*1024, 80)
	return message
}

func patchCommitAgainstFirstParent(commit *object.Commit) (*object.Patch, error) {
	if commit.NumParents() == 0 {
		tree, err := commit.Tree()
		if err != nil {
			return nil, err
		}
		return (&object.Tree{}).Patch(tree)
	}
	parent, err := commit.Parent(0)
	if err != nil {
		return nil, err
	}
	return parent.Patch(commit)
}

func commitFileChangesFromPatch(patch *object.Patch) []CommitFileChange {
	if patch == nil {
		return nil
	}
	filePatches := patch.FilePatches()
	stats := patch.Stats()
	changes := make([]CommitFileChange, 0, len(filePatches))
	for index, filePatch := range filePatches {
		from, to := filePatch.Files()
		path, oldPath := patchPath(from, to)
		change := CommitFileChange{
			Path:      path,
			OldPath:   oldPath,
			Status:    patchStatus(from, to),
			Binary:    filePatch.IsBinary(),
			Submodule: patchHasSubmoduleMode(from, to),
		}
		if index < len(stats) {
			change.Additions = stats[index].Addition
			change.Deletions = stats[index].Deletion
		}
		changes = append(changes, change)
	}
	slices.SortFunc(changes, func(a, b CommitFileChange) int {
		return cmp.Compare(a.Path, b.Path)
	})
	return changes
}

func patchPath(from, to fdiff.File) (string, string) {
	if to == nil {
		if from == nil {
			return "", ""
		}
		return from.Path(), ""
	}
	if from != nil && from.Path() != to.Path() {
		return to.Path(), from.Path()
	}
	return to.Path(), ""
}

func patchStatus(from, to fdiff.File) string {
	switch {
	case from == nil && to != nil:
		return "added"
	case from != nil && to == nil:
		return "deleted"
	case from != nil && to != nil && from.Path() != to.Path():
		return "renamed"
	default:
		return "modified"
	}
}

func patchHasSubmoduleMode(from, to fdiff.File) bool {
	return from != nil && from.Mode() == filemode.Submodule || to != nil && to.Mode() == filemode.Submodule
}

func commitDiffstat(files []CommitFileChange) CommitDiffstat {
	var stat CommitDiffstat
	stat.FilesChanged = len(files)
	for _, file := range files {
		stat.Additions += file.Additions
		stat.Deletions += file.Deletions
	}
	return stat
}

func commitPatchExcerpt(patch *object.Patch, maxBytes, maxLines int) string {
	if patch == nil {
		return ""
	}
	var b strings.Builder
	files := 0
	lines := 0
	for _, filePatch := range patch.FilePatches() {
		if filePatch.IsBinary() {
			continue
		}
		from, to := filePatch.Files()
		path, oldPath := patchPath(from, to)
		if path == "" {
			continue
		}
		if files >= 5 || lines >= maxLines {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		if oldPath != "" {
			fmt.Fprintf(&b, "--- %s => %s\n", oldPath, path)
		} else {
			fmt.Fprintf(&b, "--- %s\n", path)
		}
		files++
		for _, chunk := range filePatch.Chunks() {
			if chunk.Type() == fdiff.Equal {
				continue
			}
			prefix := "+"
			if chunk.Type() == fdiff.Delete {
				prefix = "-"
			}
			for line := range strings.SplitSeq(chunk.Content(), "\n") {
				if line == "" || lines >= maxLines {
					continue
				}
				trimmed := strings.TrimSpace(line)
				if trimmed == "" {
					continue
				}
				fmt.Fprintf(&b, "%s %s\n", prefix, trimmed)
				lines++
				if b.Len() >= maxBytes {
					limited, _ := textutil.Limit(b.String(), maxBytes, maxLines)
					return strings.TrimSpace(limited)
				}
			}
		}
	}
	limited, _ := textutil.Limit(b.String(), maxBytes, maxLines)
	return strings.TrimSpace(limited)
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
