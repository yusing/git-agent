package search

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/metadata"
)

const remoteRefreshTTL = 15 * time.Minute

type remoteCache struct {
	URL             string    `json:"url"`
	LastFetchedAt   time.Time `json:"last_fetched_at,omitempty"`
	LastResolvedRev string    `json:"last_resolved_rev,omitempty"`
}

func resolveRemoteIndexSelection(ctx context.Context, remoteURL, rev string, filters Filters, reindex, fetchAllowed bool) (indexSelection, error) {
	remote := sanitizeRemoteURL(remoteURL)
	metadataDir, err := metadata.RemoteDir(remote)
	if err != nil {
		return indexSelection{}, err
	}
	repoDir := filepath.Join(metadataDir, "repo.git")
	cachePath := filepath.Join(metadataDir, "remote.json")

	lock, err := lockIndex(ctx, filepath.Join(metadataDir, "remote"))
	if err != nil {
		return indexSelection{}, err
	}
	defer func() { _ = lock.Unlock() }()

	cache := loadRemoteCache(cachePath)
	repo, ok, err := openRemoteRepo(repoDir)
	if err != nil {
		return indexSelection{}, err
	}
	if !ok && !fetchAllowed {
		return indexSelection{}, fmt.Errorf("no cached remote repository for %s; run git-agent search --remote %s --index first", remote, remote)
	}
	needFetch := fetchAllowed && (!ok || reindex || cache.LastFetchedAt.IsZero() || time.Since(cache.LastFetchedAt) >= remoteRefreshTTL)
	shallowFetch := strings.TrimSpace(rev) == ""
	if !ok {
		repo, err = initRemoteRepo(repoDir, remote)
		if err != nil {
			return indexSelection{}, err
		}
	}
	if needFetch {
		if err := fetchRemote(ctx, repo, remoteURL, shallowFetch); err != nil {
			return indexSelection{}, fmt.Errorf("fetch remote %s: %s", remote, sanitizeRemoteError(err, remoteURL, remote))
		}
		cache.LastFetchedAt = time.Now().UTC()
	}

	wrapped, err := gitctx.OpenGitDir(repoDir)
	if err != nil {
		return indexSelection{}, err
	}
	sourceRev := strings.TrimSpace(rev)
	if sourceRev == "" {
		sourceRev = "HEAD"
	}
	resolvedRev, err := resolveRemoteRef(wrapped, sourceRev)
	fetchAndResolve := func(shallow bool) (string, error) {
		if err := fetchRemote(ctx, repo, remoteURL, shallow); err != nil {
			return "", fmt.Errorf("fetch remote %s: %s", remote, sanitizeRemoteError(err, remoteURL, remote))
		}
		cache.LastFetchedAt = time.Now().UTC()
		wrapped, err = gitctx.OpenGitDir(repoDir)
		if err != nil {
			return "", err
		}
		return resolveRemoteRef(wrapped, sourceRev)
	}
	if err != nil && fetchAllowed && !needFetch {
		resolvedRev, err = fetchAndResolve(true)
	}
	if err != nil && fetchAllowed {
		resolvedRev, err = fetchAndResolve(false)
	}
	if err != nil {
		return indexSelection{}, fmt.Errorf("resolve --rev %q for remote %s: %w", sourceRev, remote, err)
	}
	cache.URL = remote
	cache.LastResolvedRev = resolvedRev
	if fetchAllowed {
		if err := saveRemoteCache(cachePath, cache); err != nil {
			return indexSelection{}, err
		}
	}
	source := Source{Mode: "remote", Remote: remote, Rev: sourceRev, ResolvedRev: resolvedRev}
	return indexSelection{
		root:        "",
		metadataDir: metadataDir,
		indexDir:    indexDir(metadataDir, source.Mode, "", resolvedRev, filters),
		source:      source,
		resolvedRev: resolvedRev,
		repo:        wrapped,
	}, nil
}

func sanitizeRemoteURL(value string) string {
	trimmed := strings.TrimSpace(value)
	parsed, err := url.Parse(trimmed)
	if err == nil {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		parsed.RawFragment = ""
		return parsed.String()
	}
	return trimmed
}

func sanitizeRemoteError(err error, raw, sanitized string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if raw != "" && raw != sanitized {
		message = strings.ReplaceAll(message, raw, sanitized)
	}
	return message
}

func loadRemoteCache(path string) remoteCache {
	var cache remoteCache
	data, err := os.ReadFile(path)
	if err != nil {
		return cache
	}
	_ = sonic.Unmarshal(data, &cache)
	return cache
}

func saveRemoteCache(path string, cache remoteCache) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := sonic.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func openRemoteRepo(path string) (*git.Repository, bool, error) {
	repo, err := git.PlainOpen(path)
	if err == nil {
		return repo, true, nil
	}
	if errors.Is(err, git.ErrRepositoryNotExists) || errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return nil, false, err
}

func initRemoteRepo(path, remote string) (*git.Repository, error) {
	repo, err := git.PlainInit(path, true)
	if err != nil {
		return nil, err
	}
	_, err = repo.CreateRemote(&gitconfig.RemoteConfig{
		Name:  "origin",
		URLs:  []string{remote},
		Fetch: remoteFetchRefSpecs(),
	})
	if err != nil {
		return nil, err
	}
	return repo, nil
}

func fetchRemote(ctx context.Context, repo *git.Repository, remoteURL string, shallow bool) error {
	depth := 0
	if shallow {
		depth = 1
	}
	err := repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin",
		RemoteURL:  remoteURL,
		RefSpecs:   remoteFetchRefSpecs(),
		Depth:      depth,
		Tags:       plumbing.NoTags,
		Force:      true,
		Prune:      true,
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}

func remoteFetchRefSpecs() []gitconfig.RefSpec {
	return []gitconfig.RefSpec{
		gitconfig.RefSpec("+HEAD:refs/remotes/origin/HEAD"),
		gitconfig.RefSpec(fmt.Sprintf(gitconfig.DefaultFetchRefSpec, "origin")),
		gitconfig.RefSpec("+refs/tags/*:refs/tags/*"),
	}
}

func resolveRemoteRef(repo *gitctx.Repository, rev string) (string, error) {
	candidates := []string{rev}
	if rev == "HEAD" {
		candidates = append(candidates, "origin/HEAD", "refs/remotes/origin/HEAD")
	} else if !strings.Contains(rev, "/") && !plumbing.IsHash(rev) {
		candidates = append(candidates, "origin/"+rev, "refs/remotes/origin/"+rev)
	}
	var lastErr error
	for _, candidate := range candidates {
		resolved, err := repo.ResolveRef(candidate)
		if err == nil {
			return resolved, nil
		}
		lastErr = err
	}
	return "", lastErr
}
