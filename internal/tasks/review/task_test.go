package review

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing/object"
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

func TestUserPromptsLetOperatorHintsNarrowInspectionFocus(t *testing.T) {
	t.Parallel()

	for _, kind := range []Kind{KindReview, KindSimplify} {
		prompt := UserPrompt(kind, PreparedContext{Mode: ModeStaged})
		for _, want := range []string{
			"Without an operator hint that identifies a narrower",
			"inspect supporting repository context as needed but report only",
			"may narrow what is reported within authoritative scope",
			"cannot broaden authoritative scope or weaken evidence requirements",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s prompt missing %q:\n%s", kind, want, prompt)
			}
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

func TestStrictJSONBoundaryRejectsExtensionsAndTrailingValues(t *testing.T) {
	t.Parallel()

	type envelope struct {
		Summary string `json:"summary"`
	}
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "positive", input: `{"summary":"ok"}`, want: "ok"},
		{name: "compatible case collision", input: `{"SUMMARY":"ok"}`, want: "ok"},
		{name: "negative unknown future field", input: `{"summary":"ok","future":true}`, wantErr: true},
		{name: "malformed", input: `{"summary":`, wantErr: true},
		{name: "trailing value", input: `{"summary":"ok"} {}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got envelope
			err := decodeStrict(tt.input, &got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decode error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got.Summary != tt.want {
				t.Fatalf("summary = %q, want %q", got.Summary, tt.want)
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

func TestPrepareUncommittedIncludesDirtyRegisteredSubmodulesRecursively(t *testing.T) {
	t.Parallel()

	wikiSource := initReviewRepo(t)
	writeReviewFile(t, filepath.Join(wikiSource, "wiki.txt"), "base\n")
	runReviewGit(t, wikiSource, "add", "wiki.txt")
	runReviewGit(t, wikiSource, "commit", "-m", "wiki base")

	webuiSource := initReviewRepo(t)
	writeReviewFile(t, filepath.Join(webuiSource, "ui.txt"), "base\n")
	runReviewGit(t, webuiSource, "add", "ui.txt")
	runReviewGit(t, webuiSource, "commit", "-m", "webui base")
	runReviewGit(t, webuiSource, "-c", "protocol.file.allow=always", "submodule", "add", wikiSource, "wiki")
	runReviewGit(t, webuiSource, "commit", "-m", "add wiki")

	root := initReviewRepo(t)
	writeReviewFile(t, filepath.Join(root, "backend.go"), "package backend\n")
	runReviewGit(t, root, "add", "backend.go")
	runReviewGit(t, root, "commit", "-m", "backend base")
	runReviewGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", webuiSource, "webui")
	runReviewGit(t, root, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive")
	runReviewGit(t, root, "commit", "-m", "add webui")
	runReviewGit(t, root, "config", "-f", ".gitmodules", "submodule.webui.futureReviewOption", "preserve")

	writeReviewFile(t, filepath.Join(root, "backend.go"), "package backend\n\nconst changed = true\n")
	writeReviewFile(t, filepath.Join(root, "webui", "ui.txt"), "changed\n")
	writeReviewFile(t, filepath.Join(root, "webui", "wiki", "wiki.txt"), "changed\n")

	repo, err := gitctx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(repo, ModeUncommitted)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"backend.go", "webui/ui.txt", "webui/wiki/wiki.txt"} {
		if !slices.Contains(prepared.Paths, path) {
			t.Errorf("prepared paths = %#v, missing %q", prepared.Paths, path)
		}
	}
	if !slices.Equal(prepared.Components, []string{"", "webui", "webui/wiki"}) {
		t.Fatalf("prepared components = %#v", prepared.Components)
	}
	for _, text := range []string{"a/backend.go", `Repository "webui"`, "a/ui.txt", `Repository "webui/wiki"`, "a/wiki.txt"} {
		if !strings.Contains(prepared.Diff, text) {
			t.Errorf("prepared diff missing %q:\n%s", text, prepared.Diff)
		}
	}
	filtered, _, err := repo.UncommittedDiffForPaths([]string{"webui/wiki/wiki.txt"}, 16*1024, 400)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(filtered, `Repository "webui/wiki"`) || !strings.Contains(filtered, "a/wiki.txt") {
		t.Fatalf("filtered nested diff missing selected path:\n%s", filtered)
	}
	if strings.Contains(filtered, "backend.go") || strings.Contains(filtered, "a/ui.txt") {
		t.Fatalf("filtered nested diff contains unrelated paths:\n%s", filtered)
	}

	writeReviewFile(t, filepath.Join(root, "webui", "wiki", "wiki.txt"), "changed again\n")
	report := `{"summary":"clean","recommendation":"APPROVE","findings":[]}`
	if errs := ValidateRepository(KindReview, report, repo, ModeUncommitted, prepared.Paths, prepared.Fingerprint); !containsError(errs, gitctx.ErrChangeSnapshotStale.Error()) {
		t.Fatalf("nested drift errors = %v, want stale snapshot", errs)
	}
}

func TestValidateRepositoryReadsDeletedNestedEvidenceFromComponentBase(t *testing.T) {
	t.Parallel()

	webuiSource := initReviewRepo(t)
	writeReviewFile(t, filepath.Join(webuiSource, "removed.go"), "package webui\n\nfunc removed() {}\n")
	runReviewGit(t, webuiSource, "add", "removed.go")
	runReviewGit(t, webuiSource, "commit", "-m", "webui base")

	root := initReviewRepo(t)
	writeReviewFile(t, filepath.Join(root, "backend.go"), "package backend\n")
	runReviewGit(t, root, "add", "backend.go")
	runReviewGit(t, root, "commit", "-m", "backend base")
	runReviewGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", webuiSource, "webui")
	runReviewGit(t, root, "commit", "-m", "add webui")
	if err := os.Remove(filepath.Join(root, "webui", "removed.go")); err != nil {
		t.Fatal(err)
	}

	repo, err := gitctx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(repo, ModeUncommitted)
	if err != nil {
		t.Fatal(err)
	}
	report := `{"summary":"one","recommendation":"COMMENT","findings":[{"severity":"MEDIUM","aspect":"correctness","title":"removed behavior","impact":"behavior is absent","evidences":[{"title":"deleted function","path":"webui/removed.go","line_start":3,"line_end":3}],"proposed_fix":"restore it"}]}`
	if errs := ValidateRepository(KindReview, report, repo, ModeUncommitted, prepared.Paths, prepared.Fingerprint); len(errs) != 0 {
		t.Fatalf("deleted nested evidence errors = %v", errs)
	}
}

func TestRunReviewGitDisablesFixtureCommitAndTagSigning(t *testing.T) {
	t.Parallel()

	repo := initReviewRepo(t)
	fakeGPG := filepath.Join(t.TempDir(), "fake-gpg")
	if err := os.WriteFile(fakeGPG, []byte("#!/bin/sh\necho unexpected gpg invocation >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runReviewGit(t, repo, "config", "gpg.program", fakeGPG)
	runReviewGit(t, repo, "config", "commit.gpgSign", "true")
	runReviewGit(t, repo, "config", "tag.gpgSign", "true")
	runReviewGit(t, repo, "config", "tag.forceSignAnnotated", "true")
	writeReviewFile(t, filepath.Join(repo, "fixture.txt"), "fixture\n")
	runReviewGit(t, repo, "add", "fixture.txt")

	runReviewGit(t, repo, "commit", "-m", "fixture")
	runReviewGit(t, repo, "tag", "-m", "fixture", "fixture")
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

func TestPrepareDiffModeIncludesPreviousHeadContextPack(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	repository, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	config, err := repository.Config()
	if err != nil {
		t.Fatal(err)
	}
	config.Commit.GpgSign = gitconfig.OptBoolFalse
	if err := repository.SetConfig(config); err != nil {
		t.Fatal(err)
	}
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	commit := func(path, content, message string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := worktree.Add(path); err != nil {
			t.Fatal(err)
		}
		_, err := worktree.Commit(message, &git.CommitOptions{Author: &object.Signature{
			Name: "Test", Email: "test@example.com", When: time.Unix(1, 0),
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	generated := "// Code generated by fixture. DO NOT EDIT.\npackage app\n"
	commit("app.go", generated, "initial")
	if err := os.Rename(filepath.Join(dir, "app.go"), filepath.Join(dir, "generated.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := worktree.Remove("app.go"); err != nil {
		t.Fatal(err)
	}
	commit("generated.go", generated, "rename generated app")
	if err := os.WriteFile(filepath.Join(dir, "generated.go"), []byte("package app\n\nfunc Current() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []Mode{ModeUncommitted, ModeStaged} {
		if mode == ModeStaged {
			if _, err := worktree.Add("generated.go"); err != nil {
				t.Fatal(err)
			}
		}
		prepared, err := Prepare(repo, mode)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.PreviousHeadContextPack.Overview.Files != 1 {
			t.Fatalf("%s previous HEAD files = %d, want 1", mode, prepared.PreviousHeadContextPack.Overview.Files)
		}
		if prepared.PreviousHeadContextPack.Overview.GeneratedFiles != 1 {
			t.Fatalf("%s previous HEAD generated files = %d, want 1", mode, prepared.PreviousHeadContextPack.Overview.GeneratedFiles)
		}
		for _, kind := range []Kind{KindReview, KindSimplify} {
			prompt := UserPrompt(kind, prepared)
			if !strings.Contains(prompt, `"previous_head_context_pack"`) {
				t.Fatalf("%s %s prompt missing previous HEAD context pack: %s", kind, mode, prompt)
			}
		}
	}
}

func TestPrepareClassifiesDeletedGeneratedFilesFromHead(t *testing.T) {
	dir := initReviewRepo(t)
	generated := "// Code generated by fixture. DO NOT EDIT.\npackage app\n"
	writeReviewFile(t, filepath.Join(dir, "generated.go"), generated)
	runReviewGit(t, dir, "add", "generated.go")
	runReviewGit(t, dir, "commit", "-m", "add generated fixture")
	if err := os.Remove(filepath.Join(dir, "generated.go")); err != nil {
		t.Fatal(err)
	}

	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []Mode{ModeUncommitted, ModeStaged} {
		if mode == ModeStaged {
			runReviewGit(t, dir, "add", "-u")
		}
		prepared, err := Prepare(repo, mode)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.ContextPack.Overview.GeneratedFiles != 1 {
			t.Fatalf("%s generated files = %d, want 1; pack = %#v", mode, prepared.ContextPack.Overview.GeneratedFiles, prepared.ContextPack)
		}
		plan, err := PlanBudget(BudgetInput{
			Kind: KindReview, Prepared: prepared, ToolNames: fullDiffBudgetTools,
		})
		if err != nil {
			t.Fatal(err)
		}
		if plan.EffectiveLines != 1 || plan.EffectiveFiles != 1 {
			t.Fatalf("%s effective size = %d lines/%d files, want 1/1", mode, plan.EffectiveLines, plan.EffectiveFiles)
		}
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

func TestReviewPromptsConstrainExternalDocumentation(t *testing.T) {
	t.Parallel()

	for _, kind := range []Kind{KindReview, KindSimplify} {
		prompt := UserPrompt(kind, PreparedContext{Mode: ModeCodebase})
		for _, required := range []string{
			"External lookups verify public language or library contracts only",
			"never replaces exact repository evidence",
			"deduplicated material source URLs or local documentation locators",
			"Disclose concise lookup limitations",
		} {
			if !strings.Contains(prompt, required) {
				t.Fatalf("%s prompt missing %q: %s", kind, required, prompt)
			}
		}
	}
}

func TestSimplifyPromptAuditsOverengineering(t *testing.T) {
	t.Parallel()

	prompt := UserPrompt(KindSimplify, PreparedContext{Mode: ModeCodebase})
	for _, required := range []string{
		"overengineering",
		"unnecessary abstractions",
		"premature generalization",
		"needless indirection",
		"disproportionate architecture",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("simplify prompt missing %q: %s", required, prompt)
		}
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

func initReviewRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runReviewGit(t, dir, "init")
	runReviewGit(t, dir, "config", "user.name", "Test User")
	runReviewGit(t, dir, "config", "user.email", "test@example.com")
	return dir
}

func runReviewGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	gitArgs := append([]string{"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "tag.forceSignAnnotated=false"}, args...)
	cmd := exec.Command("git", gitArgs...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func writeReviewFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
