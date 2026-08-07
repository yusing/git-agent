package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/provider"
	"github.com/yusing/git-agent/internal/skillcmd"
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

type fakeSkillRunner struct{}

func (fakeSkillRunner) Run(context.Context, skillcmd.Command) (skillcmd.Output, error) {
	return skillcmd.Output{Stdout: "skill instructions"}, nil
}

type executionLog struct {
	mu    sync.Mutex
	order []string
}

func (l *executionLog) add(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.order = append(l.order, name)
}

func (l *executionLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.order)
}

type recordingTool struct {
	name    string
	order   *executionLog
	content string
	err     error
}

type delayedTool struct {
	name    string
	delay   time.Duration
	content string
}

func testToolDefinition(name string) tools.Definition {
	return tools.Definition{
		Name: name, Strict: true,
		Schema: map[string]any{"type": "object", "additionalProperties": true},
	}
}

func (t delayedTool) Definition() tools.Definition { return testToolDefinition(t.name) }

func (t delayedTool) Execute(ctx context.Context, _ tools.Invocation) (tools.Result, error) {
	timer := time.NewTimer(t.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	case <-timer.C:
		return tools.Result{Content: t.content}, nil
	}
}

func (t recordingTool) Definition() tools.Definition { return testToolDefinition(t.name) }

func (t recordingTool) Execute(_ context.Context, invocation tools.Invocation) (tools.Result, error) {
	t.order.add(invocation.Name)
	if t.err != nil {
		return tools.Result{}, t.err
	}
	return tools.Result{Content: t.content}, nil
}

type blockingTool struct {
	name    string
	started chan<- string
	release <-chan struct{}
}

func (t blockingTool) Definition() tools.Definition { return testToolDefinition(t.name) }

func (t blockingTool) Execute(ctx context.Context, _ tools.Invocation) (tools.Result, error) {
	select {
	case t.started <- t.name:
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	}
	select {
	case <-t.release:
		return tools.Result{Content: t.name + " output"}, nil
	case <-ctx.Done():
		return tools.Result{}, ctx.Err()
	}
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
		Client: client, PromptCacheKey: "review:task-id",
	}
	outcome, err := runner.RunNode(t.Context(), Request{
		UserPrompt: "review", MaxSteps: 2, ControlTool: &control, ParallelToolCalls: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Final != nil || outcome.Branch == nil || outcome.Branch.ToolCalls != 1 {
		t.Fatalf("outcome = %#v", outcome)
	}
	if outcome.Branch.ToolCallsByName["branch"] != 1 {
		t.Fatalf("branch tool calls by name = %#v", outcome.Branch.ToolCallsByName)
	}
	if len(client.requests) != 1 || len(client.requests[0].Tools) != 1 || client.requests[0].Tools[0].Name != "branch" {
		t.Fatalf("request tools = %#v", client.requests)
	}
	if !client.requests[0].ParallelToolCalls {
		t.Fatalf("control-tool request disabled parallel calls: %#v", client.requests[0])
	}
	if client.requests[0].PromptCacheKey != "review:task-id" ||
		!slices.ContainsFunc(client.requests[0].Input, func(item openai.Item) bool { return item.PromptCacheBreakpoint }) {
		t.Fatalf("root request cache controls = %#v", client.requests[0])
	}

	sameModel := outcome.Branch.ForkInput(`{"branch_id":"b1"}`, true)
	if !slices.ContainsFunc(sameModel, func(item openai.Item) bool { return item.Type == "reasoning" }) {
		t.Fatalf("same-model fork omitted reasoning: %#v", sameModel)
	}
	if !slices.ContainsFunc(sameModel, func(item openai.Item) bool { return item.PromptCacheBreakpoint }) {
		t.Fatalf("same-model fork omitted prompt cache breakpoint: %#v", sameModel)
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

func TestResultHistoryReturnsClone(t *testing.T) {
	result := Result{messages: []openai.Item{openai.NewMessage("assistant", "answer")}}
	history := result.History()
	history[0].Content = "changed"
	if got := result.History()[0].Content; got != "answer" {
		t.Fatalf("stored history = %q, want answer", got)
	}
}

func TestRunnerExecutesAdmittedToolBatchConcurrently(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		started := make(chan string)
		release := make(chan struct{})
		registry := tools.NewRegistry(nil, nil)
		names := []string{"first_tool", "second_tool", "third_tool"}
		for _, name := range names {
			registry.Register(blockingTool{name: name, started: started, release: release})
		}
		client := &fakeClient{responses: []openai.Response{
			{ToolCalls: []openai.ToolCall{
				{ID: "fc_1", CallID: "call_1", Name: names[0], Arguments: `{"value":1}`},
				{ID: "fc_2", CallID: "call_2", Name: names[1], Arguments: `{"value":2}`},
				{ID: "fc_3", CallID: "call_3", Name: names[2], Arguments: `{"value":3}`},
			}},
			{Text: "done"},
		}}
		var events []trace.Event
		recorder, err := trace.NewEventStream("explore", func(event trace.Event) error {
			events = append(events, event)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		runner := OpenAIRunner{
			Config: config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 3},
			Client: client, Tools: registry, ToolSpecs: registry.Definitions(names), Trace: recorder,
		}
		result := make(chan Result, 1)
		errResult := make(chan error, 1)
		go func() {
			got, err := runner.Run(t.Context(), Request{
				UserPrompt: "inspect", MaxSteps: 3, ParallelToolCalls: true,
			})
			result <- got
			errResult <- err
		}()

		startedNames := make([]string, 0, len(names))
		for range names {
			startedNames = append(startedNames, <-started)
		}
		close(release)
		got := <-result
		if err := <-errResult; err != nil {
			t.Fatal(err)
		}
		slices.Sort(startedNames)
		wantStarted := slices.Clone(names)
		slices.Sort(wantStarted)
		if !slices.Equal(startedNames, wantStarted) {
			t.Fatalf("started tools = %v, want %v", startedNames, wantStarted)
		}
		if got.Text != "done" || got.ToolCalls != 3 || len(client.requests) != 2 {
			t.Fatalf("result = %#v, requests = %d", got, len(client.requests))
		}
		if !client.requests[0].ParallelToolCalls || !client.requests[1].ParallelToolCalls {
			t.Fatalf("parallel policy was not preserved across requests: %#v", client.requests)
		}
		var outputCallIDs []string
		for _, item := range client.requests[1].Input {
			if item.Type == "function_call_output" {
				outputCallIDs = append(outputCallIDs, item.CallID)
			}
		}
		if !slices.Equal(outputCallIDs, []string{"call_1", "call_2", "call_3"}) {
			t.Fatalf("output call IDs = %v", outputCallIDs)
		}
		var toolEvents []string
		for _, event := range events {
			if event.Kind == "tool-call" || event.Kind == "tool-output" {
				toolEvents = append(toolEvents, event.Kind)
			}
		}
		if !slices.Equal(toolEvents, []string{
			"tool-call", "tool-call", "tool-call",
			"tool-output", "tool-output", "tool-output",
		}) {
			t.Fatalf("tool event order = %v", toolEvents)
		}
		for _, name := range names {
			if got.ToolCallsByName[name] != 1 {
				t.Fatalf("tool counts = %#v", got.ToolCallsByName)
			}
		}
	})
}

func TestRunnerRejectsConcurrentBatchWhenReviewSnapshotChanges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	path := filepath.Join(dir, "app.txt")
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "app.txt")
	runGit(t, dir, "commit", "-m", "base")
	if err := os.WriteFile(path, []byte("launch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := repo.UncommittedFingerprint()
	if err != nil {
		t.Fatal(err)
	}

	control := tools.Definition{Name: "branch", Strict: true, Schema: map[string]any{"type": "object"}}
	started := make(chan string)
	release := make(chan struct{})
	names := []string{"read_file", "grep"}
	registry := tools.NewReviewRegistry(repo, nil, tools.ReviewModeUncommitted, tools.ReviewScope{}, fingerprint)
	for _, name := range names {
		registry.Register(blockingTool{name: name, started: started, release: release})
	}
	client := &fakeClient{responses: []openai.Response{{ToolCalls: []openai.ToolCall{
		{ID: "fc_1", CallID: "call_1", Name: "branch", Arguments: `{"branches":[]}`},
		{ID: "fc_2", CallID: "call_2", Name: names[0], Arguments: `{}`},
		{ID: "fc_3", CallID: "call_3", Name: names[1], Arguments: `{}`},
	}}}}
	var events []trace.Event
	recorder, err := trace.NewEventStream("review", func(event trace.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := OpenAIRunner{
		Config: config.Config{MaxSteps: 2, MaxToolCalls: 3}, Client: client,
		Tools: registry, ToolSpecs: registry.Definitions(names), Trace: recorder,
	}
	outcomeResult := make(chan NodeResult, 1)
	errResult := make(chan error, 1)
	go func() {
		outcome, err := runner.RunNode(t.Context(), Request{
			UserPrompt: "review", MaxSteps: 2, ControlTool: &control, ParallelToolCalls: true,
		})
		outcomeResult <- outcome
		errResult <- err
	}()

	startedNames := []string{<-started, <-started}
	if err := os.WriteFile(path, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	close(release)
	outcome := <-outcomeResult
	err = <-errResult
	slices.Sort(startedNames)
	wantStarted := slices.Clone(names)
	slices.Sort(wantStarted)
	if !slices.Equal(startedNames, wantStarted) {
		t.Fatalf("started tools = %v, want %v", startedNames, wantStarted)
	}
	if !errors.Is(err, gitctx.ErrChangeSnapshotStale) {
		t.Fatalf("error = %v, want stale snapshot", err)
	}
	if outcome.Branch != nil || outcome.Final != nil {
		t.Fatalf("stale batch produced outcome = %#v", outcome)
	}
	if slices.ContainsFunc(events, func(event trace.Event) bool { return event.Kind == "tool-output" }) {
		t.Fatalf("stale batch traced tool output: %#v", events)
	}
}

func TestRunnerReportsProviderToolAndValidationTimings(t *testing.T) {
	t.Parallel()
	const toolDelay = 2 * time.Millisecond
	names := []string{"list_files", "grep", "read_file"}
	registry := tools.NewRegistry(nil, nil)
	for _, name := range names {
		registry.Register(delayedTool{name: name, delay: toolDelay, content: name + " output"})
	}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{
			{ID: "fc_1", CallID: "call_1", Name: names[0], Arguments: `{}`},
			{ID: "fc_2", CallID: "call_2", Name: names[1], Arguments: `{}`},
			{ID: "fc_3", CallID: "call_3", Name: names[2], Arguments: `{}`},
		}},
		{Text: "done"},
	}}
	var timings []Timing
	runner := OpenAIRunner{
		Config:    config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 3},
		Client:    client,
		Tools:     registry,
		ToolSpecs: registry.Definitions(names),
		Validator: func(string) []string { return nil },
		Timing:    func(timing Timing) { timings = append(timings, timing) },
	}
	if _, err := runner.Run(t.Context(), Request{
		UserPrompt: "explore", MaxSteps: 3, ParallelToolCalls: true,
	}); err != nil {
		t.Fatal(err)
	}

	phaseCounts := map[string]int{}
	toolNames := map[string]bool{}
	for _, timing := range timings {
		phaseCounts[timing.Phase]++
		if timing.Duration < 0 {
			t.Fatalf("negative timing: %#v", timing)
		}
		if timing.Phase == "tool" {
			toolNames[timing.Tool] = true
			if timing.Step != 1 || timing.Duration < toolDelay {
				t.Errorf("tool timing = %#v, want step 1 and duration >= %s", timing, toolDelay)
			}
		}
	}
	for phase, want := range map[string]int{
		"provider_request": 2,
		"tool":             len(names),
		"tool_batch":       1,
		"validation":       1,
	} {
		if got := phaseCounts[phase]; got != want {
			t.Errorf("%s timing count = %d, want %d: %#v", phase, got, want, timings)
		}
	}
	for _, name := range names {
		if !toolNames[name] {
			t.Errorf("missing timing for tool %s: %#v", name, timings)
		}
	}
}

func BenchmarkRunnerExploreToolBatch(b *testing.B) {
	const toolDelay = 2 * time.Millisecond
	names := []string{"list_files", "grep", "read_file"}
	registry := tools.NewRegistry(nil, nil)
	for _, name := range names {
		registry.Register(delayedTool{name: name, delay: toolDelay, content: name + " output"})
	}
	toolSpecs := registry.Definitions(names)
	b.ReportMetric(float64(len(names)), "tools/op")
	b.ResetTimer()

	for b.Loop() {
		client := &fakeClient{responses: []openai.Response{
			{ToolCalls: []openai.ToolCall{
				{ID: "fc_1", CallID: "call_1", Name: names[0], Arguments: `{}`},
				{ID: "fc_2", CallID: "call_2", Name: names[1], Arguments: `{}`},
				{ID: "fc_3", CallID: "call_3", Name: names[2], Arguments: `{}`},
			}},
			{Text: "done"},
		}}
		runner := OpenAIRunner{
			Config: config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 3},
			Client: client, Tools: registry, ToolSpecs: toolSpecs,
		}
		result, err := runner.Run(b.Context(), Request{
			UserPrompt: "explore", MaxSteps: 3, ParallelToolCalls: true,
		})
		if err != nil {
			b.Fatal(err)
		}
		if result.Text != "done" || result.ToolCalls != len(names) {
			b.Fatalf("result = %#v", result)
		}
	}
}

func TestRunnerRejectsInvalidToolBatchBeforeExecution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		calls   []openai.ToolCall
		wantErr string
	}{
		{
			name: "disallowed call",
			calls: []openai.ToolCall{
				{ID: "fc_1", CallID: "call_1", Name: "first_tool", Arguments: `{}`},
				{ID: "fc_2", CallID: "call_2", Name: "not_allowed", Arguments: `{}`},
			},
			wantErr: "not allowed",
		},
		{
			name: "missing call ID",
			calls: []openai.ToolCall{
				{ID: "fc_1", CallID: "call_1", Name: "first_tool", Arguments: `{}`},
				{Name: "second_tool", Arguments: `{}`},
			},
			wantErr: "call ID is required",
		},
		{
			name: "duplicate call ID",
			calls: []openai.ToolCall{
				{ID: "fc_1", CallID: "call_same", Name: "first_tool", Arguments: `{}`},
				{ID: "fc_2", CallID: "call_same", Name: "second_tool", Arguments: `{}`},
			},
			wantErr: "duplicate function call ID",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var executionOrder executionLog
			registry := tools.NewRegistry(nil, nil)
			names := []string{"first_tool", "second_tool"}
			for _, name := range names {
				registry.Register(recordingTool{name: name, order: &executionOrder, content: "output"})
			}
			client := &fakeClient{responses: []openai.Response{{ToolCalls: test.calls}}}
			runner := OpenAIRunner{
				Config: config.Config{Model: "test", MaxSteps: 2, MaxToolCalls: 4},
				Client: client, Tools: registry, ToolSpecs: registry.Definitions(names),
			}

			_, err := runner.Run(t.Context(), Request{UserPrompt: "inspect", MaxSteps: 2, ParallelToolCalls: true})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want %q", err, test.wantErr)
			}
			if len(executionOrder.snapshot()) != 0 {
				t.Fatalf("executed invalid batch members: %v", executionOrder.snapshot())
			}
		})
	}
}

func TestRunnerDetectsDuplicateCallsWithinBatchBeforeExecution(t *testing.T) {
	t.Parallel()

	var executionOrder executionLog
	registry := tools.NewRegistry(nil, nil)
	registry.Register(recordingTool{name: "first_tool", order: &executionOrder, content: "output"})
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{
			{ID: "fc_1", CallID: "call_1", Name: "first_tool", Arguments: `{"value":1}`},
			{ID: "fc_2", CallID: "call_2", Name: "first_tool", Arguments: `{ "value": 1 }`},
		}},
		{Text: "finalized"},
	}}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 3},
		Client: client, Tools: registry, ToolSpecs: registry.Definitions([]string{"first_tool"}),
	}

	result, err := runner.Run(t.Context(), Request{UserPrompt: "inspect", MaxSteps: 3, ParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "finalized" || result.ToolCalls != 0 || len(executionOrder.snapshot()) != 0 {
		t.Fatalf("result = %#v, execution order = %v", result, executionOrder.snapshot())
	}
	if len(client.requests) != 2 || client.requests[1].ParallelToolCalls {
		t.Fatalf("forced-finalization requests = %#v", client.requests)
	}
	var outputCallIDs []string
	for _, item := range client.requests[1].Input {
		if item.Type == "function_call_output" {
			outputCallIDs = append(outputCallIDs, item.CallID)
		}
	}
	if !slices.Equal(outputCallIDs, []string{"call_1", "call_2"}) {
		t.Fatalf("forced-finalization output call IDs = %v", outputCallIDs)
	}
}

func TestRunnerRejectsBatchContainingCallRepeatedFromPriorStep(t *testing.T) {
	t.Parallel()

	var executionOrder executionLog
	registry := tools.NewRegistry(nil, nil)
	names := []string{"first_tool", "second_tool"}
	for _, name := range names {
		registry.Register(recordingTool{name: name, order: &executionOrder, content: "output"})
	}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "first_tool", Arguments: `{"value":1}`}}},
		{ToolCalls: []openai.ToolCall{
			{ID: "fc_2", CallID: "call_2", Name: "second_tool", Arguments: `{"value":2}`},
			{ID: "fc_3", CallID: "call_3", Name: "first_tool", Arguments: `{ "value": 1 }`},
		}},
		{Text: "finalized"},
	}}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 4, MaxToolCalls: 4},
		Client: client, Tools: registry, ToolSpecs: registry.Definitions(names),
	}

	result, err := runner.Run(t.Context(), Request{UserPrompt: "inspect", MaxSteps: 4, ParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "finalized" || result.ToolCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
	if !slices.Equal(executionOrder.snapshot(), []string{"first_tool"}) {
		t.Fatalf("execution order = %v, want only the prior admitted call", executionOrder.snapshot())
	}
}

func TestRunnerRejectsCallIDInheritedFromBranchContinuation(t *testing.T) {
	t.Parallel()

	var executionOrder executionLog
	registry := tools.NewRegistry(nil, nil)
	registry.Register(recordingTool{name: "first_tool", order: &executionOrder, content: "output"})
	client := &fakeClient{responses: []openai.Response{{ToolCalls: []openai.ToolCall{{
		ID: "fc_child", CallID: "call_parent", Name: "first_tool", Arguments: `{"value":2}`,
	}}}}}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 2, MaxToolCalls: 2},
		Client: client, Tools: registry, ToolSpecs: registry.Definitions([]string{"first_tool"}),
	}
	input := []openai.Item{
		{Type: "function_call", RawJSON: `{"id":"fc_parent","type":"function_call","call_id":"call_parent","name":"branch","arguments":"{\"branches\":[]}"}`},
		openai.NewFunctionCallOutput("call_parent", `{"branch_id":"b1"}`),
	}

	_, err := runner.Run(t.Context(), Request{
		Input: input, MaxSteps: 2, ParallelToolCalls: true,
	})
	if err == nil || !strings.Contains(err.Error(), `duplicate function call ID "call_parent"`) {
		t.Fatalf("error = %v, want inherited call-ID rejection", err)
	}
	if len(executionOrder.snapshot()) != 0 {
		t.Fatalf("executed call with inherited ID: %v", executionOrder.snapshot())
	}
}

func TestRunnerDetectsInvocationRepeatedFromInheritedHistory(t *testing.T) {
	t.Parallel()

	var executionOrder executionLog
	registry := tools.NewRegistry(nil, nil)
	registry.Register(recordingTool{name: "first_tool", order: &executionOrder, content: "output"})
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{{
			ID: "fc_new", CallID: "call_new", Name: "first_tool", Arguments: `{ "value": 1 }`,
		}}},
		{Text: "finalized"},
	}}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 2, MaxToolCalls: 2},
		Client: client, Tools: registry, ToolSpecs: registry.Definitions([]string{"first_tool"}),
	}
	input := []openai.Item{
		openai.NewFunctionCall(openai.ToolCall{ID: "fc_old", CallID: "call_old", Name: "first_tool", Arguments: `{"value":1}`}),
		openai.NewFunctionCallOutput("call_old", "prior output"),
	}

	result, err := runner.Run(t.Context(), Request{
		Input: input, MaxSteps: 2, ParallelToolCalls: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "finalized" || result.ToolCalls != 0 || len(executionOrder.snapshot()) != 0 {
		t.Fatalf("result = %#v, execution order = %v", result, executionOrder.snapshot())
	}
}

func TestRunnerChecksContextBudgetAfterCompletingAdmittedBatch(t *testing.T) {
	t.Parallel()

	var executionOrder executionLog
	registry := tools.NewRegistry(nil, nil)
	names := []string{"first_tool", "second_tool", "third_tool"}
	largeOutput := strings.Repeat("x", 5000)
	for _, name := range names {
		registry.Register(recordingTool{name: name, order: &executionOrder, content: largeOutput})
	}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{
			{ID: "fc_1", CallID: "call_1", Name: names[0], Arguments: `{}`},
			{ID: "fc_2", CallID: "call_2", Name: names[1], Arguments: `{}`},
			{ID: "fc_3", CallID: "call_3", Name: names[2], Arguments: `{}`},
		}},
		{Text: "context final"},
	}}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 3, ContextTokens: 2000},
		Client: client, Tools: registry, ToolSpecs: registry.Definitions(names),
	}

	result, err := runner.Run(t.Context(), Request{UserPrompt: "inspect", MaxSteps: 3, ParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	executed := executionOrder.snapshot()
	slices.Sort(executed)
	if result.Text != "context final" || result.ToolCalls != 3 || !slices.Equal(executed, names) {
		t.Fatalf("result = %#v, executed tools = %v", result, executed)
	}
	if len(client.requests) != 2 || len(client.requests[1].Tools) != 0 {
		t.Fatalf("context finalization requests = %#v", client.requests)
	}
	var outputCallIDs []string
	for _, item := range client.requests[1].Input {
		if item.Type == "function_call_output" {
			outputCallIDs = append(outputCallIDs, item.CallID)
		}
	}
	if !slices.Equal(outputCallIDs, []string{"call_1", "call_2", "call_3"}) {
		t.Fatalf("context-finalization output call IDs = %v", outputCallIDs)
	}
}

func TestRunnerCompletesBatchAfterEncodedToolFailure(t *testing.T) {
	t.Parallel()

	var executionOrder executionLog
	registry := tools.NewRegistry(nil, nil)
	registry.Register(recordingTool{name: "failing_tool", order: &executionOrder, err: errors.New("expected failure")})
	registry.Register(recordingTool{name: "successful_tool", order: &executionOrder, content: "success"})
	names := []string{"failing_tool", "successful_tool"}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{
			{ID: "fc_1", CallID: "call_1", Name: names[0], Arguments: `{}`},
			{ID: "fc_2", CallID: "call_2", Name: names[1], Arguments: `{}`},
		}},
		{Text: "recovered"},
	}}
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 2},
		Client: client, Tools: registry, ToolSpecs: registry.Definitions(names),
	}

	result, err := runner.Run(t.Context(), Request{UserPrompt: "inspect", MaxSteps: 3, ParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	executed := executionOrder.snapshot()
	slices.Sort(executed)
	if result.Text != "recovered" || result.ToolCalls != 2 || !slices.Equal(executed, names) {
		t.Fatalf("result = %#v, executed tools = %v", result, executed)
	}
	var outputs []openai.Item
	for _, item := range client.requests[1].Input {
		if item.Type == "function_call_output" {
			outputs = append(outputs, item)
		}
	}
	if len(outputs) != 2 || outputs[0].CallID != "call_1" || outputs[1].CallID != "call_2" {
		t.Fatalf("outputs = %#v", outputs)
	}
	if !strings.Contains(outputs[0].Output, "expected failure") || outputs[1].Output != "success" {
		t.Fatalf("encoded failure/success outputs = %#v", outputs)
	}
}

func TestRunnerCompletesOrdinaryCallsBeforeForkingMixedBranchBatch(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		control := tools.Definition{Name: "branch", Strict: true, Schema: map[string]any{"type": "object"}}
		started := make(chan string)
		release := make(chan struct{})
		names := []string{"read_file", "grep"}
		registry := tools.NewRegistry(nil, nil)
		for _, name := range names {
			registry.Register(blockingTool{name: name, started: started, release: release})
		}
		client := &fakeClient{responses: []openai.Response{{
			ToolCalls: []openai.ToolCall{
				{ID: "fc_1", CallID: "call_1", Name: "branch", Arguments: `{"branches":[]}`},
				{ID: "fc_2", CallID: "call_2", Name: names[0], Arguments: `{}`},
				{ID: "fc_3", CallID: "call_3", Name: names[1], Arguments: `{}`},
			},
		}}}
		runner := OpenAIRunner{
			Config: config.Config{MaxSteps: 2, MaxToolCalls: 3}, Client: client,
			Tools: registry, ToolSpecs: registry.Definitions(names),
		}
		outcomeResult := make(chan NodeResult, 1)
		errResult := make(chan error, 1)
		go func() {
			outcome, err := runner.RunNode(t.Context(), Request{
				UserPrompt: "review", MaxSteps: 2, ControlTool: &control, ParallelToolCalls: true,
			})
			outcomeResult <- outcome
			errResult <- err
		}()

		startedNames := []string{<-started, <-started}
		close(release)
		outcome := <-outcomeResult
		if err := <-errResult; err != nil {
			t.Fatal(err)
		}
		slices.Sort(startedNames)
		wantStarted := slices.Clone(names)
		slices.Sort(wantStarted)
		if !slices.Equal(startedNames, wantStarted) {
			t.Fatalf("started tools = %v, want %v", startedNames, wantStarted)
		}
		if outcome.Final != nil || outcome.Branch == nil || outcome.Branch.ToolCalls != 3 {
			t.Fatalf("outcome = %#v", outcome)
		}
		if outcome.Branch.ToolCallsByName["branch"] != 1 ||
			outcome.Branch.ToolCallsByName[names[0]] != 1 || outcome.Branch.ToolCallsByName[names[1]] != 1 {
			t.Fatalf("tool calls by name = %#v", outcome.Branch.ToolCallsByName)
		}
		if len(client.requests) != 1 || !client.requests[0].ParallelToolCalls {
			t.Fatalf("mixed control request policy = %#v", client.requests)
		}
		fork := outcome.Branch.ForkInput(`{"branch_id":"b1"}`, true)
		var outputCallIDs []string
		for _, item := range fork {
			if item.Type == "function_call_output" {
				outputCallIDs = append(outputCallIDs, item.CallID)
			}
		}
		if !slices.Equal(outputCallIDs, []string{"call_2", "call_3", "call_1"}) {
			t.Fatalf("fork output call IDs = %v, want ordinary outputs before branch output", outputCallIDs)
		}
	})
}

func TestRunnerRejectsMultipleBranchCallsBeforeOrdinaryExecution(t *testing.T) {
	t.Parallel()

	control := tools.Definition{Name: "branch", Strict: true, Schema: map[string]any{"type": "object"}}
	var executionOrder executionLog
	registry := tools.NewRegistry(nil, nil)
	registry.Register(recordingTool{name: "read_file", order: &executionOrder, content: "file content"})
	client := &fakeClient{responses: []openai.Response{{ToolCalls: []openai.ToolCall{
		{ID: "fc_1", CallID: "call_1", Name: "branch", Arguments: `{"branches":[]}`},
		{ID: "fc_2", CallID: "call_2", Name: "read_file", Arguments: `{}`},
		{ID: "fc_3", CallID: "call_3", Name: "branch", Arguments: `{"branches":[{}]}`},
	}}}}
	runner := OpenAIRunner{
		Config: config.Config{MaxSteps: 2, MaxToolCalls: 3}, Client: client,
		Tools: registry, ToolSpecs: registry.Definitions([]string{"read_file"}),
	}
	_, err := runner.RunNode(t.Context(), Request{
		UserPrompt: "review", MaxSteps: 2, ControlTool: &control, ParallelToolCalls: true,
	})
	if err == nil || !strings.Contains(err.Error(), "may be called at most once") {
		t.Fatalf("error = %v", err)
	}
	if len(executionOrder.snapshot()) != 0 {
		t.Fatalf("executed tools from invalid batch = %v", executionOrder.snapshot())
	}
	if len(client.requests) != 1 || !client.requests[0].ParallelToolCalls {
		t.Fatalf("multiple-control request policy = %#v", client.requests)
	}
}

func TestRunnerMixedBranchBatchReturnsRecoverableOrdinaryErrorBeforeFork(t *testing.T) {
	t.Parallel()

	control := tools.Definition{Name: "branch", Strict: true, Schema: map[string]any{"type": "object"}}
	var executionOrder executionLog
	registry := tools.NewRegistry(nil, nil)
	registry.Register(recordingTool{name: "read_file", order: &executionOrder, err: errors.New("missing file")})
	client := &fakeClient{responses: []openai.Response{{ToolCalls: []openai.ToolCall{
		{ID: "fc_1", CallID: "call_1", Name: "branch", Arguments: `{"branches":[]}`},
		{ID: "fc_2", CallID: "call_2", Name: "read_file", Arguments: `{}`},
	}}}}
	runner := OpenAIRunner{
		Config: config.Config{MaxSteps: 2, MaxToolCalls: 2}, Client: client,
		Tools: registry, ToolSpecs: registry.Definitions([]string{"read_file"}),
	}
	outcome, err := runner.RunNode(t.Context(), Request{
		UserPrompt: "review", MaxSteps: 2, ControlTool: &control, ParallelToolCalls: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Branch == nil {
		t.Fatalf("outcome = %#v", outcome)
	}
	fork := outcome.Branch.ForkInput(`{"branch_id":"b1"}`, true)
	var outputs []openai.Item
	for _, item := range fork {
		if item.Type == "function_call_output" {
			outputs = append(outputs, item)
		}
	}
	if len(outputs) != 2 || outputs[0].CallID != "call_2" || !strings.Contains(outputs[0].Output, "missing file") ||
		outputs[1].CallID != "call_1" {
		t.Fatalf("fork outputs = %#v", outputs)
	}
}

func TestRunnerMixedBranchBatchDoesNotForkAfterFatalOrdinaryError(t *testing.T) {
	t.Parallel()

	control := tools.Definition{Name: "branch", Strict: true, Schema: map[string]any{"type": "object"}}
	var executionOrder executionLog
	registry := tools.NewRegistry(nil, nil)
	registry.Register(recordingTool{name: "read_file", order: &executionOrder, err: gitctx.ErrChangeSnapshotStale})
	client := &fakeClient{responses: []openai.Response{{ToolCalls: []openai.ToolCall{
		{ID: "fc_1", CallID: "call_1", Name: "branch", Arguments: `{"branches":[]}`},
		{ID: "fc_2", CallID: "call_2", Name: "read_file", Arguments: `{}`},
	}}}}
	runner := OpenAIRunner{
		Config: config.Config{MaxSteps: 2, MaxToolCalls: 2}, Client: client,
		Tools: registry, ToolSpecs: registry.Definitions([]string{"read_file"}),
	}
	outcome, err := runner.RunNode(t.Context(), Request{
		UserPrompt: "review", MaxSteps: 2, ControlTool: &control, ParallelToolCalls: true,
	})
	if !errors.Is(err, gitctx.ErrChangeSnapshotStale) {
		t.Fatalf("error = %v, want stale snapshot", err)
	}
	if outcome.Branch != nil || outcome.Final != nil {
		t.Fatalf("fatal ordinary error produced outcome = %#v", outcome)
	}
}

func TestRunnerRejectsMixedBranchBatchWithoutRegistry(t *testing.T) {
	t.Parallel()

	control := tools.Definition{Name: "branch", Strict: true, Schema: map[string]any{"type": "object"}}
	client := &fakeClient{responses: []openai.Response{{ToolCalls: []openai.ToolCall{
		{ID: "fc_1", CallID: "call_1", Name: "branch", Arguments: `{"branches":[]}`},
		{ID: "fc_2", CallID: "call_2", Name: "read_file", Arguments: `{}`},
	}}}}
	runner := OpenAIRunner{
		Config: config.Config{MaxSteps: 2, MaxToolCalls: 2}, Client: client,
		ToolSpecs: []tools.Definition{{Name: "read_file", Strict: true, Schema: map[string]any{"type": "object"}}},
	}
	_, err := runner.RunNode(t.Context(), Request{
		UserPrompt: "review", MaxSteps: 2, ControlTool: &control, ParallelToolCalls: true,
	})
	if err == nil || !strings.Contains(err.Error(), "no registry is configured") {
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
	registry := tools.NewReviewRegistry(repo, nil, tools.ReviewModeCodebase, tools.ReviewScope{}, gitctx.ChangeFingerprint{})
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
	finalInput := client.requests[2].Input
	if len(finalInput) < 2 {
		t.Fatalf("forced-finalization input = %#v", finalInput)
	}
	output := finalInput[len(finalInput)-2]
	if output.Type != "function_call_output" || output.CallID != "call_2" {
		t.Fatalf("forced-finalization tool output = %#v", output)
	}
	var envelope struct {
		OK    bool   `json:"ok"`
		Tool  string `json:"tool"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(output.Output), &envelope); err != nil {
		t.Fatalf("decode forced-finalization tool output: %v", err)
	}
	if envelope.OK || envelope.Tool != "branch" || !strings.Contains(envelope.Error, "finalizing without tools") {
		t.Fatalf("forced-finalization tool output envelope = %#v", envelope)
	}
	if !slices.ContainsFunc(events, func(event trace.Event) bool {
		return event.Kind == "budget" &&
			event.Value["kind"] == string(BudgetKindToolCalls) &&
			event.Value["decision"] == "finalize"
	}) {
		t.Fatalf("budget events = %#v", events)
	}
}

func TestCompletePendingFunctionCallsValidatesConversationHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       []openai.Item
		wantErr     string
		wantAddedID string
		wantTool    string
	}{
		{
			name: "positive complete pair",
			input: []openai.Item{
				openai.NewFunctionCall(openai.ToolCall{ID: "fc_done", CallID: "call_done", Name: "read_file", Arguments: `{}`}),
				openai.NewFunctionCallOutput("call_done", `{"ok":true}`),
			},
		},
		{
			name: "negative missing output",
			input: []openai.Item{
				{Type: "function_call", RawJSON: `{"id":"fc_pending","type":"function_call","call_id":"call_pending","name":"grep","arguments":"{}"}`},
			},
			wantAddedID: "call_pending",
			wantTool:    "grep",
		},
		{
			name: "malformed raw call",
			input: []openai.Item{
				{Type: "function_call", RawJSON: `{"type":"function_call","call_id":`},
			},
			wantErr: "decode function call",
		},
		{
			name: "orphan output",
			input: []openai.Item{
				openai.NewFunctionCallOutput("call_missing", `{"ok":false}`),
			},
			wantErr: "has no preceding call",
		},
		{
			name: "unrelated text collision",
			input: []openai.Item{
				openai.NewMessage("user", `the text says "type":"function_call" and call_pending`),
			},
		},
		{
			name: "unknown future item",
			input: []openai.Item{
				{Type: "future_tool_exchange", RawJSON: `{"type":"future_tool_exchange","call_id":"call_future"}`},
			},
		},
		{
			name: "unknown future tool",
			input: []openai.Item{
				openai.NewFunctionCall(openai.ToolCall{ID: "fc_future", CallID: "call_future", Name: "future_tool", Arguments: `{}`}),
			},
			wantAddedID: "call_future",
			wantTool:    "future_tool",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			completed, err := completePendingFunctionCalls(test.input)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.wantAddedID == "" {
				if !slices.Equal(completed, test.input) {
					t.Fatalf("completed history changed:\n got: %#v\nwant: %#v", completed, test.input)
				}
				return
			}
			if len(completed) != len(test.input)+1 {
				t.Fatalf("completed history = %#v", completed)
			}
			output := completed[len(completed)-1]
			if output.Type != "function_call_output" || output.CallID != test.wantAddedID {
				t.Fatalf("completed output = %#v", output)
			}
			var envelope struct {
				OK   bool   `json:"ok"`
				Tool string `json:"tool"`
			}
			if err := json.Unmarshal([]byte(output.Output), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.OK || envelope.Tool != test.wantTool {
				t.Fatalf("completed output envelope = %#v", envelope)
			}
		})
	}
}

func TestRequestInstructionsRequireReadFilePathProvenance(t *testing.T) {
	t.Parallel()

	instructions := requestInstructions("", []openai.ToolSpec{{Name: "read_file"}}, nil)
	if !containsAll(instructions,
		"path copied verbatim from prepared context or prior repository-tool output",
		"Discover paths with available inventory or search tools first",
		"Package import paths, package names, types, and symbols do not imply filenames",
	) {
		t.Fatalf("read_file instructions missing path provenance contract: %s", instructions)
	}

	withoutReadFile := requestInstructions("", []openai.ToolSpec{{Name: "repo_summary"}}, nil)
	if strings.Contains(withoutReadFile, "do not imply filenames") {
		t.Fatalf("instructions mention read_file contract without read_file: %s", withoutReadFile)
	}

	withHostedSearch := requestInstructions("", nil, []provider.HostedCapability{{Kind: provider.HostedCapabilityWebSearch}})
	if !strings.Contains(withHostedSearch, "web_search (provider-hosted)") || strings.Contains(withHostedSearch, "No tools are available") {
		t.Fatalf("instructions do not advertise hosted web search: %s", withHostedSearch)
	}
}

func TestRunnerObservesProviderUsageAcrossSteps(t *testing.T) {
	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{responses: []openai.Response{
		{Usage: openai.Usage{InputTokens: 20, CachedInputTokens: 8, CacheWriteInputTokens: 12, OutputTokens: 4, ReasoningTokens: 3, TotalTokens: 24}, ToolCalls: []openai.ToolCall{{ID: "call-1", Name: "repo_summary", Arguments: `{}`}}},
		{Usage: openai.Usage{InputTokens: 30, OutputTokens: 6, TotalTokens: 36}, Text: "done"},
	}}
	registry := tools.NewRegistry(repo, nil)
	var usage openai.Usage
	var metrics bytes.Buffer
	runner := OpenAIRunner{
		Config: config.Config{MaxSteps: 2, MaxToolCalls: 2}, Client: client, Tools: registry,
		ToolSpecs: registry.Definitions([]string{"repo_summary"}),
		ObserveUsage: func(value openai.Usage) {
			usage.Add(value)
		},
		UsageOutput: &metrics,
	}
	if _, err := runner.Run(t.Context(), Request{UserPrompt: "review", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 50 || usage.CachedInputTokens != 8 || usage.CacheWriteInputTokens != 12 || usage.OutputTokens != 10 || usage.ReasoningTokens != 3 || usage.TotalTokens != 60 {
		t.Fatalf("usage = %#v", usage)
	}
	if output := metrics.String(); !containsAll(output,
		"llm.usage", "step=1", "input_tokens=20", "cached_input_tokens=8",
		"cache_write_input_tokens=12", "output_tokens=4", "step=2", "input_tokens=30",
	) {
		t.Fatalf("usage metrics = %q", output)
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
	var resp openai.Response
	if len(f.responses) > 0 {
		resp = f.responses[0]
		f.responses = f.responses[1:]
	}
	if len(f.responseErrors) > 0 {
		err := f.responseErrors[0]
		f.responseErrors = f.responseErrors[1:]
		if err != nil {
			return resp, err
		}
	}
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
			{TurnState: "sticky-route"},
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
	registry := tools.NewRegistry(repo, nil)
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
	if client.requests[0].TurnState != "" || client.requests[1].TurnState != "sticky-route" || client.requests[2].TurnState != "sticky-route" {
		t.Fatalf("request turn states = %q, %q, %q", client.requests[0].TurnState, client.requests[1].TurnState, client.requests[2].TurnState)
	}
	for index := 1; index < len(client.requests); index++ {
		if len(client.requests[index].HostedCapabilities) != 0 {
			t.Fatalf("request %d re-enabled hosted capability: %#v", index, client.requests[index].HostedCapabilities)
		}
		if len(client.requests[index].Tools) != 1 || client.requests[index].Tools[0].Name != "repo_summary" {
			t.Fatalf("request %d lost local tools: %#v", index, client.requests[index].Tools)
		}
		if client.requests[index].Instructions != client.requests[0].Instructions {
			t.Fatalf("request %d instructions changed across cacheable steps", index)
		}
		if !slices.ContainsFunc(client.requests[index].Input, func(item openai.Item) bool {
			return strings.Contains(item.Content, "hosted web lookup was unavailable")
		}) {
			t.Fatalf("request %d missing appended capability disclosure: %#v", index, client.requests[index].Input)
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
	history := result.History()
	if len(history) == 0 || history[len(history)-1].Content != "Add parser" {
		t.Fatalf("history ends with %#v, want repaired assistant response", history)
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
		{TurnState: "sticky-route", ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "repo_summary", Arguments: "{}"}}},
		{Text: "Add parser"},
	}}
	registry := tools.NewRegistry(repo, nil)
	runner := OpenAIRunner{
		Config:         config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 3, MaxToolCalls: 2},
		Client:         client,
		Tools:          registry,
		ToolSpecs:      registry.Definitions([]string{"repo_summary"}),
		PromptCacheKey: "test:stable-instructions",
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
	if !slices.ContainsFunc(client.requests[1].Input, func(item openai.Item) bool {
		return item.Type == "function_call_output" && item.CallID == "call_1"
	}) {
		t.Fatalf("missing tool output input: %#v", client.requests[1].Input)
	}
	if client.requests[0].TurnState != "" || client.requests[1].TurnState != "sticky-route" {
		t.Fatalf("request turn states = %q, %q", client.requests[0].TurnState, client.requests[1].TurnState)
	}
	if client.requests[0].Instructions != client.requests[1].Instructions {
		t.Fatalf("instructions changed between cacheable requests")
	}
	if instructions := client.requests[0].Instructions; !containsAll(instructions, "bounded agent loop", "reduce material uncertainty", "do not call tools just to repeat provided context", "Do not ask the user for more evidence") {
		t.Fatalf("request instructions missing stable tool guidance: %s", instructions)
	}
	if strings.Contains(client.requests[0].Instructions, "model step") {
		t.Fatalf("request instructions contain dynamic budget state: %s", client.requests[0].Instructions)
	}
	for index, request := range client.requests {
		if len(request.Input) == 0 {
			t.Fatalf("request %d has no input", index)
		}
		budget := request.Input[len(request.Input)-1]
		if !budget.PromptCacheBreakpoint || !strings.Contains(budget.Content, "model_step:") {
			t.Fatalf("request %d missing appended cacheable budget: %#v", index, budget)
		}
	}
	if !slices.Equal(client.requests[0].Input, client.requests[1].Input[:len(client.requests[0].Input)]) {
		t.Fatalf("second request does not preserve the first request input as an exact prefix")
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
	registry := tools.NewReviewRegistry(repo, nil, tools.ReviewModeCodebase, tools.ReviewScope{}, gitctx.ChangeFingerprint{})
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
	if result.ToolCallsByName["grep"] != 2 || len(result.ToolCallsByName) != 1 {
		t.Fatalf("tool calls by name = %#v", result.ToolCallsByName)
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

func TestRunnerCollectsDistinctUsedSkillsAndToolCallCounts(t *testing.T) {
	t.Parallel()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	manager := skillcmd.DiscoverWithOptions(skillcmd.Options{
		Dir: repoDir, Lookup: func(string) (string, error) { return "/tools/skills-mgr", nil },
		Runner: fakeSkillRunner{},
	})
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: tools.SkillsReadToolName, Arguments: `{"locator":"go","range":""}`}}},
		{ToolCalls: []openai.ToolCall{{ID: "fc_2", CallID: "call_2", Name: tools.SkillsReadToolName, Arguments: `{"locator":"go/references/style.md","range":""}`}}},
		{Text: "done"},
	}}
	registry := tools.NewRegistry(repo, manager)
	runner := OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 2}, Client: client,
		Tools: registry, ToolSpecs: registry.Definitions([]string{tools.SkillsReadToolName}),
	}

	result, err := runner.Run(t.Context(), Request{UserPrompt: "review", MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(result.UsedSkills, []string{"go"}) {
		t.Fatalf("used skills = %#v", result.UsedSkills)
	}
	if result.ToolCallsByName[tools.SkillsReadToolName] != 2 || len(result.ToolCallsByName) != 1 {
		t.Fatalf("tool calls by name = %#v", result.ToolCallsByName)
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
	registry := tools.NewReviewRegistry(repo, nil, tools.ReviewModeCodebase, tools.ReviewScope{}, gitctx.ChangeFingerprint{})
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
	outputIndex := slices.IndexFunc(client.requests[1].Input, func(item openai.Item) bool {
		return item.Type == "function_call_output" && item.CallID == "call_1"
	})
	if outputIndex < 0 {
		t.Fatalf("tool error output missing from %#v", client.requests[1].Input)
	}
	output := client.requests[1].Input[outputIndex]
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
	correctedIndex := slices.IndexFunc(client.requests[2].Input, func(item openai.Item) bool {
		return item.Type == "function_call_output" && item.CallID == "call_2"
	})
	if correctedIndex < 0 || !strings.Contains(client.requests[2].Input[correctedIndex].Output, "package actual") {
		t.Fatalf("corrected tool output missing from %#v", client.requests[2].Input)
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
	registry := tools.NewReviewRegistry(repo, nil, tools.ReviewModeUncommitted, tools.ReviewScope{}, fingerprint)
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
	registry := tools.NewReviewRegistry(repo, nil, tools.ReviewModeCodebase, tools.ReviewScope{}, gitctx.ChangeFingerprint{})
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
	registry := tools.NewRegistry(repo, nil)
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
			Usage:     openai.Usage{InputTokens: 217600},
		},
		{Text: "all findings"},
	}}
	registry := tools.NewRegistry(repo, nil)
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
		Tools:  tools.NewRegistry(repo, nil),
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
		Tools:  tools.NewRegistry(nil, nil),
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
	registry := tools.NewRegistry(repo, nil)
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

	var executionOrder executionLog
	registry := tools.NewRegistry(nil, nil)
	names := []string{"first_tool", "second_tool"}
	for _, name := range names {
		registry.Register(recordingTool{name: name, order: &executionOrder, content: "output"})
	}
	client := &fakeClient{responses: []openai.Response{
		{ToolCalls: []openai.ToolCall{
			{ID: "fc_1", CallID: "call_1", Name: names[0], Arguments: "{}"},
			{ID: "fc_2", CallID: "call_2", Name: names[1], Arguments: "{}"},
		}},
		{Text: "Add parser"},
	}}
	runner := OpenAIRunner{
		Config:    config.Config{Model: "test", BaseURL: "http://example", APIKey: "key", MaxSteps: 3, MaxToolCalls: 1},
		Client:    client,
		Tools:     registry,
		ToolSpecs: registry.Definitions(names),
	}

	result, err := runner.Run(context.Background(), Request{
		SystemPrompt: "system",
		UserPrompt:   "user",
		MaxSteps:     3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Add parser" || result.ToolCalls != 0 {
		t.Fatalf("result = %#v", result)
	}
	if len(executionOrder.snapshot()) != 0 {
		t.Fatalf("executed over-budget batch members: %v", executionOrder.snapshot())
	}
	if len(client.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(client.requests))
	}
	if len(client.requests[1].Tools) != 0 {
		t.Fatalf("expected forced finalization without tools, got %#v", client.requests[1].Tools)
	}
	var outputCallIDs []string
	for _, item := range client.requests[1].Input {
		if item.Type == "function_call_output" {
			outputCallIDs = append(outputCallIDs, item.CallID)
		}
	}
	if !slices.Equal(outputCallIDs, []string{"call_1", "call_2"}) {
		t.Fatalf("forced-finalization output call IDs = %v", outputCallIDs)
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
