package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/explore"
	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/projectidentity"
)

type exploreFakeResponseClient struct {
	mu       sync.Mutex
	requests []openai.Request
	deadline atomic.Bool
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
		answers = append(answers, explore.Answer{ItemID: match[1], Answer: "context for " + match[1]})
	}
	text, err := sonic.ConfigStd.MarshalToString(map[string]any{"answers": answers})
	if err != nil {
		return openai.Response{}, err
	}
	return openai.Response{Text: text, Continuation: []openai.Item{openai.NewMessage("assistant", text)}}, nil
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
	text, err := sonic.ConfigStd.MarshalToString(map[string]any{"answers": []explore.Answer{{
		ItemID: match[1], Answer: "main.go:3 owns Answer",
	}}})
	if err != nil {
		return openai.Response{}, err
	}
	return openai.Response{Text: text, Continuation: []openai.Item{openai.NewMessage("assistant", text)}}, nil
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
	if output.ID == "" || output.Answer == "" {
		t.Fatalf("initial output = %#v", output)
	}
	initialEmbeddingCalls := embedder.calls.Load()
	if initialEmbeddingCalls == 0 {
		t.Fatal("initial explore skipped semantic retrieval")
	}

	for depth := 1; depth <= explore.MaxFollowUps; depth++ {
		output = runExploreForTest(t, app, &stdout, "--follow-up", output.ID, "continue", "at", "depth")
		if got := embedder.calls.Load(); got != initialEmbeddingCalls {
			t.Fatalf("depth %d embedding calls = %d, want %d", depth, got, initialEmbeddingCalls)
		}
	}
	beforeReset := embedder.calls.Load()
	output = runExploreForTest(t, app, &stdout, "--follow-up", output.ID, "start", "fresh")
	if got := embedder.calls.Load(); got <= beforeReset {
		t.Fatalf("fourth follow-up embedding calls = %d, want > %d", got, beforeReset)
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
	if session.Depth != 0 || session.ParentID != "" {
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
		if countPromptCacheBreakpoints(request.Input) != 1 {
			t.Fatalf("request %d prompt cache breakpoints = %d, want 1", index, countPromptCacheBreakpoints(request.Input))
		}
	}
	if requests[4].PromptCacheKey == "" || requests[4].PromptCacheKey == rootCacheKey {
		t.Fatalf("fresh reset prompt cache key = %q, previous %q", requests[4].PromptCacheKey, rootCacheKey)
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

func TestExploreHelpDoesNotResolveProviderConfiguration(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	err := New().Run(t.Context(), []string{"explore", "--help"})
	if err == nil || !strings.Contains(err.Error(), "Usage: git-agent explore [--fast] [--follow-up <search-id>] <question...>") {
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
	if err := app.Run(t.Context(), []string{"--cwd", workspace, "explore", "where", "is", "Answer"}); err != nil {
		t.Fatal(err)
	}

	requests := responses.recordedRequests()
	if len(requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(requests))
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
	if err := sonic.ConfigStd.Unmarshal(stdout.Bytes(), &output); err != nil {
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
	if !strings.Contains(output.Answer, "main.go:3") {
		t.Fatalf("answer = %q", output.Answer)
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
	if !strings.Contains(output.Answer, "main.go:3") {
		t.Fatalf("answer = %q", output.Answer)
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
	if err := sonic.ConfigStd.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout.String())
	}
	return output
}
