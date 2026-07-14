package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bytedance/sonic"
	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/giturl"
	"github.com/yusing/git-agent/internal/metadata"
)

const remoteRefreshTTL = 15 * time.Minute

const maxRemoteProgressDetailBytes = 4 << 10

type remoteProgressWriter struct {
	mu          sync.Mutex
	pending     strings.Builder
	progressLog func(Progress) error
	rawRemote   string
	remote      string
	status      string
}

type remoteProgressOutput interface {
	io.Writer
	Flush() error
}

func newRemoteProgressWriter(progressLog func(Progress) error, rawRemote, status string) remoteProgressOutput {
	if progressLog == nil {
		return nil
	}
	return &remoteProgressWriter{
		progressLog: progressLog,
		rawRemote:   rawRemote,
		remote:      giturl.Sanitize(rawRemote),
		status:      status,
	}
}

func (w *remoteProgressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for i, b := range p {
		if b == '\r' || b == '\n' {
			if err := w.emit(); err != nil {
				return i + 1, err
			}
			continue
		}
		if w.pending.Len() < maxRemoteProgressDetailBytes {
			w.pending.WriteByte(b)
		}
	}
	return len(p), nil
}

func (w *remoteProgressWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.emit()
}

func (w *remoteProgressWriter) emit() error {
	detail := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(w.pending.String(), "�")))
	w.pending.Reset()
	detail = sanitizeRemoteText(detail, w.rawRemote, w.remote)
	if detail == "" {
		return nil
	}
	return w.progressLog(Progress{Status: w.status, Detail: detail})
}

type remoteCache struct {
	URL             string    `json:"url"`
	LastFetchedAt   time.Time `json:"last_fetched_at,omitempty"`
	LastResolvedRev string    `json:"last_resolved_rev,omitempty"`
}

func resolveRemoteIndexSelection(ctx context.Context, remoteURL, rev string, filters Filters, reindex, fetchAllowed bool, progressLog func(Progress) error) (indexSelection, error) {
	remote := giturl.Sanitize(remoteURL)
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
	lockOwned := true
	defer func() {
		if lockOwned {
			_ = lock.Unlock()
		}
	}()

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
	wrapped, err := gitctx.OpenGitDir(repoDir)
	if err != nil {
		return indexSelection{}, err
	}
	sourceRev := strings.TrimSpace(rev)
	if sourceRev == "" {
		sourceRev = "HEAD"
	}
	resolvedRev, resolveErr := resolveRemoteRef(wrapped, sourceRev)
	if resolveErr != nil && fetchAllowed {
		needFetch = true
	}
	if !needFetch {
		if resolveErr != nil {
			return indexSelection{}, fmt.Errorf("resolve --rev %q for remote %s: %w", sourceRev, remote, resolveErr)
		}
		source := Source{Mode: "remote", Remote: remote, Rev: sourceRev, ResolvedRev: resolvedRev, OriginIdentity: giturl.Identity(remoteURL)}
		finish := func(success bool) error {
			if !success || !fetchAllowed {
				return nil
			}
			completionLock, err := lockIndex(ctx, filepath.Join(metadataDir, "remote"))
			if err != nil {
				return err
			}
			cache.URL = giturl.Sanitize(remoteURL)
			cache.LastResolvedRev = resolvedRev
			return errors.Join(saveRemoteCache(cachePath, cache), completionLock.Unlock())
		}
		return indexSelection{
			metadataDir:  metadataDir,
			indexDir:     indexDir(metadataDir, source.Mode, "", resolvedRev, filters),
			source:       source,
			resolvedRev:  resolvedRev,
			repo:         wrapped,
			remoteFinish: finish,
		}, nil
	}

	refs, err := listRemoteRefs(ctx, repo, remoteURL)
	if err != nil {
		return indexSelection{}, fmt.Errorf("list remote %s: %s", remote, sanitizeRemoteError(err, remoteURL, remote))
	}
	translatedRev, advertisedRev, err := translateAdvertisedRevision(sourceRev, refs)
	if err != nil {
		return indexSelection{}, fmt.Errorf("resolve --rev %q for remote %s: %w", sourceRev, remote, err)
	}
	resolvedRev = advertisedRev
	if resolvedRev == "" {
		resolvedRev, err = wrapped.ResolveRef(translatedRev)
		if err != nil {
			resolvedRev, err = preflightRemoteRevision(ctx, remoteURL, sourceRev, shallowFetch, progressLog)
			if errors.Is(err, transport.ErrFilterNotSupported) {
				resolvedRev = ""
			} else if err != nil {
				return indexSelection{}, fmt.Errorf("preflight --rev %q for remote %s: %s", sourceRev, remote, sanitizeRemoteError(err, remoteURL, remote))
			}
		}
	}

	ready, overlayRepo, err := startRemoteReadyFiles(ctx, repo, remoteURL, shallowFetch, filters.Scope, progressLog, lock, cachePath, cache)
	if err != nil {
		return indexSelection{}, fmt.Errorf("start remote stream: %w", err)
	}
	lockOwned = false
	if resolvedRev == "" {
		resolvedRev, err = ready.resolve(translatedRev)
		if err != nil {
			ready.Cancel(err)
			_ = ready.Finish(false, "")
			return indexSelection{}, fmt.Errorf("resolve --rev %q for remote %s: %s", sourceRev, remote, sanitizeRemoteError(err, remoteURL, remote))
		}
	}
	if err := ready.target(resolvedRev); err != nil {
		ready.Cancel(err)
		_ = ready.Finish(false, "")
		return indexSelection{}, err
	}
	source := Source{Mode: "remote", Remote: remote, Rev: sourceRev, ResolvedRev: resolvedRev, OriginIdentity: giturl.Identity(remoteURL)}
	return indexSelection{
		metadataDir: metadataDir,
		indexDir:    indexDir(metadataDir, source.Mode, "", resolvedRev, filters),
		source:      source,
		resolvedRev: resolvedRev,
		repo:        overlayRepo,
		remoteFiles: ready,
		remoteFinish: func(success bool) error {
			if !success {
				ready.Cancel(context.Canceled)
			}
			return ready.Finish(success, resolvedRev)
		},
	}, nil
}

func listRemoteRefs(ctx context.Context, repo *git.Repository, remoteURL string) ([]*plumbing.Reference, error) {
	remote := git.NewRemote(repo.Storer, &gitconfig.RemoteConfig{Name: "origin", URLs: []string{remoteURL}})
	return remote.ListContext(ctx, &git.ListOptions{ClientOptions: remoteClientOptions(), PeelingOption: git.AppendPeeled})
}

func translateAdvertisedRevision(rev string, refs []*plumbing.Reference) (translated, resolved string, err error) {
	base := rev
	suffix := ""
	if index := strings.IndexAny(rev, "~^"); index >= 0 {
		base, suffix = rev[:index], rev[index:]
	}
	byName := make(map[string]*plumbing.Reference, len(refs))
	for _, ref := range refs {
		byName[ref.Name().String()] = ref
	}
	if base == "HEAD" {
		if hash, ok := advertisedRefHash(byName, plumbing.HEAD.String()); ok {
			base = hash.String()
		} else {
			return "", "", errors.New("remote did not advertise HEAD")
		}
	} else if !plumbing.IsHash(base) {
		candidates := []string{base, "refs/heads/" + base, "refs/tags/" + base}
		if branch, ok := strings.CutPrefix(base, "origin/"); ok {
			candidates = append(candidates, "refs/heads/"+branch)
		}
		if branch, ok := strings.CutPrefix(base, "refs/remotes/origin/"); ok {
			candidates = append(candidates, "refs/heads/"+branch)
		}
		var hash plumbing.Hash
		var ok bool
		for _, candidate := range candidates {
			if peeled, found := advertisedRefHash(byName, candidate+"^{}"); found {
				hash, ok = peeled, true
				break
			}
			if found, exists := advertisedRefHash(byName, candidate); exists {
				hash, ok = found, true
				break
			}
		}
		if !ok {
			return "", "", fmt.Errorf("revision base %q was not advertised", base)
		}
		base = hash.String()
	}
	translated = base + suffix
	if suffix == "" && plumbing.IsHash(base) {
		resolved = base
	}
	return translated, resolved, nil
}

func advertisedRefHash(refs map[string]*plumbing.Reference, name string) (plumbing.Hash, bool) {
	for range len(refs) + 1 {
		ref, ok := refs[name]
		if !ok {
			return plumbing.ZeroHash, false
		}
		if ref.Type() != plumbing.SymbolicReference {
			return ref.Hash(), ref.Hash() != plumbing.ZeroHash
		}
		name = ref.Target().String()
	}
	return plumbing.ZeroHash, false
}

func preflightRemoteRevision(ctx context.Context, remoteURL, rev string, shallow bool, progressLog func(Progress) error) (resolved string, err error) {
	tempDir, err := os.MkdirTemp("", "git-agent-preflight-*")
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tempDir)) }()
	repo, err := git.PlainInit(tempDir, true)
	if err != nil {
		return "", err
	}
	defer repo.Close()
	_, err = repo.CreateRemote(&gitconfig.RemoteConfig{Name: "origin", URLs: []string{remoteURL}, Fetch: remoteFetchRefSpecs()})
	if err != nil {
		return "", err
	}
	depth := 0
	if shallow {
		depth = 1
	}
	progress := newRemoteProgressWriter(progressLog, remoteURL, ProgressStatusFetching)
	err = repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: "origin", RemoteURL: remoteURL, ClientOptions: remoteClientOptions(), RefSpecs: remoteFetchRefSpecs(),
		Depth: depth, Tags: plumbing.NoTags, Force: true, Prune: true, Progress: progress, Filter: packp.FilterBlobNone(),
	})
	if progress != nil {
		err = errors.Join(err, progress.Flush())
	}
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", err
	}
	wrapper, err := gitctx.OpenGitDir(tempDir)
	if err != nil {
		return "", err
	}
	return resolveRemoteRef(wrapper, rev)
}

func sanitizeRemoteError(err error, raw, sanitized string) string {
	if err == nil {
		return ""
	}
	return sanitizeRemoteText(err.Error(), raw, sanitized)
}

func sanitizeRemoteText(message, raw, sanitized string) string {
	if raw != "" && raw != sanitized {
		message = strings.ReplaceAll(message, raw, sanitized)
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return message
	}
	redact := func(secret string) {
		if secret == "" {
			return
		}
		for _, value := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
			message = strings.ReplaceAll(message, value, "[REDACTED]")
		}
	}
	if parsed.User != nil {
		redact(parsed.User.Username())
		password, _ := parsed.User.Password()
		redact(password)
	}
	for _, values := range parsed.Query() {
		for _, value := range values {
			redact(value)
		}
	}
	redact(parsed.Fragment)
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

func fetchRemote(ctx context.Context, repo *git.Repository, remoteURL string, shallow bool, progressLog func(Progress) error) error {
	depth := 0
	if shallow {
		depth = 1
	}
	progress := newRemoteProgressWriter(progressLog, remoteURL, ProgressStatusFetching)
	err := repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName:    "origin",
		RemoteURL:     remoteURL,
		ClientOptions: remoteClientOptions(),
		RefSpecs:      remoteFetchRefSpecs(),
		Depth:         depth,
		Tags:          plumbing.NoTags,
		Force:         true,
		Prune:         true,
		Progress:      progress,
	})
	var progressErr error
	if progress != nil {
		progressErr = progress.Flush()
	}
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}
	return progressErr
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
