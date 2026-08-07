package search

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const (
	searchProducerProcessHelperEnv     = "GIT_AGENT_SEARCH_PRODUCER_HELPER"
	searchProducerProcessHelperHomeEnv = "GIT_AGENT_SEARCH_PRODUCER_HOME"
)

func TestConcurrentRemoteSearchUsesOneGlobalFlight(t *testing.T) {
	for _, reindex := range []bool{false, true} {
		t.Run(fmt.Sprintf("reindex=%t", reindex), func(t *testing.T) {
			remote := t.TempDir()
			writeFile(t, remote, "alpha.txt", "alpha\n")
			commitSearchRepo(t, remote)
			t.Setenv("HOME", t.TempDir())

			embedder := newBlockingEmbedder()
			t.Cleanup(embedder.releaseEmbeddings)
			waiting := make(chan struct{}, 5)
			opts := Options{
				Root:                t.TempDir(),
				Remote:              remote,
				IndexOnly:           true,
				Reindex:             reindex,
				MinScore:            DefaultMinScore,
				Limit:               DefaultLimit,
				EmbeddingModel:      "test-model",
				EmbeddingDimensions: 3,
				ProgressLog: func(progress Progress) error {
					if progress.Status == ProgressStatusWaiting {
						waiting <- struct{}{}
					}
					return nil
				},
			}

			ctx := t.Context()
			var group sync.WaitGroup
			errs := make(chan error, 6)
			group.Go(func() {
				out, err := Run(ctx, embedder, opts, "")
				if err == nil && out.Retrieval.Index != "miss" {
					err = fmt.Errorf("producer index = %q, want miss", out.Retrieval.Index)
				}
				errs <- err
			})
			select {
			case <-embedder.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("producer did not start remote indexing")
			}

			for range 5 {
				group.Go(func() {
					out, err := Run(ctx, embedder, opts, "")
					if err == nil && out.Retrieval.Index != "hit" {
						err = fmt.Errorf("waiter index = %q, want hit", out.Retrieval.Index)
					}
					errs <- err
				})
			}
			for range 5 {
				select {
				case <-waiting:
				case <-time.After(5 * time.Second):
					t.Fatal("remote waiter did not report the global lock wait")
				}
			}
			if got := embedder.calls.Load(); got != 1 {
				t.Fatalf("embedding calls while producer is blocked = %d, want 1", got)
			}

			embedder.releaseEmbeddings()
			group.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if got := embedder.calls.Load(); got != 1 {
				t.Fatalf("remote indexing calls = %d, want exactly 1", got)
			}
		})
	}
}

func TestWaitingRemoteReindexFetchesHeadAfterPriorBuild(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote := t.TempDir()
	writeFile(t, remote, "remote.txt", "remote alpha content\n")
	firstRev := commitSearchRepo(t, remote)

	embedder := newBlockingEmbedder()
	t.Cleanup(embedder.releaseEmbeddings)
	waiting := make(chan struct{}, 1)
	opts := Options{
		Root:                t.TempDir(),
		Remote:              remote,
		IndexOnly:           true,
		Reindex:             true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
		ProgressLog: func(progress Progress) error {
			if progress.Status == ProgressStatusWaiting {
				waiting <- struct{}{}
			}
			return nil
		},
	}
	type runResult struct {
		output Output
		err    error
	}
	firstDone := make(chan runResult, 1)
	secondDone := make(chan runResult, 1)
	go func() {
		output, err := Run(t.Context(), embedder, opts, "")
		firstDone <- runResult{output: output, err: err}
	}()
	select {
	case <-embedder.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first remote reindex did not reach embedding")
	}

	go func() {
		output, err := Run(t.Context(), embedder, opts, "")
		secondDone <- runResult{output: output, err: err}
	}()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("second remote reindex did not wait for the global producer")
	}

	writeFile(t, remote, "remote.txt", "remote beta content\n")
	secondRev := commitSearchRepoChange(t, remote, "second")
	embedder.releaseEmbeddings()

	first := <-firstDone
	if first.err != nil {
		t.Fatal(first.err)
	}
	if first.output.Source.ResolvedRev != firstRev {
		t.Fatalf("first resolved rev = %q, want %q", first.output.Source.ResolvedRev, firstRev)
	}
	second := <-secondDone
	if second.err != nil {
		t.Fatal(second.err)
	}
	if second.output.Source.ResolvedRev != secondRev {
		t.Fatalf("waiting resolved rev = %q, want fresh %q", second.output.Source.ResolvedRev, secondRev)
	}
	if got := embedder.calls.Load(); got != 2 {
		t.Fatalf("embedding calls = %d, want old and advanced HEAD builds", got)
	}
}

func TestGlobalSearchFlightSerializesUnrelatedIndexesWithoutCoalescing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	writeFile(t, firstRoot, "first.txt", "first\n")
	writeFile(t, secondRoot, "second.txt", "second\n")
	embedder := newBlockingEmbedder()
	t.Cleanup(embedder.releaseEmbeddings)
	waiting := make(chan struct{}, 1)
	base := Options{
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}

	var group sync.WaitGroup
	errs := make(chan error, 2)
	first := base
	first.Root = firstRoot
	group.Go(func() {
		_, err := Run(t.Context(), embedder, first, "")
		errs <- err
	})
	select {
	case <-embedder.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first index did not start embedding")
	}
	second := base
	second.Root = secondRoot
	second.ProgressLog = func(progress Progress) error {
		if progress.Status == ProgressStatusWaiting {
			waiting <- struct{}{}
		}
		return nil
	}
	group.Go(func() {
		_, err := Run(t.Context(), embedder, second, "")
		errs <- err
	})
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("unrelated index did not wait on the global flight")
	}
	if got := embedder.calls.Load(); got != 1 {
		t.Fatalf("embedding calls while first index is blocked = %d, want 1", got)
	}

	embedder.releaseEmbeddings()
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := embedder.calls.Load(); got != 2 {
		t.Fatalf("embedding calls for unrelated indexes = %d, want 2", got)
	}
}

func TestGlobalSearchFlightWaitIsCancelableAndKeepsOwnerLocked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	owner, err := lockSearchIndexProducer(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Unlock()
	root := t.TempDir()
	writeFile(t, root, "alpha.txt", "alpha\n")
	embedder := &countingEmbedder{}
	var progress []Progress
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Run(ctx, embedder, Options{
		Root:                root,
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v, want context cancellation", err)
	}
	if len(progress) != 1 || progress[0].Status != ProgressStatusWaiting {
		t.Fatalf("progress = %#v, want one waiting update", progress)
	}
	if calls := embedder.callCount(); calls != 0 {
		t.Fatalf("canceled waiter embedding calls = %d, want 0", calls)
	}
	canceled, stop := context.WithCancel(t.Context())
	stop()
	if _, err := lockSearchIndexProducer(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("owner lock was disturbed: second wait error = %v", err)
	}
}

func TestGlobalSearchFlightWaitProgressFailureStopsBeforeWork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	owner, err := lockSearchIndexProducer(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Unlock()
	root := t.TempDir()
	writeFile(t, root, "alpha.txt", "alpha\n")
	wantErr := errors.New("stop waiting progress")
	embedder := &countingEmbedder{}
	_, err = Run(t.Context(), embedder, Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
		ProgressLog: func(progress Progress) error {
			if progress.Status == ProgressStatusWaiting {
				return wantErr
			}
			return nil
		},
	}, "")
	if !errors.Is(err, wantErr) {
		t.Fatalf("wait error = %v, want %v", err, wantErr)
	}
	if calls := embedder.callCount(); calls != 0 {
		t.Fatalf("progress-failed waiter embedding calls = %d, want 0", calls)
	}
}

func TestGlobalSearchFlightUncontendedDoesNotReportWaiting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	writeFile(t, root, "alpha.txt", "alpha\n")
	var waited bool
	_, err := Run(t.Context(), fakeEmbedder{}, Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
		ProgressLog: func(progress Progress) error {
			waited = waited || progress.Status == ProgressStatusWaiting
			return nil
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if waited {
		t.Fatal("uncontended search reported a global lock wait")
	}
}

func TestGlobalSearchFlightRejectsMalformedMetadataRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home-file")
	if err := os.WriteFile(home, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	root := t.TempDir()
	writeFile(t, root, "alpha.txt", "alpha\n")
	embedder := &countingEmbedder{}
	_, err := Run(t.Context(), embedder, Options{
		Root:                root,
		IndexOnly:           true,
		MinScore:            DefaultMinScore,
		Limit:               DefaultLimit,
		EmbeddingModel:      "test-model",
		EmbeddingDimensions: 3,
	}, "")
	if err == nil {
		t.Fatal("search accepted a metadata root below a regular file")
	}
	if calls := embedder.callCount(); calls != 0 {
		t.Fatalf("malformed-root embedding calls = %d, want 0", calls)
	}
}

func TestGlobalSearchFlightSerializesAcrossProcesses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	owner, err := lockSearchIndexProducer(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ownerLocked := true
	defer func() {
		if ownerLocked {
			_ = owner.Unlock()
		}
	}()

	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSearchProducerProcessHelper$")
	command.Env = append(os.Environ(),
		searchProducerProcessHelperEnv+"=1",
		searchProducerProcessHelperHomeEnv+"="+home,
	)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 2)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	expectProcessLine(t, lines, ProgressStatusWaiting)
	if err := owner.Unlock(); err != nil {
		t.Fatal(err)
	}
	ownerLocked = false
	expectProcessLine(t, lines, "acquired")
	if err := command.Wait(); err != nil {
		t.Fatalf("helper process: %v\n%s", err, stderr.String())
	}
}

func TestSearchProducerProcessHelper(t *testing.T) {
	if os.Getenv(searchProducerProcessHelperEnv) != "1" {
		t.Skip("process helper")
	}
	if err := os.Setenv("HOME", os.Getenv(searchProducerProcessHelperHomeEnv)); err != nil {
		t.Fatal(err)
	}
	lock, err := lockSearchIndexProducer(t.Context(), func(progress Progress) error {
		_, writeErr := fmt.Fprintln(os.Stdout, progress.Status)
		return writeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "acquired"); err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func expectProcessLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	select {
	case got, ok := <-lines:
		if !ok || got != want {
			t.Fatalf("helper line = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for helper line %q", want)
	}
}
