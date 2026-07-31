package cli

import (
	"bytes"
	"context"
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

func (c *exploreFakeResponseClient) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
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
	if !strings.Contains(stderr.String(), "explore: searching") || !strings.Contains(stderr.String(), "explore: complete") {
		t.Fatalf("stderr missing progress:\n%s", stderr.String())
	}
}

func TestExploreHelpDoesNotResolveProviderConfiguration(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	err := New().Run(t.Context(), []string{"explore", "--help"})
	if err == nil || !strings.Contains(err.Error(), "Usage: git-agent explore [--follow-up <search-id>] <question...>") {
		t.Fatalf("help error = %v", err)
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
