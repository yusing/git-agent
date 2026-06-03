package commitmsg

import (
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/gitctx"
)

func TestValidateRejectsFencesAndAmendProcessPhrasing(t *testing.T) {
	t.Parallel()

	errs := Validate(ModeAmend, "Update parser\n\nThis amend also fixes docs.\n```")
	if len(errs) < 2 {
		t.Fatalf("expected multiple validation errors, got %v", errs)
	}
}

func TestPromptsNameRequiredScope(t *testing.T) {
	t.Parallel()

	if got := UserPrompt(ModeNormal, 30, 24); !containsAll(got, "Current limits: 30 total model steps, 24 total tool calls.", "staged diff", "git_staged_diff", "ignore unstaged", "task IDs", "cover every distinct staged-diff change cluster") {
		t.Fatalf("normal prompt missing staged scope: %s", got)
	}
	if got := SystemPrompt(ModeNormal); !containsAll(got, "staged paths", "authoritative scope", "distinct high-signal staged change cluster") {
		t.Fatalf("normal system prompt missing cluster coverage: %s", got)
	}
	if got := SystemPrompt(ModeNormal); !containsAll(got, "previous HEAD diff", "contrast", "avoid restating previous work") {
		t.Fatalf("normal system prompt missing previous-diff contrast guard: %s", got)
	}
	if got := SystemPrompt(ModeAmend); !containsAll(got, "final amended commit", "versus its parent", "one commit", "Do not narrate a delta or process") {
		t.Fatalf("amend prompt missing final commit scope: %s", got)
	}
	if got := UserPrompt(ModeAmend, 12, 9); !containsAll(got, "Current limits: 12 total model steps, 9 total tool calls.", "How to read the evidence", "authoritative", "do not dual-narrate", "tone / scope / task IDs") {
		t.Fatalf("amend user prompt missing evidence framing: %s", got)
	}
	if got := UserPrompt(ModePR, 12, 9); !containsAll(got, "Current limits: 12 total model steps, 9 total tool calls.", "squash merge commit message", "origin/HEAD", "git_pr_diff", "branch commits") {
		t.Fatalf("pr prompt missing branch scope: %s", got)
	}
}

func TestPreparedCommitPromptUsesStagedDiffAsAuthoritativeScope(t *testing.T) {
	t.Parallel()

	prepared := PreparedCommitContext{
		Mode:        ModeNormal,
		StagedPaths: []string{"internal/web/uc/phoneconfig/common.go", "internal/web/uc/schedtask/task.go"},
		StagedStats: []gitctx.FileStat{{Path: "internal/web/uc/schedtask/task.go", Adds: 6, Deletes: 1}},
		PreviousHeadPaths: []string{
			"tools/database/go_types_generator/main_test.go",
			"tools/database/go_types_generator/typedef.go.tmpl",
		},
		PreviousHeadStats:         []gitctx.FileStat{{Path: "tools/database/go_types_generator/typedef.go.tmpl", Adds: 42, Deletes: 1}},
		PreviousHeadDiff:          "diff --git a/tools/database/go_types_generator/typedef.go.tmpl b/tools/database/go_types_generator/typedef.go.tmpl\n+func (q {{$structName}}Query) By{{.FieldName}}Str(v string)",
		PreviousHeadDiffTruncated: true,
		Diff:                      "diff --git a/internal/web/uc/schedtask/task.go b/internal/web/uc/schedtask/task.go\n+json.Valid(task.Parameter)",
		DiffTruncated:             false,
	}
	got := UserPromptWithPreparedCommitContext(prepared, 30, 24)
	if !containsAll(got,
		"prepared_commit_context is authoritative",
		"staged_paths, staged_status, and staged_stats summarize",
		"cover every distinct staged-diff change cluster",
		"previous_head_paths, previous_head_stats, and previous_head_diff are contrast only",
		"rely on previous_head_paths/stats for contrast shape",
		"describe only the new staged delta",
		"do not copy phrasing from recent commits or previous_head_diff",
		"go_types_generator/typedef.go.tmpl",
		"internal/web/uc/schedtask/task.go",
		"json.Valid(task.Parameter)",
	) {
		t.Fatalf("prepared commit prompt missing staged authority framing:\n%s", got)
	}
	if !strings.Contains(got, `"diff_truncated": false`) {
		t.Fatalf("prepared commit prompt missing truncation signal:\n%s", got)
	}
}

func TestShapeWrapsBodyAndKeepsSubject(t *testing.T) {
	t.Parallel()

	got := Shape("Add parser\n\n" + strings.Repeat("word ", 30))
	if !strings.HasPrefix(got, "Add parser\n\n") {
		t.Fatalf("missing subject/body split: %q", got)
	}
	for _, line := range strings.Split(got, "\n")[2:] {
		if len(line) > 72 {
			t.Fatalf("line too long: %d %q", len(line), line)
		}
	}
}

func TestShapeRepairsWrappedSubjectContinuation(t *testing.T) {
	t.Parallel()

	output := `refactor(uc): adopt typed query helpers across asterisk, IM and
phoneconfig (T46750)

Switch staged UC packages from model-based query building to typed
query helpers and generated accessors.`

	got := Shape(output)
	want := `refactor(uc): adopt typed query helpers across asterisk, IM and phoneconfig (T46750)

Switch staged UC packages from model-based query building to typed
query helpers and generated accessors.`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPreserveTaskIDSuffixDoesNotLeaveWrappedSubjectShardInBody(t *testing.T) {
	t.Parallel()

	output := `refactor(uc): adopt typed query helpers across asterisk, IM and
phoneconfig

Switch staged UC packages from model-based query building to typed
query helpers and generated accessors.`

	shaped := Shape(output)
	got := PreserveTaskIDSuffix(shaped, []gitctx.CommitInfo{
		{Summary: "feat(typegen): keep generated Col field helpers when referenced (T46750)"},
	})
	want := `refactor(uc): adopt typed query helpers across asterisk, IM and phoneconfig (T46750)

Switch staged UC packages from model-based query building to typed
query helpers and generated accessors.`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestShapeDoesNotMergeLowercaseBodyForPlainSubject(t *testing.T) {
	t.Parallel()

	got := Shape(`Add parser
fixes lexer crashes when rules are empty`)
	want := `Add parser

fixes lexer crashes when rules are empty`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestShapeDoesNotMergeLowercaseBodyForShortConventionalSubject(t *testing.T) {
	t.Parallel()

	got := Shape(`fix(parser): handle empty rules
fixes lexer crashes when rules are empty`)
	want := `fix(parser): handle empty rules

fixes lexer crashes when rules are empty`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPreserveTaskIDSuffixRestoresExactRecentSubjectMatch(t *testing.T) {
	t.Parallel()

	got := PreserveTaskIDSuffix(`fix(schedtask): log skipped duplicate task creation

Log duplicate task payloads before returning.`, []gitctx.CommitInfo{
		{Summary: "fix(schedtask): log skipped duplicate task creation (T46571)"},
	})
	if !strings.HasPrefix(got, "fix(schedtask): log skipped duplicate task creation (T46571)\n") {
		t.Fatalf("missing restored task ID suffix:\n%s", got)
	}
}

func TestPreserveTaskIDSuffixUsesLatestRecentTaskIDForNewSubject(t *testing.T) {
	t.Parallel()

	output := "feat(orm): preserve where insertion order and expose condition formatters"
	got := PreserveTaskIDSuffix(output, []gitctx.CommitInfo{
		{Summary: "feat(typegen): keep generated Col field helpers when referenced (T46750)"},
		{Summary: "feat(orm): expose query tx and cloned state accessors (T46571)"},
	})
	if want := output + " (T46750)"; got != want {
		t.Fatalf("task ID suffix = %q, want %q", got, want)
	}
}

func TestPreserveTaskIDSuffixRestoresDominantRecentTaskIDForSameScope(t *testing.T) {
	t.Parallel()

	got := PreserveTaskIDSuffix(`fix(uc): update nullable query filter call sites

Update UC callers to use the non-null convenience variants for nullable
string and int filters.`, []gitctx.CommitInfo{
		{Summary: "fix(uc): switch generated query call sites to typed By helpers (T46571)"},
		{Summary: "chore(types): regenerate db types (T46571)"},
		{Summary: "fix(typegen): preserve nullable values in generated By helpers (T46571)"},
		{Summary: "fix(schedtask): handle nullable data filters in task dedupe and cleanup (T46571)"},
	})
	if !strings.HasPrefix(got, "fix(uc): update nullable query filter call sites (T46571)\n") {
		t.Fatalf("missing dominant task ID suffix:\n%s", got)
	}
}

func TestPreserveTaskIDSuffixDoesNotUseOlderTaskIDWhenLatestHasNone(t *testing.T) {
	t.Parallel()

	output := "docs(readme): update install guide"
	got := PreserveTaskIDSuffix(output, []gitctx.CommitInfo{
		{Summary: "docs(readme): clarify install flow"},
		{Summary: "fix(uc): switch generated query call sites to typed By helpers (T46571)"},
		{Summary: "chore(types): regenerate db types (T46571)"},
	})
	if got != output {
		t.Fatalf("unexpected dominant task ID suffix restore: %q", got)
	}
}

func TestPromptsReflectExampleStyleExpectations(t *testing.T) {
	t.Parallel()

	if got := SystemPrompt(ModeNormal); !containsAll(got, "type, scope, and impact", "Body optional", "three short paragraphs") {
		t.Fatalf("normal system prompt missing style guidance: %s", got)
	}
	if got := UserPrompt(ModeNormal, 30, 24); !containsAll(got, "git_staged_status", "recent commits", "full staged diff") {
		t.Fatalf("normal user prompt missing structured context guidance: %s", got)
	}
	if got := UserPrompt(ModeAmend, 30, 24); !containsAll(got, "Previous HEAD message is reference only", "polish wording only") {
		t.Fatalf("amend prompt missing example-aligned reuse guidance: %s", got)
	}
	if got := SystemPrompt(ModePR); !containsAll(got, "current branch versus origin/HEAD", "one coherent commit", "squash merge") {
		t.Fatalf("pr system prompt missing squash framing: %s", got)
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
