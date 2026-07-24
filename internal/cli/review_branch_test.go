package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/yusing/git-agent/internal/agent"
	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/openai"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
	"github.com/yusing/git-agent/internal/trace"
)

type branchClient struct {
	mu        sync.Mutex
	models    map[string]string
	b2Started chan struct{}
}

func (c *branchClient) CreateResponse(ctx context.Context, request openai.Request) (openai.Response, error) {
	branchID := branchIDFromInput(request.Input)
	if branchID == "" {
		return openai.Response{
			ToolCalls: []openai.ToolCall{{
				ID: "fc_root", CallID: "call_root", Name: reviewtask.BranchToolName,
				Arguments: `{"branches":[
					{"scope":"Inspect the first responsibility.","path_hints":["internal/agent"],"model":"gpt-5.6-sol","reasoning_effort":"high"},
					{"scope":"Inspect the second responsibility.","path_hints":[],"model":"inherit","reasoning_effort":"inherit"}
				]}`,
			}},
		}, nil
	}
	c.mu.Lock()
	c.models[branchID] = request.Model + "/" + request.ThinkingMode
	c.mu.Unlock()
	if request.OnStreamEvent != nil {
		if err := request.OnStreamEvent(openai.StreamEvent{
			Kind: "reasoning_summary.delta", ItemID: "rs_" + branchID,
			ProviderAttempt: 1, Delta: "inspect " + branchID,
		}); err != nil {
			return openai.Response{}, err
		}
	}
	if branchID == "b1" {
		select {
		case <-ctx.Done():
			return openai.Response{}, ctx.Err()
		case <-c.b2Started:
		}
		return openai.Response{Text: `{"summary":"first complete","recommendation":"COMMENT","findings":[{"severity":"LOW","aspect":"tests","title":"first","impact":"impact","evidences":[{"title":"line","path":"a.go","line_start":1,"line_end":1}],"proposed_fix":"fix"}]}`}, nil
	}
	close(c.b2Started)
	return openai.Response{Text: `{"summary":"second complete","recommendation":"COMMENT","findings":[{"severity":"LOW","aspect":"style","title":"second","impact":"impact","evidences":[{"title":"line","path":"b.go","line_start":1,"line_end":1}],"proposed_fix":"fix"}]}`}, nil
}

func TestRunReviewTreeFansOutAggregatesAndPublishesOrderedBranchEvents(t *testing.T) {
	client := &branchClient{models: map[string]string{}, b2Started: make(chan struct{})}
	var events []trace.Event
	recorder, err := trace.NewEventStream("review", func(event trace.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.OpenAIRunner{
		Config: config.Config{
			Model: "custom-parent", ThinkingEffort: "medium", MaxSteps: 3, MaxToolCalls: 3,
		},
		Client: client,
		Validator: func(text string) []string {
			return reviewtask.Validate(reviewtask.KindReview, text)
		},
		Normalize: func(text string) string { return reviewtask.Shape(reviewtask.KindReview, text) },
		Trace:     recorder,
	}
	result, err := runReviewTree(t.Context(), reviewtask.KindReview, reviewtask.DepthBalanced, runner, agent.Request{
		SystemPrompt: "review", UserPrompt: "inspect", TextFormat: reviewtask.TextFormat(reviewtask.KindReview),
		MaxSteps: 3,
	}, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCalls != 1 {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(result.Text, `"title": "first"`) || !strings.Contains(result.Text, `"title": "second"`) ||
		strings.Index(result.Text, `"title": "first"`) > strings.Index(result.Text, `"title": "second"`) {
		t.Fatalf("aggregate is not in stable leaf order:\n%s", result.Text)
	}
	if client.models["b1"] != "gpt-5.6-sol/high" || client.models["b2"] != "custom-parent/medium" {
		t.Fatalf("effective child models = %#v", client.models)
	}

	fanoutIndex := eventIndex(events, "branch.fanout")
	firstChildIndex := eventIndex(events, "branch.event")
	aggregateIndex := eventIndexWithPhase(events, "aggregating_branches")
	if fanoutIndex < 0 || firstChildIndex <= fanoutIndex || aggregateIndex <= firstChildIndex {
		t.Fatalf("event order = %#v", eventKinds(events))
	}
	if countEvents(events, "branch.completed") != 2 || countEvents(events, "branch.failed") != 0 {
		t.Fatalf("lifecycle events = %#v", eventKinds(events))
	}
	if countEvents(events, "reasoning_summary.delta") != 0 {
		t.Fatalf("child reasoning escaped branch envelope: %#v", eventKinds(events))
	}
	sawReasoning := false
	for index, event := range events {
		if event.Seq != index+1 {
			t.Fatalf("event %d seq = %d", index, event.Seq)
		}
		if event.Kind == "branch.event" {
			inner := event.Value["event"].(map[string]any)
			if _, ok := inner["seq"]; ok {
				t.Fatalf("inner event owns global sequence: %#v", inner)
			}
			sawReasoning = sawReasoning || inner["kind"] == "reasoning_summary.delta"
		}
	}
	if !sawReasoning {
		t.Fatalf("events have no scoped reasoning: %#v", events)
	}
}

func TestRunReviewTreeSupportsImmediateNestedFanout(t *testing.T) {
	client := openaiClientFunc(func(_ context.Context, request openai.Request) (openai.Response, error) {
		switch branchIDFromInput(request.Input) {
		case "":
			return branchResponse("root", "root one", "root two"), nil
		case "b1":
			return branchResponse("b1", "nested one", "nested two"), nil
		case "b2":
			return openai.Response{Text: `{"summary":"root two done","opportunities":[]}`}, nil
		case "b3":
			return openai.Response{Text: `{"summary":"nested one done","opportunities":[]}`}, nil
		case "b4":
			return openai.Response{Text: `{"summary":"nested two done","opportunities":[]}`}, nil
		default:
			return openai.Response{}, errors.New("unexpected branch")
		}
	})
	var events []trace.Event
	recorder, err := trace.NewEventStream("simplify", func(event trace.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.OpenAIRunner{
		Config: config.Config{Model: "test", ThinkingEffort: "medium", MaxSteps: 3, MaxToolCalls: 3},
		Client: client, Validator: func(text string) []string {
			return reviewtask.Validate(reviewtask.KindSimplify, text)
		},
		Trace: recorder,
	}
	result, err := runReviewTree(t.Context(), reviewtask.KindSimplify, reviewtask.DepthThorough, runner, agent.Request{
		UserPrompt: "inspect", MaxSteps: 3,
	}, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if countEvents(events, "branch.fanout") != 2 || countEvents(events, "branch.completed") != 3 {
		t.Fatalf("events = %#v", eventKinds(events))
	}
	wantOrder := []string{"nested one: nested one done", "nested two: nested two done", "root two: root two done"}
	position := -1
	for _, want := range wantOrder {
		next := strings.Index(result.Text, want)
		if next <= position {
			t.Fatalf("aggregate does not preserve recursive child order: %s", result.Text)
		}
		position = next
	}
}

func TestRunReviewTreeCancelsSiblingsAndPublishesNoPartialAggregate(t *testing.T) {
	siblingCanceled := make(chan struct{})
	client := openaiClientFunc(func(ctx context.Context, request openai.Request) (openai.Response, error) {
		switch branchIDFromInput(request.Input) {
		case "":
			return branchResponse("root", "failing", "waiting"), nil
		case "b1":
			return openai.Response{}, errors.New("provider failed")
		case "b2":
			<-ctx.Done()
			close(siblingCanceled)
			return openai.Response{}, ctx.Err()
		default:
			return openai.Response{}, errors.New("unexpected branch")
		}
	})
	var events []trace.Event
	recorder, err := trace.NewEventStream("review", func(event trace.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 3},
		Client: client, Trace: recorder,
	}
	_, err = runReviewTree(t.Context(), reviewtask.KindReview, reviewtask.DepthBalanced, runner, agent.Request{
		UserPrompt: "inspect", MaxSteps: 3,
	}, recorder)
	if err == nil {
		t.Fatal("expected branch failure")
	}
	if writeErr := recorder.WriteExact("error", map[string]any{"message": err.Error()}); writeErr != nil {
		t.Fatal(writeErr)
	}
	select {
	case <-siblingCanceled:
	default:
		t.Fatal("sibling was not canceled")
	}
	failedIndex := eventIndex(events, "branch.failed")
	errorIndex := eventIndex(events, "error")
	if failedIndex < 0 || errorIndex <= failedIndex || eventIndexWithPhase(events, "aggregating_branches") >= 0 {
		t.Fatalf("events = %#v", eventKinds(events))
	}
}

func TestRunReviewTreeReturnsInitiatingFailureInsteadOfEarlierSiblingCancellation(t *testing.T) {
	client := openaiClientFunc(func(ctx context.Context, request openai.Request) (openai.Response, error) {
		switch branchIDFromInput(request.Input) {
		case "":
			return branchResponse("root", "waiting", "failing"), nil
		case "b1":
			<-ctx.Done()
			return openai.Response{}, ctx.Err()
		case "b2":
			return openai.Response{}, errors.New("initiating provider failure")
		default:
			return openai.Response{}, errors.New("unexpected branch")
		}
	})
	recorder, err := trace.NewEventStream("review", func(trace.Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 3, MaxToolCalls: 3},
		Client: client, Trace: recorder,
	}

	_, err = runReviewTree(t.Context(), reviewtask.KindReview, reviewtask.DepthBalanced, runner, agent.Request{
		UserPrompt: "inspect", MaxSteps: 3,
	}, recorder)
	if err == nil || !strings.Contains(err.Error(), "initiating provider failure") {
		t.Fatalf("error = %v, want initiating provider failure", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("initiating failure was replaced by cancellation: %v", err)
	}
}

func TestRunReviewTreePreservesNonbranchedResult(t *testing.T) {
	client := openaiClientFunc(func(context.Context, openai.Request) (openai.Response, error) {
		return openai.Response{Text: `{"summary":"ordinary","opportunities":[]}`}, nil
	})
	var events []trace.Event
	recorder, err := trace.NewEventStream("simplify", func(event trace.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 2, MaxToolCalls: 2},
		Client: client, Validator: func(text string) []string {
			return reviewtask.Validate(reviewtask.KindSimplify, text)
		},
		Trace: recorder,
	}
	result, err := runReviewTree(t.Context(), reviewtask.KindSimplify, reviewtask.DepthBalanced, runner, agent.Request{
		UserPrompt: "inspect", MaxSteps: 2,
	}, recorder)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"summary":"ordinary","opportunities":[]}` {
		t.Fatalf("result = %#v", result)
	}
	for _, kind := range eventKinds(events) {
		if strings.HasPrefix(kind, "branch.") {
			t.Fatalf("nonbranched events = %#v", eventKinds(events))
		}
	}
}

func TestRunReviewTreeRejectsInvalidBranchBeforeFanout(t *testing.T) {
	client := openaiClientFunc(func(context.Context, openai.Request) (openai.Response, error) {
		return openai.Response{ToolCalls: []openai.ToolCall{{
			ID: "fc_root", CallID: "call_root", Name: reviewtask.BranchToolName,
			Arguments: `{"branches":[{"scope":"only one","path_hints":[],"model":"inherit","reasoning_effort":"inherit"}]}`,
		}}}, nil
	})
	var events []trace.Event
	recorder, err := trace.NewEventStream("review", func(event trace.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := agent.OpenAIRunner{
		Config: config.Config{Model: "test", MaxSteps: 2, MaxToolCalls: 2},
		Client: client, Trace: recorder,
	}
	_, err = runReviewTree(t.Context(), reviewtask.KindReview, reviewtask.DepthBalanced, runner, agent.Request{
		UserPrompt: "inspect", MaxSteps: 2,
	}, recorder)
	if err == nil || !strings.Contains(err.Error(), "requires 2 to 3 children") {
		t.Fatalf("error = %v", err)
	}
	if countEvents(events, "branch.fanout") != 0 || countEvents(events, "branch.event") != 0 {
		t.Fatalf("invalid branch emitted topology: %#v", eventKinds(events))
	}
}

type openaiClientFunc func(context.Context, openai.Request) (openai.Response, error)

func (f openaiClientFunc) CreateResponse(ctx context.Context, request openai.Request) (openai.Response, error) {
	return f(ctx, request)
}

func branchIDFromInput(items []openai.Item) string {
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		if item.Type != "function_call_output" {
			continue
		}
		var result struct {
			BranchID string `json:"branch_id"`
		}
		if json.Unmarshal([]byte(item.Output), &result) == nil && result.BranchID != "" {
			return result.BranchID
		}
	}
	return ""
}

func branchResponse(id, firstScope, secondScope string) openai.Response {
	return openai.Response{ToolCalls: []openai.ToolCall{{
		ID: "fc_" + id, CallID: "call_" + id, Name: reviewtask.BranchToolName,
		Arguments: `{"branches":[` +
			`{"scope":"` + firstScope + `","path_hints":[],"model":"inherit","reasoning_effort":"inherit"},` +
			`{"scope":"` + secondScope + `","path_hints":[],"model":"inherit","reasoning_effort":"inherit"}` +
			`]}`,
	}}}
}

func eventKinds(events []trace.Event) []string {
	kinds := make([]string, len(events))
	for index, event := range events {
		kinds[index] = event.Kind
	}
	return kinds
}

func eventIndex(events []trace.Event, kind string) int {
	for index, event := range events {
		if event.Kind == kind {
			return index
		}
	}
	return -1
}

func eventIndexWithPhase(events []trace.Event, phase string) int {
	for index, event := range events {
		if event.Kind == "runtime.status" && event.Value["phase"] == phase {
			return index
		}
	}
	return -1
}

func countEvents(events []trace.Event, kind string) int {
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}
