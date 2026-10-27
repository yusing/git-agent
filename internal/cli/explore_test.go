package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	requests []testResponseRequest
	deadline atomic.Bool
	usage    openai.Usage
}

func (c *exploreFakeResponseClient) CreateResponse(ctx context.Context, request testResponseRequest) (testResponse, error) {
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
		return testResponse{}, err
	}
	body := string(text)
	return testResponse{Text: body, Usage: c.usage}, nil
}

func TestExploreUsesSDKRequestTimeoutWithoutBatchDeadline(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
	t.Chdir(root)
	runGit(t, root, "init")
	writeFixtureFile(t, root+"/main.go", "package demo\n")

	// Observe the actual SDK timer in the child process, rather than inferring
	// client-side timeouts from the HTTP server's unrelated context deadline.
	probe := filepath.Join(t.TempDir(), "request-timeout.cjs")
	writeFixtureFile(t, probe, fmt.Sprintf(`const original = globalThis.setTimeout;
globalThis.setTimeout = (callback, milliseconds, ...args) => {
  if (milliseconds === %d) throw new Error('exploration inherited the normal command timeout');
  return original(callback, milliseconds, ...args);
};`, config.DefaultTimeout.Milliseconds()))
	t.Setenv("NODE_OPTIONS", os.Getenv("NODE_OPTIONS")+" --require="+fmt.Sprintf("%q", probe))

	responses := &exploreFakeResponseClient{}
	useExploreResponseServer(t, responses)
	app := &App{
		stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		embeddingClient: &exploreFakeEmbedder{},
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
	useExploreResponseServer(t, &exploreFakeResponseClient{})
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr,
		embeddingClient: &exploreFakeEmbedder{},
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
	usageClient := &exploreFakeResponseClient{usage: openai.Usage{InputTokens: 120, CachedInputTokens: 80, CacheWriteInputTokens: 40, OutputTokens: 12, TotalTokens: 132}}
	useExploreResponseServer(t, usageClient)
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr,

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
	useExploreResponseServer(t, &exploreFakeResponseClient{})
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: stderr,
		embeddingClient: &exploreFakeEmbedder{},
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
	useExploreResponseServer(t, &exploreFakeResponseClient{})
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr,
		embeddingClient: &exploreFakeEmbedder{},
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
	for _, tc := range []struct {
		model, effort string
		flags         []string
	}{
		{model: "gpt-6-luna", effort: "xhigh", flags: []string{"--fast"}},
		{model: "gpt-6.1-terra", effort: "high"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", t.TempDir())
			t.Setenv("OPENAI_API_KEY", "test-key")
			t.Setenv("OPENAI_MODEL", tc.model)
			t.Setenv("OPENAI_EMBEDDING_DIMENSIONS", "3")
			t.Chdir(root)
			runGit(t, root, "init")
			writeFixtureFile(t, root+"/main.go", "package demo\n")

			responses := &exploreFakeResponseClient{}
			var stdout bytes.Buffer
			useExploreResponseServer(t, responses)
			app := &App{
				stdin: strings.NewReader(""), stdout: &stdout, stderr: &bytes.Buffer{},
				embeddingClient: &exploreFakeEmbedder{},
			}
			output := runExploreForTest(t, app, &stdout, append(tc.flags, "inspect", "the", "repository")...)
			runExploreForTest(t, app, &stdout, append(tc.flags, "--follow-up", output.ID, "continue")...)
			requests := responses.recordedRequests()
			if len(requests) != 2 {
				t.Fatalf("provider requests = %d, want 2", len(requests))
			}
			for index, request := range requests {
				if request.Model != tc.model || request.ServiceTier != "priority" || request.ThinkingEffort != tc.effort {
					t.Fatalf("request %d model = %q, service tier = %q, effort = %q", index, request.Model, request.ServiceTier, request.ThinkingEffort)
				}
			}
		})
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
	useExploreResponseServer(t, responses)
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &bytes.Buffer{},
		embeddingClient: embedder,
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
	useExploreResponseServer(t, responses)
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &bytes.Buffer{},
		embeddingClient: &exploreFakeEmbedder{},
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

func (c *exploreFakeResponseClient) recordedRequests() []testResponseRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]testResponseRequest(nil), c.requests...)
}

type exploreFakeEmbedder struct {
	calls atomic.Int32
}

type exploreReadToolClient struct {
	calls atomic.Int32
	read  atomic.Bool
}

func (c *exploreReadToolClient) CreateResponse(_ context.Context, request testResponseRequest) (testResponse, error) {
	if c.calls.Add(1) == 1 {
		return testResponse{ToolCalls: []testToolCall{{
			Name:      "read_file",
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
		return testResponse{}, err
	}
	body := string(text)
	return testResponse{Text: body}, nil
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
	useExploreResponseServer(t, responses)
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr,
		embeddingClient: embedder,
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
	rootCacheKey := requests[0].PromptCacheKey
	if rootCacheKey == "" {
		t.Fatal("initial explore request omitted prompt cache key")
	}
	for index, request := range requests[:4] {
		if request.PromptCacheKey != rootCacheKey {
			t.Fatalf("request %d prompt cache key = %q, want %q", index, request.PromptCacheKey, rootCacheKey)
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

func TestExploreRejectsInvalidQueryTargetBeforeSearchOrProviderWork(t *testing.T) {
	responses := &exploreFakeResponseClient{}
	embedder := &exploreFakeEmbedder{}
	useExploreResponseServer(t, responses)
	app := &App{
		stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
		embeddingClient: embedder,
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
	useExploreResponseServer(t, responses)
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &bytes.Buffer{},
		embeddingClient: &exploreFakeEmbedder{},
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
	for _, name := range []string{
		"git_recent_commits", "git_head_show", "git_diff_against_parent", "git_show_file_at_rev", "git_log_range",
	} {
		if !strings.Contains(requests[0].Instructions, "tools."+name) {
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
	useExploreResponseServer(t, client)
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &bytes.Buffer{},
		embeddingClient: &exploreFakeEmbedder{},
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
	useExploreResponseServer(t, client)
	app := &App{
		stdin: strings.NewReader(""), stdout: &stdout, stderr: &bytes.Buffer{},
		embeddingClient: &exploreFakeEmbedder{},
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
	useExploreResponseServer(t, client)
	app := &App{
		stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{},
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
	useExploreResponseServer(t, client)
	app := &App{stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
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

// Keep the request-recording fixtures behind a real HTTP boundary so these
// tests exercise the Agents SDK transport, loop and persisted history.
func useExploreResponseServer(t *testing.T, client interface {
	CreateResponse(context.Context, testResponseRequest) (testResponse, error)
}) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wire struct {
			Model          string `json:"model"`
			Instructions   string `json:"instructions"`
			ServiceTier    string `json:"service_tier"`
			PromptCacheKey string `json:"prompt_cache_key"`
			Reasoning      struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
			Input []struct {
				Type    string         `json:"type"`
				Role    string         `json:"role"`
				Content jsontext.Value `json:"content"`
				Output  string         `json:"output"`
			} `json:"input"`
		}
		if err := json.UnmarshalRead(r.Body, &wire); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/responses/compact") {
			var output []any
			for _, item := range wire.Input {
				if item.Role == "user" || item.Role == "developer" || item.Role == "system" {
					output = append(output, map[string]any{"type": "message", "role": item.Role, "content": item.Content})
				}
			}
			output = append(output, map[string]any{"type": "compaction", "id": "cmp_test", "encrypted_content": "opaque-test-state"})
			w.Header().Set("Content-Type", "application/json")
			if err := json.MarshalWrite(w, map[string]any{"id": "compact_test", "object": "response.compaction", "output": output}); err != nil {
				t.Error(err)
			}
			return
		}
		request := testResponseRequest{Model: wire.Model, Instructions: wire.Instructions, ServiceTier: wire.ServiceTier, ThinkingEffort: wire.Reasoning.Effort, PromptCacheKey: wire.PromptCacheKey}
		for _, item := range wire.Input {
			content := ""
			if item.Content.Kind() == '"' {
				_ = json.Unmarshal(item.Content, &content)
			} else if item.Content.Kind() == '[' {
				var parts []struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(item.Content, &parts)
				for _, part := range parts {
					content += part.Text
				}
			}
			kind := item.Type
			if kind == "" && item.Role != "" {
				kind = "message"
			}
			request.Input = append(request.Input, openai.Item{Type: kind, Role: item.Role, Content: content, Output: item.Output})
		}
		response, err := client.CreateResponse(r.Context(), request)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var output []any
		if len(response.ToolCalls) > 0 {
			var calls []string
			for _, call := range response.ToolCalls {
				calls = append(calls, "tools."+call.Name+"("+call.Arguments+")")
			}
			arguments, _ := json.Marshal(map[string]any{"code": "text(await Promise.all([" + strings.Join(calls, ",") + "]));"})
			output = []any{map[string]any{"type": "function_call", "id": "fc_test", "call_id": "call_test", "name": "exec", "arguments": string(arguments)}}
		} else {
			output = []any{map[string]any{"type": "message", "id": "msg_test", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": response.Text, "annotations": []any{}}}}}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(value any) {
			data, err := json.Marshal(value)
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		for index, item := range output {
			emit(map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
			emit(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
		}
		emit(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_test", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": response.Usage.InputTokens, "output_tokens": response.Usage.OutputTokens, "total_tokens": response.Usage.TotalTokens, "input_tokens_details": map[string]any{"cached_tokens": response.Usage.CachedInputTokens, "cache_write_tokens": response.Usage.CacheWriteInputTokens}}}})
	}))
	t.Cleanup(server.Close)
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
}

type testResponseRequest struct {
	Model          string
	Instructions   string
	ServiceTier    string
	ThinkingEffort string
	PromptCacheKey string
	Input          []openai.Item
}
type testResponse struct {
	Text      string
	ToolCalls []testToolCall
	Usage     openai.Usage
}
type testToolCall struct{ Name, Arguments string }
