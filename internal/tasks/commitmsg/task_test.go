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
		Mode:          ModeNormal,
		StagedPaths:   []string{"internal/web/uc/phoneconfig/common.go", "internal/web/uc/schedtask/task.go"},
		StagedStats:   []gitctx.FileStat{{Path: "internal/web/uc/schedtask/task.go", Adds: 6, Deletes: 1}},
		Diff:          "diff --git a/internal/web/uc/schedtask/task.go b/internal/web/uc/schedtask/task.go\n+json.Valid(task.Parameter)",
		DiffTruncated: false,
	}
	got := UserPromptWithPreparedCommitContext(prepared, 30, 24)
	if !containsAll(got,
		"prepared_commit_context is authoritative",
		"staged_paths, staged_status, and staged_stats summarize",
		"cover every distinct staged-diff change cluster",
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

func TestPreserveTaskIDSuffixKeepsUnrelatedSubjectsUntouched(t *testing.T) {
	t.Parallel()

	output := "fix(schedtask): validate task creation"
	got := PreserveTaskIDSuffix(output, []gitctx.CommitInfo{
		{Summary: "fix(schedtask): log skipped duplicate task creation (T46571)"},
	})
	if got != output {
		t.Fatalf("unexpected task ID suffix restore: %q", got)
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
