package cli

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
	"github.com/yusing/git-agent/internal/tools"
	"github.com/yusing/git-agent/internal/trace"
)

const reviewTestCommand = "review-test"

var reviewTestEventInterval = 750 * time.Millisecond
var dryRunEventDelay = func() time.Duration {
	return 500*time.Millisecond + time.Duration(rand.IntN(501))*time.Millisecond
}

func (a *App) runReviewTest(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: git-agent review-test")
	}
	if !isDetachedChild() {
		return startDetachedTask(reviewTestCommand, nil, a.stdout)
	}
	if detachedTaskID() == "" {
		return errors.New("review-test detached task ID is missing")
	}

	events, err := startDetachedAgentEventServer(a.stderr, reviewTestCommand, detachedTaskID())
	if err != nil {
		return err
	}
	defer events.Close()

	for _, event := range reviewTestEvents(time.Now().UTC()) {
		if err := waitReviewTestEvent(ctx, reviewTestEventInterval); err != nil {
			return err
		}
		if err := events.Publish(event); err != nil {
			return err
		}
	}
	events.Finish()
	return nil
}

func waitReviewTestEvent(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func reviewTestEvents(started time.Time) []trace.Event {
	const summary = "Inspecting deterministic review fixture"
	values := []struct {
		kind  string
		value map[string]any
	}{
		{"reasoning_summary.delta", map[string]any{"delta": summary}},
		{"reasoning_summary.done", map[string]any{"text": summary}},
		{"tool-call", map[string]any{"name": "read_file", "arguments": `{"path":"internal/cli/app.go","line_start":1,"line_end":12}`}},
		{"tool-output", map[string]any{"name": "read_file", "content": "package cli\n"}},
		{"final", map[string]any{"text": map[string]any{
			"summary":        "Deterministic review fixture completed.",
			"recommendation": "APPROVE",
			"findings":       []any{},
		}}},
	}
	events := make([]trace.Event, len(values))
	for index, value := range values {
		events[index] = trace.Event{
			Seq:   index + 1,
			At:    started.Add(time.Duration(index+1) * reviewTestEventInterval),
			Kind:  value.kind,
			Value: value.value,
		}
	}
	return events
}

func dryRunEvents(kind reviewtask.Kind, manifest *tools.OrchestrationManifest) []trace.Event {
	report := map[string]any{"summary": "Deterministic dry-run fixture completed."}
	if kind == reviewtask.KindSimplify {
		report["opportunities"] = []any{}
	} else {
		report["recommendation"] = "APPROVE"
		report["findings"] = []any{}
	}
	if manifest != nil {
		report["orchestration_manifest_sha256"] = manifest.Digest
	}
	events := []trace.Event{
		{Kind: "reasoning_summary.delta", Value: map[string]any{"delta": "Inspecting deterministic dry-run fixture"}},
		{Kind: "reasoning_summary.delta", Value: map[string]any{"delta": " and prepared repository context"}},
		{Kind: "reasoning_summary.delta", Value: map[string]any{"delta": " before producing a final report"}},
		{Kind: "reasoning_summary.done", Value: map[string]any{"text": "Inspecting deterministic dry-run fixture and prepared repository context before producing a final report"}},
	}
	tools := []struct{ name, arguments, output string }{
		{"repo_summary", `{}`, "repository summary complete"},
		{"list_files", `{"path":"."}`, "file listing complete"},
		{"grep", `{"pattern":"fixture","path":"."}`, "search results complete"},
		{"find", `{"path":".","glob":"*.go"}`, "path discovery complete"},
		{"read_file", `{"path":"README.md","line_start":1,"line_end":5}`, "file read complete"},
	}
	for _, tool := range tools {
		events = append(events,
			trace.Event{Kind: "tool-call", Value: map[string]any{"name": tool.name, "arguments": tool.arguments}},
			trace.Event{Kind: "tool-output", Value: map[string]any{"name": tool.name, "content": tool.output}},
		)
	}
	return append(events, trace.Event{Kind: "final", Value: map[string]any{"text": report}})
}
