package explore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yusing/git-agent/internal/openai"
)

const testWorkspace = "test-workspace"

func TestCoordinatorBatchesConcurrentInitialSearches(t *testing.T) {
	store := testStore(t)
	first := testCoordinator(store)
	second := testCoordinator(store)
	var prepared atomic.Int32
	release := make(chan struct{})
	prepare := func(context.Context) (Prepared, error) {
		if prepared.Add(1) == 2 {
			close(release)
		}
		<-release
		return Prepared{SemanticResults: `{"results":[]}`}, nil
	}
	var runs atomic.Int32
	runner := func(_ context.Context, parent *Session, items []BatchItem) (map[string]BatchResult, error) {
		if parent != nil {
			t.Fatal("initial batch unexpectedly has parent")
		}
		if len(items) != 2 {
			return nil, fmt.Errorf("batch size = %d, want 2", len(items))
		}
		runs.Add(1)
		results := make(map[string]BatchResult, len(items))
		for _, item := range items {
			results[item.ID] = BatchResult{
				Answer:  "answer for " + item.Question,
				History: []openai.Item{openai.NewMessage("assistant", item.Question)},
			}
		}
		return results, nil
	}

	type callResult struct {
		output Output
		err    error
	}
	results := make(chan callResult, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		output, err := first.Run(t.Context(), nil, "first question", prepare, runner)
		results <- callResult{output: output, err: err}
	})
	wg.Go(func() {
		output, err := second.Run(t.Context(), nil, "second question", prepare, runner)
		results <- callResult{output: output, err: err}
	})
	wg.Wait()
	close(results)

	var outputs []Output
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		outputs = append(outputs, result.output)
	}
	if runs.Load() != 1 {
		t.Fatalf("batch runs = %d, want 1", runs.Load())
	}
	if outputs[0].ID == outputs[1].ID {
		t.Fatalf("batched outputs share ID %s", outputs[0].ID)
	}
	firstSession, err := store.Read(outputs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	secondSession, err := store.Read(outputs[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstSession.PromptCacheKey == "" || firstSession.PromptCacheKey != secondSession.PromptCacheKey {
		t.Fatalf("batched prompt cache keys = %q and %q", firstSession.PromptCacheKey, secondSession.PromptCacheKey)
	}
	for _, output := range outputs {
		session, err := store.Read(output.ID)
		if err != nil {
			t.Fatal(err)
		}
		if session.Depth != 0 || session.ParentID != "" {
			t.Fatalf("initial session = %#v", session)
		}
		if got := session.History[len(session.History)-1].Content; !strings.Contains(got, output.ID) {
			t.Fatalf("branch history selector = %q, want ID %s", got, output.ID)
		}
	}
}

func TestCoordinatorInitialJoinGraceIncludesSlightlyDelayedCaller(t *testing.T) {
	store := testStore(t)
	first := testCoordinator(store)
	second := testCoordinator(store)
	first.joinGrace = defaultJoinGrace
	second.joinGrace = defaultJoinGrace
	firstPrepared := make(chan struct{})
	prepareFirst := func(context.Context) (Prepared, error) {
		close(firstPrepared)
		return Prepared{SemanticResults: `{"results":[]}`}, nil
	}
	prepareSecond := func(context.Context) (Prepared, error) {
		return Prepared{SemanticResults: `{"results":[]}`}, nil
	}
	var runs atomic.Int32
	runner := func(ctx context.Context, parent *Session, items []BatchItem) (map[string]BatchResult, error) {
		if len(items) != 2 {
			return nil, fmt.Errorf("batch size = %d, want 2", len(items))
		}
		runs.Add(1)
		return answerRunner(ctx, parent, items)
	}

	type callResult struct {
		output Output
		err    error
	}
	results := make(chan callResult, 2)
	go func() {
		output, err := first.Run(t.Context(), nil, "first question", prepareFirst, runner)
		results <- callResult{output: output, err: err}
	}()
	<-firstPrepared
	time.Sleep(75 * time.Millisecond)
	go func() {
		output, err := second.Run(t.Context(), nil, "slightly delayed question", prepareSecond, runner)
		results <- callResult{output: output, err: err}
	}()
	for range 2 {
		if result := <-results; result.err != nil {
			t.Fatal(result.err)
		}
	}
	if runs.Load() != 1 {
		t.Fatalf("batch runs = %d, want 1", runs.Load())
	}
}

func TestCoordinatorCapsConcurrentBatchAtThree(t *testing.T) {
	store := testStore(t)
	coordinators := make([]*Coordinator, 4)
	for index := range coordinators {
		coordinators[index] = testCoordinator(store)
	}
	var prepared atomic.Int32
	release := make(chan struct{})
	prepare := func(context.Context) (Prepared, error) {
		if prepared.Add(1) == 4 {
			close(release)
		}
		<-release
		return Prepared{SemanticResults: `{"results":[]}`}, nil
	}
	var mu sync.Mutex
	var sizes []int
	runner := func(_ context.Context, _ *Session, items []BatchItem) (map[string]BatchResult, error) {
		mu.Lock()
		sizes = append(sizes, len(items))
		mu.Unlock()
		return answerRunner(t.Context(), nil, items)
	}
	type callResult struct {
		output Output
		err    error
	}
	results := make(chan callResult, 4)
	var wg sync.WaitGroup
	for index, coordinator := range coordinators {
		wg.Go(func() {
			output, err := coordinator.Run(t.Context(), nil, fmt.Sprintf("question %d", index), prepare, runner)
			results <- callResult{output: output, err: err}
		})
	}
	wg.Wait()
	close(results)
	ids := map[string]bool{}
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		ids[result.output.ID] = true
	}
	if len(ids) != 4 {
		t.Fatalf("distinct output IDs = %d, want 4", len(ids))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sizes) != 2 {
		t.Fatalf("batch sizes = %v, want two batches", sizes)
	}
	for _, size := range sizes {
		if size < 1 || size > 3 {
			t.Fatalf("batch sizes = %v, want each in [1,3]", sizes)
		}
	}
}

func TestCoordinatorDoesNotBatchDifferentWorkspaces(t *testing.T) {
	store := testStore(t)
	first := NewCoordinator(store, "/workspace/one", false)
	second := NewCoordinator(store, "/workspace/two", false)
	for _, coordinator := range []*Coordinator{first, second} {
		coordinator.pollInterval = time.Millisecond
		coordinator.heartbeatInterval = 5 * time.Millisecond
		coordinator.heartbeatStale = 100 * time.Millisecond
	}
	var prepared atomic.Int32
	release := make(chan struct{})
	prepare := func(context.Context) (Prepared, error) {
		if prepared.Add(1) == 2 {
			close(release)
		}
		<-release
		return Prepared{SemanticResults: `{"results":[]}`}, nil
	}
	var runs atomic.Int32
	runner := func(ctx context.Context, _ *Session, items []BatchItem) (map[string]BatchResult, error) {
		if len(items) != 1 {
			return nil, fmt.Errorf("different workspaces formed batch of %d", len(items))
		}
		runs.Add(1)
		return answerRunner(ctx, nil, items)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for index, coordinator := range []*Coordinator{first, second} {
		wg.Go(func() {
			_, err := coordinator.Run(t.Context(), nil, fmt.Sprintf("workspace %d", index), prepare, runner)
			errors <- err
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if runs.Load() != 2 {
		t.Fatalf("batch runs = %d, want 2", runs.Load())
	}
}

func TestCoordinatorRejectsParentFromDifferentWorkspace(t *testing.T) {
	coordinator := testCoordinator(testStore(t))
	parent := &Session{Workspace: "other-workspace"}
	if _, err := coordinator.Run(t.Context(), parent, "continue", nil, answerRunner); err == nil || !strings.Contains(err.Error(), "does not match current workspace") {
		t.Fatalf("cross-workspace parent error = %v", err)
	}
}

func TestCoordinatorDoesNotBatchDifferentServiceTiers(t *testing.T) {
	store := testStore(t)
	standard := NewCoordinator(store, testWorkspace, false)
	priority := NewCoordinator(store, testWorkspace, true)
	for _, coordinator := range []*Coordinator{standard, priority} {
		coordinator.pollInterval = time.Millisecond
		coordinator.heartbeatInterval = 5 * time.Millisecond
		coordinator.heartbeatStale = 100 * time.Millisecond
	}
	var prepared atomic.Int32
	release := make(chan struct{})
	prepare := func(context.Context) (Prepared, error) {
		if prepared.Add(1) == 2 {
			close(release)
		}
		<-release
		return Prepared{SemanticResults: `{"results":[]}`}, nil
	}
	var runs atomic.Int32
	runner := func(ctx context.Context, _ *Session, items []BatchItem) (map[string]BatchResult, error) {
		if len(items) != 1 {
			return nil, fmt.Errorf("different service tiers formed batch of %d", len(items))
		}
		runs.Add(1)
		return answerRunner(ctx, nil, items)
	}
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for index, coordinator := range []*Coordinator{standard, priority} {
		wg.Go(func() {
			_, err := coordinator.Run(t.Context(), nil, fmt.Sprintf("tier %d", index), prepare, runner)
			errors <- err
		})
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if runs.Load() != 2 {
		t.Fatalf("batch runs = %d, want 2", runs.Load())
	}
}

func TestSessionValidationRequiresVersionedCleanWorkspace(t *testing.T) {
	valid := Session{
		Version: sessionVersion, ID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Workspace: testWorkspace,
		PromptCacheKey: "explore:AAAAAAAAAAAAAAAAAAAAAAAAAA",
		Answer:         "answer", History: []openai.Item{openai.NewMessage("assistant", "answer")},
	}
	legacy := valid
	legacy.Version = sessionVersion - 1
	if err := validateSession(legacy); err == nil || !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("legacy session error = %v", err)
	}
	dirty := valid
	dirty.Workspace = "nested/../workspace"
	if err := validateSession(dirty); err == nil || !strings.Contains(err.Error(), "workspace is not clean") {
		t.Fatalf("unclean workspace error = %v", err)
	}
}

func TestCoordinatorForksConcurrentFollowUpsFromSameParent(t *testing.T) {
	store := testStore(t)
	parentOutput := runFresh(t, testCoordinator(store), "root")
	parent, err := store.FollowUpParent(parentOutput.ID, testWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	first := testCoordinator(store)
	second := testCoordinator(store)
	ready := make(chan struct{})
	var entered atomic.Int32
	runner := func(_ context.Context, gotParent *Session, items []BatchItem) (map[string]BatchResult, error) {
		if gotParent == nil || gotParent.ID != parent.ID {
			return nil, fmt.Errorf("parent = %#v, want %s", gotParent, parent.ID)
		}
		if len(items) != 2 {
			return nil, fmt.Errorf("batch size = %d, want 2", len(items))
		}
		results := make(map[string]BatchResult, len(items))
		for _, item := range items {
			results[item.ID] = BatchResult{Answer: item.Question, History: append(parent.History, openai.NewMessage("assistant", item.Question))}
		}
		return results, nil
	}

	type callResult struct {
		output Output
		err    error
	}
	results := make(chan callResult, 2)
	var wg sync.WaitGroup
	call := func(coordinator *Coordinator, question string) {
		if entered.Add(1) == 2 {
			close(ready)
		}
		<-ready
		output, err := coordinator.Run(t.Context(), parent, question, nil, runner)
		results <- callResult{output: output, err: err}
	}
	wg.Go(func() { call(first, "branch one") })
	wg.Go(func() { call(second, "branch two") })
	wg.Wait()
	close(results)

	var outputs []Output
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		outputs = append(outputs, result.output)
	}
	if outputs[0].ID == outputs[1].ID {
		t.Fatalf("follow-up siblings share ID %s", outputs[0].ID)
	}
	for _, output := range outputs {
		session, err := store.Read(output.ID)
		if err != nil {
			t.Fatal(err)
		}
		if session.ParentID != parent.ID || session.Depth != 1 {
			t.Fatalf("follow-up session = %#v", session)
		}
	}
}

func TestCoordinatorDoesNotBatchDifferentParents(t *testing.T) {
	store := testStore(t)
	firstOutput := runFresh(t, testCoordinator(store), "first root")
	secondOutput := runFresh(t, testCoordinator(store), "second root")
	firstParent, err := store.FollowUpParent(firstOutput.ID, testWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	secondParent, err := store.FollowUpParent(secondOutput.ID, testWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	runner := func(ctx context.Context, _ *Session, items []BatchItem) (map[string]BatchResult, error) {
		if len(items) != 1 {
			return nil, fmt.Errorf("different parents formed batch of %d", len(items))
		}
		runs.Add(1)
		return answerRunner(ctx, nil, items)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for index, parent := range []*Session{firstParent, secondParent} {
		wg.Go(func() {
			<-start
			_, err := testCoordinator(store).Run(t.Context(), parent, fmt.Sprintf("follow-up %d", index), nil, runner)
			errors <- err
		})
	}
	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if runs.Load() != 2 {
		t.Fatalf("batch runs = %d, want 2", runs.Load())
	}
}

func TestFourthFollowUpResetsToFreshSearch(t *testing.T) {
	store := testStore(t)
	output := runFresh(t, testCoordinator(store), "root")
	for depth := 1; depth <= MaxFollowUps; depth++ {
		parent, err := store.FollowUpParent(output.ID, testWorkspace)
		if err != nil {
			t.Fatal(err)
		}
		if parent == nil {
			t.Fatalf("depth %d unexpectedly reset", depth)
		}
		output, err = testCoordinator(store).Run(t.Context(), parent, fmt.Sprintf("follow-up %d", depth), nil, answerRunner)
		if err != nil {
			t.Fatal(err)
		}
		session, err := store.Read(output.ID)
		if err != nil {
			t.Fatal(err)
		}
		if session.Depth != depth {
			t.Fatalf("depth = %d, want %d", session.Depth, depth)
		}
	}
	parent, err := store.FollowUpParent(output.ID, testWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	if parent != nil {
		t.Fatalf("exhausted parent = %#v, want nil", parent)
	}
	var prepared atomic.Bool
	output, err = testCoordinator(store).Run(t.Context(), nil, "fourth follow-up", func(context.Context) (Prepared, error) {
		prepared.Store(true)
		return Prepared{SemanticResults: `{"fresh":true}`}, nil
	}, answerRunner)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.Load() {
		t.Fatal("fourth follow-up did not perform fresh semantic search")
	}
	session, err := store.Read(output.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Depth != 0 || session.ParentID != "" {
		t.Fatalf("reset session = %#v", session)
	}
}

func TestStoreUsesOwnerOnlyPermissions(t *testing.T) {
	store := testStore(t)
	output := runFresh(t, testCoordinator(store), "permissions")
	info, err := os.Stat(filepath.Join(store.sessionDir, output.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("session mode = %o, want 600", got)
	}
	info, err = os.Stat(filepath.Dir(store.sessionDir))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("store mode = %o, want 700", got)
	}
}

func TestCoordinatorDoesNotStealLiveLock(t *testing.T) {
	store := testStore(t)
	first := testCoordinator(store)
	second := testCoordinator(store)
	keyDir, err := first.prepareKeyDir(nil)
	if err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.withCoordLock(t.Context(), keyDir, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- second.withCoordLock(t.Context(), keyDir, func() error {
			close(secondEntered)
			return nil
		})
	}()
	select {
	case <-secondEntered:
		t.Fatal("second coordinator stole a lock from its live holder")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorMarksAbandonedLeaderFailed(t *testing.T) {
	store := testStore(t)
	coordinator := testCoordinator(store)
	coordinator.heartbeatStale = 10 * time.Millisecond
	keyDir, err := coordinator.prepareKeyDir(nil)
	if err != nil {
		t.Fatal(err)
	}
	batchID, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	batchDir := filepath.Join(keyDir, "batch-"+batchID)
	if err := os.MkdirAll(filepath.Join(batchDir, "items"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(batchDir, "open"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	heartbeat := filepath.Join(batchDir, "heartbeat")
	if err := touch(heartbeat); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(heartbeat, old, old); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.withCoordLock(t.Context(), keyDir, func() error {
		return coordinator.sweepLocked(keyDir)
	}); err != nil {
		t.Fatal(err)
	}
	var state batchState
	if err := readJSONFile(filepath.Join(batchDir, "state.json"), &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != "failed" || !strings.Contains(state.Error, "leader exited") {
		t.Fatalf("state = %#v", state)
	}
}

func TestCoordinatorReportsPhaseTimings(t *testing.T) {
	t.Parallel()
	coordinator := testCoordinator(testStore(t))
	var phases []string
	coordinator.Timing = func(phase string, duration time.Duration) {
		if duration < 0 {
			t.Errorf("negative duration for %s: %s", phase, duration)
		}
		phases = append(phases, phase)
	}
	runFresh(t, coordinator, "timed question")

	want := []string{
		"reservation",
		"semantic_search",
		"join_grace",
		"batch_join",
		"batch_collection",
		"persistence",
		"result_wait",
	}
	if !slices.Equal(phases, want) {
		t.Fatalf("timing phases = %v, want %v", phases, want)
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testCoordinator(store *Store) *Coordinator {
	coordinator := NewCoordinator(store, testWorkspace, false)
	coordinator.batchWait = time.Second
	coordinator.joinGrace = 50 * time.Millisecond
	coordinator.pollInterval = time.Millisecond
	coordinator.heartbeatInterval = 5 * time.Millisecond
	coordinator.heartbeatStale = 100 * time.Millisecond
	return coordinator
}

func runFresh(t *testing.T, coordinator *Coordinator, question string) Output {
	t.Helper()
	output, err := coordinator.Run(t.Context(), nil, question, func(context.Context) (Prepared, error) {
		return Prepared{SemanticResults: `{"results":[]}`}, nil
	}, answerRunner)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func answerRunner(_ context.Context, _ *Session, items []BatchItem) (map[string]BatchResult, error) {
	results := make(map[string]BatchResult, len(items))
	for _, item := range items {
		results[item.ID] = BatchResult{
			Answer:  "answer for " + item.Question,
			History: []openai.Item{openai.NewMessage("assistant", item.Question)},
		}
	}
	return results, nil
}
