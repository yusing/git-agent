package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/tools"
)

type fakeClient struct {
	responses []openai.Response
	requests  []openai.Request
}

func (f *fakeClient) CreateResponse(_ context.Context, request openai.Request) (openai.Response, error) {
	f.requests = append(f.requests, request)
	resp := f.responses[0]
	f.responses = f.responses[1:]
	return resp, nil
}

func TestRunnerRepairsInvalidOutputOnce(t *testing.T) {
	t.Parallel()

	client := &fakeClient{responses: []openai.Response{
		{Text: "```bad```"},
		{Text: "Add parser"},
	}}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 2},
		Client: client,
		Validator: func(text string) []string {
			if text == "Add parser" {
				return nil
			}
			return []string{"bad"}
		},
	}

	result, err := runner.Run(context.Background(), Request{
		SystemPrompt:      "system",
		UserPrompt:        "user",
		RepairOnValidator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Add parser" || result.RepairCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
	if len(client.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(client.requests))
	}
}

func TestRunnerNormalizesBeforeValidationAndReturn(t *testing.T) {
	t.Parallel()

	client := &fakeClient{responses: []openai.Response{
		{Text: "Add parser\n\nbody line"},
	}}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 1},
		Client: client,
		Normalize: func(text string) string {
			return strings.ReplaceAll(text, "\n", " ")
		},
		Validator: func(text string) []string {
			if strings.Contains(text, "\n") {
				return []string{"not normalized"}
			}
			return nil
		},
	}

	result, err := runner.Run(context.Background(), Request{
		SystemPrompt:      "system",
		UserPrompt:        "user",
		RepairOnValidator: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Add parser  body line" {
		t.Fatalf("result text = %q", result.Text)
	}
	if result.RepairCalls != 0 {
		t.Fatalf("repair calls = %d, want 0", result.RepairCalls)
	}
}

func TestRunnerForwardsTextFormat(t *testing.T) {
	t.Parallel()

	client := &fakeClient{responses: []openai.Response{
		{Text: `{"sections":[]}`},
	}}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 1},
		Client: client,
	}

	_, err := runner.Run(context.Background(), Request{
		SystemPrompt: "system",
		UserPrompt:   "user",
		TextFormat: &openai.TextFormat{
			Name:   "release_note",
			Schema: map[string]any{"type": "object"},
			Strict: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(client.requests))
	}
	if client.requests[0].TextFormat == nil || client.requests[0].TextFormat.Name != "release_note" {
		t.Fatalf("text format = %#v", client.requests[0].TextFormat)
	}
}

func TestRunnerExecutesToolCallRoundTrip(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "repo_summary", Arguments: "{}"}}},
		{Text: "Add parser"},
	}}
	registry := tools.NewRegistry(repo)
	runner := OpenAIRunner{
		Config:    config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 3, MaxToolCalls: 2},
		Client:    client,
		Tools:     registry,
		ToolSpecs: registry.Definitions([]string{"repo_summary"}),
	}

	result, err := runner.Run(context.Background(), Request{
		SystemPrompt: "system",
		UserPrompt:   "user",
		MaxSteps:     3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCalls != 1 || result.Text != "Add parser" {
		t.Fatalf("result = %#v", result)
	}
	if got := client.requests[1].Input[len(client.requests[1].Input)-1]; got.Type != "function_call_output" || got.CallID != "call_1" {
		t.Fatalf("missing tool output input: %#v", got)
	}
}

func TestRunnerRejectsToolCallsOutsideAllowedSet(t *testing.T) {
	t.Parallel()

	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "repo_summary", Arguments: "{}"}}},
	}}
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 1},
		Client: client,
		Tools:  tools.NewRegistry(repo),
	}

	_, err = runner.Run(t.Context(), Request{
		SystemPrompt:     "system",
		UserPrompt:       "user",
		AllowedToolNames: []string{"read_file"},
		MaxSteps:         1,
	})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected disallowed tool error, got %v", err)
	}
}

func TestRunnerFinalizesWhenStepBudgetRunsOut(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "repo_summary", Arguments: "{}"}}},
		{Text: "Add parser"},
	}}
	registry := tools.NewRegistry(repo)
	runner := OpenAIRunner{
		Config:    config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 1, MaxToolCalls: 2},
		Client:    client,
		Tools:     registry,
		ToolSpecs: registry.Definitions([]string{"repo_summary"}),
	}

	result, err := runner.Run(context.Background(), Request{
		SystemPrompt: "system",
		UserPrompt:   "user",
		MaxSteps:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Add parser" || result.ToolCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
	if len(client.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(client.requests))
	}
	if len(client.requests[1].Tools) != 0 {
		t.Fatalf("expected forced finalization without tools, got %#v", client.requests[1].Tools)
	}
}

func TestRunnerFinalizesWhenToolBudgetRunsOut(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{
			{ID: "fc_1", CallID: "call_1", Name: "repo_summary", Arguments: "{}"},
			{ID: "fc_2", CallID: "call_2", Name: "repo_summary", Arguments: "{}"},
		}},
		{Text: "Add parser"},
	}}
	registry := tools.NewRegistry(repo)
	runner := OpenAIRunner{
		Config:    config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 3, MaxToolCalls: 1},
		Client:    client,
		Tools:     registry,
		ToolSpecs: registry.Definitions([]string{"repo_summary"}),
	}

	result, err := runner.Run(context.Background(), Request{
		SystemPrompt: "system",
		UserPrompt:   "user",
		MaxSteps:     3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Add parser" || result.ToolCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
	if len(client.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(client.requests))
	}
	if len(client.requests[1].Tools) != 0 {
		t.Fatalf("expected forced finalization without tools, got %#v", client.requests[1].Tools)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}
