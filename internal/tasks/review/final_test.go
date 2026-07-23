package review

import (
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/checks"
)

func TestBuildFinalReviewReportPreservesProviderAndFutureCheckerResults(t *testing.T) {
	first, err := checks.NewResult("golangci-lint", nil)
	if err != nil {
		t.Fatal(err)
	}
	future, err := checks.NewSkipped("future-language-checker", "no supported project")
	if err != nil {
		t.Fatal(err)
	}
	results := []checks.Result{first, future}
	report, err := BuildFinalReviewReport(
		`{"summary":"ready","recommendation":"APPROVE","findings":[]}`,
		results,
		"abc123",
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary != "ready" || report.Recommendation != "APPROVE" || report.Findings == nil {
		t.Fatalf("provider fields not preserved: %#v", report)
	}
	if len(report.Checks) != 2 || report.Checks[0].Name != "golangci-lint" ||
		report.Checks[1].Name != "future-language-checker" {
		t.Fatalf("checks = %#v", report.Checks)
	}
	if report.OrchestrationManifestSHA256 != "abc123" {
		t.Fatalf("orchestration digest = %q", report.OrchestrationManifestSHA256)
	}

	results[0].Name = "mutated"
	if report.Checks[0].Name != "golangci-lint" {
		t.Fatalf("final report aliases caller results: %#v", report.Checks)
	}
}

func TestBuildFinalReviewReportRejectsUnknownProviderFields(t *testing.T) {
	result, err := checks.NewResult("checker", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildFinalReviewReport(
		`{"summary":"ready","recommendation":"APPROVE","findings":[],"future":true}`,
		[]checks.Result{result},
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateFinalReviewReportRejectsMissingDuplicateAndMalformedChecks(t *testing.T) {
	pass, err := checks.NewResult("checker", nil)
	if err != nil {
		t.Fatal(err)
	}
	base := FinalReviewReport{Summary: "ready", Recommendation: "APPROVE", Findings: []Finding{}}
	cases := map[string][]checks.Result{
		"missing":   nil,
		"duplicate": {pass, pass},
		"malformed": {{Name: "checker", Status: "future", Diagnostics: []checks.Diagnostic{}}},
	}
	for name, results := range cases {
		t.Run(name, func(t *testing.T) {
			report := base
			report.Checks = results
			if err := ValidateFinalReviewReport(report); err == nil {
				t.Fatalf("checks accepted: %#v", results)
			}
		})
	}
}
