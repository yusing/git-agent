package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/provider"
	"github.com/yusing/git-agent/internal/tools"
	"github.com/yusing/git-agent/internal/trace"
)

type fakeClient struct {
	responses      []openai.Response
	responseErrors []error
	requests       []openai.Request
	streamEvents   []openai.StreamEvent
	retryEvents    []openai.RetryEvent
}

func TestRunnerReturnsTerminalBranchOutcomeAndPortableForks(t *testing.T) {
	t.Parallel()

	control := tools.Definition{
		Name: "branch", Description: "retire and fan out", Strict: true,
		Schema: map[string]any{"type": "object", "additionalProperties": false},
	}
	client := &fakeClient{responses: []openai.Response{{
		Continuation: []openai.Item{
			{Type: "reasoning", RawJSON: `{"id":"rs_1","type":"reasoning","encrypted_content":"cipher"}`},
			{Type: "function_call", RawJSON: `{"id":"fc_1","type":"function_call","call_id":"call_1","name":"branch","arguments":"{}"}`},
		},
		ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "branch", Arguments: `{"branches":[]}`}},
	}}}
	runner := OpenAIRunner{
		Config: config.Config{Model: "parent", MaxSteps: 2, MaxToolCalls: 2},
		Client: client,
	}
	outcome, err := runner.RunNode(t.Context(), Request{
		UserPrompt: "review", MaxSteps: 2, ControlTool: &control,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Final != nil || outcome.Branch == nil || outcome.Branch.ToolCalls != 1 {
		t.Fatalf("outcome = %#v", outcome)
	}
	if len(client.requests) != 1 || len(client.requests[0].Tools) != 1 || client.requests[0].Tools[0].Name != "branch" {
		t.Fatalf("request tools = %#v", client.requests)
	}

	sameModel := outcome.Branch.ForkInput(`{"branch_id":"b1"}`, true)
	if !slices.ContainsFunc(sameModel, func(item openai.Item) bool { return item.Type == "reasoning" }) {
		t.Fatalf("same-model fork omitted reasoning: %#v", sameModel)
	}
	crossModel := outcome.Branch.ForkInput(`{"branch_id":"b1"}`, false)
	if slices.ContainsFunc(crossModel, func(item openai.Item) bool { return item.Type == "reasoning" || item.Type == "web_search_call" }) {
		t.Fatalf("cross-model fork retained opaque items: %#v", crossModel)
	}
	if !slices.ContainsFunc(crossModel, func(item openai.Item) bool { return item.Type == "function_call" }) ||
		!slices.ContainsFunc(crossModel, func(item openai.Item) bool { return item.Type == "function_call_output" && item.CallID == "call_1" }) {
		t.Fatalf("cross-model fork lost portable context: %#v", crossModel)
	}
}

func TestRunnerRejectsBranchMixedWithRepositoryCall(t *testing.T) {
	t.Parallel()

	control := tools.Definition{Name: "branch", Strict: true, Schema: map[string]any{"type": "object"}}
	client := &fakeClient{responses: []openai.Response{{
		ToolCalls: []openai.ToolCall{
			{ID: "fc_1", CallID: "call_1", Name: "branch", Arguments: `{}`},
			{ID: "fc_2", CallID: "call_2", Name: "read_file", Arguments: `{}`},
		},
	}}}
	runner := OpenAIRunner{Config: config.Config{MaxSteps: 2, MaxToolCalls: 2}, Client: client}
	_, err := runner.RunNode(t.Context(), Request{UserPrompt: "review", MaxSteps: 2, ControlTool: &control})
	if err == nil || !strings.Contains(err.Error(), "must be the only local function call") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunnerForcesFinalizationWhenBranchExceedsToolBudget(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	control := tools.Definition{Name: "branch", Strict: true, Schema: map[string]any{"type": "object"}}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "repo_summary", Arguments: `{}`}}},
		{ToolCalls: []openai.ToolCall{{ID: "fc_2", CallID: "call_2", Name: "branch", Arguments: `{}`}}},
		{Text: "forced final"},
	}}
	registry := tools.NewReviewRegistryWithSkills(repo, nil, tools.ReviewModeCodebase, tools.ReviewScope{}, gitctx.ChangeFingerprint{})
	var events []trace.Event
	recorder, err := trace.NewEventStream("review", func(event trace.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := OpenAIRunner{
		Config:    config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 1},
		Client:    client,
		Tools:     registry,
		ToolSpecs: registry.Definitions([]string{"repo_summary"}),
		Trace:     recorder,
	}

	outcome, err := runner.RunNode(t.Context(), Request{
		UserPrompt: "review", MaxSteps: 3, ControlTool: &control,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Branch != nil || outcome.Final == nil || outcome.Final.Text != "forced final" || outcome.Final.ToolCalls != 1 {
		t.Fatalf("outcome = %#v", outcome)
	}
	if len(client.requests) != 3 || len(client.requests[2].Tools) != 0 || len(client.requests[2].HostedCapabilities) != 0 {
		t.Fatalf("forced-finalization request = %#v", client.requests)
	}
	if !slices.ContainsFunc(events, func(event trace.Event) bool {
		return event.Kind == "budget" &&
			event.Value["kind"] == string(BudgetKindToolCalls) &&
			event.Value["decision"] == "finalize"
	}) {
		t.Fatalf("budget events = %#v", events)
	}
}

func TestRequestInstructionsRequireReadFilePathProvenance(t *testing.T) {
	t.Parallel()

	instructions := requestInstructions("", []openai.ToolSpec{{Name: "read_file"}}, nil, 1, 3, 0, 2)
	if !containsAll(instructions,
		"path copied verbatim from prepared context or prior repository-tool output",
		"Discover paths with available inventory or search tools first",
		"Package import paths, package names, types, and symbols do not imply filenames",
	) {
		t.Fatalf("read_file instructions missing path provenance contract: %s", instructions)
	}

	withoutReadFile := requestInstructions("", []openai.ToolSpec{{Name: "repo_summary"}}, nil, 1, 3, 0, 2)
	if strings.Contains(withoutReadFile, "do not imply filenames") {
		t.Fatalf("instructions mention read_file contract without read_file: %s", withoutReadFile)
	}

	withHostedSearch := requestInstructions("", nil, []provider.HostedCapability{{Kind: provider.HostedCapabilityWebSearch}}, 1, 3, 0, 2)
	if !strings.Contains(withHostedSearch, "web_search (provider-hosted)") || strings.Contains(withHostedSearch, "No tools are available") {
		t.Fatalf("instructions do not advertise hosted web search: %s", withHostedSearch)
	}
}

func TestProviderUsageJSONBoundaryRemainsForwardCompatible(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want int
	}{
		{name: "positive usage", raw: `{"usage":{"input_tokens":42}}`, want: 42},
		{name: "unknown future fields", raw: `{"future_top":true,"usage":{"input_tokens":42,"future_nested":"ok"}}`, want: 42},
		{name: "unrelated key collision", raw: `{"usage":{"input_tokens_backup":99}}`},
		{name: "negative wrong type", raw: `{"usage":{"input_tokens":"42"}}`},
		{name: "malformed", raw: `{"usage":`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := responseInputTokens(openai.Response{RawJSON: tt.raw}); got != tt.want {
				t.Fatalf("input tokens = %d, want %d", got, tt.want)
			}
		})
	}
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
	for _, event := range f.retryEvents {
		if request.OnRetry != nil {
			if err := request.OnRetry(event); err != nil {
				return openai.Response{}, err
			}
		}
	}
	if len(f.responseErrors) > 0 {
		err := f.responseErrors[0]
		f.responseErrors = f.responseErrors[1:]
		if err != nil {
			return openai.Response{}, err
		}
	}
	resp := f.responses[0]
	f.responses = f.responses[1:]
	return resp, nil
}

func TestRunnerDisablesRejectedHostedCapabilityAndRetriesStepOnce(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	longQuery := strings.Repeat("q", 700)
	longSource := "https://example.test/" + strings.Repeat("s", 2200)
	client := &fakeClient{
		responseErrors: []error{
			&provider.UnsupportedCapabilityError{Failure: provider.CapabilityFailure{Capability: provider.HostedCapabilityWebSearch, Reason: "raw upstream detail"}},
			nil,
			nil,
		},
		responses: []openai.Response{
			{
				Continuation: []openai.Item{
					{Type: "reasoning", RawJSON: `{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"cipher"}`},
					{Type: "web_search_call", RawJSON: `{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search"}}`},
					{Type: "function_call", RawJSON: `{"id":"fc_1","type":"function_call","call_id":"call_1","name":"repo_summary","arguments":"{}"}`},
				},
				ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "repo_summary", Arguments: `{}`}},
				HostedToolCalls: []openai.HostedToolCall{{
					ID: "ws_1", Type: "web_search", Status: "completed", Action: "search",
					Queries: []string{longQuery}, Sources: []string{longSource},
				}},
			},
			{Text: `{"summary":"done; hosted web lookup unavailable"}`},
		},
	}
	registry := tools.NewRegistryWithSkills(repo, nil)
	var events []trace.Event
	recorder, err := trace.NewEventStream("review", func(event trace.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 2}, Client: client,
		Tools: registry, ToolSpecs: registry.Definitions([]string{"repo_summary"}), Trace: recorder,
		HostedCapabilities: []provider.HostedCapability{{Kind: provider.HostedCapabilityWebSearch, MaxCalls: 4}},
	}

	result, err := runner.Run(t.Context(), Request{UserPrompt: "review", MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCalls != 1 || len(client.requests) != 3 {
		t.Fatalf("result=%#v requests=%d", result, len(client.requests))
	}
	if len(client.requests[0].HostedCapabilities) != 1 {
		t.Fatalf("initial hosted capabilities = %#v", client.requests[0].HostedCapabilities)
	}
	if !strings.Contains(client.requests[0].Instructions, "web_search (provider-hosted)") {
		t.Fatalf("initial request does not advertise hosted web search: %s", client.requests[0].Instructions)
	}
	for index := 1; index < len(client.requests); index++ {
		if len(client.requests[index].HostedCapabilities) != 0 {
			t.Fatalf("request %d re-enabled hosted capability: %#v", index, client.requests[index].HostedCapabilities)
		}
		if len(client.requests[index].Tools) != 1 || client.requests[index].Tools[0].Name != "repo_summary" {
			t.Fatalf("request %d lost local tools: %#v", index, client.requests[index].Tools)
		}
		if !strings.Contains(client.requests[index].Instructions, "hosted web lookup was unavailable") {
			t.Fatalf("request %d missing disclosure instruction: %s", index, client.requests[index].Instructions)
		}
		if strings.Contains(client.requests[index].Instructions, "web_search (provider-hosted)") {
			t.Fatalf("request %d still advertises disabled hosted search: %s", index, client.requests[index].Instructions)
		}
	}
	lastInput := client.requests[2].Input
	var continuationEnd, outputIndex int
	for index, item := range lastInput {
		if item.Type == "function_call" {
			continuationEnd = index
		}
		if item.Type == "function_call_output" {
			outputIndex = index
		}
	}
	if continuationEnd == 0 || outputIndex <= continuationEnd {
		t.Fatalf("continuation/output order = %#v", lastInput)
	}
	encodedEvents, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedEvents), "raw upstream detail") || strings.Contains(string(encodedEvents), longQuery) || strings.Contains(string(encodedEvents), longSource) {
		t.Fatalf("trace contains unsanitized capability or hosted metadata: %s", encodedEvents)
	}
	if !strings.Contains(string(encodedEvents), "provider_rejected_capability") || !strings.Contains(string(encodedEvents), "hosted-tool-call") {
		t.Fatalf("trace missing hosted events: %s", encodedEvents)
	}
}

func TestRunnerLeavesUnrelatedProviderErrorsTerminal(t *testing.T) {
	t.Parallel()

	upstream := errors.New("rate limited")
	client := &fakeClient{responseErrors: []error{upstream}}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 2}, Client: client,
		HostedCapabilities: []provider.HostedCapability{{Kind: provider.HostedCapabilityWebSearch}},
	}
	_, err := runner.Run(t.Context(), Request{UserPrompt: "review", MaxSteps: 2})
	if !errors.Is(err, upstream) || len(client.requests) != 1 {
		t.Fatalf("error=%v requests=%d", err, len(client.requests))
	}
}

func TestRunnerPublishesReasoningSummaryEvents(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		responses: []openai.Response{{Text: "done"}},
		streamEvents: []openai.StreamEvent{
			{Kind: "reasoning_summary.delta", ProviderAttempt: 1, ItemID: "rs_1", Delta: "Inspecting "},
			{Kind: "reasoning_summary.done", ProviderAttempt: 1, ItemID: "rs_1", Text: "Inspecting changed files"},
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
	if !hasEvent(events, "reasoning_summary.delta", "delta", "Inspecting ") {
		t.Fatalf("trace missing reasoning delta: %#v", events)
	}
	if !hasEvent(events, "reasoning_summary.done", "text", "Inspecting changed files") {
		t.Fatalf("trace missing reasoning done: %#v", events)
	}
	for _, event := range events {
		if event.Kind == "reasoning_summary.delta" && fmt.Sprint(event.Value["provider_attempt"]) != "1" {
			t.Fatalf("reasoning provider attempt = %#v", event)
		}
	}
}

func TestRunnerPublishesProviderRetryRuntimeStatus(t *testing.T) {
	t.Parallel()

	client := &fakeClient{
		responses:   []openai.Response{{Text: "done"}},
		retryEvents: []openai.RetryEvent{{Attempt: 1, MaxAttempts: 1, Reason: openai.RetryReasonPeerStreamReset}},
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
		Config: config.Config{Model: "test", MaxSteps: 1, MaxToolCalls: 2, ContextTokens: 217600},
		Client: client,
		Trace:  recorder,
	}

	if _, err := runner.Run(t.Context(), Request{UserPrompt: "review", MaxSteps: 1}); err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != "runtime.status" || event.Value["phase"] != "retrying_provider" {
			continue
		}
		if fmt.Sprint(event.Value["step"]) != "1" || fmt.Sprint(event.Value["max_steps"]) != "1" ||
			fmt.Sprint(event.Value["tool_calls"]) != "0" || fmt.Sprint(event.Value["max_tool_calls"]) != "2" ||
			fmt.Sprint(event.Value["retry_attempt"]) != "1" || fmt.Sprint(event.Value["max_retry_attempts"]) != "1" ||
			fmt.Sprint(event.Value["abandoned_provider_attempt"]) != "1" || fmt.Sprint(event.Value["provider_attempt"]) != "2" ||
			event.Value["retry_reason"] != string(openai.RetryReasonPeerStreamReset) {
			t.Fatalf("retry runtime status = %#v", event)
		}
		return
	}
	t.Fatalf("trace missing retry runtime status: %#v", events)
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
	if instructions := client.requests[0].Instructions; !containsAll(instructions, "bounded agent loop", "model step 1 of 3", "2 of 2 local function tool calls remaining", "reduce material uncertainty", "do not call tools just to repeat provided context", "Conclude before the remaining budget reaches zero", "Do not ask the user for more evidence") {
		t.Fatalf("request instructions missing tool economy guidance: %s", instructions)
	}
	if strings.Contains(client.requests[0].Instructions, "Use skills_read") {
		t.Fatalf("request instructions should not mention unavailable skills_read: %s", client.requests[0].Instructions)
	}
}

func TestRunnerContinuesAfterDistinctCallsReturnEqualOutputs(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "grep", Arguments: `{"pattern":"first-missing-pattern"}`}}},
		{ToolCalls: []openai.ToolCall{{ID: "fc_2", CallID: "call_2", Name: "grep", Arguments: `{"pattern":"second-missing-pattern"}`}}},
		{Text: "done"},
	}}
	registry := tools.NewReviewRegistryWithSkills(repo, nil, tools.ReviewModeCodebase, tools.ReviewScope{}, gitctx.ChangeFingerprint{})
	runner := OpenAIRunner{
		Config:    config.Config{Model: "test", MaxSteps: 4, MaxToolCalls: 3},
		Client:    client,
		Tools:     registry,
		ToolSpecs: registry.Definitions([]string{"grep"}),
	}

	result, err := runner.Run(t.Context(), Request{UserPrompt: "review", MaxSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" || result.ToolCalls != 2 || len(client.requests) != 3 {
		t.Fatalf("result = %#v, requests = %d", result, len(client.requests))
	}
	var outputCallIDs []string
	for _, item := range client.requests[2].Input {
		if item.Type == "function_call_output" {
			outputCallIDs = append(outputCallIDs, item.CallID)
		}
	}
	if !slices.Equal(outputCallIDs, []string{"call_1", "call_2"}) {
		t.Fatalf("final request output call IDs = %v", outputCallIDs)
	}
}

func TestRunnerReturnsToolErrorsToModelForRecovery(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	if err := os.WriteFile(filepath.Join(repoDir, "actual.go"), []byte("package actual\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "read_file", Arguments: `{"path":"missing.go"}`}}},
		{ToolCalls: []openai.ToolCall{{ID: "fc_2", CallID: "call_2", Name: "read_file", Arguments: `{"path":"actual.go"}`}}},
		{Text: "recovered"},
	}}
	registry := tools.NewReviewRegistryWithSkills(repo, nil, tools.ReviewModeCodebase, tools.ReviewScope{}, gitctx.ChangeFingerprint{})
	var events []trace.Event
	recorder, err := trace.NewEventStream("review", func(event trace.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := OpenAIRunner{
		Config:    config.Config{Model: "test", MaxSteps: 4, MaxToolCalls: 3},
		Client:    client,
		Tools:     registry,
		ToolSpecs: registry.Definitions([]string{"read_file"}),
		Trace:     recorder,
	}

	result, err := runner.Run(t.Context(), Request{UserPrompt: "review", MaxSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "recovered" || result.ToolCalls != 2 || len(client.requests) != 3 {
		t.Fatalf("result = %#v, requests = %d", result, len(client.requests))
	}
	output := client.requests[1].Input[len(client.requests[1].Input)-1]
	if output.Type != "function_call_output" || output.CallID != "call_1" {
		t.Fatalf("tool error output item = %#v", output)
	}
	var envelope struct {
		OK    bool   `json:"ok"`
		Tool  string `json:"tool"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(output.Output), &envelope); err != nil {
		t.Fatalf("tool error output is not JSON: %v\n%s", err, output.Output)
	}
	if envelope.OK || envelope.Tool != "read_file" || !strings.Contains(envelope.Error, "missing.go") {
		t.Fatalf("tool error envelope = %#v", envelope)
	}
	corrected := client.requests[2].Input[len(client.requests[2].Input)-1]
	if corrected.Type != "function_call_output" || corrected.CallID != "call_2" || !strings.Contains(corrected.Output, "package actual") {
		t.Fatalf("corrected tool output item = %#v", corrected)
	}
	var traced bool
	for _, event := range events {
		content, ok := event.Value["content"].(map[string]any)
		if event.Kind == "tool-output" && ok && content["ok"] == false && content["tool"] == "read_file" {
			traced = true
		}
	}
	if !traced {
		t.Fatalf("trace missing tool error output: %#v", events)
	}
}

func TestRunnerStopsWhenReviewSnapshotChanges(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	path := filepath.Join(repoDir, "actual.go")
	if err := os.WriteFile(path, []byte("package actual\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := repo.UncommittedFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewReviewRegistryWithSkills(repo, nil, tools.ReviewModeUncommitted, tools.ReviewScope{}, fingerprint)
	if err := os.WriteFile(path, []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{responses: []openai.Response{{
		ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "read_file", Arguments: `{"path":"actual.go"}`}},
	}, {Text: "must not recover"}}}
	runner := OpenAIRunner{
		Config:    config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 2},
		Client:    client,
		Tools:     registry,
		ToolSpecs: registry.Definitions([]string{"read_file"}),
	}

	_, err = runner.Run(t.Context(), Request{UserPrompt: "review", MaxSteps: 3})
	if !errors.Is(err, gitctx.ErrChangeSnapshotStale) {
		t.Fatalf("error = %v, want stale review snapshot", err)
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want no recovery request", len(client.requests))
	}
}

func TestRunnerDoesNotRecoverToolErrorsAfterCancellation(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "read_file", Arguments: `{"path":"missing.go"}`}}},
		{Text: "must not recover"},
	}}
	registry := tools.NewReviewRegistryWithSkills(repo, nil, tools.ReviewModeCodebase, tools.ReviewScope{}, gitctx.ChangeFingerprint{})
	runner := OpenAIRunner{
		Config:    config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 2},
		Client:    client,
		Tools:     registry,
		ToolSpecs: registry.Definitions([]string{"read_file"}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = runner.Run(ctx, Request{UserPrompt: "review", MaxSteps: 3})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if len(client.requests) != 1 {
		t.Fatalf("requests = %d, want no recovery request", len(client.requests))
	}
}

func TestRunnerFinalizesOnRepeatedCanonicalToolCall(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "repo_summary", Arguments: `{}`}}},
		{ToolCalls: []openai.ToolCall{{ID: "fc_2", CallID: "call_2", Name: "repo_summary", Arguments: `{ }`}}},
		{Text: "done"},
	}}
	registry := tools.NewRegistryWithSkills(repo, nil)
	var events []trace.Event
	recorder, err := trace.NewEventStream("review", func(event trace.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 10, MaxToolCalls: 10},
		Client: client, Tools: registry,
		ToolSpecs: registry.Definitions([]string{"repo_summary"}), Trace: recorder,
	}

	result, err := runner.Run(t.Context(), Request{UserPrompt: "review", MaxSteps: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" || result.ToolCalls != 1 || len(client.requests) != 3 {
		t.Fatalf("result = %#v, requests = %d", result, len(client.requests))
	}
	foundGuard := false
	for _, event := range events {
		if event.Kind == "budget" && event.Value["reason"] == "repeated_tool_call" {
			foundGuard = true
		}
	}
	if !foundGuard {
		t.Fatalf("events missing repeated-tool guard: %#v", events)
	}
}

func TestRunnerRejectsInitialRequestAtEstimatedContextThreshold(t *testing.T) {
	t.Parallel()

	client := &fakeClient{responses: []openai.Response{{Text: "unexpected provider call"}}}
	var events []trace.Event
	recorder, err := trace.NewEventStream("review", func(event trace.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 10, MaxToolCalls: 10, ContextTokens: 1},
		Client: client, Trace: recorder,
	}

	_, err = runner.Run(t.Context(), Request{UserPrompt: "review", MaxSteps: 10})
	if err == nil || !strings.Contains(err.Error(), "initial request") {
		t.Fatalf("error = %v, want initial-request context-budget error", err)
	}
	if len(client.requests) != 0 {
		t.Fatalf("provider requests = %d, want 0", len(client.requests))
	}
	foundStatus, foundBudget := false, false
	for _, event := range events {
		if event.Kind == "runtime.status" && event.Value["estimated_context_tokens"] != nil && fmt.Sprint(event.Value["step"]) == "1" {
			foundStatus = true
		}
		if event.Kind == "budget" && event.Value["reason"] == "initial_context_budget_exhausted" {
			foundBudget = true
		}
	}
	if !foundStatus || !foundBudget {
		t.Fatalf("runtime status/budget missing: %#v", events)
	}
}

func TestRunnerFinalizesImmediatelyAtReportedContextThreshold(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{responses: []openai.Response{
		{
			ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "repo_summary", Arguments: `{}`}},
			RawJSON:   `{"usage":{"input_tokens":217600}}`,
		},
		{Text: "all findings"},
	}}
	registry := tools.NewRegistryWithSkills(repo, nil)
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 10, MaxToolCalls: 10, ContextTokens: 217600},
		Client: client, Tools: registry,
		ToolSpecs: registry.Definitions([]string{"repo_summary"}),
	}

	result, err := runner.Run(t.Context(), Request{UserPrompt: "review", MaxSteps: 10})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "all findings" || result.ToolCalls != 0 {
		t.Fatalf("result = %#v, want immediate finalization before tool execution", result)
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

func TestRunnerRejectsAllowedNameWithoutExposedDefinition(t *testing.T) {
	t.Parallel()

	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "go_doc", Arguments: `{"target":"strings","symbol":"","flags":[]}`}}},
	}}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 1},
		Client: client,
		Tools:  tools.NewRegistryWithSkills(nil, nil),
	}

	_, err := runner.Run(t.Context(), Request{
		SystemPrompt:     "system",
		UserPrompt:       "user",
		AllowedToolNames: []string{"go_doc"},
		MaxSteps:         1,
	})
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expected unavailable tool rejection, got %v", err)
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
		Config:             config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 1, MaxToolCalls: 2},
		Client:             client,
		Tools:              registry,
		ToolSpecs:          registry.Definitions([]string{"repo_summary"}),
		HostedCapabilities: []provider.HostedCapability{{Kind: provider.HostedCapabilityWebSearch, MaxCalls: 4}},
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
	if len(client.requests[0].HostedCapabilities) != 1 || len(client.requests[1].HostedCapabilities) != 0 {
		t.Fatalf("forced finalization hosted capabilities: first=%#v final=%#v", client.requests[0].HostedCapabilities, client.requests[1].HostedCapabilities)
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
	gitArgs := append([]string{"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "tag.forceSignAnnotated=false"}, args...)
	cmd := exec.Command("git", gitArgs...)
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

func hasEvent(events []trace.Event, kind, key, value string) bool {
	for _, event := range events {
		if event.Kind == kind && event.Value[key] == value {
			return true
		}
	}
	return false
}
