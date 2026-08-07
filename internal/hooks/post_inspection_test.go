package hooks

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunPostInspectionRendersTemplateAndWritesPayloadToStdin(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "payload.json")
	payload := PostInspection{
		SchemaVersion: SchemaVersion,
		Session:       InspectionSession{ID: "turn-1", Title: "review O'Brien"},
		Metrics: InspectionMetrics{
			Usage:      Usage{InputTokens: 12, CachedInputTokens: 5, CacheWriteInputTokens: 3, UncachedInputTokens: 7},
			UsedSkills: []string{"go"}, ToolCalls: []ToolCallMetric{{Name: "jq", Count: 3}},
			Branches: []BranchMetric{},
		},
		Report: map[string]any{"findings": []any{}},
	}
	hook := "test " + shellQuote(payload.Session.Title) + " = {{shellquote .Session.Title}} && tee " + shellQuote(output)
	if err := RunPostInspection(t.Context(), []string{hook}, payload); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(string(data), `"schema_version":2`, `"id":"turn-1"`, `"used_skills":["go"]`, `"tool_calls":[{"name":"jq","count":3}]`, `"cache_write_input_tokens":3`, `"uncached_input_tokens":7`, `"findings":[]`) {
		t.Fatalf("payload = %s", data)
	}
}

func TestRunPostInspectionBoundsInheritedPipeWait(t *testing.T) {
	started := time.Now()
	err := RunPostInspection(t.Context(), []string{"sleep 3 >&2 &"}, PostInspection{SchemaVersion: SchemaVersion})
	if err == nil || !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("error = %v, want exec.ErrWaitDelay", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("hook waited %s for inherited pipe", elapsed)
	}
}

func TestRunPostInspectionRendersCompleteReportJSONAcrossReportShapes(t *testing.T) {
	tests := []struct {
		name   string
		report map[string]any
		want   []string
	}{
		{
			name: "review findings",
			report: map[string]any{
				"findings": []any{map[string]any{"title": "bug"}},
			},
			want: []string{`"findings"`, `"title":"bug"`},
		},
		{
			name: "simplify opportunities without findings",
			report: map[string]any{
				"opportunities": []any{map[string]any{"title": "reuse"}},
			},
			want: []string{`"opportunities"`, `"title":"reuse"`},
		},
		{
			name: "unrelated collision",
			report: map[string]any{
				"findings_backup": []any{"not findings"}, "summary": "complete",
			},
			want: []string{`"findings_backup"`, `"summary":"complete"`},
		},
		{
			name: "future report field",
			report: map[string]any{
				"summary": "complete", "future_metadata": map[string]any{"version": 2},
			},
			want: []string{`"future_metadata"`, `"version":2`},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "report.json")
			hook := "printf '%s' {{shellquote (json .Report)}} > " + shellQuote(output)
			if err := RunPostInspection(t.Context(), []string{hook}, PostInspection{Report: test.report}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if !containsAll(string(data), test.want...) {
				t.Fatalf("report JSON = %s", data)
			}
		})
	}
}

func containsAll(value string, expected ...string) bool {
	for _, item := range expected {
		if !strings.Contains(value, item) {
			return false
		}
	}
	return true
}
