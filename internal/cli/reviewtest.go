package cli

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/yusing/git-agent/internal/checks"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
	"github.com/yusing/git-agent/internal/tools"
	"github.com/yusing/git-agent/internal/trace"
)

var dryRunEventDelay = func() time.Duration {
	return 500*time.Millisecond + time.Duration(rand.IntN(501))*time.Millisecond
}

func waitDryRunEvent(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func dryRunEvents(kind reviewtask.Kind, manifest *tools.OrchestrationManifest, checkResults []checks.Result) ([]trace.Event, error) {
	var report any
	if kind == reviewtask.KindSimplify {
		simplifyReport := map[string]any{
			"summary":       "Deterministic dry-run fixture completed.",
			"opportunities": []any{},
		}
		if manifest != nil {
			simplifyReport["orchestration_manifest_sha256"] = manifest.Digest
		}
		report = simplifyReport
	} else {
		digest := ""
		if manifest != nil {
			digest = manifest.Digest
		}
		finalReport, err := reviewtask.BuildFinalReviewReport(
			`{"summary":"Deterministic dry-run fixture completed.","recommendation":"APPROVE","findings":[]}`,
			checkResults,
			digest,
		)
		if err != nil {
			return nil, err
		}
		report = finalReport
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
	return append(events, trace.Event{Kind: "final", Value: map[string]any{"text": report}}), nil
}
