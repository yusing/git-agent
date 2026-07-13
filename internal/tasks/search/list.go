package search

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/giturl"
	"github.com/yusing/git-agent/internal/metadata"
)

// IndexInfo describes one on-disk search index under a project metadata root.
type IndexInfo struct {
	Mode           string    `json:"mode"`
	Root           string    `json:"root,omitempty"`
	Remote         string    `json:"remote,omitempty"`
	ResolvedRev    string    `json:"resolved_rev,omitempty"`
	EmbeddingModel string    `json:"embedding_model"`
	Dimensions     int       `json:"dimensions"`
	CreatedAt      time.Time `json:"created_at"`
	Files          int       `json:"files"`
	Chunks         int       `json:"chunks"`
	Filters        []string  `json:"filters,omitempty"`
	Dir            string    `json:"dir"`
}

// IndexListing describes cached repository context and its completed indexes.
type IndexListing struct {
	RepoDir string
	Indexes []IndexInfo
}

// RemoteInfo describes one cached remote repository.
type RemoteInfo struct {
	Remote          string    `json:"remote"`
	LastFetchedAt   time.Time `json:"last_fetched_at,omitempty"`
	LastResolvedRev string    `json:"last_resolved_rev,omitempty"`
	Dir             string    `json:"dir"`
}

// ListFilesOptions selects which physical index ls-files should open.
type ListFilesOptions struct {
	Root    string
	Remote  string
	Rev     string
	Scope   []string
	NoTests bool
}

// IndexFiles is the ls-files result for one index.
type IndexFiles struct {
	Index IndexInfo `json:"index"`
	Files []string  `json:"files"`
}

// ListIndexes lists completed search indexes for the project containing root.
func ListIndexes(ctx context.Context, root, remote string) (IndexListing, error) {
	var listing IndexListing
	var metadataDir string
	if strings.TrimSpace(remote) != "" {
		var err error
		metadataDir, err = metadata.RemoteDir(giturl.Sanitize(remote))
		if err != nil {
			return IndexListing{}, err
		}
		repoDir := filepath.Join(metadataDir, "repo.git")
		if _, ok, err := openRemoteRepo(repoDir); err != nil {
			return IndexListing{}, err
		} else if ok {
			listing.RepoDir = repoDir
		}
	} else {
		selection, err := resolveIndexSelection(ctx, root, "", "", Filters{}, false, false, nil)
		if err != nil {
			return IndexListing{}, err
		}
		metadataDir = selection.metadataDir
	}
	searchRoot := filepath.Join(metadataDir, "search")
	info, err := os.Stat(searchRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return listing, nil
	}
	if err != nil {
		return IndexListing{}, err
	}
	if !info.IsDir() {
		return IndexListing{}, fmt.Errorf("search metadata path %s is not a directory", searchRoot)
	}

	var indexes []IndexInfo
	err = filepath.WalkDir(searchRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == "query-locks" {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != "manifest.json" {
			return nil
		}
		dir := filepath.Dir(path)
		var found IndexInfo
		if err := withIndexLock(ctx, dir, func() error {
			var err error
			found, err = inspectIndex(dir, searchRoot)
			return err
		}); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return nil
		}
		indexes = append(indexes, found)
		return nil
	})
	if err != nil {
		return IndexListing{}, err
	}
	slices.SortFunc(indexes, func(a, b IndexInfo) int {
		if c := cmp.Compare(a.Mode, b.Mode); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ResolvedRev, b.ResolvedRev); c != 0 {
			return c
		}
		if c := slices.Compare(a.Filters, b.Filters); c != 0 {
			return c
		}
		return cmp.Compare(a.Dir, b.Dir)
	})
	listing.Indexes = indexes
	return listing, nil
}

// ListRemotes lists cached remote repositories.
func ListRemotes() ([]RemoteInfo, error) {
	root, err := metadata.RemoteRoot()
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("remote metadata path %s is not a directory", root)
	}
	var remotes []RemoteInfo
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Name() != "remote.json" {
			return nil
		}
		cache := loadRemoteCache(path)
		if cache.URL == "" {
			return nil
		}
		remotes = append(remotes, RemoteInfo{
			Remote:          cache.URL,
			LastFetchedAt:   cache.LastFetchedAt,
			LastResolvedRev: cache.LastResolvedRev,
			Dir:             filepath.Dir(path),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(remotes, func(a, b RemoteInfo) int {
		return cmp.Compare(a.Remote, b.Remote)
	})
	return remotes, nil
}

// ListIndexFiles lists unique file paths stored in the selected search index.
func ListIndexFiles(ctx context.Context, opts ListFilesOptions) (IndexFiles, error) {
	scope, err := normalizeScopes(opts.Scope)
	if err != nil {
		return IndexFiles{}, err
	}
	root := opts.Root
	if strings.TrimSpace(opts.Remote) == "" {
		root, scope, err = localSearchRootAndScope(root, scope)
		if err != nil {
			return IndexFiles{}, err
		}
	}
	filters := Filters{Scope: scope}
	selection, err := resolveIndexSelection(ctx, root, opts.Remote, opts.Rev, filters, false, false, nil)
	if err != nil {
		return IndexFiles{}, err
	}

	info, entries, err := loadSelectedIndexFiles(ctx, selection)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return IndexFiles{}, fmt.Errorf("no search index at %s; run git-agent search --index first", selection.indexDir)
		}
		return IndexFiles{}, err
	}
	if len(scope) > 0 {
		entries.paths = slices.DeleteFunc(entries.paths, func(path string) bool { return !pathInScope(path, scope) })
	}
	if opts.NoTests {
		entries.paths = slices.DeleteFunc(entries.paths, isTestPath)
	}
	return IndexFiles{Index: info, Files: entries.paths}, nil
}

func localSearchRootAndScope(rootOpt string, scope []string) (string, []string, error) {
	root, err := filepath.Abs(cmp.Or(rootOpt, "."))
	if err != nil {
		return "", nil, err
	}
	repo, err := gitctx.Open(root)
	if err != nil {
		return root, scope, nil
	}
	rel, err := filepath.Rel(repo.RootPath, root)
	if err != nil {
		return "", nil, err
	}
	if rel == "." {
		return repo.RootPath, scope, nil
	}
	rel = filepath.ToSlash(rel)
	if len(scope) == 0 {
		return repo.RootPath, []string{rel}, nil
	}
	for i := range scope {
		scope[i] = filepath.ToSlash(filepath.Join(rel, scope[i]))
	}
	return repo.RootPath, scope, nil
}

func loadSelectedIndexFiles(ctx context.Context, selection indexSelection) (IndexInfo, indexEntries, error) {
	var info IndexInfo
	var entries indexEntries
	err := withIndexLock(ctx, selection.indexDir, func() error {
		var err error
		info, entries, err = inspectIndexEntries(selection.indexDir, filepath.Join(selection.metadataDir, "search"))
		return err
	})
	return info, entries, err
}

// FormatFileTree renders sorted indexed paths as a tree.
func FormatFileTree(paths []string) string {
	if len(paths) == 0 {
		return ".\n"
	}
	root := &treeNode{name: ".", children: map[string]*treeNode{}}
	for _, path := range paths {
		path = filepath.ToSlash(strings.TrimSpace(path))
		if path == "" || path == "." {
			continue
		}
		parts := strings.Split(path, "/")
		node := root
		for i, part := range parts {
			if part == "" || part == "." {
				continue
			}
			child, ok := node.children[part]
			if !ok {
				child = &treeNode{name: part, children: map[string]*treeNode{}}
				node.children[part] = child
			}
			if i == len(parts)-1 {
				child.isFile = true
			}
			node = child
		}
	}
	var b strings.Builder
	b.WriteString(".\n")
	writeTreeChildren(&b, root, "")
	return b.String()
}

// FormatIndexes renders index summaries for human stdout.
func FormatIndexes(listing IndexListing) string {
	var b strings.Builder
	if listing.RepoDir != "" {
		fmt.Fprintf(&b, "remote repo=%s\n", listing.RepoDir)
	}
	if len(listing.Indexes) == 0 {
		b.WriteString("no search indexes\n")
		return b.String()
	}
	for i, index := range listing.Indexes {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s", index.Mode)
		if index.ResolvedRev != "" {
			fmt.Fprintf(&b, " rev=%s", shortRev(index.ResolvedRev))
		}
		if index.Root != "" {
			fmt.Fprintf(&b, " root=%s", index.Root)
		}
		if index.Remote != "" {
			fmt.Fprintf(&b, " remote=%s", index.Remote)
		}
		if len(index.Filters) > 0 {
			fmt.Fprintf(&b, " filters=%s", strings.Join(index.Filters, ","))
		}
		fmt.Fprintf(&b, " files=%d chunks=%d model=%s dims=%d created=%s\n",
			index.Files, index.Chunks, index.EmbeddingModel, index.Dimensions,
			index.CreatedAt.UTC().Format(time.RFC3339))
		fmt.Fprintf(&b, "  %s\n", index.Dir)
	}
	return b.String()
}

// FormatRemotes renders cached remote summaries for human stdout.
func FormatRemotes(remotes []RemoteInfo) string {
	if len(remotes) == 0 {
		return "no cached remotes\n"
	}
	var b strings.Builder
	for i, remote := range remotes {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s", remote.Remote)
		if remote.LastResolvedRev != "" {
			fmt.Fprintf(&b, " rev=%s", shortRev(remote.LastResolvedRev))
		}
		if !remote.LastFetchedAt.IsZero() {
			fmt.Fprintf(&b, " fetched=%s", remote.LastFetchedAt.UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "\n  %s\n", remote.Dir)
	}
	return b.String()
}

// FormatRemoteCompletions writes one cached remote URL per line.
func FormatRemoteCompletions(w io.Writer, remotes []RemoteInfo) error {
	for _, remote := range remotes {
		if _, err := fmt.Fprintln(w, remote.Remote); err != nil {
			return err
		}
	}
	return nil
}

type treeNode struct {
	name     string
	children map[string]*treeNode
	isFile   bool
}

func writeTreeChildren(b *strings.Builder, node *treeNode, prefix string) {
	names := slices.Sorted(maps.Keys(node.children))
	for i, name := range names {
		child := node.children[name]
		last := i == len(names)-1
		branch := "├── "
		nextPrefix := prefix + "│   "
		if last {
			branch = "└── "
			nextPrefix = prefix + "    "
		}
		label := name
		if !child.isFile && len(child.children) > 0 {
			label += "/"
		}
		fmt.Fprintf(b, "%s%s%s\n", prefix, branch, label)
		if len(child.children) > 0 {
			writeTreeChildren(b, child, nextPrefix)
		}
	}
}

type indexSelection struct {
	root        string
	metadataDir string
	indexDir    string
	source      Source
	resolvedRev string
	repo        *gitctx.Repository
}

func resolveIndexSelection(ctx context.Context, rootOpt, remote, rev string, filters Filters, reindex, fetchAllowed bool, progressLog func(Progress) error) (indexSelection, error) {
	if strings.TrimSpace(remote) != "" {
		return resolveRemoteIndexSelection(ctx, remote, rev, filters, reindex, fetchAllowed, progressLog)
	}
	root, err := filepath.Abs(cmp.Or(rootOpt, "."))
	if err != nil {
		return indexSelection{}, err
	}
	source := Source{Mode: "filesystem", Root: root}
	indexRoot := root
	var resolvedRev string
	var repo *gitctx.Repository
	if rev != "" {
		repo, err = gitctx.Open(root)
		if err != nil {
			return indexSelection{}, fmt.Errorf("--rev requires a Git repository: %w", err)
		}
		resolvedRev, err = repo.ResolveRef(rev)
		if err != nil {
			return indexSelection{}, fmt.Errorf("resolve --rev %q: %w", rev, err)
		}
		indexRoot = repo.RootPath
		source = Source{Mode: "revision", Rev: rev, ResolvedRev: resolvedRev}
	} else if found, err := gitctx.Open(root); err == nil {
		indexRoot = found.RootPath
		repo = found
	}
	origin := repositoryOrigin(repo)
	originIdentity := ""
	if origin != "" {
		originIdentity = giturl.Identity(origin)
	}
	source.OriginIdentity = originIdentity
	legacyMetadataDir, err := metadata.Dir(indexRoot)
	if err != nil {
		return indexSelection{}, err
	}
	metadataDir, err := metadata.SearchDir(indexRoot, originIdentity)
	if err != nil {
		return indexSelection{}, err
	}
	if legacyMetadataDir != metadataDir {
		if err := migrateSearchMetadata(ctx, legacyMetadataDir, metadataDir); err != nil {
			return indexSelection{}, err
		}
	}
	return indexSelection{
		root:        root,
		metadataDir: metadataDir,
		indexDir:    indexDir(metadataDir, source.Mode, root, resolvedRev, filters),
		source:      source,
		resolvedRev: resolvedRev,
		repo:        repo,
	}, nil
}

func repositoryOrigin(repo *gitctx.Repository) string {
	if repo == nil {
		return ""
	}
	cfg, err := repo.Repo.Config()
	if err != nil {
		return ""
	}
	remote := cfg.Remotes["origin"]
	if remote == nil || len(remote.URLs) == 0 {
		return ""
	}
	return remote.URLs[0]
}

func inspectIndex(dir, searchRoot string) (IndexInfo, error) {
	found, err := loadManifest(dir)
	if err != nil {
		return IndexInfo{}, err
	}
	if found.FileCount == 0 && found.ChunkCount == 0 {
		info, _, err := inspectIndexEntriesFromManifest(found, dir, searchRoot)
		return info, err
	}
	filters := filtersFromIndexPath(dir, searchRoot)
	return indexInfoFromManifest(found, filters, dir, found.FileCount, found.ChunkCount), nil
}

func inspectIndexEntries(dir, searchRoot string) (IndexInfo, indexEntries, error) {
	found, err := loadManifest(dir)
	if err != nil {
		return IndexInfo{}, indexEntries{}, err
	}
	return inspectIndexEntriesFromManifest(found, dir, searchRoot)
}

func inspectIndexEntriesFromManifest(found manifest, dir, searchRoot string) (IndexInfo, indexEntries, error) {
	entries, err := loadIndexEntries(dir)
	if err != nil {
		return IndexInfo{}, indexEntries{}, err
	}
	filters := filtersFromIndexPath(dir, searchRoot)
	return indexInfoFromManifest(found, filters, dir, len(entries.paths), entries.chunks), entries, nil
}

func indexInfoFromManifest(found manifest, filters []string, dir string, files, chunks int) IndexInfo {
	return IndexInfo{
		Mode:           found.Mode,
		Root:           found.Root,
		Remote:         found.Remote,
		ResolvedRev:    found.ResolvedRev,
		EmbeddingModel: found.EmbeddingModel,
		Dimensions:     found.Dimensions,
		CreatedAt:      found.CreatedAt,
		Files:          files,
		Chunks:         chunks,
		Filters:        filters,
		Dir:            dir,
	}
}

func filtersFromIndexPath(dir, searchRoot string) []string {
	rel, err := filepath.Rel(searchRoot, dir)
	if err != nil {
		return nil
	}
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")
	if len(parts) < 2 {
		return nil
	}
	// Drop mode bucket (fs|revs) and its id (root hash or revision).
	parts = parts[2:]
	var filters []string
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		filters = append(filters, part)
	}
	return filters
}

type indexEntries struct {
	paths  []string
	chunks int
}

func loadIndexEntries(dir string) (indexEntries, error) {
	records, err := loadVectorIndexRecords(dir)
	if err != nil {
		return indexEntries{}, err
	}
	return indexEntries{
		paths:  uniqueSortedPathsFrom(records, func(record vectorIndexRecord) string { return record.Path }),
		chunks: len(records),
	}, nil
}

func loadVectorIndexRecords(dir string) ([]vectorIndexRecord, error) {
	data, err := os.ReadFile(filepath.Join(dir, "vectors.index.json"))
	if err != nil {
		return nil, err
	}
	var records []vectorIndexRecord
	if err := sonic.Unmarshal(data, &records); err != nil {
		return nil, err
	}
	return records, nil
}

func uniqueSortedPathsFrom[T any](items []T, pathOf func(T) string) []string {
	return slices.Sorted(maps.Keys(uniquePathSetFrom(items, pathOf)))
}

func uniquePathCountFrom[T any](items []T, pathOf func(T) string) int {
	return len(uniquePathSetFrom(items, pathOf))
}

func uniquePathSetFrom[T any](items []T, pathOf func(T) string) map[string]struct{} {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		path := filepath.ToSlash(pathOf(item))
		if path == "" {
			continue
		}
		seen[path] = struct{}{}
	}
	return seen
}

func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}
