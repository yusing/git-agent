package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/protocol/packp"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/go-git/go-git/v6/storage"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/giturl"
	"golang.org/x/sync/errgroup"
)

const remoteFileBuffer = 16

var errRemoteFetchDone = errors.New("remote fetch completed")

// remoteReadyFiles is the private handoff between remote Git and indexing.
// Files contains only target-tree candidates; Git-specific fetch state stays
// behind Wait and Finish.
type remoteReadyFiles struct {
	Files <-chan fileContent
	ctx   context.Context

	cancel      context.CancelCauseFunc
	resolve     func(string) (string, error)
	target      func(string) error
	filesResult func() (remoteReadyResult, error)
	wait        func() (remoteReadyResult, error)
	finish      func(bool, string) error
}

type remoteReadyResult struct {
	Skipped      SkippedCounts
	SkippedFiles []SkippedFile
}

func discoverCachedRemoteFiles(repo *gitctx.Repository, rev string, scope []string, visit func(fileContent) error) (SkippedCounts, []SkippedFile, error) {
	commit, err := repo.ResolveCommit(rev)
	if err != nil {
		return SkippedCounts{}, nil, fmt.Errorf("resolve cached commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return SkippedCounts{}, nil, fmt.Errorf("load cached tree: %w", err)
	}
	entries, err := collectRemoteTreeEntries(repo.Repo.Storer, tree)
	if err != nil {
		return SkippedCounts{}, nil, fmt.Errorf("walk cached tree: %w", err)
	}
	var ignoreFiles []revisionIgnoreFile
	for _, entry := range entries {
		if !searchIgnoreFileNames[filepath.Base(entry.path)] {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(entry.path))
		if dir == "." {
			dir = ""
		}
		if !scopeMayContainDir(dir, scope) || shouldSkipPath(dir, scope) {
			continue
		}
		text, _, readErr := readRemoteBlob(repo.Repo.Storer, entry.hash)
		if readErr != nil {
			return SkippedCounts{}, nil, fmt.Errorf("read cached ignore file %s: %w", entry.path, readErr)
		}
		ignoreFiles = append(ignoreFiles, revisionIgnoreFile{path: entry.path, dir: dir, name: filepath.Base(entry.path), text: text})
	}
	matcher := buildRevisionIgnoreMatcher(ignoreFiles)
	var skipped SkippedCounts
	var skippedFiles []SkippedFile
	for _, entry := range entries {
		path := filepath.ToSlash(entry.path)
		if entry.mode == filemode.Submodule || !pathInScope(path, scope) {
			continue
		}
		if shouldSkipPath(path, scope) {
			skipped.Dirs++
			skippedFiles = append(skippedFiles, SkippedFile{Path: path, Reason: "dot_path"})
			continue
		}
		if revisionPathIgnored(matcher, path) {
			continue
		}
		text, size, readErr := readRemoteBlob(repo.Repo.Storer, entry.hash)
		reason := ""
		switch {
		case errors.Is(readErr, fs.ErrNotExist), errors.Is(readErr, plumbing.ErrObjectNotFound):
			reason = "oversized"
		case readErr != nil:
			return skipped, skippedFiles, fmt.Errorf("read cached file %s: %w", path, readErr)
		case isBinary([]byte(text)):
			reason = "binary"
		case !isIndexableText(path, []byte(text)):
			reason = "non_text"
		}
		if reason != "" {
			switch reason {
			case "oversized":
				skipped.Oversized++
			case "binary":
				skipped.Binary++
			case "non_text":
				skipped.NonText++
			}
			skippedFiles = append(skippedFiles, SkippedFile{Path: path, Reason: reason})
			continue
		}
		if err := visit(fileContent{path: path, blob: entry.hash.String(), source: "revision", text: text, size: size}); err != nil {
			return skipped, skippedFiles, err
		}
	}
	return skipped, skippedFiles, nil
}

func (ready *remoteReadyFiles) Cancel(err error) {
	if ready != nil {
		ready.cancel(err)
	}
}

func (ready *remoteReadyFiles) Wait() (remoteReadyResult, error) {
	if ready == nil {
		return remoteReadyResult{}, nil
	}
	return ready.wait()
}

func (ready *remoteReadyFiles) Finish(success bool, resolvedRev string) error {
	if ready == nil {
		return nil
	}
	return ready.finish(success, resolvedRev)
}

type fallbackObjectStorage struct {
	storer.EncodedObjectStorer
	fallback storer.EncodedObjectStorer
	notify   chan struct{}
}

func (s *fallbackObjectStorage) RawObjectWriter(typ plumbing.ObjectType, size int64) (io.WriteCloser, error) {
	w, err := s.EncodedObjectStorer.RawObjectWriter(typ, size)
	if err != nil {
		return nil, err
	}
	return &notifyWriteCloser{WriteCloser: w, notify: s.signal}, nil
}

func (s *fallbackObjectStorage) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	hash, err := s.EncodedObjectStorer.SetEncodedObject(obj)
	if err == nil {
		s.signal()
	}
	return hash, err
}

func (s *fallbackObjectStorage) EncodedObject(typ plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	obj, err := s.EncodedObjectStorer.EncodedObject(typ, hash)
	if err == nil || !errors.Is(err, plumbing.ErrObjectNotFound) {
		return obj, err
	}
	return s.fallback.EncodedObject(typ, hash)
}

func (s *fallbackObjectStorage) HasEncodedObject(hash plumbing.Hash) error {
	err := s.EncodedObjectStorer.HasEncodedObject(hash)
	if err == nil || !errors.Is(err, plumbing.ErrObjectNotFound) {
		return err
	}
	return s.fallback.HasEncodedObject(hash)
}

func (s *fallbackObjectStorage) EncodedObjectSize(hash plumbing.Hash) (int64, error) {
	size, err := s.EncodedObjectStorer.EncodedObjectSize(hash)
	if err == nil || !errors.Is(err, plumbing.ErrObjectNotFound) {
		return size, err
	}
	return s.fallback.EncodedObjectSize(hash)
}

func (s *fallbackObjectStorage) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

type notifyWriteCloser struct {
	io.WriteCloser
	notify func()
	once   sync.Once
}

func (w *notifyWriteCloser) Close() error {
	err := w.WriteCloser.Close()
	w.once.Do(w.notify)
	return err
}

type overlayStorage struct {
	storage.Storer
	objects *fallbackObjectStorage
}

func (s *overlayStorage) RawObjectWriter(typ plumbing.ObjectType, size int64) (io.WriteCloser, error) {
	return s.objects.RawObjectWriter(typ, size)
}

func (s *overlayStorage) NewEncodedObject() plumbing.EncodedObject {
	return s.objects.NewEncodedObject()
}

func (s *overlayStorage) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	return s.objects.SetEncodedObject(obj)
}

func (s *overlayStorage) EncodedObject(typ plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	return s.objects.EncodedObject(typ, hash)
}

func (s *overlayStorage) IterEncodedObjects(typ plumbing.ObjectType) (storer.EncodedObjectIter, error) {
	return s.objects.IterEncodedObjects(typ)
}

func (s *overlayStorage) HasEncodedObject(hash plumbing.Hash) error {
	return s.objects.HasEncodedObject(hash)
}

func (s *overlayStorage) EncodedObjectSize(hash plumbing.Hash) (int64, error) {
	return s.objects.EncodedObjectSize(hash)
}

func (s *overlayStorage) AddAlternate(remote string) error {
	return s.objects.AddAlternate(remote)
}

type streamingFetchStorage struct {
	storage.Storer
	pipe *io.PipeWriter
}

func (s *streamingFetchStorage) PackfileWriter() (io.WriteCloser, error) {
	packWriter, ok := s.Storer.(storer.PackfileWriter)
	if !ok {
		return nil, errors.New("remote cache does not support pack files")
	}
	cache, err := packWriter.PackfileWriter()
	if err != nil {
		return nil, err
	}
	return &teePackWriter{cache: cache, parser: s.pipe}, nil
}

type teePackWriter struct {
	cache  io.WriteCloser
	parser *io.PipeWriter
}

func (w *teePackWriter) Write(p []byte) (int, error) {
	n, err := w.cache.Write(p)
	if err != nil {
		_ = w.parser.CloseWithError(err)
		return n, err
	}
	if n != len(p) {
		err = io.ErrShortWrite
		_ = w.parser.CloseWithError(err)
		return n, err
	}
	if _, err := w.parser.Write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *teePackWriter) Close() error {
	cacheErr := w.cache.Close()
	if cacheErr != nil {
		return errors.Join(cacheErr, w.parser.CloseWithError(cacheErr))
	}
	return w.parser.Close()
}

type remoteTreeEntry struct {
	path string
	hash plumbing.Hash
	mode filemode.FileMode
}

type remoteStreamState struct {
	ctx         context.Context
	repo        *git.Repository
	overlayRepo *gitctx.Repository
	overlay     *overlayStorage
	notify      <-chan struct{}
	fetchDone   <-chan struct{}
	files       chan fileContent
	target      chan string
	resultMu    sync.Mutex
	result      remoteReadyResult
	progressLog func(Progress) error
	remoteURL   string
	shallow     bool
	filter      packp.Filter
}

func startRemoteReadyFiles(ctx context.Context, repo *git.Repository, remoteURL string, shallow bool, scope []string, progressLog func(Progress) error, lock *indexLock, cachePath string, cache remoteCache) (*remoteReadyFiles, *gitctx.Repository, error) {
	streamCtx, cancel := context.WithCancelCause(ctx)
	tempDir, err := os.MkdirTemp("", "git-agent-remote-*")
	if err != nil {
		cancel(err)
		return nil, nil, err
	}
	tempRepo, err := git.PlainInit(tempDir, true)
	if err != nil {
		cancel(err)
		_ = os.RemoveAll(tempDir)
		return nil, nil, fmt.Errorf("initialize overlay: %w", err)
	}
	notify := make(chan struct{}, 1)
	objects := &fallbackObjectStorage{
		EncodedObjectStorer: tempRepo.Storer,
		fallback:            repo.Storer,
		notify:              notify,
	}
	overlay := &overlayStorage{Storer: tempRepo.Storer, objects: objects}
	combinedRepo, err := git.Open(overlay, nil)
	if err != nil {
		cancel(err)
		_ = tempRepo.Close()
		_ = os.RemoveAll(tempDir)
		return nil, nil, fmt.Errorf("open overlay: %w", err)
	}
	wrapper := &gitctx.Repository{RootPath: tempDir, Repo: combinedRepo, IsDetached: true}
	files := make(chan fileContent, remoteFileBuffer)
	target := make(chan string, 1)
	fetchDone := make(chan struct{})
	state := &remoteStreamState{
		ctx:         streamCtx,
		repo:        repo,
		overlayRepo: wrapper,
		overlay:     overlay,
		notify:      notify,
		fetchDone:   fetchDone,
		files:       files,
		target:      target,
		progressLog: progressLog,
		remoteURL:   remoteURL,
		shallow:     shallow,
		filter:      packp.FilterBlobLimit(MaxFileBytes+1, packp.BlobLimitPrefixNone),
	}

	group, groupCtx := errgroup.WithContext(streamCtx)
	state.ctx = groupCtx
	producerDone := make(chan struct{})
	var producerErr error
	group.Go(func() error {
		defer close(fetchDone)
		err := state.fetch()
		if err != nil {
			cancel(err)
			return fmt.Errorf("fetch pack: %w", err)
		}
		return nil
	})
	group.Go(func() error {
		defer close(producerDone)
		defer close(files)
		err := state.produce(scope)
		if err != nil {
			cancel(err)
			producerErr = fmt.Errorf("produce target files: %w", err)
			return producerErr
		}
		return nil
	})

	var waitOnce sync.Once
	var waitResult remoteReadyResult
	var waitErr error
	wait := func() (remoteReadyResult, error) {
		waitOnce.Do(func() {
			waitErr = group.Wait()
			state.resultMu.Lock()
			waitResult = state.result
			state.resultMu.Unlock()
		})
		return waitResult, waitErr
	}
	var finishOnce sync.Once
	var finishErr error
	finish := func(success bool, resolvedRev string) error {
		finishOnce.Do(func() {
			_, streamErr := wait()
			if success && streamErr == nil {
				cache.URL = giturl.Sanitize(remoteURL)
				cache.LastFetchedAt = time.Now().UTC()
				cache.LastResolvedRev = resolvedRev
				finishErr = saveRemoteCache(cachePath, cache)
			}
			cancel(context.Canceled)
			finishErr = errors.Join(finishErr, combinedRepo.Close(), tempRepo.Close(), os.RemoveAll(tempDir), lock.Unlock())
		})
		return finishErr
	}
	resolve := func(rev string) (string, error) {
		finalAttempt := false
		for {
			var resolved string
			var err error
			if finalAttempt {
				var hash *plumbing.Hash
				hash, err = repo.ResolveRevision(plumbing.Revision(rev))
				if err == nil {
					resolved = hash.String()
				}
			} else {
				resolved, err = wrapper.ResolveRef(rev)
			}
			if err == nil {
				return resolved, nil
			}
			if finalAttempt {
				return "", err
			}
			if !errors.Is(err, plumbing.ErrObjectNotFound) && !errors.Is(err, plumbing.ErrReferenceNotFound) {
				return "", err
			}
			if err := state.waitObjectSignal(); errors.Is(err, errRemoteFetchDone) {
				finalAttempt = true
			} else if err != nil {
				return "", err
			}
		}
	}
	sendTarget := func(rev string) error {
		select {
		case target <- rev:
			return nil
		case <-streamCtx.Done():
			return context.Cause(streamCtx)
		}
	}
	filesResult := func() (remoteReadyResult, error) {
		<-producerDone
		state.resultMu.Lock()
		defer state.resultMu.Unlock()
		return state.result, producerErr
	}
	return &remoteReadyFiles{Files: files, ctx: streamCtx, cancel: cancel, resolve: resolve, target: sendTarget, filesResult: filesResult, wait: wait, finish: finish}, wrapper, nil
}

func (s *remoteStreamState) fetch() error {
	if s.progressLog != nil {
		if err := s.progressLog(Progress{Status: ProgressStatusFetching}); err != nil {
			return err
		}
	}
	err := s.fetchAttempt(s.filter)
	if errors.Is(err, transport.ErrFilterNotSupported) {
		err = s.fetchAttempt("")
	}
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}
	return nil
}

func (s *remoteStreamState) fetchAttempt(filter packp.Filter) error {
	reader, writer := io.Pipe()
	parseDone := make(chan error, 1)
	go func() {
		_, err := packfile.NewParser(reader, packfile.WithStorage(s.overlay)).Parse()
		_ = reader.CloseWithError(err)
		parseDone <- err
	}()
	remote := git.NewRemote(&streamingFetchStorage{Storer: s.repo.Storer, pipe: writer}, &gitconfig.RemoteConfig{
		Name:  "origin",
		URLs:  []string{s.remoteURL},
		Fetch: remoteFetchRefSpecs(),
	})
	depth := 0
	if s.shallow {
		depth = 1
	}
	progress := newRemoteProgressWriter(s.progressLog, s.remoteURL, ProgressStatusFetching)
	fetchErr := remote.FetchContext(s.ctx, &git.FetchOptions{
		RemoteName:    "origin",
		RemoteURL:     s.remoteURL,
		ClientOptions: remoteClientOptions(),
		RefSpecs:      remoteFetchRefSpecs(),
		Depth:         depth,
		Tags:          plumbing.NoTags,
		Force:         true,
		Prune:         true,
		Progress:      progress,
		Filter:        filter,
	})
	_ = writer.CloseWithError(fetchErr)
	parseErr := <-parseDone
	if progress != nil {
		fetchErr = errors.Join(fetchErr, progress.Flush())
	}
	if errors.Is(fetchErr, transport.ErrFilterNotSupported) {
		return fetchErr
	}
	if fetchErr != nil {
		return fetchErr
	}
	if parseErr != nil && !errors.Is(parseErr, packfile.ErrEmptyPackfile) {
		return fmt.Errorf("parse remote pack: %w", parseErr)
	}
	return nil
}

func (s *remoteStreamState) produce(scope []string) error {
	var rev string
	select {
	case rev = <-s.target:
	case <-s.ctx.Done():
		return context.Cause(s.ctx)
	}
	entries, err := s.waitTreeEntries(rev)
	if err != nil {
		return err
	}
	matcher, err := s.waitRemoteIgnoreMatcher(entries, scope)
	if err != nil {
		return err
	}
	var result remoteReadyResult
	for _, entry := range entries {
		path := filepath.ToSlash(entry.path)
		if entry.mode == filemode.Submodule || !pathInScope(path, scope) {
			continue
		}
		if shouldSkipPath(path, scope) {
			result.Skipped.Dirs++
			result.SkippedFiles = append(result.SkippedFiles, SkippedFile{Path: path, Reason: "dot_path"})
			continue
		}
		if revisionPathIgnored(matcher, path) {
			continue
		}
		file, reason, err := s.waitRemoteFile(entry)
		if err != nil {
			return err
		}
		if reason != "" {
			switch reason {
			case "oversized":
				result.Skipped.Oversized++
			case "binary":
				result.Skipped.Binary++
			case "non_text":
				result.Skipped.NonText++
			}
			result.SkippedFiles = append(result.SkippedFiles, SkippedFile{Path: path, Reason: reason})
			continue
		}
		select {
		case s.files <- file:
		case <-s.ctx.Done():
			return context.Cause(s.ctx)
		}
	}
	s.resultMu.Lock()
	s.result = result
	s.resultMu.Unlock()
	return nil
}

func (s *remoteStreamState) waitTreeEntries(rev string) ([]remoteTreeEntry, error) {
	finalAttempt := false
	for {
		var commit *object.Commit
		var err error
		if finalAttempt {
			commit, err = s.repo.CommitObject(plumbing.NewHash(rev))
		} else {
			commit, err = s.overlayRepo.ResolveCommit(rev)
		}
		if err == nil {
			tree, treeErr := commit.Tree()
			if treeErr == nil {
				entries, walkErr := collectRemoteTreeEntries(s.overlay, tree)
				if walkErr == nil {
					return entries, nil
				}
				err = walkErr
			} else {
				err = fmt.Errorf("load target tree: %w", treeErr)
			}
		}
		if finalAttempt {
			hash := plumbing.NewHash(rev)
			return nil, fmt.Errorf("target commit %s unavailable (overlay: %v, cache: %v): %w", rev, s.overlay.objects.EncodedObjectStorer.HasEncodedObject(hash), s.repo.Storer.HasEncodedObject(hash), err)
		}
		if !errors.Is(err, plumbing.ErrObjectNotFound) && !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil, err
		}
		if err := s.waitObjectSignal(); errors.Is(err, errRemoteFetchDone) {
			finalAttempt = true
		} else if err != nil {
			return nil, err
		}
	}
}

func collectRemoteTreeEntries(objects storer.EncodedObjectStorer, tree *object.Tree) ([]remoteTreeEntry, error) {
	var entries []remoteTreeEntry
	var walk func(string, *object.Tree) error
	walk = func(prefix string, current *object.Tree) error {
		for _, entry := range current.Entries {
			name := entry.Name
			if prefix != "" {
				name = prefix + "/" + name
			}
			if entry.Mode == filemode.Dir {
				child, err := object.GetTree(objects, entry.Hash)
				if err != nil {
					return fmt.Errorf("load target subtree %s: %w", name, err)
				}
				if err := walk(name, child); err != nil {
					return err
				}
				continue
			}
			entries = append(entries, remoteTreeEntry{path: filepath.ToSlash(name), hash: entry.Hash, mode: entry.Mode})
		}
		return nil
	}
	if err := walk("", tree); err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b remoteTreeEntry) int { return strings.Compare(a.path, b.path) })
	return entries, nil
}

func (s *remoteStreamState) waitRemoteIgnoreMatcher(entries []remoteTreeEntry, scope []string) (gitignoreMatcher, error) {
	var ignoreFiles []revisionIgnoreFile
	for _, entry := range entries {
		if !searchIgnoreFileNames[filepath.Base(entry.path)] {
			continue
		}
		dir := filepath.ToSlash(filepath.Dir(entry.path))
		if dir == "." {
			dir = ""
		}
		if !scopeMayContainDir(dir, scope) || shouldSkipPath(dir, scope) {
			continue
		}
		text, _, err := s.waitBlob(entry.hash)
		if err != nil {
			return nil, err
		}
		ignoreFiles = append(ignoreFiles, revisionIgnoreFile{path: entry.path, dir: dir, name: filepath.Base(entry.path), text: text})
	}
	return buildRevisionIgnoreMatcher(ignoreFiles), nil
}

func (s *remoteStreamState) waitRemoteFile(entry remoteTreeEntry) (fileContent, string, error) {
	text, size, err := s.waitBlob(entry.hash)
	if errors.Is(err, fs.ErrNotExist) {
		return fileContent{}, "oversized", nil
	}
	if err != nil {
		return fileContent{}, "", err
	}
	path := filepath.ToSlash(entry.path)
	if size > MaxFileBytes {
		return fileContent{}, "oversized", nil
	}
	if isBinary([]byte(text)) {
		return fileContent{}, "binary", nil
	}
	if !isIndexableText(path, []byte(text)) {
		return fileContent{}, "non_text", nil
	}
	return fileContent{path: path, blob: entry.hash.String(), source: "revision", text: text, size: size}, "", nil
}

func (s *remoteStreamState) waitBlob(hash plumbing.Hash) (string, int64, error) {
	for {
		text, size, err := readRemoteBlob(s.overlay, hash)
		if err == nil {
			return text, size, nil
		}
		if !errors.Is(err, plumbing.ErrObjectNotFound) {
			return "", 0, err
		}
		select {
		case <-s.fetchDone:
			text, size, finalErr := readRemoteBlob(s.repo.Storer, hash)
			if finalErr != nil {
				return "", 0, fs.ErrNotExist
			}
			return text, size, nil
		default:
		}
		if err := s.waitObjectSignal(); err != nil {
			return "", 0, err
		}
	}
}

func readRemoteBlob(objects storer.EncodedObjectStorer, hash plumbing.Hash) (string, int64, error) {
	blob, err := object.GetBlob(objects, hash)
	if err != nil {
		return "", 0, err
	}
	if blob.Size > MaxFileBytes {
		return "", blob.Size, fs.ErrNotExist
	}
	reader, err := blob.Reader()
	if err != nil {
		return "", 0, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, MaxFileBytes+1))
	closeErr := reader.Close()
	if int64(len(data)) > MaxFileBytes {
		return "", blob.Size, errors.Join(fs.ErrNotExist, readErr, closeErr)
	}
	return string(data), blob.Size, errors.Join(readErr, closeErr)
}

func (s *remoteStreamState) waitObjectSignal() error {
	select {
	case <-s.notify:
		return nil
	case <-s.fetchDone:
		select {
		case <-s.notify:
			return nil
		default:
			return errRemoteFetchDone
		}
	case <-s.ctx.Done():
		return context.Cause(s.ctx)
	}
}

// Keep the matcher type local to avoid leaking go-git details through the
// stream interface.
type gitignoreMatcher interface {
	Match(path []string, isDir bool) bool
}
