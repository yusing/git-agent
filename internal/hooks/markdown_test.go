package hooks

import (
	"strings"
	"testing"
)

func TestFormatMarkdownRendersReviewMetricsFindingsAndChecks(t *testing.T) {
	payload := PostInspection{
		Session: InspectionSession{
			ID: "session_[one]", Command: "review", Mode: "uncommitted",
			Model: "gpt-5.6-sol", ReasoningEffort: "medium", ElapsedMS: 1250,
		},
		Metrics: InspectionMetrics{
			Usage:           Usage{InputTokens: 100, CachedInputTokens: 30, UncachedInputTokens: 70, OutputTokens: 20, ReasoningTokens: 12, TotalTokens: 120},
			BranchesCreated: 1,
			Branches: []BranchMetric{{
				ID: "b1", ParentID: "root", Model: "gpt-5.6-terra", ReasoningEffort: "low",
				Usage: Usage{InputTokens: 40, CachedInputTokens: 10, UncachedInputTokens: 30, OutputTokens: 8, ReasoningTokens: 3, TotalTokens: 48},
			}},
		},
		Report: map[string]any{
			"summary":        "Found *one* issue.\n# Fake heading\n- Fake finding\n> Fake quote\n~~~fake fence\n1. Fake ordered finding\n2) Fake recommendation\n    spoofed code\n\tspoofed tab code",
			"recommendation": "FIX",
			"findings": []any{map[string]any{
				"severity": "HIGH", "aspect": "security", "title": "Protect [metrics]",
				"impact": "Clients can read telemetry.", "proposed_fix": "Require authentication.",
				"evidences": []any{map[string]any{
					"title": "Public handler", "path": "main`tick``.go", "line_start": 42, "line_end": 47,
				}},
			}},
			"checks": []any{map[string]any{
				"name": "golangci-lint", "status": "findings", "reason": "partial *scope*",
				"error": "checker > failed", "omitted": 3,
				"diagnostics": []any{map[string]any{
					"path": "main.go", "line": 38, "column": 4, "code": "gocritic", "message": "defer *will* not run",
				}},
			}},
		},
	}

	got := formatMarkdown(payload)
	for _, want := range []string{
		`- **Session ID:** session\_\[one\]`, "## Usage", "Input tokens:** 100",
		"## Branches (1)", "### Branch `b1`", `Model:** gpt\-5\.6\-terra`,
		"## Summary\n\nFound \\*one\\* issue\\.\n\\# Fake heading\n\\- Fake finding\n\\> Fake quote\n\\~\\~\\~fake fence\n1\\. Fake ordered finding\n2\\) Fake recommendation\nspoofed code\nspoofed tab code",
		"### HIGH — Protect \\[metrics\\]", "```main`tick``.go:42-47``` — Public handler",
		"**Proposed fix**", "Require authentication\\.", "**Reason**\n\npartial \\*scope\\*",
		"**Error**\n\nchecker \\> failed", "Additional diagnostics omitted:** 3",
		`### golangci\-lint — findings`, "`main.go:38:4` `gocritic` — defer \\*will\\* not run",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Markdown missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `{"usage"`) || strings.Contains(got, `"findings":`) {
		t.Fatalf("Markdown contains raw JSON:\n%s", got)
	}
}

func TestFormatMarkdownHandlesSimplifyEmptyMalformedUnrelatedAndFutureReports(t *testing.T) {
	tests := []struct {
		name        string
		report      any
		want        string
		unavailable bool
	}{
		{name: "simplify", report: map[string]any{
			"summary": "One opportunity", "opportunities": []any{map[string]any{
				"aspect": "reuse", "title": "Share parser", "body": "Duplicate parsing.",
				"evidences": []any{}, "proposed_change": "Use one parser.",
			}},
		}, want: "## Opportunities\n\n### reuse — Share parser"},
		{name: "empty findings", report: map[string]any{"findings": []any{}}, want: "## Findings\n\nNone"},
		{name: "malformed findings", report: map[string]any{"findings": "not-an-array"}, want: "## Findings\n\nUnavailable"},
		{name: "unrelated collision", report: map[string]any{"findings_backup": []any{}}, want: "No findings or opportunities field."},
		{name: "future field", report: map[string]any{"summary": "Future", "future_items": []any{}}, want: "## Summary\n\nFuture"},
		{name: "malformed report", report: []any{"not-an-object"}, want: "## Report\n\nUnavailable", unavailable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := formatMarkdown(PostInspection{Report: test.report})
			if !strings.Contains(got, test.want) {
				t.Fatalf("Markdown missing %q:\n%s", test.want, got)
			}
			if strings.Contains(got, `{"`) {
				t.Fatalf("Markdown contains raw JSON:\n%s", got)
			}
		})
	}
}

func TestTemplateExposesFormatMarkdown(t *testing.T) {
	payload := PostInspection{
		Session: InspectionSession{ID: "turn-1"},
		Report:  map[string]any{"summary": "Complete", "findings": []any{}},
	}
	rendered, err := render(`{{format_markdown .}}`, payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, `Session ID:** turn\-1`) || !strings.Contains(rendered, "## Findings\n\nNone") {
		t.Fatalf("rendered Markdown:\n%s", rendered)
	}
}

func TestFormatMarkdownRendersEveryCheckStatusAndMalformedOptionalFields(t *testing.T) {
	payload := PostInspection{Report: map[string]any{
		"findings": []any{},
		"checks": []any{
			map[string]any{"name": "pass-check", "status": "pass", "diagnostics": []any{}},
			map[string]any{"name": "skip-check", "status": "skipped", "reason": "not applicable", "diagnostics": []any{}},
			map[string]any{"name": "error-check", "status": "error", "error": "could not start", "diagnostics": []any{}},
			map[string]any{"name": "bounded-check", "status": "findings", "omitted": 4, "diagnostics": []any{}},
			map[string]any{"name": "future-check", "status": "future", "reason": map[string]any{"future": true}, "omitted": "unknown"},
		},
	}}

	got := formatMarkdown(payload)
	for _, want := range []string{
		`### pass\-check — pass`, `### skip\-check — skipped`, "**Reason**\n\nnot applicable",
		`### error\-check — error`, "**Error**\n\ncould not start",
		`### bounded\-check — findings`, "Additional diagnostics omitted:** 4",
		`### future\-check — future`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Markdown missing %q:\n%s", want, got)
		}
	}
}
