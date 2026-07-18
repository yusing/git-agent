package search

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/giturl"
	"github.com/yusing/git-agent/internal/metadata"
	"github.com/yusing/git-agent/internal/openai"
	gossh "golang.org/x/crypto/ssh"
)

func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "git-agent-search-home-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("HOME", home); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}

func TestIndexSyncSharesOnlyCurrentHEADRecords(t *testing.T) {
	syncRemote := newEmptySyncRemote(t)
	origin := "https://example.test/acme/widget.git"
	firstRoot := t.TempDir()
	writeFile(t, firstRoot, "app.go", "package app\n\nfunc Stable() {}\n")
	head := commitSearchRepo(t, firstRoot)
	setTestOrigin(t, firstRoot, origin)

	firstEmbedder := &countingEmbedder{}
	opts := Options{
		Root:                firstRoot,
		IndexRemote:         syncRemote,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}
	if _, err := Run(t.Context(), firstEmbedder, opts, ""); err != nil {
		t.Fatal(err)
	}
	if firstEmbedder.calls.Load() == 0 {
		t.Fatal("first machine did not build HEAD index")
	}

	sync, err := openIndexSync(t.Context(), syncRemote, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := indexSyncTarget{origin: giturl.Identity(origin), revision: head, model: "test-model", dimensions: 3}
	snapshotPath, err := sync.snapshotPath(target)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := sync.close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, firstRoot, "app.go", "package app\n\nfunc Dirty() {}\n")
	writeFile(t, firstRoot, "secret.txt", "untracked local content\n")
	dirtyEmbedder := &countingEmbedder{}
	if _, err := Run(t.Context(), dirtyEmbedder, opts, ""); err != nil {
		t.Fatal(err)
	}
	if dirtyEmbedder.calls.Load() == 0 {
		t.Fatal("dirty working tree did not re-index locally")
	}
	after, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("dirty working-tree records changed synced HEAD snapshot")
	}

	secondRoot := filepath.Join(t.TempDir(), "clone")
	if _, err := git.PlainClone(secondRoot, &git.CloneOptions{URL: firstRoot}); err != nil {
		t.Fatal(err)
	}
	setTestOrigin(t, secondRoot, origin)
	secondEmbedder := &countingEmbedder{}
	opts.Root = secondRoot
	if _, err := Run(t.Context(), secondEmbedder, opts, ""); err != nil {
		t.Fatal(err)
	}
	if calls := secondEmbedder.calls.Load(); calls != 0 {
		t.Fatalf("second machine embedding calls = %d, want 0", calls)
	}
}

func TestSearchIndexSyncPreservesConfiguredProgressLog(t *testing.T) {
	syncRemote := newEmptySyncRemote(t)
	origin := "https://example.test/acme/widget.git"
	root := t.TempDir()
	writeFile(t, root, "app.go", "package app\n\nfunc Stable() {}\n")
	commitSearchRepo(t, root)
	setTestOrigin(t, root, origin)

	var progress []Progress
	_, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		IndexRemote:         syncRemote,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
		ProgressLog: func(update Progress) error {
			progress = append(progress, update)
			return nil
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	var phases []string
	for _, update := range progress {
		if update.Status != "" && update.Detail == "" {
			phases = append(phases, update.Status)
		}
	}
	if want := []string{ProgressStatusFetching, ProgressStatusPushing}; !slices.Equal(phases, want) {
		t.Fatalf("index sync phases = %#v, want %#v; all progress = %#v", phases, want, progress)
	}
}

func TestRemoteSearchPullsSelectedRevisionFromIndexSync(t *testing.T) {
	sourceRemote := t.TempDir()
	writeFile(t, sourceRemote, "remote.txt", "shared remote revision\n")
	revision := commitSearchRepo(t, sourceRemote)
	syncRemote := newEmptySyncRemote(t)

	firstHome := t.TempDir()
	t.Setenv("HOME", firstHome)
	first := &countingEmbedder{}
	opts := Options{
		Root:                t.TempDir(),
		Remote:              sourceRemote,
		Rev:                 revision,
		IndexRemote:         syncRemote,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}
	if _, err := Run(t.Context(), first, opts, ""); err != nil {
		t.Fatal(err)
	}
	if first.calls.Load() == 0 {
		t.Fatal("first remote search did not embed selected revision")
	}

	t.Setenv("HOME", t.TempDir())
	second := &countingEmbedder{}
	if _, err := Run(t.Context(), second, opts, ""); err != nil {
		t.Fatal(err)
	}
	if calls := second.calls.Load(); calls != 0 {
		t.Fatalf("second remote search embedding calls = %d, want 0", calls)
	}
}

func TestLocalRevisionSearchPullsSelectedRevisionFromIndexSync(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.go", "package app\n\nfunc First() {}\n")
	firstRevision := commitSearchRepo(t, root)
	writeFile(t, root, "app.go", "package app\n\nfunc Second() {}\n")
	commitSearchRepoChange(t, root, "second")
	setTestOrigin(t, root, "https://example.test/acme/local-revision.git")
	syncRemote := newEmptySyncRemote(t)
	opts := Options{
		Root:                root,
		Rev:                 firstRevision,
		IndexRemote:         syncRemote,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}
	t.Setenv("HOME", t.TempDir())
	first := &countingEmbedder{}
	if _, err := Run(t.Context(), first, opts, ""); err != nil {
		t.Fatal(err)
	}
	if first.calls.Load() == 0 {
		t.Fatal("first revision search did not embed")
	}
	t.Setenv("HOME", t.TempDir())
	second := &countingEmbedder{}
	if _, err := Run(t.Context(), second, opts, ""); err != nil {
		t.Fatal(err)
	}
	if calls := second.calls.Load(); calls != 0 {
		t.Fatalf("second revision search embedding calls = %d, want 0", calls)
	}
}

func TestSyncAllPublishesEveryCompletedRevisionOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	writeFile(t, root, "app.go", "package app\n\nfunc First() {}\n")
	firstRevision := commitSearchRepo(t, root)
	writeFile(t, root, "app.go", "package app\n\nfunc Second() {}\n")
	secondRevision := commitSearchRepoChange(t, root, "second")
	origin := "https://example.test/acme/full-sync.git"
	setTestOrigin(t, root, origin)
	opts := Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}
	for _, revision := range []string{firstRevision, secondRevision} {
		opts.Rev = revision
		if _, err := Run(t.Context(), fakeEmbedder{}, opts, ""); err != nil {
			t.Fatal(err)
		}
	}
	opts.Rev = ""
	if _, err := Run(t.Context(), fakeEmbedder{}, opts, ""); err != nil {
		t.Fatal(err)
	}
	remoteSource := t.TempDir()
	writeFile(t, remoteSource, "remote.txt", "cached remote revision\n")
	remoteRevision := commitSearchRepo(t, remoteSource)
	if _, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                t.TempDir(),
		Remote:              remoteSource,
		Rev:                 remoteRevision,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}, ""); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".git-agent"), "corrupt/search/revs/bad/manifest.json", "{\"version\":999}\n")

	syncRemote := newEmptySyncRemote(t)
	var progress []Progress
	summary, err := SyncAll(t.Context(), syncRemote, SyncAllOptions{
		ProgressLog: func(update Progress) error {
			progress = append(progress, update)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Indexes != 3 || summary.Records == 0 || summary.Skipped != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if len(progress) != 7 {
		t.Fatalf("progress = %#v, want 7 updates", progress)
	}
	for i, wantStatus := range []string{
		ProgressStatusFetching,
		ProgressStatusScanning,
		ProgressStatusSyncing,
		ProgressStatusSyncing,
		ProgressStatusSyncing,
		ProgressStatusSyncing,
		ProgressStatusPushing,
	} {
		if progress[i].Status != wantStatus {
			t.Fatalf("progress[%d].Status = %q, want %q", i, progress[i].Status, wantStatus)
		}
	}
	for i, update := range progress[2:6] {
		if update.Done != i || update.Total != 3 {
			t.Fatalf("progress[%d] = %#v, want done=%d total=3", i+2, update, i)
		}
	}
	sync, err := openIndexSync(t.Context(), syncRemote, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sync.close()
	for _, revision := range []string{firstRevision, secondRevision} {
		target := indexSyncTarget{origin: giturl.Identity(origin), revision: revision, model: "test-model", dimensions: 3}
		path, err := sync.snapshotPath(target)
		if err != nil {
			t.Fatalf("resolve synced revision %s: %v", revision, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read synced revision %s: %v", revision, err)
		}
		var snapshot syncedIndex
		if err := sonic.Unmarshal(data, &snapshot); err != nil || len(snapshot.Records) == 0 {
			t.Fatalf("snapshot %s = %#v, %v", revision, snapshot, err)
		}
	}
	remoteTarget := indexSyncTarget{origin: giturl.Identity(remoteSource), revision: remoteRevision, model: "test-model", dimensions: 3}
	remotePath, err := sync.snapshotPath(remoteTarget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(remotePath); err != nil {
		t.Fatalf("remote revision was not synced: %v", err)
	}
}

func newEmptySyncRemote(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "indexes.git")
	repo, err := git.PlainInit(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIndexSyncFailsWhenRemoteIsUnreachable(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.go", "package app\n")
	commitSearchRepo(t, root)
	setTestOrigin(t, root, "https://example.test/acme/unreachable.git")
	_, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		IndexRemote:         filepath.Join(t.TempDir(), "missing.git"),
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}, "")
	if err == nil || !strings.Contains(err.Error(), "index remote reach failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestIndexSyncDisablesCommitSigning(t *testing.T) {
	root := t.TempDir()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Commit.GpgSign = config.OptBoolTrue
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := setSyncRemote(repo, filepath.Join(t.TempDir(), "remote.git")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "schema.json", "{\"version\":1}\n")
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	sync := &indexSync{repo: repo, worktree: worktree}
	if err := sync.commitPending("unsigned index commit"); err != nil {
		t.Fatalf("index commit inherited commit.gpgSign: %v", err)
	}
}

func TestFetchRemoteUsesDefaultSSHKeyWithoutAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SSH_AUTH_SOCK", "")
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := gossh.MarshalPrivateKey(privateKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "known_hosts"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const remoteURL = "ssh://git@127.0.0.1:1/repo.git"
	repo, err := initRemoteRepo(filepath.Join(t.TempDir(), "repo.git"), remoteURL)
	if err != nil {
		t.Fatal(err)
	}
	err = fetchRemote(t.Context(), repo, remoteURL, false, nil)
	if err == nil {
		t.Fatal("SSH fetch unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), "SSH agent requested but SSH_AUTH_SOCK not-specified") {
		t.Fatalf("SSH fetch did not fall back to default key: %v", err)
	}
}

func TestIndexSyncSerializesSharedWorkingTree(t *testing.T) {
	syncRemote := newEmptySyncRemote(t)
	first, err := openIndexSync(t.Context(), syncRemote, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = openIndexSync(ctx, syncRemote, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("second sync error = %v, want canceled lock wait", err)
	}
}

func TestIndexSyncRetryReportsActualOperationPhases(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	remote := newEmptySyncRemote(t)
	var progress []Progress
	sync, err := openIndexSync(t.Context(), remote, func(update Progress) error {
		progress = append(progress, update)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sync.close()
	if err := sync.push(t.Context()); err != nil {
		t.Fatal(err)
	}

	cloneDir := filepath.Join(t.TempDir(), "clone")
	clone, err := git.PlainClone(cloneDir, &git.CloneOptions{URL: remote})
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := clone.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	schemaPath := filepath.Join(cloneDir, "schema.json")
	schema, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(schemaPath, append(schema, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Add("schema.json"); err != nil {
		t.Fatal(err)
	}
	signature := &object.Signature{Name: "Search Test", Email: "search@example.test", When: time.Unix(1, 0)}
	if _, err := worktree.Commit("advance remote", &git.CommitOptions{Author: signature, Committer: signature}); err != nil {
		t.Fatal(err)
	}
	if err := clone.Push(&git.PushOptions{}); err != nil {
		t.Fatal(err)
	}

	progress = nil
	if err := sync.pushWithRetry(t.Context()); err != nil {
		t.Fatal(err)
	}
	var phases []string
	for _, update := range progress {
		if update.Detail == "" {
			phases = append(phases, update.Status)
		}
	}
	want := []string{ProgressStatusPushing, ProgressStatusFetching, ProgressStatusPushing}
	if !slices.Equal(phases, want) {
		t.Fatalf("operation phases = %#v, want %#v", phases, want)
	}
}

func TestValidateSyncTreeRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("safe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "snapshot.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := validateSyncTree(root); err == nil || !strings.Contains(err.Error(), "contains symlink") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestValidateSyncTreeRejectsUnexpectedFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "not index data\n")
	if err := validateSyncTree(root); err == nil || !strings.Contains(err.Error(), "contains unsafe path") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestSnapshotPathRejectsUnsafeRevision(t *testing.T) {
	sync := &indexSync{dir: t.TempDir()}
	_, err := sync.snapshotPath(indexSyncTarget{
		origin:     "https://example.test/acme/widget",
		revision:   "../../.git/config",
		model:      "test-model",
		dimensions: 3,
	})
	if err == nil || !strings.Contains(err.Error(), "target is invalid") {
		t.Fatalf("snapshot path error = %v", err)
	}
}

func TestSyncTargetFromLegacyManifestDoesNotGuessCurrentOrigin(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.go", "package app\n")
	revision := commitSearchRepo(t, root)
	setTestOrigin(t, root, "https://example.test/acme/reused.git")
	_, ok := syncTargetFromManifest(filepath.Join(root, "search", "revs", revision), manifest{
		Mode:           "revision",
		Root:           root,
		ResolvedRev:    revision,
		EmbeddingModel: "test-model",
		Dimensions:     3,
	})
	if ok {
		t.Fatal("legacy manifest without persisted origin identity was accepted")
	}
}

func TestMergeCompatibleRecordsKeepsRemoteAndLocalIdentities(t *testing.T) {
	record := func(input string, vector []float64) vectorRecord {
		return vectorRecord{EmbeddingInputHash: input, EmbeddingModel: "model", Dimensions: 3, Vector: vector}
	}
	remote := []vectorRecord{record("shared", []float64{1, 0, 0}), record("remote", []float64{0, 1, 0})}
	remote[0].Path = "first.go"
	remote = append(remote, remote[0])
	remote[2].Path = "second.go"
	local := []vectorRecord{record("shared", []float64{0.9, 0.1, 0}), record("local", []float64{0, 0, 1}), record("wrong-model", []float64{1, 1, 1})}
	local[0].Path = "first.go"
	local[2].EmbeddingModel = "other"

	merged := mergeCompatibleRecords(remote, local, "model", 3)
	if len(merged) != 4 {
		t.Fatalf("merged records = %#v", merged)
	}
	for _, got := range merged {
		if got.EmbeddingInputHash == "shared" && got.Vector[0] != 1 {
			t.Fatalf("shared record did not preserve remote value: %#v", got.Vector)
		}
	}
}

func setTestOrigin(t *testing.T, root, remoteURL string) {
	t.Helper()
	repo, err := git.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Remotes["origin"] = &config.RemoteConfig{Name: "origin", URLs: []string{remoteURL}}
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

type fakeEmbedder struct{}

func (fakeEmbedder) CreateEmbeddings(_ context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	vectors := make([][]float64, len(request.Inputs))
	for i, input := range request.Inputs {
		vectors[i] = vectorFor(input)
	}
	return openai.EmbeddingResponse{Model: request.Model, Vectors: vectors, Dimensions: 3}, nil
}

type countingEmbedder struct {
	calls    atomic.Int64
	maxBatch atomic.Int64
}

type splittingEmbedder struct {
	calls atomic.Int64
}

func (e *splittingEmbedder) CreateEmbeddings(_ context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	e.calls.Add(1)
	if len(request.Inputs) > 1 {
		return openai.EmbeddingResponse{}, errors.New("batch too large")
	}
	return fakeEmbedder{}.CreateEmbeddings(context.Background(), request)
}

func (e *countingEmbedder) CreateEmbeddings(_ context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	e.calls.Add(1)
	for {
		current := e.maxBatch.Load()
		if int64(len(request.Inputs)) <= current || e.maxBatch.CompareAndSwap(current, int64(len(request.Inputs))) {
			break
		}
	}
	vectors := make([][]float64, len(request.Inputs))
	for i, input := range request.Inputs {
		vectors[i] = vectorFor(input)
	}
	return openai.EmbeddingResponse{Model: request.Model, Vectors: vectors, Dimensions: 3}, nil
}

type failOnPathEmbedder string

func (e failOnPathEmbedder) CreateEmbeddings(ctx context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	for _, input := range request.Inputs {
		if strings.Contains(input, string(e)) {
			return openai.EmbeddingResponse{}, errors.New("boom")
		}
	}
	return fakeEmbedder{}.CreateEmbeddings(ctx, request)
}

type embeddingRule struct {
	contains string
	vector   []float64
}

type ruleEmbedder []embeddingRule

func (e ruleEmbedder) CreateEmbeddings(_ context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	vectors := make([][]float64, len(request.Inputs))
	for i, input := range request.Inputs {
		vectors[i] = []float64{0, 0, 1}
		for _, rule := range e {
			if strings.Contains(input, rule.contains) {
				vectors[i] = rule.vector
				break
			}
		}
	}
	return openai.EmbeddingResponse{Model: request.Model, Vectors: vectors, Dimensions: 3}, nil
}

type recordingEmbedder struct {
	fakeEmbedder
	inputs []string
}

func (e *recordingEmbedder) CreateEmbeddings(ctx context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	for _, input := range request.Inputs {
		e.inputs = append(e.inputs, strings.Clone(input))
	}
	return e.fakeEmbedder.CreateEmbeddings(ctx, request)
}

type blockingEmbedder struct {
	calls       atomic.Int64
	entered     chan struct{}
	secondCall  chan struct{}
	secondSaved atomic.Bool
	release     chan struct{}
	released    atomic.Bool
}

func newBlockingEmbedder() *blockingEmbedder {
	return &blockingEmbedder{
		entered:    make(chan struct{}),
		secondCall: make(chan struct{}),
		release:    make(chan struct{}),
	}
}

func (e *blockingEmbedder) CreateEmbeddings(ctx context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	switch e.calls.Add(1) {
	case 1:
		close(e.entered)
	default:
		if e.secondSaved.CompareAndSwap(false, true) {
			close(e.secondCall)
		}
	}
	select {
	case <-e.release:
	case <-ctx.Done():
		return openai.EmbeddingResponse{}, ctx.Err()
	}
	return fakeEmbedder{}.CreateEmbeddings(ctx, request)
}

func (e *blockingEmbedder) releaseEmbeddings() {
	if e.released.CompareAndSwap(false, true) {
		close(e.release)
	}
}

type blockingQueryEmbedder struct {
	query    string
	blocking *blockingEmbedder
}

func newBlockingQueryEmbedder(query string) *blockingQueryEmbedder {
	return &blockingQueryEmbedder{
		query:    query,
		blocking: newBlockingEmbedder(),
	}
}

func (e *blockingQueryEmbedder) CreateEmbeddings(ctx context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	if len(request.Inputs) == 1 && request.Inputs[0] == e.query {
		return e.blocking.CreateEmbeddings(ctx, request)
	}
	return fakeEmbedder{}.CreateEmbeddings(ctx, request)
}

func (e *countingEmbedder) callCount() int64 {
	return e.calls.Load()
}

func (e *countingEmbedder) maxBatchSize() int64 {
	return e.maxBatch.Load()
}

func TestFilesystemSearchDoesNotRequireGitAndIndexesCurrentFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "node_modules/\n")
	writeFile(t, root, ".gitagentignore", "search-only.txt\n")
	writeFile(t, root, "notes.txt", "release notes live here\n")
	writeFile(t, root, "icon.svg", `<svg xmlns="http://www.w3.org/2000/svg"><title>release notes live here</title></svg>`)
	writeFile(t, root, "search-only.txt", "release notes live here\n")
	writeFile(t, root, "node_modules/ignored.txt", "release notes live here\n")
	writeFile(t, root, ".omx/ignored.txt", "release notes live here\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "release notes")
	if err != nil {
		t.Fatal(err)
	}
	if out.Source.Mode != "filesystem" || out.Source.Root != root {
		t.Fatalf("source = %#v", out.Source)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %#v", out.Results)
	}
	if got := out.Results[0].Range; got != "notes.txt:1-1" {
		t.Fatalf("range = %q", got)
	}
	if strings.Contains(out.Results[0].Excerpt, "ignored") {
		t.Fatalf("excerpt includes skipped dependency dir: %s", out.Results[0].Excerpt)
	}
	if strings.Contains(out.Results[0].Range, "search-only.txt") {
		t.Fatalf("range includes .gitagentignore file: %s", out.Results[0].Range)
	}
	if out.Retrieval.Skipped.NonText != 1 {
		t.Fatalf("non-text skipped = %d, want 1", out.Retrieval.Skipped.NonText)
	}
}

func TestSearchUsesDefaultIgnorePatterns(t *testing.T) {
	for _, tt := range []struct {
		name string
		rev  bool
	}{
		{name: "filesystem"},
		{name: "revision", rev: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "notes.txt", "release notes live here\n")
			for _, path := range []string{
				"build.gradle.lockfile",
				"bun.lock",
				"bun.lockb",
				"Cartfile.resolved",
				"cabal.project.freeze",
				"Cargo.lock",
				"composer.lock",
				"conda-lock.yaml",
				"conda-lock.yml",
				"cpanfile.snapshot",
				"deno.lock",
				"flake.lock",
				"Gemfile.lock",
				"go.sum",
				"mix.lock",
				"MODULE.bazel",
				"npm-shrinkwrap.json",
				"package-lock.json",
				"Package.resolved",
				"packages.lock.json",
				"pdm.lock",
				"Pipfile.lock",
				"pixi.lock",
				"Podfile.lock",
				"poetry.lock",
				"pnpm-lock.yaml",
				"pubspec.lock",
				"renv.lock",
				"shard.lock",
				"stack.yaml.lock",
				"uv.lock",
				"yarn.lock",
				"dist/checksums.sha256",
				"LICENSE",
				"third_party/COPYING",
				"third_party/NOTICE",
			} {
				writeFile(t, root, path, "release notes live here\n")
			}

			opts := Options{
				Root:                root,
				MinScore:            0.70,
				Limit:               10,
				EmbeddingModel:      "text-embedding-3-small",
				EmbeddingDimensions: 3,
				APIKey:              "test-key",
				BaseURL:             "http://example.test",
			}
			if tt.rev {
				opts.Rev = commitSearchRepo(t, root)
			}

			out, err := Run(t.Context(), fakeEmbedder{}, opts, "release notes")
			if err != nil {
				t.Fatal(err)
			}
			if got := resultRanges(out.Results); !slices.Equal(got, []string{"notes.txt:1-1"}) {
				t.Fatalf("result ranges = %#v", got)
			}
		})
	}
}

func TestFilesystemSearchScopeKeepsRootIgnoreRules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitagentignore", "ignored.txt\n")
	writeFile(t, root, "foo/keep.txt", "alpha\n")
	writeFile(t, root, "foo/ignored.txt", "alpha\n")
	writeFile(t, root, "bar/keep.txt", "alpha\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Scope:               []string{"foo/"},
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Retrieval.Filters.Scope; !slices.Equal(got, []string{"foo"}) {
		t.Fatalf("scope filter = %#v", got)
	}
	if len(out.Results) != 1 || out.Results[0].Range != "foo/keep.txt:1-1" {
		t.Fatalf("results = %#v", out.Results)
	}
	unscoped := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	base, err := Run(t.Context(), fakeEmbedder{}, unscoped, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if out.Diagnostics.IndexDir != base.Diagnostics.IndexDir {
		t.Fatalf("index dir = %q, want shared %q", out.Diagnostics.IndexDir, base.Diagnostics.IndexDir)
	}
}

func TestFilesystemSearchFromNestedGitDirectoryReusesRootIndex(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "foo/bar/keep.txt", "alpha nested\n")
	writeFile(t, root, "foo/sibling.txt", "alpha sibling\n")
	writeFile(t, root, "outside.txt", "alpha outside\n")
	commitSearchRepo(t, root)

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                filepath.Join(root, "foo"),
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	base, err := Run(t.Context(), embedder, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := loadManifest(base.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Root != root {
		t.Fatalf("index root = %q, want repository root %q", manifest.Root, root)
	}
	if got := base.Retrieval.Filters.Scope; !slices.Equal(got, []string{"foo"}) {
		t.Fatalf("base scope = %#v, want repository-relative foo", got)
	}
	if got := resultRanges(base.Results); !slices.Equal(got, []string{"foo/bar/keep.txt:1-1", "foo/sibling.txt:1-1"}) {
		t.Fatalf("base results = %#v", got)
	}
	indexedCalls := embedder.callCount()

	opts.Root = filepath.Join(root, "foo", "bar")
	nested, err := Run(t.Context(), embedder, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if nested.Diagnostics.IndexDir != base.Diagnostics.IndexDir {
		t.Fatalf("nested index dir = %q, want root index %q", nested.Diagnostics.IndexDir, base.Diagnostics.IndexDir)
	}
	if got := nested.Retrieval.Filters.Scope; !slices.Equal(got, []string{"foo/bar"}) {
		t.Fatalf("nested scope = %#v, want repository-relative foo/bar", got)
	}
	if got := resultRanges(nested.Results); !slices.Equal(got, []string{"foo/bar/keep.txt:1-1"}) {
		t.Fatalf("nested results = %#v", got)
	}
	if embedder.callCount() != indexedCalls {
		t.Fatalf("nested search embedded again: calls = %d, want cached %d", embedder.callCount(), indexedCalls)
	}
}

func TestFilesystemSearchScopeIncludesHiddenDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".foo/keep.txt", "alpha\n")
	writeFile(t, root, ".foo/.foo/.foo/deep.txt", "alpha\n")
	writeFile(t, root, "visible.txt", "alpha\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Scope:               []string{".foo"},
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".foo/.foo/.foo/deep.txt:1-1", ".foo/keep.txt:1-1"}
	if got := resultRanges(out.Results); !slices.Equal(got, want) {
		t.Fatalf("result ranges = %#v", got)
	}
	if !strings.Contains(out.Diagnostics.IndexDir, "scope-") {
		t.Fatalf("index dir = %q, want scoped cache for hidden scope", out.Diagnostics.IndexDir)
	}
}

func TestFilesystemSearchScopeUsesIgnoreRulesInsideHiddenDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".foo/.gitagentignore", "ignored.txt\n")
	writeFile(t, root, ".foo/keep.txt", "alpha\n")
	writeFile(t, root, ".foo/ignored.txt", "alpha\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Scope:               []string{".foo"},
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got := resultRanges(out.Results); !slices.Equal(got, []string{".foo/keep.txt:1-1"}) {
		t.Fatalf("result ranges = %#v", got)
	}
}

func TestSearchFiltersByVectorThenSortsTiesByPath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "b.txt", "alpha\n")
	writeFile(t, root, "a.txt", "alpha\n")
	writeFile(t, root, "c.txt", "opposite\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		MinScore:            0.99,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results = %#v", out.Results)
	}
	if out.Results[0].Range != "a.txt:1-1" || out.Results[1].Range != "b.txt:1-1" {
		t.Fatalf("unexpected sort order: %#v", out.Results)
	}
}

func TestScoreChunksFiltersFinalHybridScore(t *testing.T) {
	chunks := []Chunk{
		{ID: "positive", Path: "positive.txt", text: "target"},
		{ID: "negative", Path: "negative.txt", text: "unrelated"},
		{ID: "lexical-collision", Path: "target.txt", text: "target target target"},
		{ID: "unknown-vector", Path: "future.txt", text: "target"},
	}
	vectors := map[string][]float64{
		"positive":          {1, 0},
		"negative":          {-1, 0},
		"lexical-collision": {0, 1},
	}

	all, err := scoreChunks(chunks, vectors, []float64{1, 0}, "target", 1e-9, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lexicalCollision scoredChunk
	for _, item := range all {
		if item.chunk.ID == "lexical-collision" {
			lexicalCollision = item
			break
		}
	}
	if lexicalCollision.chunk.ID == "" || lexicalCollision.rank <= lexicalCollision.vectorRelatedness {
		t.Fatalf("lexical collision = %#v, want hybrid lift", lexicalCollision)
	}

	got, err := scoreChunks(chunks, vectors, []float64{1, 0}, "target", lexicalCollision.rank, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].chunk.ID != "positive" || got[1].chunk.ID != "lexical-collision" {
		t.Fatalf("scored chunks = %#v, want positive and hybrid lexical matches", got)
	}
	if got[1].vectorRelatedness >= lexicalCollision.rank || got[1].rank < lexicalCollision.rank {
		t.Fatalf("lexical collision vector = %v rank = %v, want post-rank threshold", got[1].vectorRelatedness, got[1].rank)
	}
}

func TestRenderResultsUsesFinalHybridScore(t *testing.T) {
	results, err := renderResults([]scoredChunk{{
		chunk:             Chunk{Path: "target.txt", StartLine: 1, EndLine: 1},
		vectorRelatedness: 0.76,
		rank:              0.99,
	}})
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 1 {
		t.Fatalf("results = %#v", results)
	}
	if got := results[0].Relatedness; got != 0.99 {
		t.Fatalf("relatedness = %v, want final hybrid score", got)
	}
	if got := results[0].Scores.Rank; got != 0.99 {
		t.Fatalf("rank = %v, want hybrid rank", got)
	}
}

func TestSearchRejectsInvalidMinScore(t *testing.T) {
	for _, threshold := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.1, 0, 1.1} {
		_, err := Run(t.Context(), fakeEmbedder{}, Options{
			Root:                t.TempDir(),
			MinScore:            threshold,
			Limit:               1,
			EmbeddingModel:      "test-model",
			EmbeddingDimensions: 3,
		}, "target")
		if err == nil || !strings.Contains(err.Error(), "--min-score") {
			t.Fatalf("threshold %v error = %v, want min-score validation", threshold, err)
		}
	}
}

func TestSearchHybridRankingLiftsPathAndTextMatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "service.txt", "semantically plausible but unrelated\n")
	writeFile(t, root, "editors/integration.md", "editor integration setup\n")

	out, err := Run(t.Context(), ruleEmbedder{
		{contains: "implementation entrypoint for editor integration", vector: []float64{1, 0, 0}},
		{contains: "path: service.txt", vector: []float64{0.94, 0.06, 0}},
		{contains: "path: editors/integration.md", vector: []float64{0.93, 0.07, 0}},
	}, Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "editor integration")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 {
		t.Fatalf("results = %#v", out.Results)
	}
	if got := out.Results[0].Range; got != "editors/integration.md:1-1" {
		t.Fatalf("top result = %q, want path/text match before higher vector match; results = %#v", got, out.Results)
	}
	if out.Results[0].Scores.Path <= 0 {
		t.Fatalf("path score = %v, want positive", out.Results[0].Scores.Path)
	}
	if out.Results[0].Scores.Text <= 0 {
		t.Fatalf("text score = %v, want positive", out.Results[0].Scores.Text)
	}
	if out.Results[0].Scores.Rank != out.Results[0].Relatedness {
		t.Fatalf("rank score = %v relatedness = %v", out.Results[0].Scores.Rank, out.Results[0].Relatedness)
	}
	if out.Results[0].Relatedness <= out.Results[0].Scores.VectorRelatedness {
		t.Fatalf("hybrid score = %v, want lexical lift above vector %v", out.Results[0].Relatedness, out.Results[0].Scores.VectorRelatedness)
	}
	if out.Results[0].Scores.VectorRelatedness >= out.Results[1].Scores.VectorRelatedness {
		t.Fatalf("vector relatedness scores = %#v then %#v, want top to have lower vector score", out.Results[0].Scores, out.Results[1].Scores)
	}
}

func TestSearchHybridRankingLiftsSymbolMatch(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "other.go", "package main\n\nfunc unrelated() {}\n")
	writeFile(t, root, "server.go", "package main\n\nfunc languageServerCommand() {}\n")

	out, err := Run(t.Context(), ruleEmbedder{
		{contains: "implementation entrypoint for language server command", vector: []float64{1, 0, 0}},
		{contains: "path: other.go", vector: []float64{0.94, 0.06, 0}},
		{contains: "path: server.go", vector: []float64{0.93, 0.07, 0}},
	}, Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "language server command")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) == 0 {
		t.Fatalf("results = %#v", out.Results)
	}
	if got := out.Results[0].Range; !strings.HasPrefix(got, "server.go:") {
		t.Fatalf("top result = %q, want symbol match before higher vector match; results = %#v", got, out.Results)
	}
	if out.Results[0].Symbol == nil || out.Results[0].Symbol.Name != "languageServerCommand" {
		t.Fatalf("symbol = %#v", out.Results[0].Symbol)
	}
	if out.Results[0].Scores.Symbol <= 0 {
		t.Fatalf("symbol score = %v, want positive", out.Results[0].Scores.Symbol)
	}
}

func TestSearchCodeOnlyFiltersDocs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "release notes live here\n")
	writeFile(t, root, "main.go", "package main\n\nfunc releaseNotes() {}\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		CodeOnly:            true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "release notes")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Retrieval.Filters.Code {
		t.Fatalf("filters = %#v", out.Retrieval.Filters)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %#v", out.Results)
	}
	if got := out.Results[0].Range; !strings.HasPrefix(got, "main.go:") {
		t.Fatalf("range = %q", got)
	}
}

func TestSearchFilteringOptionsShareIndexDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "alpha docs\n")
	writeFile(t, root, "app.go", "package main\n\nfunc alpha() {}\n")
	writeFile(t, root, "app_test.go", "package main\n\nfunc TestAlpha() {}\n")

	opts := Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	base, err := Run(t.Context(), fakeEmbedder{}, opts, "")
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name    string
		code    bool
		noTests bool
		scope   []string
	}{
		{name: "code", code: true},
		{name: "no-tests", noTests: true},
		{name: "code-no-tests", code: true, noTests: true},
		{name: "scope", scope: []string{"app.go"}},
		{name: "scope-code-no-tests", code: true, noTests: true, scope: []string{"app.go"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := opts
			opts.CodeOnly = tt.code
			opts.NoTests = tt.noTests
			opts.Scope = tt.scope
			out, err := Run(t.Context(), fakeEmbedder{}, opts, "")
			if err != nil {
				t.Fatal(err)
			}
			if out.Diagnostics.IndexDir != base.Diagnostics.IndexDir {
				t.Fatalf("index dir = %q, want shared %q", out.Diagnostics.IndexDir, base.Diagnostics.IndexDir)
			}
		})
	}
}

func TestSearchCodeOnlySharesDefaultIndexAndKeepsReplayFiltered(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "release notes live here\n")
	writeFile(t, root, "main.go", "package main\n\nfunc releaseNotes() {}\n")

	embedder := &recordingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "release notes")
	if err != nil {
		t.Fatal(err)
	}
	firstInputCount := len(embedder.inputs)
	clearHistoryFilters(t, first.Diagnostics.IndexDir)

	opts.CodeOnly = true
	second, err := Run(t.Context(), embedder, opts, "release notes")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.IndexDir != first.Diagnostics.IndexDir {
		t.Fatalf("code index dir = %q, want shared default dir %q", second.Diagnostics.IndexDir, first.Diagnostics.IndexDir)
	}
	if second.Replay.Mode != "none" {
		t.Fatalf("code replay = %#v, want no replay from default history", second.Replay)
	}
	if len(embedder.inputs) != firstInputCount {
		t.Fatalf("code search embedded again: inputs = %#v", embedder.inputs[firstInputCount:])
	}
	if got := resultRanges(second.Results); !slices.Equal(got, []string{"main.go:3-3"}) {
		t.Fatalf("code result ranges = %#v", got)
	}

	third, err := Run(t.Context(), embedder, opts, "release notes")
	if err != nil {
		t.Fatal(err)
	}
	if third.Replay.Mode != "hit" {
		t.Fatalf("second code replay = %#v, want hit", third.Replay)
	}
	if len(embedder.inputs) != firstInputCount {
		t.Fatalf("second code search embedded again: inputs = %#v", embedder.inputs[firstInputCount:])
	}
}

func TestSearchCodeOnlySeedsSharedDefaultIndex(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "alpha docs\n")
	writeFile(t, root, "app.js", "function alpha() {}\n")

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		IndexOnly:           true,
		CodeOnly:            true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Diagnostics.EmbeddedChunks != 1 {
		t.Fatalf("code embedded chunks = %d, want 1", first.Diagnostics.EmbeddedChunks)
	}

	opts.CodeOnly = false
	second, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.IndexDir != first.Diagnostics.IndexDir {
		t.Fatalf("default index dir = %q, want shared code dir %q", second.Diagnostics.IndexDir, first.Diagnostics.IndexDir)
	}
	if second.Diagnostics.ReusedChunks != 1 || second.Diagnostics.EmbeddedChunks != 1 {
		t.Fatalf("default diagnostics = %#v, want one reused code chunk and one embedded doc chunk", second.Diagnostics)
	}
}

func TestSearchCodeOnlyReindexPreservesSharedNonCodeVectors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "alpha docs\n")
	writeFile(t, root, "app.js", "function alpha() {}\n")

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		IndexOnly:           true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Diagnostics.EmbeddedChunks != 2 {
		t.Fatalf("default embedded chunks = %d, want 2", first.Diagnostics.EmbeddedChunks)
	}

	opts.CodeOnly = true
	opts.Reindex = true
	second, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.IndexDir != first.Diagnostics.IndexDir {
		t.Fatalf("code index dir = %q, want shared default dir %q", second.Diagnostics.IndexDir, first.Diagnostics.IndexDir)
	}
	if second.Diagnostics.ReusedChunks != 0 || second.Diagnostics.EmbeddedChunks != 1 {
		t.Fatalf("code reindex diagnostics = %#v, want one rebuilt code chunk", second.Diagnostics)
	}
	listing, err := ListIndexes(t.Context(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	indexes := listing.Indexes
	if len(indexes) != 1 {
		t.Fatalf("indexes = %#v, want one shared index", indexes)
	}
	if indexes[0].Files != 2 || indexes[0].Chunks != 2 {
		t.Fatalf("index summary = files:%d chunks:%d, want shared persisted counts 2/2", indexes[0].Files, indexes[0].Chunks)
	}
	listed, err := ListIndexFiles(t.Context(), ListFilesOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(listed.Files, "README.md") || !slices.Contains(listed.Files, "app.js") {
		t.Fatalf("listed files = %v, want shared code and non-code paths", listed.Files)
	}
	calls := embedder.callCount()

	opts.CodeOnly = false
	opts.Reindex = false
	third, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if third.Diagnostics.ReusedChunks != 2 || third.Diagnostics.EmbeddedChunks != 0 {
		t.Fatalf("default diagnostics = %#v, want all chunks reused", third.Diagnostics)
	}
	if embedder.callCount() != calls {
		t.Fatalf("default search embedded after code reindex: calls = %d, want %d", embedder.callCount(), calls)
	}
}

func TestSearchCodeOnlyDropsStaleSharedNonCodeVectors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "alpha docs\n")
	writeFile(t, root, "app.js", "function alpha() {}\n")

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		IndexOnly:           true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}

	opts.CodeOnly = true
	opts.Reindex = true
	second, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.IndexDir != first.Diagnostics.IndexDir {
		t.Fatalf("code index dir = %q, want shared default dir %q", second.Diagnostics.IndexDir, first.Diagnostics.IndexDir)
	}
	records, err := loadVectors(second.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Path == "README.md" {
			t.Fatalf("stale non-code record was preserved: %#v", record)
		}
	}
}

func TestSearchDropsStaleVectorsWhenFileRemovedWithoutMissingChunks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	writeFile(t, root, "b.txt", "beta\n")

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		IndexOnly:           true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Diagnostics.EmbeddedChunks != 2 {
		t.Fatalf("first embedded chunks = %d, want 2", first.Diagnostics.EmbeddedChunks)
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}

	second, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.ReusedChunks != 1 || second.Diagnostics.EmbeddedChunks != 0 {
		t.Fatalf("second diagnostics = %#v, want one reused chunk and no embeddings", second.Diagnostics)
	}
	records, err := loadVectors(second.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Path != "b.txt" {
		t.Fatalf("records = %#v, want only b.txt", records)
	}
}

func TestSearchNoTestsStaleCleanupRetainsSharedTestVectors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "alpha docs\n")
	writeFile(t, root, "app.go", "package main\n\nfunc alpha() {}\n")
	writeFile(t, root, "app_test.go", "package main\n\nfunc TestAlpha() {}\n")

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		IndexOnly:           true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Diagnostics.EmbeddedChunks == 0 {
		t.Fatalf("first embedded chunks = %d, want non-zero", first.Diagnostics.EmbeddedChunks)
	}
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}

	opts.NoTests = true
	second, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.ReusedChunks == 0 || second.Diagnostics.EmbeddedChunks != 0 {
		t.Fatalf("second diagnostics = %#v, want reused chunks and no embeddings", second.Diagnostics)
	}
	records, err := loadVectors(second.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(records, func(record vectorRecord) bool { return record.Path == "README.md" }) {
		t.Fatalf("stale doc record was preserved: %#v", records)
	}
	if !slices.ContainsFunc(records, func(record vectorRecord) bool { return record.Path == "app_test.go" }) {
		t.Fatalf("shared test vector was dropped: %#v", records)
	}
}

func TestSearchScopeStaleCleanupRetainsSharedOutOfScopeVectors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "pkg/app.txt", "alpha app\n")
	writeFile(t, root, "pkg/stale.txt", "alpha stale\n")
	writeFile(t, root, "docs/guide.txt", "alpha guide\n")

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		IndexOnly:           true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Diagnostics.EmbeddedChunks != 3 {
		t.Fatalf("first embedded chunks = %d, want 3", first.Diagnostics.EmbeddedChunks)
	}
	if err := os.Remove(filepath.Join(root, "pkg/stale.txt")); err != nil {
		t.Fatal(err)
	}

	opts.Scope = []string{"pkg"}
	second, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.IndexDir != first.Diagnostics.IndexDir {
		t.Fatalf("index dir = %q, want shared %q", second.Diagnostics.IndexDir, first.Diagnostics.IndexDir)
	}
	if second.Diagnostics.ReusedChunks != 1 || second.Diagnostics.EmbeddedChunks != 0 {
		t.Fatalf("second diagnostics = %#v, want one reused scoped chunk and no embeddings", second.Diagnostics)
	}
	records, err := loadVectors(second.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(records, func(record vectorRecord) bool { return record.Path == "pkg/stale.txt" }) {
		t.Fatalf("stale scoped record was preserved: %#v", records)
	}
	if !slices.ContainsFunc(records, func(record vectorRecord) bool { return record.Path == "docs/guide.txt" }) {
		t.Fatalf("shared out-of-scope vector was dropped: %#v", records)
	}
}

func TestSearchReindexClearsIndexWhenAllFilesIgnored(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		IndexOnly:           true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Diagnostics.EmbeddedChunks != 1 {
		t.Fatalf("first embedded chunks = %d, want 1", first.Diagnostics.EmbeddedChunks)
	}

	writeFile(t, root, ".gitignore", "*.txt\n")
	opts.Reindex = true
	second, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.Chunks != 0 || second.Diagnostics.EmbeddedChunks != 0 || second.Diagnostics.ReusedChunks != 0 {
		t.Fatalf("second diagnostics = %#v, want empty reindex", second.Diagnostics)
	}
	records, err := loadVectors(second.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %#v, want empty index", records)
	}
	listed, err := ListIndexFiles(t.Context(), ListFilesOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Files) != 0 || listed.Index.Files != 0 || listed.Index.Chunks != 0 {
		t.Fatalf("listed = %#v, want empty index", listed)
	}
	if listed.Index.Dimensions != opts.EmbeddingDimensions {
		t.Fatalf("dimensions = %d, want %d", listed.Index.Dimensions, opts.EmbeddingDimensions)
	}
}

func TestSearchReplaysLegacyScopedHistoryWithoutFilters(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "pkg/a.txt", "alpha\n")

	embedder := &recordingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		Scope:               []string{"pkg"},
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	clearHistoryFilters(t, first.Diagnostics.IndexDir)
	firstInputCount := len(embedder.inputs)

	second, err := Run(t.Context(), embedder, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if second.Replay.Mode != "hit" {
		t.Fatalf("legacy scoped replay = %#v, want hit", second.Replay)
	}
	if len(embedder.inputs) != firstInputCount {
		t.Fatalf("legacy scoped search embedded again: inputs = %#v", embedder.inputs[firstInputCount:])
	}
}

func clearHistoryFilters(t *testing.T, indexDir string) {
	t.Helper()
	entries, err := loadHistory(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		entries[i].Filters = nil
	}
	if err := writeJSON(filepath.Join(indexDir, "history.json"), entries); err != nil {
		t.Fatal(err)
	}
}

func clearEmbeddingInputHashes(t *testing.T, indexDir string) {
	t.Helper()
	index, err := loadVectorIndexRecords(indexDir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range index {
		index[i].EmbeddingInputHash = ""
	}
	if err := writeJSON(filepath.Join(indexDir, "vectors.index.json"), index); err != nil {
		t.Fatal(err)
	}
}

func TestSearchFramesQueryForImplementationRetrieval(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n\nfunc releaseNotes() {}\n")

	embedder := &recordingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "release notes")
	if err != nil {
		t.Fatal(err)
	}
	if first.Query != "release notes" {
		t.Fatalf("output query = %q", first.Query)
	}
	if !slices.Contains(embedder.inputs, queryEmbeddingText("release notes", 0)) {
		t.Fatalf("embedding inputs = %#v, want framed query", embedder.inputs)
	}
	firstInputCount := len(embedder.inputs)

	second, err := Run(t.Context(), embedder, opts, "release notes")
	if err != nil {
		t.Fatal(err)
	}
	if second.Replay.Mode != "hit" {
		t.Fatalf("second replay = %#v, want hit", second.Replay)
	}
	if len(embedder.inputs) != firstInputCount {
		t.Fatalf("second search embedded again: inputs = %#v", embedder.inputs[firstInputCount:])
	}
}

func TestQueryFramingPreservesQueryUnderSmallEmbeddingCap(t *testing.T) {
	if got := queryEmbeddingText("alpha", len("implementation entrypoint for ")); got != "alpha" {
		t.Fatalf("query embedding text = %q, want raw query", got)
	}
}

func TestQueryEmbeddingTextReturnsCappedProviderInput(t *testing.T) {
	query := "alpha beta gamma delta"
	if got := queryEmbeddingText(query, 10); got != "alpha beta" {
		t.Fatalf("query embedding text = %q, want capped raw query", got)
	}
}

func TestSearchCachesQueryByFinalCappedEmbeddingInput(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "alpha.txt", "alpha beta gamma delta\n")

	embedder := &recordingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingMaxInput:   24,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	query := strings.Repeat("alpha ", 12)
	if _, err := Run(t.Context(), embedder, opts, query); err != nil {
		t.Fatal(err)
	}
	firstInputCount := len(embedder.inputs)

	opts.EmbeddingMaxInput = 12
	if _, err := Run(t.Context(), embedder, opts, query); err != nil {
		t.Fatal(err)
	}
	if len(embedder.inputs) == firstInputCount {
		t.Fatalf("second search reused query embedding despite different final capped input")
	}
	if got, want := embedder.inputs[len(embedder.inputs)-1], queryEmbeddingText(query, opts.EmbeddingMaxInput); got != want {
		t.Fatalf("second query embedding input = %q, want %q", got, want)
	}
}

func TestSearchTermsSplitGoInitialisms(t *testing.T) {
	terms := searchTerms("HTTPServerCommand URLParser JSONEncoder")
	for _, want := range []string{"http", "server", "command", "url", "parser", "json", "encoder"} {
		if !slices.Contains(terms, want) {
			t.Fatalf("search terms = %#v, missing %q", terms, want)
		}
	}
}

func TestSearchNoTestsFiltersCommonTestPaths(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"main.go":              "alpha\n",
		"main_test.go":         "alpha\n",
		"button.test.ts":       "alpha\n",
		"button.spec.ts":       "alpha\n",
		"test.js":              "alpha\n",
		"spec.ts":              "alpha\n",
		"tests/helper.go":      "alpha\n",
		"test/helper.py":       "alpha\n",
		"__tests__/view.ts":    "alpha\n",
		"spec/model.rb":        "alpha\n",
		"testdata/sample.json": "alpha\n",
	} {
		writeFile(t, root, name, content)
	}

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               20,
		NoTests:             true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Retrieval.Filters.NoTests {
		t.Fatalf("filters = %#v", out.Retrieval.Filters)
	}
	got := resultRanges(out.Results)
	want := []string{"main.go:1-1", "testdata/sample.json:1-1"}
	if !slices.Equal(got, want) {
		t.Fatalf("result ranges = %#v, want %#v", got, want)
	}
	if strings.Contains(out.Diagnostics.IndexDir, "no-tests") {
		t.Fatalf("index dir = %q, want shared cache without no-tests filter", out.Diagnostics.IndexDir)
	}
	records, err := loadVectors(out.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(records, func(record vectorRecord) bool { return record.Path == "main_test.go" }) {
		t.Fatalf("shared cache should retain test file vectors")
	}
}

func TestIsTestPath(t *testing.T) {
	tests := []string{
		"widget_test.rs",
		"widget_tests.rs",
		"test_widget.py",
		"widget_test.py",
		"widget_spec.rb",
		"widget-test.cpp",
		"widget-unittest.cc",
		"WidgetTest.java",
		"WidgetTests.cs",
		"WidgetTestCase.kt",
		"TestWidget.java",
		"integration_test/widget.dart",
		"specs/widget.rb",
	}
	for _, path := range tests {
		if !isTestPath(path) {
			t.Errorf("isTestPath(%q) = false, want true", path)
		}
	}

	nonTests := []string{
		"contest.rs",
		"latest.py",
		"testimonial.ts",
		"testament.java",
		"testing/widget.go",
		"testdata/sample.json",
	}
	for _, path := range nonTests {
		if isTestPath(path) {
			t.Errorf("isTestPath(%q) = true, want false", path)
		}
	}
}

func TestSearchNoTestsWithCodeAndScope(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "codex-rs/core/src/session/orchestrated.rs", "alpha\n")
	writeFile(t, root, "codex-rs/core/src/tools/spec_plan_tests.rs", "alpha\n")
	writeFile(t, root, "outside.rs", "alpha\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               20,
		CodeOnly:            true,
		NoTests:             true,
		Scope:               []string{"codex-rs/core/src"},
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	got := resultRanges(out.Results)
	want := []string{"codex-rs/core/src/session/orchestrated.rs:1-1"}
	if !slices.Equal(got, want) {
		t.Fatalf("result ranges = %#v, want %#v", got, want)
	}
}

func TestRevisionSearchNoTestsFiltersCommittedTestPaths(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "alpha\n")
	writeFile(t, root, "main_test.go", "alpha\n")
	writeFile(t, root, "tests/helper.go", "alpha\n")
	rev := commitSearchRepo(t, root)

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Rev:                 rev,
		MinScore:            0.70,
		Limit:               10,
		NoTests:             true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	got := resultRanges(out.Results)
	want := []string{"main.go:1-1"}
	if !slices.Equal(got, want) {
		t.Fatalf("result ranges = %#v, want %#v", got, want)
	}
}

func TestRevisionSearchReusesFilesystemEmbeddings(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	writeFile(t, root, "b.txt", "beta\n")
	rev := commitSearchRepo(t, root)

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	filesystem, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if filesystem.Diagnostics.EmbeddedChunks != 2 {
		t.Fatalf("filesystem diagnostics = %#v, want two embedded chunks", filesystem.Diagnostics)
	}

	opts.Rev = rev
	revision, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if revision.Diagnostics.IndexDir == filesystem.Diagnostics.IndexDir {
		t.Fatalf("revision index dir = filesystem index dir %q", revision.Diagnostics.IndexDir)
	}
	if revision.Diagnostics.ReusedChunks != 2 || revision.Diagnostics.EmbeddedChunks != 0 {
		t.Fatalf("revision diagnostics = %#v, want two reused chunks and no embeddings", revision.Diagnostics)
	}
	records, err := loadVectors(revision.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Source != "revision" || record.Blob == "" {
			t.Fatalf("reused record kept stale filesystem metadata: %#v", record)
		}
	}
}

func TestFilesystemAndRevisionIndexesShareVectorPayload(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	writeFile(t, root, "b.txt", "beta\n")
	rev := commitSearchRepo(t, root)

	opts := Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	filesystem, err := Run(t.Context(), fakeEmbedder{}, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	opts.Rev = rev
	revision, err := Run(t.Context(), fakeEmbedder{}, opts, "")
	if err != nil {
		t.Fatal(err)
	}

	selection, err := resolveIndexSelection(t.Context(), root, "", "", Filters{}, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertFileSize(t, sharedVectorPayloadPath(selection.metadataDir), 2*3*4)
	assertFileSize(t, filepath.Join(filesystem.Diagnostics.IndexDir, "vectors.f32"), 0)
	assertFileSize(t, filepath.Join(revision.Diagnostics.IndexDir, "vectors.f32"), 0)
}

func TestChangedRevisionAppendsOnlyNewSharedVectorPayload(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	writeFile(t, root, "b.txt", "beta\n")
	firstRev := commitSearchRepo(t, root)

	opts := Options{
		Root:                root,
		Rev:                 firstRev,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	if _, err := Run(t.Context(), fakeEmbedder{}, opts, ""); err != nil {
		t.Fatal(err)
	}

	writeFile(t, root, "b.txt", "changed beta\n")
	opts.Rev = commitSearchRepoChange(t, root, "change b")
	if _, err := Run(t.Context(), fakeEmbedder{}, opts, ""); err != nil {
		t.Fatal(err)
	}

	selection, err := resolveIndexSelection(t.Context(), root, "", "", Filters{}, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertFileSize(t, sharedVectorPayloadPath(selection.metadataDir), 3*3*4)
}

func TestLegacyLocalVectorIndexMigratesWithoutEmbedding(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	first, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	records, err := loadVectors(first.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := resolveIndexSelection(t.Context(), root, "", "", Filters{}, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(newVectorStore(selection.metadataDir).dir); err != nil {
		t.Fatal(err)
	}
	writeLegacyBinaryVectors(t, first.Diagnostics.IndexDir, records)
	found, err := loadManifest(first.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	found.VectorStore = ""
	found.Version = legacyIndexVersion
	if err := writeJSON(filepath.Join(first.Diagnostics.IndexDir, "manifest.json"), found); err != nil {
		t.Fatal(err)
	}

	second, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.EmbeddedChunks != 0 || embedder.callCount() != 1 {
		t.Fatalf("second diagnostics = %#v calls = %d, want migration without embedding", second.Diagnostics, embedder.callCount())
	}
	if !indexUsesSharedVectors(second.Diagnostics.IndexDir) {
		t.Fatal("migrated index does not use shared vectors")
	}
	assertFileSize(t, sharedVectorPayloadPath(selection.metadataDir), 3*4)
	assertFileSize(t, filepath.Join(second.Diagnostics.IndexDir, "vectors.f32"), 0)
}

func TestLegacyVectorLoaderRejectsSharedReferences(t *testing.T) {
	dir := t.TempDir()
	index := []vectorIndexRecord{{
		EmbeddingInputHash: "input-hash",
		EmbeddingModel:     "text-embedding-3-small",
		Dimensions:         3,
		Offset:             0,
		VectorKey:          "shared-key",
		VectorChecksum:     1,
	}}
	if err := writeJSON(filepath.Join(dir, "vectors.index.json"), index); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vectors.f32"), encodeVector([]float64{1, 0, 0}), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBinaryVectors(dir); err == nil {
		t.Fatal("legacy loader accepted shared vector reference")
	}
}

func TestCorruptSharedVectorIsRebuilt(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	first, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	index, err := loadVectorIndexRecords(first.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := resolveIndexSelection(t.Context(), root, "", "", Filters{}, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	payloadPath := sharedVectorPayloadPath(selection.metadataDir)
	payload, err := os.OpenFile(payloadPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := payload.WriteAt([]byte{0xff}, index[0].Offset); err != nil {
		_ = payload.Close()
		t.Fatal(err)
	}
	if err := payload.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.EmbeddedChunks != 1 || embedder.callCount() != 2 {
		t.Fatalf("second diagnostics = %#v calls = %d, want corrupt vector rebuilt", second.Diagnostics, embedder.callCount())
	}
	assertFileSize(t, payloadPath, 2*3*4)
	if _, err := loadVectors(second.Diagnostics.IndexDir); err != nil {
		t.Fatalf("load rebuilt vectors: %v", err)
	}
}

func TestFailedIndexWriteInvalidatesSnapshot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	first, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	records, err := loadVectors(first.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := resolveIndexSelection(t.Context(), root, "", "", Filters{}, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}

	records[0].Dimensions = 0
	err = saveIndex(
		t.Context(),
		selection.metadataDir,
		first.Diagnostics.IndexDir,
		Source{Mode: "filesystem"},
		root,
		"",
		opts.EmbeddingModel,
		opts.EmbeddingDimensions,
		records,
		nil,
	)
	if err == nil {
		t.Fatal("saveIndex accepted invalid vector record")
	}
	if _, err := loadVectors(first.Diagnostics.IndexDir); err == nil {
		t.Fatal("failed write left prior snapshot queryable")
	}

	second, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.EmbeddedChunks != 1 || embedder.callCount() != 2 {
		t.Fatalf("second diagnostics = %#v calls = %d, want incomplete snapshot rebuilt", second.Diagnostics, embedder.callCount())
	}
}

func TestReindexAppendsSharedVectorWithoutChangingOtherSnapshot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	rev := commitSearchRepo(t, root)
	opts := Options{
		Root:                root,
		Rev:                 rev,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	revision, err := Run(t.Context(), fakeEmbedder{}, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	revisionIndex, err := loadVectorIndexRecords(revision.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	opts.Rev = ""
	filesystem, err := Run(t.Context(), fakeEmbedder{}, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	opts.Reindex = true
	filesystem, err = Run(t.Context(), fakeEmbedder{}, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	filesystemIndex, err := loadVectorIndexRecords(filesystem.Diagnostics.IndexDir)
	if err != nil {
		t.Fatal(err)
	}
	if filesystemIndex[0].Offset == revisionIndex[0].Offset {
		t.Fatalf("reindexed offset = %d, want new generation after %d", filesystemIndex[0].Offset, revisionIndex[0].Offset)
	}
	if _, err := loadVectors(revision.Diagnostics.IndexDir); err != nil {
		t.Fatalf("load original revision snapshot: %v", err)
	}
	selection, err := resolveIndexSelection(t.Context(), root, "", "", Filters{}, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertFileSize(t, sharedVectorPayloadPath(selection.metadataDir), 2*3*4)
}

func TestConcurrentVectorStoreWritesDeduplicatePayload(t *testing.T) {
	metadataDir := t.TempDir()
	store := newVectorStore(metadataDir)
	record := vectorRecord{
		ChunkID:            "c000001",
		EmbeddingInputHash: "input-hash",
		EmbeddingModel:     "text-embedding-3-small",
		Dimensions:         3,
		Vector:             []float64{1, 0, 0},
	}
	var group sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		group.Go(func() {
			_, err := store.put(t.Context(), []vectorRecord{record}, nil)
			errs <- err
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertFileSize(t, sharedVectorPayloadPath(metadataDir), 3*4)
}

func TestVectorStoreSeparatesInputModelAndDimensions(t *testing.T) {
	metadataDir := t.TempDir()
	store := newVectorStore(metadataDir)
	records := []vectorRecord{
		{EmbeddingInputHash: "input-a", EmbeddingModel: "model-a", Dimensions: 3, Vector: []float64{1, 0, 0}},
		{EmbeddingInputHash: "input-b", EmbeddingModel: "model-a", Dimensions: 3, Vector: []float64{0, 1, 0}},
		{EmbeddingInputHash: "input-a", EmbeddingModel: "model-b", Dimensions: 3, Vector: []float64{0, 0, 1}},
		{EmbeddingInputHash: "input-a", EmbeddingModel: "model-a", Dimensions: 4, Vector: []float64{1, 0, 0, 0}},
	}
	if _, err := store.put(t.Context(), records, nil); err != nil {
		t.Fatal(err)
	}
	assertFileSize(t, sharedVectorPayloadPath(metadataDir), (3+3+3+4)*4)
}

func TestVectorStoreCatalogRecoveryKeepsLastValidGeneration(t *testing.T) {
	metadataDir := t.TempDir()
	store := newVectorStore(metadataDir)
	record := func(inputHash string) vectorRecord {
		return vectorRecord{
			EmbeddingInputHash: inputHash,
			EmbeddingModel:     "text-embedding-3-small",
			Dimensions:         3,
			Vector:             []float64{1, 0, 0},
		}
	}
	if _, err := store.put(t.Context(), []vectorRecord{record("input-a")}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.put(t.Context(), []vectorRecord{record("input-b")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, vectorStoreCatalogName(2)), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.put(t.Context(), []vectorRecord{record("input-c")}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.dir, vectorStoreCatalogName(3)), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.put(t.Context(), []vectorRecord{record("input-a")}, nil); err != nil {
		t.Fatal(err)
	}
	assertFileSize(t, sharedVectorPayloadPath(metadataDir), 3*3*4)
}

func TestRevisionSearchReusesUnchangedChunksFromAnotherRevision(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	writeFile(t, root, "b.txt", "beta\n")
	firstRev := commitSearchRepo(t, root)

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		Rev:                 firstRev,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	first, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Diagnostics.EmbeddedChunks != 2 {
		t.Fatalf("first diagnostics = %#v, want two embedded chunks", first.Diagnostics)
	}

	writeFile(t, root, "b.txt", "changed beta\n")
	opts.Rev = commitSearchRepoChange(t, root, "change b")
	second, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.ReusedChunks != 1 || second.Diagnostics.EmbeddedChunks != 1 {
		t.Fatalf("second diagnostics = %#v, want one reused and one embedded chunk", second.Diagnostics)
	}
}

func TestParallelRevisionSearchesReuseSharedFilesystemIndex(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	writeFile(t, root, "b.txt", "beta\n")
	firstRev := commitSearchRepo(t, root)

	embedder := &countingEmbedder{}
	base := Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	if _, err := Run(t.Context(), embedder, base, ""); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "b.txt", "changed beta\n")
	secondRev := commitSearchRepoChange(t, root, "change b")

	type result struct {
		out Output
		err error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	for _, rev := range []string{firstRev, secondRev} {
		group.Go(func() {
			opts := base
			opts.Rev = rev
			out, err := Run(t.Context(), embedder, opts, "")
			results <- result{out: out, err: err}
		})
	}
	group.Wait()
	close(results)

	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		switch result.out.Source.ResolvedRev {
		case firstRev:
			if result.out.Diagnostics.ReusedChunks != 2 || result.out.Diagnostics.EmbeddedChunks != 0 {
				t.Fatalf("first revision diagnostics = %#v", result.out.Diagnostics)
			}
		case secondRev:
			if result.out.Diagnostics.ReusedChunks != 1 || result.out.Diagnostics.EmbeddedChunks != 1 {
				t.Fatalf("second revision diagnostics = %#v", result.out.Diagnostics)
			}
		default:
			t.Fatalf("unexpected revision %q", result.out.Source.ResolvedRev)
		}
	}
}

func TestSearchDoesNotReuseChunkEmbeddingAfterInputCapChanges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha beta gamma delta epsilon\n")

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingMaxInput:   200,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	if _, err := Run(t.Context(), embedder, opts, ""); err != nil {
		t.Fatal(err)
	}

	opts.EmbeddingMaxInput = 16
	second, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.ReusedChunks != 0 || second.Diagnostics.EmbeddedChunks != 1 {
		t.Fatalf("second diagnostics = %#v, want changed capped input re-embedded", second.Diagnostics)
	}
}

func TestReuseVectorsKeepsDistinctTargetChunkMetadata(t *testing.T) {
	opts := Options{
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	first := Chunk{
		ID:          "c000001",
		Path:        "repeat.txt",
		Source:      "revision",
		Blob:        "first-blob",
		StartLine:   1,
		EndLine:     1,
		ContentHash: "same-content",
		text:        "repeated text",
	}
	second := first
	second.ID = "c000002"
	second.Blob = "second-blob"
	second.StartLine = 101
	second.EndLine = 101
	vector := []float64{1, 0, 0}

	_, records, reused := reuseVectors(
		[]Chunk{first, second},
		[]vectorRecord{vectorRecordForChunk(first, vector, opts)},
		opts,
	)
	if reused != 2 || len(records) != 2 {
		t.Fatalf("reused = %d records = %d, want two", reused, len(records))
	}
	if records[0].StartLine != 1 || records[0].Blob != "first-blob" ||
		records[1].StartLine != 101 || records[1].Blob != "second-blob" {
		t.Fatalf("records kept stale metadata: %#v", records)
	}
	if cacheRecordKey(records[0]) == cacheRecordKey(records[1]) {
		t.Fatalf("distinct target records share cache record key: %#v", records)
	}
}

func TestSearchRebuildsLegacyVectorWithoutEmbeddingInputHash(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")

	opts := Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	first, err := Run(t.Context(), fakeEmbedder{}, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	clearEmbeddingInputHashes(t, first.Diagnostics.IndexDir)

	second, err := Run(t.Context(), fakeEmbedder{}, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.ReusedChunks != 0 || second.Diagnostics.EmbeddedChunks != 1 {
		t.Fatalf("second diagnostics = %#v, want legacy vector rebuilt", second.Diagnostics)
	}
}

func TestFilteredSearchPreservesLegacySharedRecords(t *testing.T) {
	for _, tt := range []struct {
		name          string
		files         map[string]string
		configure     func(*Options)
		rebuiltPath   string
		preservedPath string
	}{
		{
			name: "code",
			files: map[string]string{
				"app.go":    "package app\n",
				"README.md": "alpha docs\n",
			},
			configure:     func(opts *Options) { opts.CodeOnly = true },
			rebuiltPath:   "app.go",
			preservedPath: "README.md",
		},
		{
			name: "scope",
			files: map[string]string{
				"a.txt": "alpha\n",
				"b.txt": "beta\n",
			},
			configure:     func(opts *Options) { opts.Scope = []string{"a.txt"} },
			rebuiltPath:   "a.txt",
			preservedPath: "b.txt",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for path, content := range tt.files {
				writeFile(t, root, path, content)
			}
			opts := Options{
				Root:                root,
				IndexOnly:           true,
				MinScore:            DefaultMinScore,
				Limit:               DefaultLimit,
				EmbeddingModel:      "text-embedding-3-small",
				EmbeddingDimensions: 3,
			}
			first, err := Run(t.Context(), fakeEmbedder{}, opts, "")
			if err != nil {
				t.Fatal(err)
			}
			clearEmbeddingInputHashes(t, first.Diagnostics.IndexDir)

			tt.configure(&opts)
			if _, err := Run(t.Context(), fakeEmbedder{}, opts, ""); err != nil {
				t.Fatal(err)
			}
			records, err := loadVectors(first.Diagnostics.IndexDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 2 {
				t.Fatalf("records = %#v, want rebuilt and preserved records", records)
			}
			for _, record := range records {
				switch record.Path {
				case tt.rebuiltPath:
					if record.EmbeddingInputHash == "" {
						t.Fatalf("rebuilt record has no input hash: %#v", record)
					}
				case tt.preservedPath:
					if record.EmbeddingInputHash != "" {
						t.Fatalf("legacy record unexpectedly rebuilt: %#v", record)
					}
				default:
					t.Fatalf("unexpected record: %#v", record)
				}
			}
		})
	}
}

func TestRevisionReindexDoesNotReuseFilesystemEmbeddings(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	rev := commitSearchRepo(t, root)

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
	}
	if _, err := Run(t.Context(), embedder, opts, ""); err != nil {
		t.Fatal(err)
	}

	opts.Rev = rev
	opts.Reindex = true
	revision, err := Run(t.Context(), embedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if revision.Diagnostics.ReusedChunks != 0 || revision.Diagnostics.EmbeddedChunks != 1 {
		t.Fatalf("revision diagnostics = %#v, want forced embedding rebuild", revision.Diagnostics)
	}
}

func TestRemoteSearchUsesCachedCommittedTree(t *testing.T) {
	remote := t.TempDir()
	writeFile(t, remote, "remote.txt", "remote alpha content\n")
	rev := commitSearchRepo(t, remote)
	root := t.TempDir()

	var progress []Progress
	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Remote:              remote,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
		ProgressLog: func(update Progress) error {
			progress = append(progress, update)
			return nil
		},
	}, "remote alpha")
	if err != nil {
		t.Fatal(err)
	}
	if out.Source.Mode != "remote" || out.Source.Remote != remote || out.Source.Rev != "HEAD" || out.Source.ResolvedRev != rev {
		t.Fatalf("source = %#v, want remote HEAD %s", out.Source, rev)
	}
	if len(out.Results) == 0 || !strings.Contains(out.Results[0].Range, "remote.txt") {
		t.Fatalf("results = %#v, want remote.txt", out.Results)
	}
	if len(progress) == 0 || progress[0].Status != ProgressStatusFetching {
		t.Fatalf("first progress = %#v, want remote fetch status", progress)
	}

	remotes, err := ListRemotes()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(remotes, func(info RemoteInfo) bool { return info.Remote == remote }) {
		t.Fatalf("remotes = %#v, missing %q", remotes, remote)
	}
	var completions strings.Builder
	if err := FormatRemoteCompletions(&completions, remotes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(completions.String(), remote+"\n") {
		t.Fatalf("completions = %q, missing %q", completions.String(), remote)
	}

	listing, err := ListIndexes(t.Context(), root, remote)
	if err != nil {
		t.Fatal(err)
	}
	indexes := listing.Indexes
	if len(indexes) != 1 || indexes[0].Mode != "remote" || indexes[0].Remote != remote || indexes[0].ResolvedRev != rev {
		t.Fatalf("indexes = %#v, want remote index", indexes)
	}
	metadataDir, err := metadata.RemoteDir(remote)
	if err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(metadataDir, "repo.git")
	if listing.RepoDir != repoDir {
		t.Fatalf("repo dir = %q, want %q", listing.RepoDir, repoDir)
	}
	if text := FormatIndexes(listing); !strings.Contains(text, "repo="+repoDir+"\n") {
		t.Fatalf("formatted indexes missing remote repo dir:\n%s", text)
	}
	listed, err := ListIndexFiles(t.Context(), ListFilesOptions{Root: root, Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(listed.Files, "remote.txt") {
		t.Fatalf("files = %#v, missing remote.txt", listed.Files)
	}
	packFiles, err := filepath.Glob(filepath.Join(repoDir, "objects", "pack", "*.pack"))
	if err != nil || len(packFiles) == 0 {
		t.Fatalf("cached packs = %#v, err = %v", packFiles, err)
	}
	assertNoLooseGitObjects(t, filepath.Join(repoDir, "objects"))
}

func TestTranslateAdvertisedRevisionResolvesSymbolicAndPeeledRefs(t *testing.T) {
	head := plumbing.NewHash("1111111111111111111111111111111111111111")
	peeled := plumbing.NewHash("2222222222222222222222222222222222222222")
	refs := []*plumbing.Reference{
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main")),
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), head),
		plumbing.NewHashReference(plumbing.ReferenceName("refs/tags/v1"), plumbing.NewHash("3333333333333333333333333333333333333333")),
		plumbing.NewHashReference(plumbing.ReferenceName("refs/tags/v1^{}"), peeled),
	}
	translated, resolved, err := translateAdvertisedRevision("HEAD~1", refs)
	if err != nil || translated != head.String()+"~1" || resolved != "" {
		t.Fatalf("HEAD~1 = %q, %q, %v", translated, resolved, err)
	}
	translated, resolved, err = translateAdvertisedRevision("v1", refs)
	if err != nil || translated != peeled.String() || resolved != peeled.String() {
		t.Fatalf("annotated tag = %q, %q, %v", translated, resolved, err)
	}
}

func TestRemoteEmbeddingFailurePublishesNoMetadataOrIndex(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote := t.TempDir()
	writeFile(t, remote, "fail.go", "package fail\n")
	_ = commitSearchRepo(t, remote)
	before, err := filepath.Glob(filepath.Join(os.TempDir(), "git-agent-remote-*"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(t.Context(), failOnPathEmbedder("fail.go"), Options{
		Root: t.TempDir(), Remote: remote, IndexOnly: true,
		MinScore: DefaultMinScore, Limit: DefaultLimit,
		EmbeddingModel: "test-model", EmbeddingDimensions: 3,
	}, "")
	if err == nil {
		t.Fatal("expected embedding failure")
	}
	metadataDir, err := metadata.RemoteDir(remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(metadataDir, "remote.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("remote metadata error = %v, want not exist", err)
	}
	var manifests []string
	_ = filepath.WalkDir(filepath.Join(metadataDir, "search"), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr == nil && !entry.IsDir() && entry.Name() == "manifest.json" {
			manifests = append(manifests, path)
		}
		return nil
	})
	if len(manifests) != 0 {
		t.Fatalf("published manifests = %#v", manifests)
	}
	after, err := filepath.Glob(filepath.Join(os.TempDir(), "git-agent-remote-*"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, after) {
		t.Fatalf("temporary overlays before = %#v, after = %#v", before, after)
	}
}

func assertNoLooseGitObjects(t *testing.T, objectsDir string) {
	t.Helper()
	entries, err := os.ReadDir(objectsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || len(entry.Name()) != 2 || strings.ContainsAny(entry.Name(), "ghijklmnopqrstuvwxyz") {
			continue
		}
		objects, err := os.ReadDir(filepath.Join(objectsDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(objects) > 0 {
			t.Fatalf("loose Git objects remain in %s", filepath.Join(objectsDir, entry.Name()))
		}
	}
}

func TestRemoteProgressWriterReportsCompleteUpdates(t *testing.T) {
	var updates []Progress
	rawRemote := "https://user:secret@example.test/repo.git?token=abc#fragment"
	writer := newRemoteProgressWriter(func(update Progress) error {
		updates = append(updates, update)
		return nil
	}, rawRemote, ProgressStatusFetching)
	if _, err := writer.Write([]byte("Enumerating objects: 25%\rCounting obj")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("ects: 50%\r")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("Fetching " + rawRemote + "\r")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("Credentials user secret abc fragment\r")); err != nil {
		t.Fatal(err)
	}
	want := []Progress{
		{Status: ProgressStatusFetching, Detail: "Enumerating objects: 25%"},
		{Status: ProgressStatusFetching, Detail: "Counting objects: 50%"},
		{Status: ProgressStatusFetching, Detail: "Fetching https://example.test/repo.git"},
		{Status: ProgressStatusFetching, Detail: "Credentials [REDACTED] [REDACTED] [REDACTED] [REDACTED]"},
	}
	if !slices.Equal(updates, want) {
		t.Fatalf("updates = %#v, want %#v", updates, want)
	}
}

func TestRemoteProgressWriterWithoutCallbackIsNilWriter(t *testing.T) {
	var output io.Writer = newRemoteProgressWriter(nil, "https://example.test/repo.git", ProgressStatusFetching)
	if output != nil {
		t.Fatalf("progress output = %#v, want nil", output)
	}
}

func TestRemoteProgressWriterHandlesUntrustedTransportInput(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{name: "positive", input: []byte("Receiving objects: 42%\r"), want: "Receiving objects: 42%"},
		{name: "negative", input: []byte(" \t\x00\r\n"), want: ""},
		{name: "malformed", input: []byte{'B', 'a', 'd', ':', ' ', 0xff, 0x00, 'x', '\r'}, want: "Bad: �x"},
		{name: "unrelated-collision", input: []byte("Repository objects: repo.git mirror 8%\r"), want: "Repository objects: repo.git mirror 8%"},
		{name: "unknown-future", input: []byte("Negotiating quantum deltas: 7 qubits\r"), want: "Negotiating quantum deltas: 7 qubits"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var updates []Progress
			writer := newRemoteProgressWriter(func(update Progress) error {
				updates = append(updates, update)
				return nil
			}, "https://example.test/repo.git", ProgressStatusFetching)
			n, err := writer.Write(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if n != len(tt.input) {
				t.Fatalf("bytes written = %d, want %d", n, len(tt.input))
			}
			if tt.want == "" {
				if len(updates) != 0 {
					t.Fatalf("updates = %#v, want none", updates)
				}
				return
			}
			want := []Progress{{Status: ProgressStatusFetching, Detail: tt.want}}
			if !slices.Equal(updates, want) {
				t.Fatalf("updates = %#v, want %#v", updates, want)
			}
		})
	}
}

func TestRemoteProgressWriterReturnsConsumedBytesOnCallbackError(t *testing.T) {
	progressErr := errors.New("progress sink failed")
	writer := newRemoteProgressWriter(func(Progress) error {
		return progressErr
	}, "https://example.test/repo.git", ProgressStatusFetching)
	n, err := writer.Write([]byte("ok\rnot consumed"))
	if n != len("ok\r") || !errors.Is(err, progressErr) {
		t.Fatalf("Write = %d, %v; want %d, %v", n, err, len("ok\r"), progressErr)
	}
}

func TestRemoteProgressWriterReportsConfiguredStatus(t *testing.T) {
	var got Progress
	writer := newRemoteProgressWriter(func(update Progress) error {
		got = update
		return nil
	}, "https://example.test/indexes.git", ProgressStatusPushing)
	if _, err := writer.Write([]byte("Writing objects: 75%\r")); err != nil {
		t.Fatal(err)
	}
	want := Progress{Status: ProgressStatusPushing, Detail: "Writing objects: 75%"}
	if got != want {
		t.Fatalf("update = %#v, want %#v", got, want)
	}
}

func TestRemoteSearchReindexFetchesUpdatedHead(t *testing.T) {
	remote := t.TempDir()
	writeFile(t, remote, "remote.txt", "remote alpha content\n")
	firstRev := commitSearchRepo(t, remote)
	root := t.TempDir()
	opts := Options{
		Root:                root,
		Remote:              remote,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}
	first, err := Run(t.Context(), fakeEmbedder{}, opts, "remote alpha")
	if err != nil {
		t.Fatal(err)
	}
	if first.Source.ResolvedRev != firstRev {
		t.Fatalf("resolved rev = %q, want %q", first.Source.ResolvedRev, firstRev)
	}

	writeFile(t, remote, "remote.txt", "remote beta content\n")
	secondRev := commitSearchRepoChange(t, remote, "second")
	opts.Reindex = true
	second, err := Run(t.Context(), fakeEmbedder{}, opts, "remote beta")
	if err != nil {
		t.Fatal(err)
	}
	if second.Source.ResolvedRev != secondRev || second.Source.ResolvedRev == firstRev {
		t.Fatalf("resolved rev = %q, want new %q", second.Source.ResolvedRev, secondRev)
	}
	if len(second.Results) == 0 || !strings.Contains(second.Results[0].Excerpt, "remote beta content") {
		t.Fatalf("results = %#v, want beta content", second.Results)
	}
}

func TestRemoteSearchExplicitRevCanResolveParent(t *testing.T) {
	remote := t.TempDir()
	writeFile(t, remote, "remote.txt", "remote alpha content\n")
	firstRev := commitSearchRepo(t, remote)
	writeFile(t, remote, "remote.txt", "remote beta content\n")
	_ = commitSearchRepoChange(t, remote, "second")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                t.TempDir(),
		Remote:              remote,
		Rev:                 "HEAD~1",
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}, "remote alpha")
	if err != nil {
		t.Fatal(err)
	}
	if out.Source.ResolvedRev != firstRev {
		t.Fatalf("resolved rev = %q, want parent %q", out.Source.ResolvedRev, firstRev)
	}
	if len(out.Results) == 0 || !strings.Contains(out.Results[0].Excerpt, "remote alpha content") {
		t.Fatalf("results = %#v, want alpha content", out.Results)
	}
}

func TestRemoteSearchCanResolveParentAfterShallowHeadCache(t *testing.T) {
	remote := t.TempDir()
	writeFile(t, remote, "remote.txt", "remote alpha content\n")
	firstRev := commitSearchRepo(t, remote)
	writeFile(t, remote, "remote.txt", "remote beta content\n")
	_ = commitSearchRepoChange(t, remote, "second")
	root := t.TempDir()
	baseOpts := Options{
		Root:                root,
		Remote:              remote,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}
	if _, err := Run(t.Context(), fakeEmbedder{}, baseOpts, "remote beta"); err != nil {
		t.Fatal(err)
	}

	baseOpts.Rev = "HEAD~1"
	out, err := Run(t.Context(), fakeEmbedder{}, baseOpts, "remote alpha")
	if err != nil {
		t.Fatal(err)
	}
	if out.Source.ResolvedRev != firstRev {
		t.Fatalf("resolved rev = %q, want parent %q", out.Source.ResolvedRev, firstRev)
	}
}

func TestSanitizeRemoteErrorDropsCredentialsQueryAndFragment(t *testing.T) {
	raw := "https://user:secret@example.test/repo.git?token=abc&x=1#access_token=def"
	got := giturl.Sanitize(raw)
	if got != "https://example.test/repo.git" {
		t.Fatalf("sanitized remote = %q", got)
	}
	errText := sanitizeRemoteError(errors.New("clone "+raw+" failed"), raw, got)
	if strings.Contains(errText, "secret") || strings.Contains(errText, "token=abc") || strings.Contains(errText, "access_token") {
		t.Fatalf("sanitized error leaked credential material: %q", errText)
	}
}

func TestRevisionSearchUsesIgnoreFilesFromResolvedCommit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "ignored-by-gitignore.txt\n")
	writeFile(t, root, ".gitagentignore", "ignored-by-rev.txt\nignored-dir/\nignored-binary.dat\n")
	writeFile(t, root, "notes.txt", "release notes live here\n")
	writeFile(t, root, "ignored-by-gitignore.txt", "release notes live here\n")
	writeFile(t, root, "ignored-by-rev.txt", "release notes live here\n")
	writeFile(t, root, "ignored-binary.dat", "release\x00notes\n")
	writeFile(t, root, "ignored-dir/file.txt", "release notes live here\n")
	writeFile(t, root, "icon.svg", `<svg xmlns="http://www.w3.org/2000/svg"><title>release notes live here</title></svg>`)
	writeFile(t, root, "binary.dat", "release\x00notes\n")
	writeFile(t, root, "large.txt", strings.Repeat("x", int(MaxFileBytes)+1))
	rev := commitSearchRepo(t, root)

	writeFile(t, root, ".gitagentignore", "notes.txt\n")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Rev:                 rev,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "release notes")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %#v", out.Results)
	}
	if got := out.Results[0].Range; got != "notes.txt:1-1" {
		t.Fatalf("range = %q", got)
	}
	if out.Retrieval.Skipped.NonText != 1 {
		t.Fatalf("non-text skipped = %d, want 1", out.Retrieval.Skipped.NonText)
	}
	if out.Retrieval.Skipped.Binary != 1 {
		t.Fatalf("binary skipped = %d, want 1", out.Retrieval.Skipped.Binary)
	}
	if out.Retrieval.Skipped.Oversized != 1 {
		t.Fatalf("oversized skipped = %d, want 1", out.Retrieval.Skipped.Oversized)
	}
	for _, want := range []SkippedFile{
		{Path: "binary.dat", Reason: "binary"},
		{Path: "icon.svg", Reason: "non_text"},
		{Path: "large.txt", Reason: "oversized"},
	} {
		if !slices.Contains(out.Diagnostics.SkippedFiles, want) {
			t.Fatalf("skipped files missing %#v: %#v", want, out.Diagnostics.SkippedFiles)
		}
	}
	if slices.ContainsFunc(out.Diagnostics.SkippedFiles, func(file SkippedFile) bool {
		return file.Path == "ignored-binary.dat"
	}) {
		t.Fatalf("skipped files include ignored binary: %#v", out.Diagnostics.SkippedFiles)
	}
}

func TestRemoteStreamAppliesTargetTreeFilters(t *testing.T) {
	matcher := buildRevisionIgnoreMatcher([]revisionIgnoreFile{
		{path: ".gitignore", name: ".gitignore", text: "ignored.txt\n"},
		{path: ".gitagentignore", name: ".gitagentignore", text: "ignored-dir/\nignored-binary.dat\n"},
	})
	if revisionPathIgnored(matcher, "pkg/keep.go") {
		t.Fatal("fixture ignore matcher excludes pkg/keep.go")
	}
	remote := t.TempDir()
	writeFile(t, remote, ".gitignore", "ignored.txt\n")
	writeFile(t, remote, ".gitagentignore", "ignored-dir/\nignored-binary.dat\n")
	writeFile(t, remote, "pkg/keep.go", "package pkg\n")
	writeFile(t, remote, "pkg/keep_test.go", "package pkg\n")
	writeFile(t, remote, "ignored.txt", "ignored\n")
	writeFile(t, remote, "ignored-dir/file.go", "package ignored\n")
	writeFile(t, remote, "ignored-binary.dat", "ignored\x00binary\n")
	writeFile(t, remote, "binary.dat", "binary\x00data\n")
	writeFile(t, remote, "large.go", strings.Repeat("x", int(MaxFileBytes)+1))
	_ = commitSearchRepo(t, remote)

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root: t.TempDir(), Remote: remote, CodeOnly: true, NoTests: true,
		IndexOnly: true, MinScore: DefaultMinScore, Limit: DefaultLimit,
		EmbeddingModel: "test-model", EmbeddingDimensions: 3,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.Diagnostics.Files != 2 || out.Diagnostics.Chunks != 2 {
		t.Fatalf("diagnostics = %#v, want both scoped Go files indexed", out.Diagnostics)
	}
	if out.Retrieval.Skipped.Binary != 1 || out.Retrieval.Skipped.Oversized != 1 {
		t.Fatalf("skipped = %#v", out.Retrieval.Skipped)
	}
	listed, err := ListIndexFiles(t.Context(), ListFilesOptions{Root: t.TempDir(), Remote: remote, NoTests: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(listed.Files, []string{"pkg/keep.go"}) {
		t.Fatalf("listed files = %#v", listed.Files)
	}

	cached, err := Run(t.Context(), &countingEmbedder{}, Options{
		Root: t.TempDir(), Remote: remote, CodeOnly: true, NoTests: true,
		IndexOnly: true, MinScore: DefaultMinScore, Limit: DefaultLimit,
		EmbeddingModel: "test-model", EmbeddingDimensions: 3,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if cached.Retrieval.Skipped.Oversized != 1 || cached.Diagnostics.Files != 2 {
		t.Fatalf("cached diagnostics = %#v, skipped = %#v", cached.Diagnostics, cached.Retrieval.Skipped)
	}
}

func TestRemoteStreamEmbeddingStartsBeforePackCompletes(t *testing.T) {
	files := make(chan fileContent)
	releasePack := make(chan struct{})
	packCompleted := make(chan struct{})
	embedder := newBlockingEmbedder()
	t.Cleanup(embedder.releaseEmbeddings)
	done := make(chan error, 1)
	metadataDir := t.TempDir()
	indexDir := filepath.Join(t.TempDir(), "index")
	bodyStore, err := newChunkBodyStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := bodyStore.close(); err != nil {
			t.Error(err)
		}
	})
	go func() {
		_, err := indexRemoteFileStream(t.Context(), embedder, files, metadataDir, indexDir, Options{
			Reindex: true, EmbeddingModel: "test-model", EmbeddingDimensions: 3,
			EmbeddingBatchInputs: 1, EmbeddingConcurrency: 1,
		}, false, bodyStore)
		done <- err
	}()
	go func() {
		files <- fileContent{path: "ready.go", source: "revision", text: "package ready\n", size: 14}
		<-releasePack
		close(files)
		close(packCompleted)
	}()
	<-embedder.entered
	select {
	case <-packCompleted:
		t.Fatal("pack completed before embedding started")
	default:
	}
	embedder.releaseEmbeddings()
	close(releasePack)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCachedRemoteTreatsFilteredBlobAsOversized(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "keep.go", "package keep\n")
	writeFile(t, root, "large.go", strings.Repeat("x", int(MaxFileBytes)+1))
	rev := commitSearchRepo(t, root)
	repo, err := git.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(plumbing.NewHash(rev))
	if err != nil {
		t.Fatal(err)
	}
	large, err := commit.File("large.go")
	if err != nil {
		t.Fatal(err)
	}
	hash := large.Hash.String()
	if err := os.Remove(filepath.Join(root, ".git", "objects", hash[:2], hash[2:])); err != nil {
		t.Fatal(err)
	}
	wrapped, err := gitctx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	var files []fileContent
	skipped, skippedFiles, err := discoverCachedRemoteFiles(wrapped, rev, nil, func(file fileContent) error {
		files = append(files, file)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].path != "keep.go" || skipped.Oversized != 1 ||
		!slices.Contains(skippedFiles, SkippedFile{Path: "large.go", Reason: "oversized"}) {
		t.Fatalf("files = %#v, skipped = %#v, skipped files = %#v", files, skipped, skippedFiles)
	}
}

func TestExactReuseStopsAfterAllInputsResolve(t *testing.T) {
	metadataDir := t.TempDir()
	firstDir := filepath.Join(metadataDir, "search", "a")
	blockedDir := filepath.Join(metadataDir, "search", "b")
	if err := os.MkdirAll(firstDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(blockedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	chunk := chunksForFile(fileContent{path: "ready.go", source: "revision", text: "package ready\n", size: 14})[0]
	inputHash := chunkEmbeddingInputHash(chunk, 0)
	if err := writeJSON(filepath.Join(firstDir, "manifest.json"), manifest{
		Version: legacyIndexVersion, EmbeddingModel: "test-model", Dimensions: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(firstDir, "embeddings.json"), []vectorRecord{{
		EmbeddingInputHash: inputHash, EmbeddingModel: "test-model", Dimensions: 3, Vector: []float64{1, 0, 0},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(blockedDir, "manifest.json"), manifest{
		Version: legacyIndexVersion, EmbeddingModel: "test-model", Dimensions: 3,
	}); err != nil {
		t.Fatal(err)
	}
	lock, err := lockIndex(t.Context(), blockedDir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	records, err := loadExactReuseVectors(canceled, metadataDir, filepath.Join(metadataDir, "search", "target"), []Chunk{chunk}, Options{
		EmbeddingModel: "test-model", EmbeddingDimensions: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].EmbeddingInputHash != inputHash {
		t.Fatalf("records = %#v", records)
	}
}

func TestRevisionSearchScopeKeepsRootIgnoreRules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitagentignore", "ignored.txt\n")
	writeFile(t, root, "foo/keep.txt", "alpha\n")
	writeFile(t, root, "foo/ignored.txt", "alpha\n")
	writeFile(t, root, "bar/keep.txt", "alpha\n")
	rev := commitSearchRepo(t, root)

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Rev:                 rev,
		Scope:               []string{"foo"},
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].Range != "foo/keep.txt:1-1" {
		t.Fatalf("results = %#v", out.Results)
	}
}

func TestRevisionSearchScopeIncludesHiddenDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".foo/keep.txt", "alpha\n")
	writeFile(t, root, ".foo/.foo/.foo/deep.txt", "alpha\n")
	writeFile(t, root, "visible.txt", "alpha\n")
	rev := commitSearchRepo(t, root)

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Rev:                 rev,
		Scope:               []string{".foo"},
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".foo/.foo/.foo/deep.txt:1-1", ".foo/keep.txt:1-1"}
	if got := resultRanges(out.Results); !slices.Equal(got, want) {
		t.Fatalf("result ranges = %#v", got)
	}
}

func TestRevisionSearchScopeUsesIgnoreRulesInsideHiddenDir(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".foo/.gitagentignore", "ignored.txt\n")
	writeFile(t, root, ".foo/keep.txt", "alpha\n")
	writeFile(t, root, ".foo/ignored.txt", "alpha\n")
	rev := commitSearchRepo(t, root)

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Rev:                 rev,
		Scope:               []string{".foo"},
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if got := resultRanges(out.Results); !slices.Equal(got, []string{".foo/keep.txt:1-1"}) {
		t.Fatalf("result ranges = %#v", got)
	}
}

func TestShouldSkipPathHonorsScopedHiddenSubtree(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		scope []string
		want  bool
	}{
		{
			name:  "scoped hidden dir includes nested hidden dirs",
			path:  ".foo/.foo/.foo/deep.txt",
			scope: []string{".foo"},
			want:  false,
		},
		{
			name:  "visible scope does not include nested hidden dirs",
			path:  "foo/.foo/deep.txt",
			scope: []string{"foo"},
			want:  true,
		},
		{
			name:  "specific nested hidden scope includes subtree",
			path:  "foo/.foo/.bar/deep.txt",
			scope: []string{"foo/.foo"},
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSkipPath(tt.path, tt.scope); got != tt.want {
				t.Fatalf("shouldSkipPath(%q, %#v) = %v, want %v", tt.path, tt.scope, got, tt.want)
			}
		})
	}
}

func TestRevisionSearchScopeSkipsOutOfScopeBlobsBeforeLoading(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "foo/keep.txt", "alpha\n")
	rev := commitSearchRepo(t, root)
	rev = addMissingBlobToCommittedTree(t, root, rev, "bar.txt")

	out, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		Rev:                 rev,
		Scope:               []string{"foo"},
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].Range != "foo/keep.txt:1-1" {
		t.Fatalf("results = %#v", out.Results)
	}
}

func TestIsIndexableTextContentTypes(t *testing.T) {
	tests := []struct {
		name string
		path string
		data string
		want bool
	}{
		{
			name: "go source bypasses mime table",
			path: "main.go",
			data: "package main\nfunc main() {}\n",
			want: true,
		},
		{
			name: "tsx source bypasses non-code mime mapping",
			path: "component.tsx",
			data: "export function Component() { return <div /> }\n",
			want: true,
		},
		{
			name: "markdown text",
			path: "README.md",
			data: "# title\n",
			want: true,
		},
		{
			name: "json application text",
			path: "data.json",
			data: `{"release":"notes"}`,
			want: true,
		},
		{
			name: "yaml application text",
			path: "config.yaml",
			data: "release: notes\n",
			want: true,
		},
		{
			name: "toml application text",
			path: "config.toml",
			data: "release = \"notes\"\n",
			want: true,
		},
		{
			name: "sql application text",
			path: "schema.sql",
			data: "select 1;\n",
			want: true,
		},
		{
			name: "xml text",
			path: "feed.xml",
			data: "<feed />\n",
			want: true,
		},
		{
			name: "unknown plain text",
			path: "LOCKFILE",
			data: "release notes\n",
			want: true,
		},
		{
			name: "svg image xml",
			path: "icon.svg",
			data: `<svg xmlns="http://www.w3.org/2000/svg"><title>release notes</title></svg>`,
			want: false,
		},
		{
			name: "pdf by extension",
			path: "doc.pdf",
			data: "%PDF-1.7\nrelease notes\n",
			want: false,
		},
		{
			name: "png by extension",
			path: "image.png",
			data: "release notes\n",
			want: false,
		},
		{
			name: "octet stream by extension",
			path: "archive.bin",
			data: "release notes\n",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isIndexableText(tt.path, []byte(tt.data)); got != tt.want {
				t.Fatalf("isIndexableText(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestDiscoverFilesystemFilesClassifiesSkipReasons(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "keep.txt", "release notes\n")
	writeFile(t, root, "config.yaml", "release: notes\n")
	writeFile(t, root, "component.tsx", "export const releaseNotes = true\n")
	writeFile(t, root, "icon.svg", `<svg xmlns="http://www.w3.org/2000/svg"><title>release notes</title></svg>`)
	writeFile(t, root, "manual.pdf", "%PDF-1.7\nrelease notes\n")
	writeFile(t, root, "binary.dat", "release\x00notes\n")

	var files []fileContent
	skipped, skippedFiles, err := discoverFilesystemFiles(root, nil, func(string, ...slog.Attr) {}, func(file fileContent) error {
		files = append(files, file)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, file := range files {
		paths = append(paths, file.path)
	}
	wantPaths := []string{"component.tsx", "config.yaml", "keep.txt"}
	if strings.Join(paths, ",") != strings.Join(wantPaths, ",") {
		t.Fatalf("paths = %#v, want %#v", paths, wantPaths)
	}
	if skipped.Binary != 1 {
		t.Fatalf("binary skipped = %d, want 1", skipped.Binary)
	}
	if skipped.NonText != 2 {
		t.Fatalf("non-text skipped = %d, want 2", skipped.NonText)
	}
	wantSkipped := []SkippedFile{
		{Path: "binary.dat", Reason: "binary"},
		{Path: "icon.svg", Reason: "non_text"},
		{Path: "manual.pdf", Reason: "non_text"},
	}
	if fmt.Sprint(skippedFiles) != fmt.Sprint(wantSkipped) {
		t.Fatalf("skipped files = %#v, want %#v", skippedFiles, wantSkipped)
	}
}

func TestDiscoverFilesystemFilesHonorsAllowlistIgnoreRules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "*\n!.gitignore\n!allowed/\n!allowed/wanted.txt   \n!allowed/space\\ \n!plain\n!plain/wanted.txt\n")
	writeFile(t, root, "allowed/wanted.txt", "wanted\n")
	writeFile(t, root, "allowed/space ", "wanted\n")
	writeFile(t, root, "allowed/ignored.txt", "ignored\n")
	writeFile(t, root, "ignored/deep.txt", "ignored\n")
	writeFile(t, root, "plain/wanted.txt", "wanted\n")
	writeFile(t, root, "plain/ignored.txt", "ignored\n")
	writeFile(t, root, "plain/plain", "wanted\n")

	var paths []string
	_, _, err := discoverFilesystemFiles(root, nil, func(string, ...slog.Attr) {}, func(file fileContent) error {
		paths = append(paths, file.path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"allowed/space ", "allowed/wanted.txt", "plain/plain", "plain/wanted.txt"}; !slices.Equal(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestDiscoverRevisionFilesHonorsAllowlistIgnoreRules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "*\n!.gitignore\n!allowed/\n!allowed/wanted.txt   \n!allowed/space\\ \n!plain\n!plain/wanted.txt\n")
	writeFile(t, root, "allowed/wanted.txt", "wanted\n")
	writeFile(t, root, "allowed/space ", "wanted\n")
	writeFile(t, root, "allowed/ignored.txt", "ignored\n")
	writeFile(t, root, "ignored/deep.txt", "ignored\n")
	writeFile(t, root, "plain/wanted.txt", "wanted\n")
	writeFile(t, root, "plain/ignored.txt", "ignored\n")
	writeFile(t, root, "plain/plain", "wanted\n")
	rev := commitSearchRepo(t, root)

	var paths []string
	repo, err := gitctx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = discoverRevisionFiles(repo, rev, nil, func(string, ...slog.Attr) {}, func(file fileContent) error {
		paths = append(paths, file.path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"allowed/space ", "allowed/wanted.txt", "plain/plain", "plain/wanted.txt"}; !slices.Equal(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestDiscoverFilesystemFilesRestoresReincludedDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "restored/\n!restored/\n")
	writeFile(t, root, "restored/file.txt", "restored\n")
	writeFile(t, root, "restored/nested/file.txt", "restored\n")

	var paths []string
	_, _, err := discoverFilesystemFiles(root, nil, func(string, ...slog.Attr) {}, func(file fileContent) error {
		paths = append(paths, file.path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"restored/file.txt", "restored/nested/file.txt"}; !slices.Equal(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestDiscoverRevisionFilesRestoresReincludedDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "restored/\n!restored/\n")
	writeFile(t, root, "restored/file.txt", "restored\n")
	writeFile(t, root, "restored/nested/file.txt", "restored\n")
	rev := commitSearchRepo(t, root)

	var paths []string
	repo, err := gitctx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = discoverRevisionFiles(repo, rev, nil, func(string, ...slog.Attr) {}, func(file fileContent) error {
		paths = append(paths, file.path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"restored/file.txt", "restored/nested/file.txt"}; !slices.Equal(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func BenchmarkDiscoverFilesystemFiles(b *testing.B) {
	root := b.TempDir()
	const fileCount = 16
	content := strings.Repeat("searchable content\n", 4<<10)
	for i := range fileCount {
		writeFile(b, root, fmt.Sprintf("%02d.futuretext", i), content)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		visited := 0
		_, _, err := discoverFilesystemFiles(root, nil, func(string, ...slog.Attr) {}, func(file fileContent) error {
			visited++
			runtime.KeepAlive(file.text)
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
		if visited != fileCount {
			b.Fatalf("visited files = %d, want %d", visited, fileCount)
		}
	}
}

func TestFilesystemChunkingDoesNotRetainAcceptedBodies(t *testing.T) {
	root := t.TempDir()
	bodyStore, err := newChunkBodyStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := bodyStore.close(); err != nil {
			t.Error(err)
		}
	})
	builder := newSearchChunkBuilder(Options{}, false, bodyStore)
	const fileCount = 16
	content := strings.Repeat("x", 900<<10)
	for i := range fileCount {
		writeFile(t, root, fmt.Sprintf("%02d.futuretext", i), content)
	}
	content = ""
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	visited := 0
	var maxRetained uint64
	_, _, err = discoverFilesystemFiles(root, nil, func(string, ...slog.Attr) {}, func(file fileContent) error {
		wantPath := fmt.Sprintf("%02d.futuretext", visited)
		if file.path != wantPath {
			t.Fatalf("visited path = %q, want %q", file.path, wantPath)
		}
		visited++
		runtime.GC()
		var current runtime.MemStats
		runtime.ReadMemStats(&current)
		if current.HeapAlloc > baseline.HeapAlloc {
			maxRetained = max(maxRetained, current.HeapAlloc-baseline.HeapAlloc)
		}
		return builder.add(file)
	})
	if err != nil {
		t.Fatal(err)
	}
	if visited != fileCount {
		t.Fatalf("visited files = %d, want %d", visited, fileCount)
	}
	built := builder.finish()
	if len(built.chunks) != fileCount {
		t.Fatalf("retained chunks = %d, want %d", len(built.chunks), fileCount)
	}
	if maxRetained > 6*MaxFileBytes {
		t.Fatalf("discovery retained %d bytes across callbacks, want at most %d", maxRetained, 6*MaxFileBytes)
	}
}

func TestSearchChunkBuilderTransfersFileBodyOwnership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		path             string
		text             string
		codeOnly         bool
		wantFiles        int
		wantChunks       int
		wantCurrentKeys  bool
		wantChunkText    string
		wantEmbeddingSub string
	}{
		{
			name:             "positive selected source",
			path:             "main.go",
			text:             "package main\n\nfunc main() {}\n",
			codeOnly:         true,
			wantFiles:        1,
			wantChunks:       2,
			wantCurrentKeys:  true,
			wantEmbeddingSub: "path: main.go",
		},
		{
			name:            "negative filtered text retains identity only",
			path:            "notes.txt",
			text:            "context compaction instructions\n",
			codeOnly:        true,
			wantCurrentKeys: true,
		},
		{
			name:             "malformed go falls back to line chunks",
			path:             "broken.go",
			text:             "package broken\nfunc (\n",
			codeOnly:         true,
			wantFiles:        1,
			wantChunks:       1,
			wantCurrentKeys:  true,
			wantChunkText:    "package broken\nfunc (",
			wantEmbeddingSub: "package broken",
		},
		{
			name:            "unrelated extension collision stays filtered",
			path:            "archive.go.backup",
			text:            "package collision\n",
			codeOnly:        true,
			wantCurrentKeys: true,
		},
		{
			name:             "unknown future text type follows default ownership",
			path:             "contract.futuretext",
			text:             "future context format\n",
			wantFiles:        1,
			wantChunks:       1,
			wantChunkText:    "future context format",
			wantEmbeddingSub: "future context format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bodyStore, err := newChunkBodyStore()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := bodyStore.close(); err != nil {
					t.Error(err)
				}
			})
			file := fileContent{
				path:   tt.path,
				source: "filesystem",
				text:   tt.text,
				size:   int64(len(tt.text)),
			}
			builder := newSearchChunkBuilder(Options{
				EmbeddingModel:      "test-model",
				EmbeddingDimensions: 3,
			}, tt.codeOnly, bodyStore)
			if err := builder.add(file); err != nil {
				t.Fatal(err)
			}
			built := builder.finish()
			if built.fileCount != tt.wantFiles {
				t.Fatalf("selected files = %d, want %d", built.fileCount, tt.wantFiles)
			}
			if len(built.chunks) != tt.wantChunks {
				t.Fatalf("chunks = %d, want %d", len(built.chunks), tt.wantChunks)
			}
			if got := len(built.currentVectorKeys) > 0; got != tt.wantCurrentKeys {
				t.Fatalf("current vector keys present = %v, want %v", got, tt.wantCurrentKeys)
			}
			if tt.wantChunks == 0 {
				return
			}
			chunkText, err := loadChunkBodyForTest(built.chunks[0])
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantChunkText != "" && chunkText != tt.wantChunkText {
				t.Fatalf("chunk text = %q, want %q", chunkText, tt.wantChunkText)
			}
			embeddingInput := embeddingText(built.chunks[0], chunkText)
			if !strings.Contains(embeddingInput, tt.wantEmbeddingSub) {
				t.Fatalf("embedding input %q does not contain %q", embeddingInput, tt.wantEmbeddingSub)
			}
		})
	}
}

func TestChunkBodyStorePreservesChunkSnapshots(t *testing.T) {
	tests := []struct {
		name string
		path string
		text string
	}{
		{name: "normalized newlines", path: "notes.txt", text: "first\r\nsecond\r\n"},
		{name: "overlapping chunks", path: "large.futuretext", text: strings.Repeat("line\n", chunkLines+20)},
		{name: "empty body", path: "empty.txt", text: "\n"},
		{name: "generated path only", path: "generated.go", text: "// Code generated. DO NOT EDIT.\npackage generated\n\nvar Secret = 1\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := fileContent{path: tt.path, source: "filesystem", text: tt.text, size: int64(len(tt.text))}
			want := chunksForFile(file)
			bodyStore, err := newChunkBodyStore()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := bodyStore.close(); err != nil {
					t.Error(err)
				}
			})
			builder := newSearchChunkBuilder(Options{}, false, bodyStore)
			if err := builder.add(file); err != nil {
				t.Fatal(err)
			}
			got := builder.finish().chunks
			if len(got) != len(want) {
				t.Fatalf("chunks = %d, want %d", len(got), len(want))
			}
			for i := range got {
				if got[i].text != "" {
					t.Fatalf("chunk %d retained %d body bytes in memory", i, len(got[i].text))
				}
				body, err := loadChunkBodyForTest(got[i])
				if err != nil {
					t.Fatal(err)
				}
				if body != want[i].text {
					t.Fatalf("chunk %d body = %q, want %q", i, body, want[i].text)
				}
				if got[i].ContentHash != want[i].ContentHash {
					t.Fatalf("chunk %d content hash = %q, want %q", i, got[i].ContentHash, want[i].ContentHash)
				}
				if gotHash, wantHash := chunkEmbeddingInputHash(got[i], 0), chunkEmbeddingInputHash(want[i], 0); gotHash != wantHash {
					t.Fatalf("chunk %d embedding hash = %q, want %q", i, gotHash, wantHash)
				}
			}
			wantStoreSize := int64(len(normalizeChunkBody(tt.text)))
			hasBody := false
			for i := range got {
				hasBody = hasBody || !got[i].pathOnly
			}
			if !hasBody {
				wantStoreSize = 0
			}
			if bodyStore.size != wantStoreSize {
				t.Fatalf("snapshot bytes = %d, want %d", bodyStore.size, wantStoreSize)
			}
		})
	}
}

func TestChunkBodyStoreReadFailureIsReturned(t *testing.T) {
	bodyStore, err := newChunkBodyStore()
	if err != nil {
		t.Fatal(err)
	}
	builder := newSearchChunkBuilder(Options{}, false, bodyStore)
	if err := builder.add(fileContent{path: "future.txt", source: "filesystem", text: "future body"}); err != nil {
		t.Fatal(err)
	}
	chunk := builder.finish().chunks[0]
	if err := bodyStore.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := loadChunkBodyForTest(chunk); err == nil {
		t.Fatal("loadChunkBody succeeded after the snapshot store closed")
	}
}

func TestRemoteIndexLabelsChunkSnapshotReadFailure(t *testing.T) {
	bodyStore, err := newChunkBodyStore()
	if err != nil {
		t.Fatal(err)
	}
	files := make(chan fileContent, 1)
	files <- fileContent{path: "future.txt", source: "revision", text: "future body", size: 11}
	close(files)
	_, err = indexRemoteFileStream(t.Context(), &countingEmbedder{}, files, t.TempDir(), filepath.Join(t.TempDir(), "index"), Options{
		Reindex:                true,
		EmbeddingModel:         "test-model",
		EmbeddingDimensions:    3,
		EmbeddingBatchInputs:   1,
		EmbeddingConcurrency:   1,
		EmbeddingBatchMaxChars: DefaultEmbeddingBatchMaxChars,
		ProgressLog: func(Progress) error {
			return bodyStore.close()
		},
	}, false, bodyStore)
	if err == nil {
		t.Fatal("indexRemoteFileStream succeeded after the snapshot store closed")
	}
	if !strings.Contains(err.Error(), "prepare embedding inputs") {
		t.Fatalf("error = %q, want local input preparation label", err)
	}
	if strings.Contains(err.Error(), "embedding request failed") {
		t.Fatalf("error = %q, local snapshot failure mislabeled as provider failure", err)
	}
}

func TestPreparedChunkEmbeddingPlanningDoesNotAllocate(t *testing.T) {
	line := strings.Repeat("x", maxEmbeddingLineChars+500)
	chunks := chunksForFile(fileContent{
		path:   "large.futuretext",
		source: "filesystem",
		text:   strings.Repeat(line+"\n", chunkLines+1),
	})
	prepareChunkEmbeddingMetadata(chunks, DefaultEmbeddingMaxInputChars)

	allocs := testing.AllocsPerRun(100, func() {
		_ = totalChunkEmbeddingInputChars(chunks, DefaultEmbeddingMaxInputChars)
		_ = chunkEmbeddingBatchEnd(chunks, 0, DefaultEmbeddingBatchInputs, DefaultEmbeddingBatchMaxChars, DefaultEmbeddingMaxInputChars)
		for _, chunk := range chunks {
			_ = chunkEmbeddingInputHash(chunk, DefaultEmbeddingMaxInputChars)
		}
		runtime.KeepAlive(chunks)
	})
	if allocs != 0 {
		t.Fatalf("prepared embedding planning allocated %.2f objects per run", allocs)
	}
}

func BenchmarkPreparedChunkEmbeddingPlanning(b *testing.B) {
	line := strings.Repeat("x", maxEmbeddingLineChars+500)
	chunks := chunksForFile(fileContent{
		path:   "large.futuretext",
		source: "filesystem",
		text:   strings.Repeat(line+"\n", chunkLines+1),
	})
	prepareChunkEmbeddingMetadata(chunks, DefaultEmbeddingMaxInputChars)
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		_ = totalChunkEmbeddingInputChars(chunks, DefaultEmbeddingMaxInputChars)
		_ = chunkEmbeddingBatchEnd(chunks, 0, DefaultEmbeddingBatchInputs, DefaultEmbeddingBatchMaxChars, DefaultEmbeddingMaxInputChars)
		for _, chunk := range chunks {
			_ = chunkEmbeddingInputHash(chunk, DefaultEmbeddingMaxInputChars)
		}
	}
	runtime.KeepAlive(chunks)
}

func BenchmarkPrepareChunkEmbeddingMetadata(b *testing.B) {
	line := strings.Repeat("x", maxEmbeddingLineChars+500)
	chunks := chunksForFile(fileContent{
		path:   "large.futuretext",
		source: "filesystem",
		text:   strings.Repeat(line+"\n", chunkLines+1),
	})
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		for i := range chunks {
			chunks[i].embeddingInputHash = ""
			chunks[i].embeddingInputBytes = 0
			chunks[i].embeddingMaxChars = 0
		}
		prepareChunkEmbeddingMetadata(chunks, DefaultEmbeddingMaxInputChars)
	}
	runtime.KeepAlive(chunks)
}

func BenchmarkChunkEmbeddingInputs(b *testing.B) {
	bodyStore, err := newChunkBodyStore()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := bodyStore.close(); err != nil {
			b.Error(err)
		}
	})
	line := strings.Repeat("x", maxEmbeddingLineChars+500)
	builder := newSearchChunkBuilder(Options{EmbeddingMaxInput: DefaultEmbeddingMaxInputChars}, false, bodyStore)
	if err := builder.add(fileContent{
		path:   "large.futuretext",
		source: "filesystem",
		text:   strings.Repeat(line+"\n", chunkLines+1),
	}); err != nil {
		b.Fatal(err)
	}
	chunks := builder.finish().chunks
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		inputs, err := chunkEmbeddingInputs(chunks, DefaultEmbeddingMaxInputChars)
		if err != nil {
			b.Fatal(err)
		}
		runtime.KeepAlive(inputs.texts)
		inputs.release()
	}
}

func TestDenseHandwrittenGoFilesKeepSymbolChunks(t *testing.T) {
	var b strings.Builder
	b.WriteString("package handwritten\n\n")
	for i := range 60 {
		fmt.Fprintf(&b, "func F%d() {}\n", i)
	}

	chunks := chunksForFile(fileContent{
		path:   "handwritten.go",
		source: "filesystem",
		text:   b.String(),
	})
	if len(chunks) < 60 {
		t.Fatalf("chunks = %d, want symbol chunks retained", len(chunks))
	}
	hasFunction := false
	for _, chunk := range chunks {
		if chunk.Symbol != nil && chunk.Symbol.Type == "function" {
			hasFunction = true
			break
		}
	}
	if !hasFunction {
		t.Fatalf("chunks have no function symbols: %#v", chunks)
	}
}

func TestLargeGoDeclarationsAreSplit(t *testing.T) {
	var b strings.Builder
	b.WriteString("package handwritten\n\nfunc Large() {\n")
	for i := range chunkLines + 20 {
		fmt.Fprintf(&b, "\t_ = %d\n", i)
	}
	b.WriteString("}\n")

	chunks := chunksForFile(fileContent{
		path:   "large.go",
		source: "filesystem",
		text:   b.String(),
	})
	foundLarge := false
	for _, chunk := range chunks {
		if chunk.EndLine-chunk.StartLine+1 > chunkLines {
			t.Fatalf("chunk range = %d-%d, want at most %d lines", chunk.StartLine, chunk.EndLine, chunkLines)
		}
		if chunk.Symbol != nil && chunk.Symbol.Name == "Large" {
			foundLarge = true
		}
	}
	if !foundLarge {
		t.Fatalf("large function symbol missing: %#v", chunks)
	}
}

func TestGeneratedGoFilesUsePathOnlyChunks(t *testing.T) {
	file := fileContent{
		path:   "internal/web/uc/types/user_profile.go",
		source: "filesystem",
		text: strings.Join([]string{
			"// database exporter output. DO NOT EDIT.",
			"package types",
			"",
			"type UserProfile struct {",
			"    SecretGeneratedField string",
			"}",
			"",
		}, "\n"),
		size: 1234,
	}
	chunks := chunksForFile(file)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want one path-only chunk", len(chunks))
	}
	chunk := chunks[0]
	if chunk.text != "" || excerptText(chunk, chunk.text) != "" {
		t.Fatalf("generated content leaked into chunk text/excerpt: text=%q excerpt=%q", chunk.text, excerptText(chunk, chunk.text))
	}
	embeddingInput := chunkEmbeddingText(chunk)
	if !strings.Contains(embeddingInput, "path: internal/web/uc/types/user_profile.go") {
		t.Fatalf("embedding text missing path: %q", embeddingInput)
	}
	if strings.Contains(embeddingInput, "SecretGeneratedField") || strings.Contains(embeddingInput, "UserProfile struct") {
		t.Fatalf("generated content leaked into embedding text: %q", embeddingInput)
	}

	changed := file
	changed.text = strings.ReplaceAll(changed.text, "SecretGeneratedField", "DifferentGeneratedField")
	if got := chunksForFile(changed)[0].ContentHash; got != chunk.ContentHash {
		t.Fatalf("path-only content hash changed with generated body: %s != %s", got, chunk.ContentHash)
	}
}

func TestEmbeddingTextClampsLongLines(t *testing.T) {
	longLine := strings.Repeat("x", maxEmbeddingLineChars+100)
	chunks := chunksForFile(fileContent{
		path:   "bundle.js",
		source: "filesystem",
		text:   longLine,
		size:   int64(len(longLine)),
	})
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want one chunk", len(chunks))
	}
	if got := chunks[0].text; got != longLine {
		t.Fatal("chunk text was clamped")
	}
	embeddingInput := chunkEmbeddingText(chunks[0])
	_, body, ok := strings.Cut(embeddingInput, "\n\n")
	if !ok {
		t.Fatalf("embedding text missing metadata separator: %q", embeddingInput)
	}
	if got := len([]rune(body)); got != maxEmbeddingLineChars {
		t.Fatalf("embedding body chars = %d, want %d", got, maxEmbeddingLineChars)
	}
}

func TestDoNotEditAfterPackageDoesNotMarkGenerated(t *testing.T) {
	chunks := chunksForFile(fileContent{
		path:   "handwritten.go",
		source: "filesystem",
		text: strings.Join([]string{
			"package handwritten",
			"",
			"// DO NOT EDIT this constant without checking callers.",
			"const Value = 1",
			"",
		}, "\n"),
	})
	if chunks[0].text == "" {
		t.Fatal("post-package DO NOT EDIT comment incorrectly marked file generated")
	}
}

func TestReindexIgnoresExistingVectors(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")

	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), fakeEmbedder{}, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if first.Retrieval.Index != "miss" {
		t.Fatalf("first index = %q", first.Retrieval.Index)
	}
	second, err := Run(t.Context(), fakeEmbedder{}, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if second.Retrieval.Index != "hit" || second.Replay.Mode != "hit" {
		t.Fatalf("second retrieval = %#v replay = %#v", second.Retrieval, second.Replay)
	}
	opts.Reindex = true
	third, err := Run(t.Context(), fakeEmbedder{}, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if third.Retrieval.Index != "miss" {
		t.Fatalf("reindex index = %q", third.Retrieval.Index)
	}
}

func TestSearchIgnoresStaleIndexVersion(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	firstCalls := embedder.callCount()

	manifestPath := filepath.Join(first.Diagnostics.IndexDir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), fmt.Sprintf(`"version":%d`, indexVersion), fmt.Sprintf(`"version":%d`, indexVersion-1), 1))
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	second, err := Run(t.Context(), embedder, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if second.Retrieval.Index != "miss" {
		t.Fatalf("second index = %q, want miss", second.Retrieval.Index)
	}
	if embedder.callCount() <= firstCalls {
		t.Fatalf("embedding calls after stale manifest = %d, want > %d", embedder.callCount(), firstCalls)
	}
}

func TestSearchPersistsIndexAfterAllEmbeddingsSucceed(t *testing.T) {
	root := t.TempDir()
	for i := range DefaultEmbeddingBatchInputs + 1 {
		writeFile(t, root, filepath.Join("pkg", fmt.Sprintf("file_%03d.txt", i)), "alpha\n")
	}

	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		IndexOnly:           true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	firstEmbedder := failOnPathEmbedder("file_010.txt")
	if _, err := Run(t.Context(), firstEmbedder, opts, ""); err == nil {
		t.Fatal("expected embedding failure")
	}

	secondEmbedder := &countingEmbedder{}
	second, err := Run(t.Context(), secondEmbedder, opts, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Diagnostics.ReusedChunks != 0 {
		t.Fatalf("reused chunks = %d, want 0", second.Diagnostics.ReusedChunks)
	}
	if second.Diagnostics.EmbeddedChunks != DefaultEmbeddingBatchInputs+1 || second.Diagnostics.EmbeddedDone != DefaultEmbeddingBatchInputs+1 {
		t.Fatalf("embedding diagnostics = %#v", second.Diagnostics)
	}
	if secondEmbedder.callCount() != 2 {
		t.Fatalf("embedding calls = %d, want 2", secondEmbedder.callCount())
	}
}

func TestParallelSearchWaitsForIndexWriterAndReusesIndex(t *testing.T) {
	for _, tt := range []struct {
		name    string
		reindex bool
	}{
		{name: "missing", reindex: false},
		{name: "reindex", reindex: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "alpha.txt", "alpha\n")

			embedder := newBlockingEmbedder()
			opts := Options{
				Root:                root,
				MinScore:            0.70,
				Limit:               10,
				IndexOnly:           true,
				Reindex:             tt.reindex,
				EmbeddingModel:      "text-embedding-3-small",
				EmbeddingDimensions: 3,
				APIKey:              "test-key",
				BaseURL:             "http://example.test",
			}

			ctx := t.Context()
			var wg sync.WaitGroup
			errs := make(chan error, 6)
			wg.Go(func() {
				out, err := Run(ctx, embedder, opts, "")
				if err == nil && out.Retrieval.Index != "miss" {
					err = fmt.Errorf("first index = %q, want miss", out.Retrieval.Index)
				}
				errs <- err
			})
			select {
			case <-embedder.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first search did not start embedding")
			}

			for range 5 {
				wg.Go(func() {
					out, err := Run(ctx, embedder, opts, "")
					if err == nil && out.Retrieval.Index != "hit" {
						err = fmt.Errorf("waiter index = %q, want hit", out.Retrieval.Index)
					}
					errs <- err
				})
			}
			select {
			case <-embedder.secondCall:
				t.Fatal("parallel waiter embedded before first writer finished")
			case <-time.After(100 * time.Millisecond):
			}

			embedder.releaseEmbeddings()
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := embedder.calls.Load(); got != 1 {
				t.Fatalf("embedding calls after parallel searches = %d, want 1", got)
			}
		})
	}
}

func TestParallelSearchWaitsForQueryEmbeddingAndReusesHistory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "alpha.txt", "alpha\n")

	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		IndexOnly:           true,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	if _, err := Run(t.Context(), fakeEmbedder{}, opts, ""); err != nil {
		t.Fatal(err)
	}

	opts.IndexOnly = false
	embedder := newBlockingQueryEmbedder(queryEmbeddingText("alpha", 0))
	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	wg.Go(func() {
		out, err := Run(ctx, embedder, opts, "alpha")
		if err == nil && len(out.Results) == 0 {
			err = errors.New("first search returned no results")
		}
		errs <- err
	})
	select {
	case <-embedder.blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first search did not start query embedding")
	}

	for range 5 {
		wg.Go(func() {
			out, err := Run(ctx, embedder, opts, "alpha")
			if err == nil && len(out.Results) == 0 {
				err = errors.New("waiter search returned no results")
			}
			errs <- err
		})
	}
	select {
	case <-embedder.blocking.secondCall:
		t.Fatal("parallel waiter embedded query before first query embedding finished")
	case <-time.After(100 * time.Millisecond):
	}

	embedder.blocking.releaseEmbeddings()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := embedder.blocking.calls.Load(); got != 1 {
		t.Fatalf("query embedding calls after parallel searches = %d, want 1", got)
	}
}

func TestSearchBatchesIndexEmbeddingsAndCachesExactQueryEmbedding(t *testing.T) {
	root := t.TempDir()
	for i := range 130 {
		writeFile(t, root, filepath.Join("pkg", fmt.Sprintf("file_%03d.go", i)), "package pkg\n\nfunc alpha() {}\n")
	}

	embedder := &countingEmbedder{}
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	first, err := Run(t.Context(), embedder, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if first.Retrieval.Index != "miss" {
		t.Fatalf("first index = %q", first.Retrieval.Index)
	}
	for _, name := range []string{"vectors.f32", "vectors.index.json"} {
		if _, err := os.Stat(filepath.Join(first.Diagnostics.IndexDir, name)); err != nil {
			t.Fatalf("missing binary vector cache %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(first.Diagnostics.IndexDir, "chunks.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("obsolete chunks cache exists: %v", err)
	}
	firstCalls := embedder.callCount()
	if firstCalls <= 2 {
		t.Fatalf("embedding calls after first run = %d, want multiple bounded batches + query", firstCalls)
	}
	if embedder.maxBatchSize() > DefaultEmbeddingBatchInputs {
		t.Fatalf("max embedding batch = %d, want <= %d", embedder.maxBatchSize(), DefaultEmbeddingBatchInputs)
	}

	second, err := Run(t.Context(), embedder, opts, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if second.Retrieval.Index != "hit" || second.Replay.Mode != "hit" {
		t.Fatalf("second retrieval = %#v replay = %#v", second.Retrieval, second.Replay)
	}
	if embedder.callCount() != firstCalls {
		t.Fatalf("embedding calls after exact replay = %d, want unchanged from %d", embedder.callCount(), firstCalls)
	}
}

func TestSearchReportsProgressWhenIndexNeedsUpdate(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "first.txt", "alpha\n")
	opts := Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}
	if _, err := Run(t.Context(), fakeEmbedder{}, opts, "alpha"); err != nil {
		t.Fatal(err)
	}

	writeFile(t, root, "second.txt", "alpha\n")
	var calls []Progress
	opts.ProgressLog = func(progress Progress) error {
		calls = append(calls, Progress{Done: progress.Done, Total: progress.Total, Reused: progress.Reused})
		return nil
	}
	if _, err := Run(t.Context(), fakeEmbedder{}, opts, "alpha"); err != nil {
		t.Fatal(err)
	}
	want := []Progress{
		{Total: 1, Reused: 1},
		{Done: 1, Total: 1, Reused: 1},
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("progress calls = %#v, want %#v", calls, want)
	}
}

func TestSearchProgressErrorStopsBeforeEmbedding(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "alpha.txt", "alpha\n")
	progressErr := errors.New("progress unavailable")
	embedder := &countingEmbedder{}

	_, err := Run(t.Context(), embedder, Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
		ProgressLog: func(Progress) error {
			return progressErr
		},
	}, "alpha")
	if !errors.Is(err, progressErr) {
		t.Fatalf("error = %v, want %v", err, progressErr)
	}
	if embedder.callCount() != 0 {
		t.Fatalf("embedding calls = %d, want 0", embedder.callCount())
	}
}

func TestSearchSplitsRejectedEmbeddingBatches(t *testing.T) {
	root := t.TempDir()
	for i := range 12 {
		writeFile(t, root, filepath.Join("pkg", fmt.Sprintf("file_%03d.go", i)), "package pkg\n\nfunc alpha() {}\n")
	}

	embedder := &splittingEmbedder{}
	out, err := Run(t.Context(), embedder, Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if out.Retrieval.Index != "miss" || len(out.Results) == 0 {
		t.Fatalf("output = %#v", out)
	}
	if embedder.calls.Load() <= 2 {
		t.Fatalf("embedding calls = %d, want split retries", embedder.calls.Load())
	}
}

func TestSearchTruncatesEmbeddingInputs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "alpha.txt", strings.Repeat("alpha ", 100))

	out, err := Run(t.Context(), lengthLimitEmbedder{max: 32}, Options{
		Root:                root,
		MinScore:            0.70,
		Limit:               10,
		EmbeddingModel:      "text-embedding-3-small",
		EmbeddingDimensions: 3,
		EmbeddingMaxInput:   32,
		APIKey:              "test-key",
		BaseURL:             "http://example.test",
	}, strings.Repeat("alpha ", 100))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) == 0 {
		t.Fatalf("results = %#v", out.Results)
	}
}

func TestEmbeddingConcurrencyUsesGOMAXPROCSCappedAtEight(t *testing.T) {
	old := runtime.GOMAXPROCS(20)
	defer runtime.GOMAXPROCS(old)

	if got := embeddingConcurrency(Options{}); got != 8 {
		t.Fatalf("embedding concurrency = %d, want cap 8", got)
	}

	runtime.GOMAXPROCS(6)
	if got := embeddingConcurrency(Options{}); got != 6 {
		t.Fatalf("embedding concurrency = %d, want GOMAXPROCS", got)
	}

	if got := embeddingConcurrency(Options{EmbeddingConcurrency: 12}); got != 12 {
		t.Fatalf("embedding concurrency override = %d, want 12", got)
	}
}

func TestEmbeddingBatchTuning(t *testing.T) {
	embedder := &countingEmbedder{}
	texts := []string{"aaaa", "bbbb", "cccc", "dddd", "eeee", "fffff", "ggggg"}

	_, _, err := embedTexts(t.Context(), embedder, Options{
		EmbeddingModel:         "text-embedding-3-small",
		EmbeddingDimensions:    3,
		EmbeddingBatchInputs:   3,
		EmbeddingBatchMaxChars: 10,
		APIKey:                 "test-key",
		BaseURL:                "http://example.test",
	}, texts)
	if err != nil {
		t.Fatal(err)
	}
	if embedder.callCount() != 4 {
		t.Fatalf("embedding calls = %d, want 4", embedder.callCount())
	}
	if embedder.maxBatchSize() > 3 {
		t.Fatalf("max embedding batch = %d, want <= 3", embedder.maxBatchSize())
	}
}

func writeFile(t testing.TB, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFileSize(t *testing.T, path string, want int64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != want {
		t.Fatalf("%s size = %d, want %d", path, info.Size(), want)
	}
}

func writeLegacyBinaryVectors(t *testing.T, dir string, records []vectorRecord) {
	t.Helper()
	var payload []byte
	index := make([]vectorIndexRecord, len(records))
	for i, record := range records {
		index[i] = vectorIndexRecordFor(record)
		index[i].Offset = int64(len(payload))
		payload = append(payload, encodeVector(record.Vector)...)
	}
	if err := os.WriteFile(filepath.Join(dir, "vectors.f32"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "vectors.index.json"), index); err != nil {
		t.Fatal(err)
	}
}

func commitSearchRepo(t *testing.T, root string) string {
	t.Helper()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := repo.Config()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Commit.GpgSign = config.OptBoolFalse
	if err := repo.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := worktree.AddGlob("."); err != nil {
		t.Fatal(err)
	}
	hash, err := worktree.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Search Test",
			Email: "search@example.test",
			When:  time.Unix(0, 0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return hash.String()
}

func commitSearchRepoChange(t *testing.T, root, message string) string {
	t.Helper()
	repo, err := git.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := worktree.AddGlob("."); err != nil {
		t.Fatal(err)
	}
	hash, err := worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Search Test",
			Email: "search@example.test",
			When:  time.Unix(0, 0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return hash.String()
}

func addMissingBlobToCommittedTree(t *testing.T, root, rev, path string) string {
	t.Helper()
	repo, err := git.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := repo.CommitObject(plumbing.NewHash(rev))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatal(err)
	}
	tree.Entries = append(tree.Entries, object.TreeEntry{
		Name: path,
		Mode: filemode.Regular,
		Hash: plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	})
	slices.SortFunc(tree.Entries, func(a, b object.TreeEntry) int {
		return strings.Compare(a.Name, b.Name)
	})
	treeObj := repo.Storer.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		t.Fatal(err)
	}
	treeHash, err := repo.Storer.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatal(err)
	}
	commit.TreeHash = treeHash
	commitObj := repo.Storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		t.Fatal(err)
	}
	hash, err := repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatal(err)
	}
	return hash.String()
}

func vectorFor(input string) []float64 {
	input = strings.ToLower(input)
	switch {
	case strings.Contains(input, "opposite"):
		return []float64{-1, 0, 0}
	case strings.Contains(input, "release"):
		return []float64{0, 1, 0}
	case strings.Contains(input, "alpha"):
		return []float64{1, 0, 0}
	default:
		return []float64{0, 0, 1}
	}
}

func resultRanges(results []Result) []string {
	ranges := make([]string, 0, len(results))
	for _, result := range results {
		ranges = append(ranges, result.Range)
	}
	slices.Sort(ranges)
	return ranges
}

type lengthLimitEmbedder struct {
	max int
}

func (e lengthLimitEmbedder) CreateEmbeddings(_ context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	for _, input := range request.Inputs {
		if len([]rune(input)) > e.max {
			return openai.EmbeddingResponse{}, fmt.Errorf("input length = %d, want <= %d", len([]rune(input)), e.max)
		}
	}
	return fakeEmbedder{}.CreateEmbeddings(context.Background(), request)
}
