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
	"github.com/yusing/git-agent/internal/trace"
)

type fakeClient struct {
	responses    []openai.Response
	requests     []openai.Request
	streamEvents []openai.StreamEvent
}

func (f *fakeClient) CreateResponse(_ context.Context, request openai.Request) (openai.Response, error) {
	f.requests = append(f.requests, request)
	for _, event := range f.streamEvents {
		if request.OnStreamEvent != nil {
			if err := request.OnStreamEvent(event); err != nil {
				return openai.Response{}, err
			}
		}
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	return resp, nil
}

func TestRunnerPublishesReasoningSummaryEvents(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		responses: []openai.Response{{Text: "done"}},
		streamEvents: []openai.StreamEvent{
			{Kind: "reasoning_summary.delta", ItemID: "rs_1", Delta: "Inspecting "},
			{Kind: "reasoning_summary.done", ItemID: "rs_1", Text: "Inspecting changed files"},
		},
	}
	var events []trace.Event
	recorder, err := trace.NewEventStream("review", func(event trace.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := OpenAIRunner{
		Config:           config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 1},
		Client:           client,
		Trace:            recorder,
		ReasoningSummary: openai.ReasoningSummaryAuto,
	}

	if _, err := runner.Run(t.Context(), Request{SystemPrompt: "system", UserPrompt: "user"}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("trace events = %#v", events)
	}
	if events[2].Kind != "reasoning_summary.delta" || events[2].Value["delta"] != "Inspecting " {
		t.Fatalf("delta trace event = %#v", events[2])
	}
	if events[3].Kind != "reasoning_summary.done" || events[3].Value["text"] != "Inspecting changed files" {
		t.Fatalf("done trace event = %#v", events[3])
	}
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
	registry := tools.NewRegistryWithSkills(repo, nil)
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
	if instructions := client.requests[0].Instructions; !containsAll(instructions, "bounded agent loop", "model step 1 of 3", "2 of 2 tool calls remaining", "reduce material uncertainty", "do not call tools just to repeat provided context", "Conclude before the remaining budget reaches zero", "Do not ask the user for more evidence") {
		t.Fatalf("request instructions missing tool economy guidance: %s", instructions)
	}
	if strings.Contains(client.requests[0].Instructions, "Use skills_read") {
		t.Fatalf("request instructions should not mention unavailable skills_read: %s", client.requests[0].Instructions)
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
		Tools:  tools.NewRegistryWithSkills(repo, nil),
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
	registry := tools.NewRegistryWithSkills(repo, nil)
	runner := OpenAIRunner{
		Config:    config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 1, MaxToolCalls: 2},
		Client:    client,
		Tools:     registry,
		ToolSpecs: registry.Definitions([]string{"repo_summary"}),
	}

	result, err := runner.Run(context.Background(), Request{
		SystemPrompt: "system",
		UserPrompt:   "user",
		TextFormat: &openai.TextFormat{
			Name: "artifact",
			Schema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"summary": map[string]any{"type": "string"}},
				"required":             []string{"summary"},
				"additionalProperties": false,
			},
			Strict: true,
		},
		MaxSteps: 1,
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
	if client.requests[1].TextFormat == nil || client.requests[1].TextFormat.Name != "artifact" {
		t.Fatalf("forced finalization dropped text format: %#v", client.requests[1].TextFormat)
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
	registry := tools.NewRegistryWithSkills(repo, nil)
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

func containsAll(text string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			return false
		}
	}
	return true
}
