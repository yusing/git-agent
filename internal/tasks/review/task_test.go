package review

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/yusing/git-agent/internal/contextpack"
	"github.com/yusing/git-agent/internal/gitctx"
)

func TestOutputSchemasRequireEvidenceLocations(t *testing.T) {
	for _, kind := range []Kind{KindReview, KindSimplify} {
		schema := OutputSchema(kind)
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Fatalf("%s schema is not strict: %#v", kind, schema)
		}
		properties := schema["properties"].(map[string]any)
		itemsName := "findings"
		if kind == KindSimplify {
			itemsName = "opportunities"
		}
		items := properties[itemsName].(map[string]any)["items"].(map[string]any)
		itemProperties := items["properties"].(map[string]any)
		evidences := itemProperties["evidences"].(map[string]any)
		if evidences["minItems"] != 1 {
			t.Fatalf("%s evidence schema = %#v", kind, evidences)
		}
		evidence := evidences["items"].(map[string]any)
		if evidence["additionalProperties"] != false {
			t.Fatalf("%s evidence schema is not strict: %#v", kind, evidence)
		}
	}
}

func TestValidateReviewEnforcesSeverityOrderRecommendationAndStyleSeverity(t *testing.T) {
	valid := `{
  "summary": "Two findings",
  "recommendation": "REQUEST_CHANGES",
  "findings": [
    {
      "severity": "HIGH",
      "aspect": "correctness",
      "title": "Wrong result",
      "impact": "Returns stale data",
      "evidences": [{"title":"Stale cache read","path":"internal/cache.go","line_start":20,"line_end":24}],
      "proposed_fix": "Invalidate before reading"
    },
    {
      "severity": "LOW",
      "aspect": "style",
      "title": "Misleading name",
      "impact": "Readers can confuse milliseconds with seconds",
      "evidences": [{"title":"Unitless name","path":"internal/cache.go","line_start":30,"line_end":30}],
      "proposed_fix": "Rename timeout to timeoutMillis"
    }
  ]
}`
	if errs := Validate(KindReview, valid); len(errs) != 0 {
		t.Fatalf("valid report errors = %v", errs)
	}

	invalid := strings.Replace(valid, `"severity": "LOW"`, `"severity": "MEDIUM"`, 1)
	if errs := Validate(KindReview, invalid); !containsError(errs, "style severity must be LOW") {
		t.Fatalf("errors = %v", errs)
	}
}

func TestValidateSimplifyRequiresEvidence(t *testing.T) {
	report := `{
  "summary": "One opportunity",
  "opportunities": [{
    "aspect": "reuse",
    "title": "Reuse parser",
    "body": "Local parser duplicates shared parser",
    "evidences": [],
    "proposed_change": "Delete local parser"
  }]
}`
	if errs := Validate(KindSimplify, report); !containsError(errs, "requires at least one item") {
		t.Fatalf("errors = %v", errs)
	}
}

func TestValidateRejectsMissingOrNullReportArrays(t *testing.T) {
	tests := []struct {
		name string
		kind Kind
		json string
		want string
	}{
		{"missing findings", KindReview, `{"summary":"clean","recommendation":"APPROVE"}`, "findings must be an array"},
		{"null findings", KindReview, `{"summary":"clean","recommendation":"APPROVE","findings":null}`, "findings must be an array"},
		{"missing opportunities", KindSimplify, `{"summary":"clean"}`, "opportunities must be an array"},
		{"null opportunities", KindSimplify, `{"summary":"clean","opportunities":null}`, "opportunities must be an array"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if errs := Validate(test.kind, test.json); !containsError(errs, test.want) {
				t.Fatalf("errors = %v", errs)
			}
		})
	}
}

func TestValidateRepositoryRejectsMissingAndOutOfRangeEvidence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	report := `{"summary":"one","recommendation":"COMMENT","findings":[{"severity":"MEDIUM","aspect":"correctness","title":"bad","impact":"bad result","evidences":[{"title":"location","path":"app.go","line_start":1,"line_end":2}],"proposed_fix":"fix"}]}`
	if errs := ValidateRepository(KindReview, report, repo, ModeCodebase, nil, gitctx.ChangeFingerprint{}); !containsError(errs, "line_end 2 exceeds") {
		t.Fatalf("out-of-range errors = %v", errs)
	}
	report = strings.Replace(report, `"path":"app.go","line_start":1,"line_end":2`, `"path":"missing.go","line_start":1,"line_end":1`, 1)
	if errs := ValidateRepository(KindReview, report, repo, ModeCodebase, nil, gitctx.ChangeFingerprint{}); !containsError(errs, "does not exist") {
		t.Fatalf("missing-path errors = %v", errs)
	}
}

func TestValidateRepositoryRejectsDriftForEmptyReport(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package launch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(repo, ModeUncommitted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.go"), []byte("package later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := `{"summary":"clean","recommendation":"APPROVE","findings":[]}`
	errs := ValidateRepository(KindReview, report, repo, ModeUncommitted, prepared.Paths, prepared.Fingerprint)
	if !containsError(errs, gitctx.ErrChangeSnapshotStale.Error()) {
		t.Fatalf("errors = %v, want stale snapshot", errs)
	}
}

func TestParseModeDefaultsToUncommittedAndRejectsConflicts(t *testing.T) {
	mode, err := ParseMode(false, false, false)
	if err != nil || mode != ModeUncommitted {
		t.Fatalf("mode = %q, err = %v", mode, err)
	}
	if _, err := ParseMode(true, false, true); err == nil {
		t.Fatal("expected conflicting mode error")
	}
}

func TestStagedContextPackUsesIndexStatus(t *testing.T) {
	prepared := PreparedContext{
		Mode:   ModeStaged,
		Paths:  []string{"app.txt"},
		Status: []gitctx.PathChange{{Path: "app.txt", Staging: "M", Worktree: "D"}},
		Stats:  []gitctx.FileStat{{Path: "app.txt", Adds: 1, Deletes: 1}},
	}
	pack := buildContextPack(nil, prepared)
	if pack.Overview.Statuses["modified"] != 1 || pack.Overview.Statuses["deleted"] != 0 {
		t.Fatalf("statuses = %#v", pack.Overview.Statuses)
	}
}

func TestFilePrefixIsBounded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if _, err := git.PlainInit(dir, false); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("x", 64*1024) + "tail-marker"
	if err := os.WriteFile(filepath.Join(dir, "large.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	prefix, _, err := repo.FilePrefix(gitctx.FileSourceWorktree, "large.go", 8*1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefix) > 8*1024+256 || strings.Contains(prefix, "tail-marker") {
		t.Fatalf("prefix was not bounded: len=%d", len(prefix))
	}
}

func TestUserPromptBoundsLargePreparedContext(t *testing.T) {
	const (
		files         = 53_000
		promptEntries = 128
	)
	prepared := PreparedContext{
		Mode:   ModeUncommitted,
		Paths:  []string{"sentinel.ts"},
		Status: []gitctx.PathChange{{Path: "sentinel.ts", Worktree: "?"}},
		Stats:  []gitctx.FileStat{{Path: "sentinel.ts"}},
		ContextPack: contextpack.ContextPack{
			Overview: contextpack.ChangeOverview{Files: files},
		},
	}
	for i := range promptEntries + 1 {
		path := fmt.Sprintf("outlier/%03d.ts", i)
		prepared.ContextPack.Outliers = append(prepared.ContextPack.Outliers, contextpack.FileSummary{Path: path})
	}

	prompt := UserPrompt(KindReview, prepared)
	if len(prompt) >= 256*1024 {
		t.Fatalf("user prompt bytes = %d, want bounded compact context", len(prompt))
	}
	if strings.Contains(prompt, `"paths"`) || strings.Contains(prompt, `"status"`) || strings.Contains(prompt, `"stats"`) {
		t.Fatal("user prompt contains duplicate raw change lists")
	}
	if !strings.Contains(prompt, `"context_pack_truncated": true`) {
		t.Fatal("user prompt does not disclose context-pack truncation")
	}
	if !strings.Contains(prompt, "Use review_changes") {
		t.Fatal("user prompt does not identify complete change-inventory tool")
	}
	if strings.Contains(prompt, fmt.Sprintf("outlier/%03d.ts", promptEntries)) {
		t.Fatal("user prompt contains outlier beyond context-pack limit")
	}
}

func containsError(errs []string, text string) bool {
	for _, err := range errs {
		if strings.Contains(err, text) {
			return true
		}
	}
	return false
}
