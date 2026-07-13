package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"github.com/yusing/git-agent/internal/giturl"
	"github.com/yusing/git-agent/internal/metadata"
)

const indexSyncVersion = 1

type syncedIndex struct {
	Version    int            `json:"version"`
	Origin     string         `json:"origin"`
	Revision   string         `json:"revision"`
	Model      string         `json:"model"`
	Dimensions int            `json:"dimensions"`
	Records    []vectorRecord `json:"records"`
}

type indexSyncTarget struct {
	origin      string
	revision    string
	model       string
	dimensions  int
	metadataDir string
	indexDir    string
	root        string
	source      Source
}

type indexSync struct {
	remoteURL   string
	dir         string
	branch      plumbing.ReferenceName
	repo        *git.Repository
	worktree    *git.Worktree
	lock        *indexLock
	progressLog func(Progress) error
}

func prepareIndexSync(ctx context.Context, remoteURL string, target indexSyncTarget) (*indexSync, error) {
	sync, err := openIndexSync(ctx, remoteURL, nil)
	if err != nil {
		return nil, err
	}
	if err := sync.importIndex(ctx, target); err != nil {
		_ = sync.close()
		return nil, err
	}
	return sync, nil
}

func openIndexSync(ctx context.Context, remoteURL string, progressLog func(Progress) error) (result *indexSync, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".git-agent", "index-sync", metadata.IdentitySHA(giturl.Identity(remoteURL)))
	lock, err := lockIndex(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("lock index sync repository: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, lock.Unlock())
		}
	}()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	repo, err := git.PlainOpen(dir)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		repo, err = git.PlainInit(dir, false)
	}
	if err != nil {
		return nil, fmt.Errorf("open index sync repository: %w", err)
	}
	if err := validateSyncTree(dir); err != nil {
		return nil, err
	}
	if err := setSyncRemote(repo, remoteURL); err != nil {
		return nil, err
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}
	sync := &indexSync{
		remoteURL:   remoteURL,
		dir:         dir,
		repo:        repo,
		worktree:    worktree,
		lock:        lock,
		progressLog: progressLog,
	}
	if err := sync.reconcile(ctx); err != nil {
		return nil, err
	}
	return sync, nil
}

func setSyncRemote(repo *git.Repository, remoteURL string) error {
	cfg, err := repo.Config()
	if err != nil {
		return err
	}
	cfg.Remotes["origin"] = &gitconfig.RemoteConfig{Name: "origin", URLs: []string{remoteURL}}
	cfg.Commit.GpgSign = gitconfig.OptBoolFalse
	return repo.SetConfig(cfg)
}

func (sync *indexSync) reconcile(ctx context.Context) error {
	if err := sync.commitPending("Save local index records before sync"); err != nil {
		return err
	}
	remote, err := sync.repo.Remote("origin")
	if err != nil {
		return err
	}
	if err := reportProgress(sync.progressLog, Progress{Status: ProgressStatusFetching}); err != nil {
		return err
	}
	refs, err := remote.ListContext(ctx, &git.ListOptions{ClientOptions: remoteClientOptions()})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		refs, err = nil, nil
	}
	if err != nil {
		return sync.remoteError("reach", err)
	}
	branch, remoteHash, ok := remoteDefaultBranch(refs)
	if !ok {
		sync.branch = plumbing.NewBranchReferenceName("main")
		if err := sync.repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, sync.branch)); err != nil {
			return err
		}
		if err := sync.ensureSchema(); err != nil {
			return err
		}
		if err := sync.commitPending("Initialize git-agent index store"); err != nil {
			return err
		}
		return nil
	}
	sync.branch = branch
	refspec := gitconfig.RefSpec("+" + branch.String() + ":" + remoteTrackingRef(branch).String())
	progress := newRemoteProgressWriter(sync.progressLog, sync.remoteURL, ProgressStatusFetching)
	err = sync.repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName:    "origin",
		ClientOptions: remoteClientOptions(),
		RefSpecs:      []gitconfig.RefSpec{refspec},
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
		return sync.remoteError("fetch", err)
	}
	if progressErr != nil {
		return progressErr
	}
	return sync.rebaseOnto(ctx, remoteHash)
}

func remoteDefaultBranch(refs []*plumbing.Reference) (plumbing.ReferenceName, plumbing.Hash, bool) {
	var head *plumbing.Reference
	branches := map[plumbing.ReferenceName]plumbing.Hash{}
	for _, ref := range refs {
		switch {
		case ref.Name() == plumbing.HEAD:
			head = ref
		case ref.Name().IsBranch() && ref.Type() == plumbing.HashReference:
			branches[ref.Name()] = ref.Hash()
		}
	}
	if head == nil {
		return "", plumbing.ZeroHash, false
	}
	if head.Type() == plumbing.SymbolicReference {
		if hash, ok := branches[head.Target()]; ok {
			return head.Target(), hash, true
		}
	}
	for _, name := range []plumbing.ReferenceName{plumbing.NewBranchReferenceName("main"), plumbing.NewBranchReferenceName("master")} {
		if hash, ok := branches[name]; ok && hash == head.Hash() {
			return name, hash, true
		}
	}
	names := make([]plumbing.ReferenceName, 0, len(branches))
	for name, hash := range branches {
		if hash == head.Hash() {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		return "", plumbing.ZeroHash, false
	}
	return names[0], branches[names[0]], true
}

func remoteTrackingRef(branch plumbing.ReferenceName) plumbing.ReferenceName {
	return plumbing.NewRemoteReferenceName("origin", branch.Short())
}

func (sync *indexSync) rebaseOnto(ctx context.Context, remoteHash plumbing.Hash) error {
	localHead, headErr := sync.repo.Head()
	if errors.Is(headErr, plumbing.ErrReferenceNotFound) {
		return sync.checkoutBranch(remoteHash)
	}
	if headErr != nil {
		return headErr
	}
	if localHead.Hash() == remoteHash {
		return sync.checkoutBranch(remoteHash)
	}
	localCommit, err := sync.repo.CommitObject(localHead.Hash())
	if err != nil {
		return err
	}
	remoteCommit, err := sync.repo.CommitObject(remoteHash)
	if err != nil {
		return err
	}
	localBehind, err := localCommit.IsAncestor(remoteCommit)
	if err != nil {
		return err
	}
	if localBehind {
		return sync.checkoutBranch(remoteHash)
	}
	remoteBehind, err := remoteCommit.IsAncestor(localCommit)
	if err != nil {
		return err
	}
	if remoteBehind {
		if err := sync.checkoutBranch(localHead.Hash()); err != nil {
			return err
		}
		return sync.push(ctx)
	}
	localSnapshots, err := sync.readSnapshots()
	if err != nil {
		return err
	}
	if err := sync.checkoutBranch(remoteHash); err != nil {
		return err
	}
	if err := sync.mergeSnapshots(localSnapshots); err != nil {
		return err
	}
	if err := sync.commitPending("Rebase and merge compatible index records"); err != nil {
		return err
	}
	return sync.push(ctx)
}

func (sync *indexSync) checkoutBranch(hash plumbing.Hash) error {
	if err := sync.repo.Storer.SetReference(plumbing.NewHashReference(sync.branch, hash)); err != nil {
		return err
	}
	if err := sync.repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, sync.branch)); err != nil {
		return err
	}
	if err := sync.worktree.Reset(&git.ResetOptions{Commit: hash, Mode: git.HardReset}); err != nil {
		return err
	}
	return validateSyncTree(sync.dir)
}

func (sync *indexSync) ensureSchema() error {
	path := filepath.Join(sync.dir, "schema.json")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.WriteFile(path, []byte("{\"version\":1}\n"), 0o600)
}

func (sync *indexSync) snapshotPath(target indexSyncTarget) (string, error) {
	if err := validateSyncTarget(target); err != nil {
		return "", err
	}
	modelKey := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", target.model, target.dimensions)))
	return filepath.Join(sync.dir, "indexes", metadata.IdentitySHA(target.origin), target.revision, hex.EncodeToString(modelKey[:])[:16]+".json"), nil
}

func (sync *indexSync) importIndex(ctx context.Context, target indexSyncTarget) error {
	path, err := sync.snapshotPath(target)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var snapshot syncedIndex
	if err := sonic.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("parse synced index: %w", err)
	}
	if err := validateSnapshot(snapshot, target); err != nil {
		return err
	}
	return withIndexLock(ctx, target.indexDir, func() error {
		local, _ := loadVectors(target.indexDir)
		records := mergeCompatibleRecords(local, snapshot.Records, target.model, target.dimensions)
		if len(records) == len(local) {
			return nil
		}
		return saveIndex(ctx, target.metadataDir, target.indexDir, target.source, target.root, target.revision, target.model, target.dimensions, records, nil)
	})
}

func (sync *indexSync) exportAndPush(ctx context.Context, target indexSyncTarget) error {
	if _, err := sync.exportIndex(ctx, target); err != nil {
		return err
	}
	if err := sync.commitPending("Update index " + target.revision[:min(12, len(target.revision))]); err != nil {
		return err
	}
	return sync.pushWithRetry(ctx)
}

func (sync *indexSync) exportIndex(ctx context.Context, target indexSyncTarget) (records int, err error) {
	err = withIndexLock(ctx, target.indexDir, func() error {
		var exportErr error
		records, exportErr = sync.exportIndexLocked(target)
		return exportErr
	})
	return records, err
}

func (sync *indexSync) exportIndexLocked(target indexSyncTarget) (int, error) {
	records, err := loadVectors(target.indexDir)
	if err != nil {
		return 0, fmt.Errorf("load revision index for sync: %w", err)
	}
	compatible, ok := compatibleIndexRecords(records, target.model, target.dimensions)
	if !ok {
		return 0, errors.New("revision index contains incompatible records")
	}
	return sync.writeSnapshot(target, compatible)
}

func (sync *indexSync) writeSnapshot(target indexSyncTarget, compatible []vectorRecord) (int, error) {
	if err := validateSyncTree(sync.dir); err != nil {
		return 0, err
	}
	path, err := sync.snapshotPath(target)
	if err != nil {
		return 0, err
	}
	snapshot := syncedIndex{
		Version:    indexSyncVersion,
		Origin:     target.origin,
		Revision:   target.revision,
		Model:      target.model,
		Dimensions: target.dimensions,
		Records:    compatible,
	}
	if existing, err := os.ReadFile(path); err == nil {
		var remote syncedIndex
		if sonic.Unmarshal(existing, &remote) == nil && validateSnapshot(remote, target) == nil {
			snapshot.Records = mergeCompatibleRecords(remote.Records, snapshot.Records, target.model, target.dimensions)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	data, err := sonic.Marshal(snapshot)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return 0, err
	}
	return len(compatible), nil
}

func compatibleIndexRecords(records []vectorRecord, model string, dimensions int) ([]vectorRecord, bool) {
	byKey := make(map[string]vectorRecord, len(records))
	if !collectCompatibleRecords(byKey, records, model, dimensions, false) {
		return nil, false
	}
	return sortedRecordValues(byKey), true
}

func validateSnapshot(snapshot syncedIndex, target indexSyncTarget) error {
	if snapshot.Version != indexSyncVersion || snapshot.Origin != target.origin || snapshot.Revision != target.revision || snapshot.Model != target.model || snapshot.Dimensions != target.dimensions {
		return errors.New("synced index metadata is incompatible with selected revision")
	}
	return nil
}

func validateSyncTarget(target indexSyncTarget) error {
	if target.origin == "" || target.model == "" || target.dimensions <= 0 || !canonicalObjectID(target.revision) {
		return errors.New("index sync target is invalid")
	}
	return nil
}

func canonicalObjectID(value string) bool {
	return canonicalLowerHex(value, 40)
}

func canonicalLowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func mergeCompatibleRecords(base, incoming []vectorRecord, model string, dims int) []vectorRecord {
	byKey := make(map[string]vectorRecord, len(base)+len(incoming))
	collectCompatibleRecords(byKey, incoming, model, dims, false)
	collectCompatibleRecords(byKey, base, model, dims, true)
	return sortedRecordValues(byKey)
}

func collectCompatibleRecords(byKey map[string]vectorRecord, records []vectorRecord, model string, dims int, replace bool) bool {
	allCompatible := true
	for _, record := range records {
		if record.EmbeddingModel != model || record.Dimensions != dims || len(record.Vector) != dims || record.EmbeddingInputHash == "" {
			allCompatible = false
			continue
		}
		key := cacheRecordKey(record)
		if _, exists := byKey[key]; !exists || replace {
			byKey[key] = record
		}
	}
	return allCompatible
}

func sortedRecordValues(byKey map[string]vectorRecord) []vectorRecord {
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]vectorRecord, 0, len(keys))
	for _, key := range keys {
		result = append(result, byKey[key])
	}
	return result
}

func (sync *indexSync) readSnapshots() (map[string]syncedIndex, error) {
	root := filepath.Join(sync.dir, "indexes")
	result := map[string]syncedIndex{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var snapshot syncedIndex
		if sonic.Unmarshal(data, &snapshot) != nil || snapshot.Version != indexSyncVersion {
			return nil
		}
		rel, err := filepath.Rel(sync.dir, path)
		if err != nil {
			return err
		}
		result[rel] = snapshot
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return result, nil
	}
	return result, err
}

func (sync *indexSync) mergeSnapshots(local map[string]syncedIndex) error {
	for rel, snapshot := range local {
		path := filepath.Join(sync.dir, rel)
		if data, err := os.ReadFile(path); err == nil {
			var remote syncedIndex
			if sonic.Unmarshal(data, &remote) == nil && compatibleSnapshots(remote, snapshot) {
				remote.Records = mergeCompatibleRecords(remote.Records, snapshot.Records, remote.Model, remote.Dimensions)
				snapshot = remote
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		data, err := sonic.Marshal(snapshot)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func compatibleSnapshots(a, b syncedIndex) bool {
	return a.Version == b.Version && a.Origin == b.Origin && a.Revision == b.Revision && a.Model == b.Model && a.Dimensions == b.Dimensions
}

func (sync *indexSync) commitPending(message string) error {
	if err := sync.worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return err
	}
	status, err := sync.worktree.Status()
	if err != nil {
		return err
	}
	if status.IsClean() {
		return nil
	}
	now := time.Now()
	signature := &object.Signature{Name: "git-agent", Email: "git-agent@localhost", When: now}
	_, err = sync.worktree.Commit(message, &git.CommitOptions{Author: signature, Committer: signature})
	return err
}

func (sync *indexSync) pushWithRetry(ctx context.Context) error {
	for attempt := range 3 {
		err := sync.push(ctx)
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "non-fast-forward") || attempt == 2 {
			return err
		}
		if err := sync.reconcile(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (sync *indexSync) push(ctx context.Context) error {
	if err := reportProgress(sync.progressLog, Progress{Status: ProgressStatusPushing}); err != nil {
		return err
	}
	progress := newRemoteProgressWriter(sync.progressLog, sync.remoteURL, ProgressStatusPushing)
	err := sync.repo.PushContext(ctx, &git.PushOptions{
		RemoteName:    "origin",
		ClientOptions: remoteClientOptions(),
		RefSpecs: []gitconfig.RefSpec{
			gitconfig.RefSpec(sync.branch.String() + ":" + sync.branch.String()),
		},
		Progress: progress,
	})
	var progressErr error
	if progress != nil {
		progressErr = progress.Flush()
	}
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return sync.remoteError("push", err)
	}
	return progressErr
}

func (sync *indexSync) remoteError(action string, err error) error {
	sanitized := giturl.Sanitize(sync.remoteURL)
	message := sanitizeRemoteError(err, sync.remoteURL, sanitized)
	return fmt.Errorf("index remote %s failed for %s: %s", action, sanitized, message)
}

func (sync *indexSync) close() error {
	if sync == nil || sync.lock == nil {
		return nil
	}
	lock := sync.lock
	sync.lock = nil
	return lock.Unlock()
}

func validateSyncTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("index sync repository contains symlink %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("index sync repository contains non-regular file %s", path)
		}
		if validSyncTreeEntry(rel, entry.IsDir()) {
			return nil
		}
		return fmt.Errorf("index sync repository contains unsafe path %s", path)
	})
}

func validSyncTreeEntry(rel string, directory bool) bool {
	if rel == "." {
		return directory
	}
	if rel == "schema.json" {
		return !directory
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if parts[0] != "indexes" {
		return false
	}
	if len(parts) == 1 {
		return directory
	}
	if !canonicalLowerHex(parts[1], 64) {
		return false
	}
	if len(parts) == 2 {
		return directory
	}
	if !canonicalObjectID(parts[2]) {
		return false
	}
	if len(parts) == 3 {
		return directory
	}
	return len(parts) == 4 && !directory && strings.HasSuffix(parts[3], ".json") && canonicalLowerHex(strings.TrimSuffix(parts[3], ".json"), 16)
}
