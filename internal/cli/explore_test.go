package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/explore"
	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/projectidentity"
	searchtask "github.com/yusing/git-agent/internal/tasks/search"
)

type exploreTimingFailWriter struct{ buffer bytes.Buffer }

func (w *exploreTimingFailWriter) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("explore.phase")) {
		return 0, errors.New("write explore timing")
	}
	return w.buffer.Write(data)
}

func TestRunWithExploreFreshnessOverlapsAndWaits(t *testing.T) {
	freshnessStarted := make(chan struct{})
	releaseFreshness := make(chan struct{})
	var waited atomic.Bool
	freshnessErr := errors.New("freshness failed")
	value, output, err := runWithExploreFreshness(
		t.Context(),
		true,
		func(context.Context) (searchtask.Output, error) {
			close(freshnessStarted)
			<-releaseFreshness
			return searchtask.Output{Query: "confirmed"}, freshnessErr
		},
		func(context.Context) (string, error) {
			<-freshnessStarted
			return "agent result", nil
		},
		func() {
			waited.Store(true)
			close(releaseFreshness)
		},
	)
	if value != "agent result" || output.Query != "confirmed" {
		t.Fatalf("overlapped result = %q, output = %#v", value, output)
	}
	if !waited.Load() || !errors.Is(err, freshnessErr) {
		t.Fatalf("waited = %v, error = %v", waited.Load(), err)
	}
}

func TestRunWithExploreFreshnessCancelsProviderOnConfirmationFailure(t *testing.T) {
	providerStarted := make(chan struct{})
	freshnessErr := errors.New("freshness failed")
	_, _, err := runWithExploreFreshness(
		t.Context(),
		true,
		func(context.Context) (searchtask.Output, error) {
			<-providerStarted
			return searchtask.Output{}, freshnessErr
		},
		func(ctx context.Context) (string, error) {
			close(providerStarted)
			<-ctx.Done()
			return "", ctx.Err()
		},
		nil,
	)
	if !errors.Is(err, freshnessErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("overlap error = %v, want freshness failure and cancellation", err)
	}
}

func TestPrepareExploreSearchSkipsColdIndexWithoutRemote(t *testing.T) {
	root := initRepo(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	writeFixtureFile(t, filepath.Join(root, "app.go"), "package app\n\nfunc Stable() {}\n")
	t.Chdir(root)
	embedder := &exploreFakeEmbedder{}
	app := &App{stderr: &bytes.Buffer{}, embeddingClient: embedder}

	prepared, err := app.prepareExploreSearch(t.Context(), "find stable", nil)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.SemanticResults != "" || len(prepared.GuidancePaths) != 0 || prepared.DeferredFreshness {
		t.Fatalf("cold explore preparation = %#v, want empty", prepared)
	}
	if calls := embedder.calls.Load(); calls != 0 {
		t.Fatalf("cold explore embedding calls = %d, want 0", calls)
	}
}

func TestPrepareExploreSearchDefersFreshnessOnlyForWarmIndex(t *testing.T) {
	root := initRepo(t)
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	writeFixtureFile(t, filepath.Join(root, "app.go"), "package app\n\nfunc Stable() {}\n")
	runGit(t, root, "add", "app.go")
	runGit(t, root, "commit", "-m", "base")
	runGit(t, root, "remote", "add", "origin", "https://example.test/acme/widget.git")
	syncRemote := t.TempDir()
	runGit(t, syncRemote, "init", "--bare")
	if err := config.SaveFile(config.File{Index: config.IndexConfig{Remote: syncRemote}}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	embedder := &exploreFakeEmbedder{}
	app := &App{stderr: &bytes.Buffer{}, embeddingClient: embedder}

	cold, err := app.prepareExploreSearch(t.Context(), "find stable", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cold.SemanticResults != "" || len(cold.GuidancePaths) != 0 || cold.DeferredFreshness {
		t.Fatalf("cold explore preparation = %#v, want empty", cold)
	}
	if calls := embedder.calls.Load(); calls != 0 {
		t.Fatalf("cold explore embedding calls = %d, want 0", calls)
	}

	client, opts, err := app.exploreSearchOptions(root)
	if err != nil {
		t.Fatal(err)
	}
	opts.IndexRemote = ""
	if _, err := searchtask.Run(t.Context(), client, opts, "build stable index"); err != nil {
		t.Fatal(err)
	}
	embedder.calls.Store(0)

	warm, err := app.prepareExploreSearch(t.Context(), "find stable", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !warm.DeferredFreshness || warm.SemanticResults == "" {
		t.Fatalf("warm explore preparation = %#v", warm)
	}
	if calls := embedder.calls.Load(); calls == 0 {
		t.Fatal("warm explore skipped semantic retrieval")
	}
}

type exploreFakeResponseClient struct {
	mu       sync.Mutex
	requests []openai.Request
	deadline atomic.Bool
	usage    openai.Usage
}

func (c *exploreFakeResponseClient) CreateResponse(ctx context.Context, request openai.Request) (openai.Response, error) {
	if _, ok := ctx.Deadline(); ok {
		c.deadline.Store(true)
	}
	c.mu.Lock()
	c.requests = append(c.requests, request)
	c.mu.Unlock()
	lastUser := ""
	for _, item := range request.Input {
		if item.Type == "message" && item.Role == "user" {
			lastUser = item.Content
		}
	}
	matches := regexp.MustCompile(`"item_id":"([A-Z2-7]{26})"`).FindAllStringSubmatch(lastUser, -1)
	answers := make([]explore.Answer, 0, len(matches))
	for _, match := range matches {
		answers = append(answers, explore.Answer{ItemID: match[1], Items: []explore.Item{{Description: "context for " + match[1], References: []string{"main.go:1"}}}})
	}
	text, err := json.Marshal(map[string]any{"answers": answers})
	if err != nil {
		return openai.Response{}, err
	}
	body := string(text)
	return openai.Response{Text: body, Continuation: []openai.Item{openai.NewMessage("assistant", body)}, Usage: c.usage}, nil
}

func TestExploreDoesNotApplyRequestTimeoutToWholeBatch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	t.Chdir(root)
	runGit(t, root, "init")
	writeFixtureFile(t, root+"/main.go", "package demo\n")

	responses := &exploreFakeResponseClient{}
	app := &App{
		stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		responseClient: responses, embeddingClient: &exploreFakeEmbedder{},
	}
	if err := app.Run(context.Background(), []string{"explore", "inspect", "the", "repository"}); err != nil {
		t.Fatal(err)
	}
	if responses.deadline.Load() {
		t.Fatal("explore applied the per-request timeout to the whole batch context")
	}
}

func TestExploreWithoutDebugReportsOnlyProgress(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	t.Chdir(root)
	runGit(t, root, "init")
	writeFixtureFile(t, root+"/main.go", "package demo\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr,
		responseClient: &exploreFakeResponseClient{}, embeddingClient: &exploreFakeEmbedder{},
	}
	runExploreForTest(t, app, &stdout, "inspect", "the", "repository")

	progress := stderr.String()
	if !strings.Contains(progress, "explore: searching") || !strings.Contains(progress, "explore: complete") {
		t.Fatalf("stderr missing progress:\n%s", progress)
	}
	if strings.Contains(progress, " INF ") || strings.Contains(progress, "explore.phase") {
		t.Fatalf("stderr contains debug trace without --debug:\n%s", progress)
	}
}

func TestExploreReportsProviderUsageWithoutChangingStdout(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	t.Chdir(root)
	runGit(t, root, "init")
	writeFixtureFile(t, root+"/main.go", "package demo\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr,
		responseClient: &exploreFakeResponseClient{
			usage: openai.Usage{InputTokens: 120, CachedInputTokens: 80, CacheWriteInputTokens: 40, OutputTokens: 12, TotalTokens: 132},
		},
		embeddingClient: &exploreFakeEmbedder{},
	}
	runExploreForTest(t, app, &stdout, "inspect", "the", "repository")

	if !jsontext.Value(stdout.Bytes()).IsValid() {
		t.Fatalf("stdout is not JSON: %q", stdout.String())
	}
	for _, want := range []string{
		"llm.usage", "step=1", "input_tokens=120", "cached_input_tokens=80",
		"cache_write_input_tokens=40", "output_tokens=12",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestExploreDebugReturnsTimingWriteError(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	t.Chdir(root)
	runGit(t, root, "init")
	writeFixtureFile(t, root+"/main.go", "package demo\n")

	var stdout bytes.Buffer
	stderr := &exploreTimingFailWriter{}
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: stderr,
		responseClient: &exploreFakeResponseClient{}, embeddingClient: &exploreFakeEmbedder{},
	}
	err := app.Run(t.Context(), []string{"explore", "--debug", "inspect", "the", "repository"})
	if err == nil || !strings.Contains(err.Error(), "write explore timing") {
		t.Fatalf("error = %v, want timing write failure", err)
	}
}

func TestExploreReportsPhaseTimingsOnStderr(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	t.Chdir(root)
	runGit(t, root, "init")
	writeFixtureFile(t, root+"/main.go", "package demo\n")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr,
		responseClient: &exploreFakeResponseClient{}, embeddingClient: &exploreFakeEmbedder{},
	}
	runExploreForTest(t, app, &stdout, "--debug", "inspect", "the", "repository")

	traceText := stderr.String()
	for _, phase := range []string{
		"setup",
		"reservation",
		"semantic_search.warm_probe.discover",
		"semantic_search",
		"join_grace",
		"batch_join",
		"batch_collection",
		"prompt_setup",
		"provider_request",
		"validation",
		"agent",
		"answer_processing",
		"persistence",
		"result_wait",
		"output",
	} {
		if !strings.Contains(traceText, "phase="+phase) {
			t.Errorf("stderr missing %s timing:\n%s", phase, traceText)
		}
	}
	for line := range strings.SplitSeq(traceText, "\n") {
		if strings.Contains(line, " INF explore.phase ") &&
			(!strings.Contains(line, "duration_ms=") || !strings.Contains(line, "elapsed_ms=")) {
			t.Errorf("incomplete explore timing line: %s", line)
		}
	}
}

func TestExploreFastRequestsPriorityServiceTier(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	t.Chdir(root)
	runGit(t, root, "init")
	writeFixtureFile(t, root+"/main.go", "package demo\n")

	responses := &exploreFakeResponseClient{}
	var stdout bytes.Buffer
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &bytes.Buffer{},
		responseClient: responses, embeddingClient: &exploreFakeEmbedder{},
	}
	output := runExploreForTest(t, app, &stdout, "--fast", "inspect", "the", "repository")
	runExploreForTest(t, app, &stdout, "--fast", "--follow-up", output.ID, "continue")
	requests := responses.recordedRequests()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requests))
	}
	for index, request := range requests {
		if got := request.ServiceTier; got != "priority" {
			t.Fatalf("request %d service tier = %q, want priority", index, got)
		}
	}
}

func TestExploreQueryTargetPersistsAndChangesWithoutBreakingCache(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	t.Chdir(root)
	runGit(t, root, "init")
	writeFixtureFile(t, root+"/main.go", "package demo\n")

	responses := &exploreFakeResponseClient{}
	embedder := &exploreFakeEmbedder{}
	var stdout bytes.Buffer
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &bytes.Buffer{},
		responseClient: responses, embeddingClient: embedder,
	}
	initial := runExploreForTest(t, app, &stdout, "--for", "diagnose", "why", "is", "search", "slow")
	initialEmbeddingCalls := embedder.calls.Load()
	inherited := runExploreForTest(t, app, &stdout, "--follow-up", initial.ID, "what", "failed")
	changed := runExploreForTest(t, app, &stdout, "--follow-up", inherited.ID, "--for", "owner", "who", "owns", "it")
	exhausted := runExploreForTest(t, app, &stdout, "--follow-up", changed.ID, "--for", "owner", "which", "callers")
	if got := embedder.calls.Load(); got != initialEmbeddingCalls {
		t.Fatalf("targeted follow-ups embedding calls = %d, want %d", got, initialEmbeddingCalls)
	}
	reset := runExploreForTest(t, app, &stdout, "--follow-up", exhausted.ID, "fresh", "context")

	requests := responses.recordedRequests()
	if len(requests) != 5 {
		t.Fatalf("provider requests = %d, want 5", len(requests))
	}
	instructions := requests[0].Instructions
	if !strings.HasPrefix(instructions, explore.TargetSystemPrompt) ||
		strings.Contains(instructions, explore.QueryTargetDiagnose.Instructions()) {
		t.Fatalf("initial instructions are not the neutral target prompt: %s", instructions)
	}
	cacheKey := requests[0].PromptCacheKey
	for index, request := range requests[:4] {
		if request.Instructions != instructions {
			t.Fatalf("request %d rewrote stable target-mode instructions", index)
		}
		if request.PromptCacheKey != cacheKey {
			t.Fatalf("request %d cache key = %q, want %q", index, request.PromptCacheKey, cacheKey)
		}
	}
	if requests[4].Instructions != instructions {
		t.Fatalf("reset instructions = %q, want stable target-mode instructions", requests[4].Instructions)
	}
	if requests[4].PromptCacheKey == cacheKey {
		t.Fatal("reset reused the exhausted chain's prompt cache key")
	}
	targetChanges := func(items []openai.Item) int {
		count := 0
		for _, item := range items {
			if item.Role == "developer" && strings.HasPrefix(item.Content, "Query target changed: ") {
				count++
			}
		}
		return count
	}
	wantInitial := explore.InitialTargetInstruction(explore.QueryTargetDiagnose)
	exactInitials := 0
	for _, item := range requests[0].Input {
		if item.Role == "developer" && item.Content == wantInitial {
			exactInitials++
		}
	}
	if exactInitials != 1 {
		t.Fatalf("initial target messages = %d, want 1", exactInitials)
	}
	if got := targetChanges(requests[1].Input); got != 0 {
		t.Fatalf("inherited target change messages = %d, want 0", got)
	}
	if got := targetChanges(requests[2].Input); got != 1 {
		t.Fatalf("changed target messages = %d, want 1", got)
	}
	if got := targetChanges(requests[3].Input); got != 1 {
		t.Fatalf("same active target messages = %d, want retained single message", got)
	}
	if got := targetChanges(requests[4].Input); got != 0 {
		t.Fatalf("reset target change messages = %d, want 0", got)
	}
	wantChange := "Query target changed: owner\n" + explore.QueryTargetOwner.Instructions()
	exactChanges := 0
	for _, item := range requests[2].Input {
		if item.Role == "developer" && item.Content == wantChange {
			exactChanges++
		}
	}
	if exactChanges != 1 {
		t.Fatalf("exact target-change messages = %d, want 1", exactChanges)
	}

	identity, err := projectidentity.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	metadataDir, err := identity.Dir()
	if err != nil {
		t.Fatal(err)
	}
	store, err := explore.NewStore(metadataDir)
	if err != nil {
		t.Fatal(err)
	}
	initialSession, err := store.Read(initial.ID)
	if err != nil {
		t.Fatal(err)
	}
	if initialSession.InstructionTarget != explore.QueryTargetDiagnose ||
		initialSession.ActiveTarget != explore.QueryTargetDiagnose {
		t.Fatalf("initial targets = %q/%q", initialSession.InstructionTarget, initialSession.ActiveTarget)
	}
	changedSession, err := store.Read(changed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if changedSession.InstructionTarget != explore.QueryTargetDiagnose ||
		changedSession.ActiveTarget != explore.QueryTargetOwner {
		t.Fatalf("changed targets = %q/%q", changedSession.InstructionTarget, changedSession.ActiveTarget)
	}
	resetSession, err := store.Read(reset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resetSession.InstructionTarget != explore.QueryTargetOwner ||
		resetSession.ActiveTarget != explore.QueryTargetOwner ||
		resetSession.Depth != 0 || resetSession.ParentID != "" {
		t.Fatalf("reset session = %#v", resetSession)
	}
}

func TestExploreAddingQueryTargetReplacesUniversalSystemPrompt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	t.Chdir(root)
	runGit(t, root, "init")
	writeFixtureFile(t, root+"/main.go", "package demo\n")

	responses := &exploreFakeResponseClient{}
	var stdout bytes.Buffer
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &bytes.Buffer{},
		responseClient: responses, embeddingClient: &exploreFakeEmbedder{},
	}
	initial := runExploreForTest(t, app, &stdout, "describe", "behavior")
	runExploreForTest(t, app, &stdout, "--follow-up", initial.ID, "--for", "behavior", "focus", "behavior")

	requests := responses.recordedRequests()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requests))
	}
	if !strings.HasPrefix(requests[0].Instructions, explore.SystemPrompt) {
		t.Fatalf("initial instructions omitted universal prompt: %s", requests[0].Instructions)
	}
	if !strings.HasPrefix(requests[1].Instructions, explore.TargetSystemPrompt) ||
		requests[1].Instructions == requests[0].Instructions {
		t.Fatalf("targeted follow-up did not replace universal instructions: %s", requests[1].Instructions)
	}
	if requests[1].PromptCacheKey != requests[0].PromptCacheKey {
		t.Fatalf("targeted follow-up cache key = %q, want %q", requests[1].PromptCacheKey, requests[0].PromptCacheKey)
	}
	want := "Query target changed: behavior\n" + explore.QueryTargetBehavior.Instructions()
	count := 0
	for _, item := range requests[1].Input {
		if item.Role == "developer" && item.Content == want {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("target-change messages = %d, want 1", count)
	}
}

func (c *exploreFakeResponseClient) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func (c *exploreFakeResponseClient) recordedRequests() []openai.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]openai.Request(nil), c.requests...)
}

type exploreFakeEmbedder struct {
	calls atomic.Int32
}

type exploreReadToolClient struct {
	calls atomic.Int32
	read  atomic.Bool
}

func (c *exploreReadToolClient) CreateResponse(_ context.Context, request openai.Request) (openai.Response, error) {
	if c.calls.Add(1) == 1 {
		return openai.Response{ToolCalls: []openai.ToolCall{{
			ID: "fc_read", CallID: "call_read", Name: "read_file",
			Arguments: `{"path":"main.go","start_line":1,"end_line":3}`,
		}}}, nil
	}
	lastUser := ""
	for _, item := range request.Input {
		if item.Type == "message" && item.Role == "user" {
			lastUser = item.Content
		}
		if item.Type == "function_call_output" && strings.Contains(item.Output, "func Answer") {
			c.read.Store(true)
		}
	}
	match := regexp.MustCompile(`"item_id":"([A-Z2-7]{26})"`).FindStringSubmatch(lastUser)
	text, err := json.Marshal(map[string]any{"answers": []explore.Answer{{
		ItemID: match[1], Items: []explore.Item{{Description: "Answer is owned by main.go", References: []string{"main.go:3"}}},
	}}})
	if err != nil {
		return openai.Response{}, err
	}
	body := string(text)
	return openai.Response{Text: body, Continuation: []openai.Item{openai.NewMessage("assistant", body)}}, nil
}

func (e *exploreFakeEmbedder) CreateEmbeddings(_ context.Context, request openai.EmbeddingRequest) (openai.EmbeddingResponse, error) {
	e.calls.Add(1)
	vectors := make([][]float64, len(request.Inputs))
	for index := range request.Inputs {
		vectors[index] = []float64{0.2, 0.4, 0.6}
	}
	return openai.EmbeddingResponse{Model: request.Model, Vectors: vectors, Dimensions: 3}, nil
}

func TestExploreInitialFollowUpsAndFreshReset(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	t.Chdir(root)
	runGit(t, root, "init")
	writeFixtureFile(t, root+"/main.go", "package demo\n\nfunc Answer() int { return 42 }\n")

	responses := &exploreFakeResponseClient{}
	embedder := &exploreFakeEmbedder{}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr,
		responseClient: responses, embeddingClient: embedder,
	}
	output := runExploreForTest(t, app, &stdout, "where is Answer implemented?")
	if output.ID == "" || len(output.Items) == 0 {
		t.Fatalf("initial output = %#v", output)
	}
	initialEmbeddingCalls := embedder.calls.Load()
	if initialEmbeddingCalls != 0 {
		t.Fatalf("cold initial explore embedding calls = %d, want 0", initialEmbeddingCalls)
	}

	for depth := 1; depth <= explore.MaxFollowUps; depth++ {
		output = runExploreForTest(t, app, &stdout, "--follow-up", output.ID, "continue", "at", "depth")
		if got := embedder.calls.Load(); got != initialEmbeddingCalls {
			t.Fatalf("depth %d embedding calls = %d, want %d", depth, got, initialEmbeddingCalls)
		}
	}
	beforeReset := embedder.calls.Load()
	output = runExploreForTest(t, app, &stdout, "--follow-up", output.ID, "--for", "owner", "start", "fresh")
	if got := embedder.calls.Load(); got != beforeReset {
		t.Fatalf("cold fresh reset embedding calls = %d, want %d", got, beforeReset)
	}
	identity, err := projectidentity.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	metadataDir, err := identity.Dir()
	if err != nil {
		t.Fatal(err)
	}
	store, err := explore.NewStore(metadataDir)
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Read(output.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Depth != 0 || session.ParentID != "" || session.InstructionTarget != explore.QueryTargetOwner || session.ActiveTarget != explore.QueryTargetOwner {
		t.Fatalf("reset session = %#v", session)
	}
	if got := responses.requestCount(); got != 5 {
		t.Fatalf("provider requests = %d, want 5", got)
	}
	requests := responses.recordedRequests()
	for index, request := range requests {
		if !request.ParallelToolCalls {
			t.Fatalf("request %d disabled parallel provider tool calls", index)
		}
	}
	rootCacheKey := requests[0].PromptCacheKey
	if rootCacheKey == "" {
		t.Fatal("initial explore request omitted prompt cache key")
	}
	for index, request := range requests[:4] {
		if request.PromptCacheKey != rootCacheKey {
			t.Fatalf("request %d prompt cache key = %q, want %q", index, request.PromptCacheKey, rootCacheKey)
		}
		if got := countPromptCacheBreakpoints(request.Input); got != index+1 {
			t.Fatalf("request %d prompt cache breakpoints = %d, want %d", index, got, index+1)
		}
	}
	if requests[4].PromptCacheKey == "" || requests[4].PromptCacheKey == rootCacheKey {
		t.Fatalf("fresh reset prompt cache key = %q, previous %q", requests[4].PromptCacheKey, rootCacheKey)
	}
	wantOwnerTarget := explore.InitialTargetInstruction(explore.QueryTargetOwner)
	ownerTargetMessages := 0
	for _, item := range requests[4].Input {
		if item.Role == "developer" && item.Content == wantOwnerTarget {
			ownerTargetMessages++
		}
	}
	if ownerTargetMessages != 1 {
		t.Fatalf("fresh reset owner target messages = %d, want 1", ownerTargetMessages)
	}
	if !strings.Contains(stderr.String(), "explore: searching") || !strings.Contains(stderr.String(), "explore: complete") {
		t.Fatalf("stderr missing progress:\n%s", stderr.String())
	}
}

func countPromptCacheBreakpoints(items []openai.Item) int {
	count := 0
	for _, item := range items {
		if item.PromptCacheBreakpoint {
			count++
		}
	}
	return count
}

func TestExploreRejectsInvalidQueryTargetBeforeSearchOrProviderWork(t *testing.T) {
	responses := &exploreFakeResponseClient{}
	embedder := &exploreFakeEmbedder{}
	app := &App{
		stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		responseClient: responses, embeddingClient: embedder,
	}
	err := app.Run(t.Context(), []string{"explore", "--for", "review", "inspect"})
	if err == nil || !strings.Contains(err.Error(), "unsupported explore query target") {
		t.Fatalf("invalid target error = %v", err)
	}
	err = app.Run(t.Context(), []string{"explore", "--for"})
	if err == nil || !strings.Contains(err.Error(), "flag needs an argument") ||
		!strings.Contains(err.Error(), "Usage: git-agent explore") {
		t.Fatalf("missing target error = %v", err)
	}
	if responses.requestCount() != 0 || embedder.calls.Load() != 0 {
		t.Fatalf("invalid target performed provider=%d embedding=%d work", responses.requestCount(), embedder.calls.Load())
	}
}

func TestExploreHelpDoesNotResolveProviderConfiguration(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	err := New().Run(t.Context(), []string{"explore", "--help"})
	if err == nil || !strings.Contains(err.Error(), "Usage: git-agent explore [--debug] [--fast] [--for <diagnose|change|behavior|owner>] [--follow-up <search-id>] <question...>") {
		t.Fatalf("help error = %v", err)
	}
}

func TestExploreGlobalCWDIsCompleteWorkspaceBoundary(t *testing.T) {
	repoRoot := t.TempDir()
	workspace := filepath.Join(repoRoot, "nested")
	siblingWorkspace := filepath.Join(repoRoot, "sibling")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	runGit(t, repoRoot, "init")
	writeFixtureFile(t, filepath.Join(repoRoot, "root.go"), "package root\n")
	writeFixtureFile(t, filepath.Join(workspace, "nested.go"), "package nested\n\nfunc Answer() int { return 42 }\n")
	writeFixtureFile(t, filepath.Join(siblingWorkspace, "sibling.go"), "package sibling\n")

	responses := &exploreFakeResponseClient{}
	var stdout bytes.Buffer
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &bytes.Buffer{},
		responseClient: responses, embeddingClient: &exploreFakeEmbedder{},
	}
	client, opts, err := app.exploreSearchOptions(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := searchtask.Run(t.Context(), client, opts, "where is Answer"); err != nil {
		t.Fatal(err)
	}
	if err := app.Run(t.Context(), []string{"--cwd", workspace, "explore", "where", "is", "Answer"}); err != nil {
		t.Fatal(err)
	}

	requests := responses.recordedRequests()
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(requests))
	}
	toolNames := make(map[string]bool, len(requests[0].Tools))
	for _, tool := range requests[0].Tools {
		toolNames[tool.Name] = true
	}
	for _, name := range []string{
		"git_recent_commits", "git_head_show", "git_diff_against_parent", "git_show_file_at_rev", "git_log_range",
	} {
		if !toolNames[name] {
			t.Fatalf("Git explore request omitted history tool %q", name)
		}
	}
	var requestText strings.Builder
	for _, item := range requests[0].Input {
		requestText.WriteString(item.Content)
		requestText.WriteByte('\n')
	}
	wantEnvironment := "<cwd>" + workspace + "</cwd>\n<repo_root>" + workspace + "</repo_root>"
	if !strings.Contains(requestText.String(), wantEnvironment) {
		t.Fatalf("request environment is not workspace-rooted:\n%s", requestText.String())
	}
	if !strings.Contains(requestText.String(), "nested.go") {
		t.Fatalf("semantic results omitted workspace-relative path:\n%s", requestText.String())
	}
	if strings.Contains(requestText.String(), "root.go") {
		t.Fatalf("semantic results escaped workspace:\n%s", requestText.String())
	}

	var output explore.Output
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	identity, err := projectidentity.Resolve(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	metadataDir, err := identity.Dir()
	if err != nil {
		t.Fatal(err)
	}
	store, err := explore.NewStore(metadataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(output.ID); err != nil {
		t.Fatalf("session did not retain ancestor Git project identity: %v", err)
	}
	beforeFollowUp := responses.requestCount()
	err = app.Run(t.Context(), []string{"--cwd", siblingWorkspace, "explore", "--follow-up", output.ID, "continue"})
	if err == nil || !strings.Contains(err.Error(), "belongs to workspace") {
		t.Fatalf("cross-workspace follow-up error = %v", err)
	}
	if got := responses.requestCount(); got != beforeFollowUp {
		t.Fatalf("cross-workspace follow-up made %d provider requests, want %d", got, beforeFollowUp)
	}
}

func TestExploreExecutesEstablishedReadTool(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	t.Chdir(root)
	runGit(t, root, "init")
	writeFixtureFile(t, root+"/main.go", "package demo\n\nfunc Answer() int { return 42 }\n")
	client := &exploreReadToolClient{}
	var stdout bytes.Buffer
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &bytes.Buffer{},
		responseClient: client, embeddingClient: &exploreFakeEmbedder{},
	}
	output := runExploreForTest(t, app, &stdout, "read", "the", "Answer", "implementation")
	if len(output.Items) != 1 || len(output.Items[0].References) != 1 || output.Items[0].References[0] != "main.go:3" {
		t.Fatalf("items = %#v", output.Items)
	}
	if client.calls.Load() != 2 || !client.read.Load() {
		t.Fatalf("provider calls = %d, read output observed = %v", client.calls.Load(), client.read.Load())
	}
}

func TestExploreExecutesReadToolOutsideGitRepository(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	t.Chdir(root)
	writeFixtureFile(t, root+"/main.go", "package demo\n\nfunc Answer() int { return 42 }\n")
	client := &exploreReadToolClient{}
	var stdout bytes.Buffer
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &bytes.Buffer{},
		responseClient: client, embeddingClient: &exploreFakeEmbedder{},
	}
	output := runExploreForTest(t, app, &stdout, "read", "the", "Answer", "implementation")
	if len(output.Items) != 1 || len(output.Items[0].References) != 1 || output.Items[0].References[0] != "main.go:3" {
		t.Fatalf("items = %#v", output.Items)
	}
	if client.calls.Load() != 2 || !client.read.Load() {
		t.Fatalf("provider calls = %d, read output observed = %v", client.calls.Load(), client.read.Load())
	}
}

func TestExploreDoesNotTreatInvalidGitRepositoryAsDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Chdir(root)
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := &exploreFakeResponseClient{}
	app := &App{
		stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		responseClient: client,
	}
	if err := app.Run(t.Context(), []string{"explore", "inspect", "the", "codebase"}); err == nil {
		t.Fatal("explore accepted an invalid Git repository as an ordinary directory")
	}
	if client.requestCount() != 0 {
		t.Fatalf("provider requests = %d, want 0", client.requestCount())
	}
}

func TestExploreUnknownFollowUpFailsBeforeProviderResolution(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	t.Chdir(root)
	runGit(t, root, "init")
	client := &exploreFakeResponseClient{}
	app := &App{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, responseClient: client}
	err := app.Run(t.Context(), []string{"explore", "--follow-up", "AAAAAAAAAAAAAAAAAAAAAAAAAA", "continue"})
	if err == nil || !strings.Contains(err.Error(), "unknown explore search ID") {
		t.Fatalf("error = %v", err)
	}
	if client.requestCount() != 0 {
		t.Fatalf("provider requests = %d, want 0", client.requestCount())
	}
}

func runExploreForTest(t *testing.T, app *App, stdout *bytes.Buffer, args ...string) explore.Output {
	t.Helper()
	stdout.Reset()
	if err := app.Run(t.Context(), append([]string{"explore"}, args...)); err != nil {
		t.Fatal(err)
	}
	var output explore.Output
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout.String())
	}
	return output
}
