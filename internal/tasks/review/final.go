package review

import (
	"fmt"
	"strings"

	"github.com/yusing/git-agent/internal/checks"
)

type FinalReviewReport struct {
	Summary                     string          `json:"summary"`
	Recommendation              string          `json:"recommendation"`
	Findings                    []Finding       `json:"findings"`
	Checks                      []checks.Result `json:"checks"`
	OrchestrationManifestSHA256 string          `json:"orchestration_manifest_sha256,omitempty"`
}

func BuildFinalReviewReport(providerText string, results []checks.Result, orchestrationDigest string) (FinalReviewReport, error) {
	var providerReport ReviewReport
	if err := decodeStrict(providerText, &providerReport); err != nil {
		return FinalReviewReport{}, fmt.Errorf("decode validated review report: %w", err)
	}
	if errs := validateReview(providerReport); len(errs) > 0 {
		return FinalReviewReport{}, fmt.Errorf("validated review report is invalid: %s", strings.Join(errs, "; "))
	}
	report := FinalReviewReport{
		Summary:                     providerReport.Summary,
		Recommendation:              providerReport.Recommendation,
		Findings:                    providerReport.Findings,
		Checks:                      cloneCheckResults(results),
		OrchestrationManifestSHA256: orchestrationDigest,
	}
	if err := ValidateFinalReviewReport(report); err != nil {
		return FinalReviewReport{}, err
	}
	return report, nil
}

func ValidateFinalReviewReport(report FinalReviewReport) error {
	providerReport := ReviewReport{
		Summary:        report.Summary,
		Recommendation: report.Recommendation,
		Findings:       report.Findings,
	}
	if errs := validateReview(providerReport); len(errs) > 0 {
		return fmt.Errorf("invalid final review report: %s", strings.Join(errs, "; "))
	}
	if len(report.Checks) == 0 {
		return fmt.Errorf("invalid final review report: checks must contain registered results")
	}
	seen := make(map[string]bool, len(report.Checks))
	for index, result := range report.Checks {
		if err := checks.ValidateResult(result); err != nil {
			return fmt.Errorf("invalid final review report: checks[%d]: %w", index, err)
		}
		if seen[result.Name] {
			return fmt.Errorf("invalid final review report: duplicate check %q", result.Name)
		}
		seen[result.Name] = true
	}
	return nil
}

func cloneCheckResults(results []checks.Result) []checks.Result {
	cloned := make([]checks.Result, len(results))
	for index, result := range results {
		cloned[index] = result
		cloned[index].Diagnostics = make([]checks.Diagnostic, len(result.Diagnostics))
		copy(cloned[index].Diagnostics, result.Diagnostics)
	}
	return cloned
}
