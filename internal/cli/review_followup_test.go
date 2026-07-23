package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	backgroundtask "github.com/yusing/git-agent/internal/background"
	"github.com/yusing/git-agent/internal/checks"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
	"github.com/yusing/git-agent/internal/trace"
)

const followUpParentTaskID = "GHIJKLMNOPQRSTUVWXYZABCDEF"

func TestReadFollowUpParent(t *testing.T) {
	store, err := backgroundtask.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.Create(followUpParentTaskID, "review", 42, now); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachTurn(followUpParentTaskID, backgroundtask.TurnMetadata{Mode: "staged"}); err != nil {
		t.Fatal(err)
	}
	report := map[string]any{"summary": "done", "findings": []any{}}
	if err := store.Complete(followUpParentTaskID, trace.Event{
		At:    now,
		Kind:  "final",
		Value: map[string]any{"text": report},
	}, nil, now); err != nil {
		t.Fatal(err)
	}

	mode, got, err := readFollowUpParent(store, reviewtask.KindReview, followUpParentTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if mode != reviewtask.ModeStaged {
		t.Fatalf("mode = %q", mode)
	}
	gotReport, ok := got.(map[string]any)
	if !ok || gotReport["summary"] != "done" {
		t.Fatalf("report = %#v", got)
	}
	if _, _, err := readFollowUpParent(store, reviewtask.KindSimplify, followUpParentTaskID); err == nil {
		t.Fatal("simplify accepted a review parent")
	}
}

func TestFollowUpRequiresAnIsolatedPrompt(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing prompt", args: []string{"review", "--follow-up", followUpParentTaskID}, want: "nonempty re-review prompt"},
		{name: "scope conflict", args: []string{"review", "--follow-up", followUpParentTaskID, "--staged", "re-check"}, want: "cannot be combined"},
		{name: "provider conflict", args: []string{"review", "--follow-up", followUpParentTaskID, "--model", "test", "re-check"}, want: "cannot be combined"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := (&App{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}).Run(t.Context(), test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestFollowUpReusesDetachedReviewPipeline(t *testing.T) {
	repoDir := initRepo(t)
	t.Chdir(repoDir)
	path := filepath.Join(repoDir, "reviewed.go")
	if err := os.WriteFile(path, []byte("package reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "reviewed.go")

	store := backgroundStoreForCurrentProject(t)
	now := time.Now().UTC()
	if err := store.Create(followUpParentTaskID, "review", 42, now); err != nil {
		t.Fatal(err)
	}
	if err := store.AttachTurn(followUpParentTaskID, backgroundtask.TurnMetadata{Mode: "staged"}); err != nil {
		t.Fatal(err)
	}
	check, err := checks.NewSkipped("golangci-lint", "not run")
	if err != nil {
		t.Fatal(err)
	}
	parentReport := reviewtask.FinalReviewReport{
		Summary: "old summary", Recommendation: "COMMENT",
		Findings: []reviewtask.Finding{{
			Severity: "LOW", Aspect: "tests", Title: "missing case", Impact: "regression risk",
			Evidences:   []reviewtask.Evidence{{Title: "file", Path: "reviewed.go", LineStart: 1, LineEnd: 1}},
			ProposedFix: "add the case",
		}},
		Checks: []checks.Result{check},
	}
	if err := store.Complete(followUpParentTaskID, trace.Event{
		At: now, Kind: "final", Value: map[string]any{"text": parentReport},
	}, nil, now); err != nil {
		t.Fatal(err)
	}

	server := newScriptedResponsesServer(t, []func(string) string{
		func(body string) string {
			for _, want := range []string{"previous_findings", "re-check the fix", "missing case"} {
				if !strings.Contains(body, want) {
					t.Fatalf("follow-up request missing %q:\n%s", want, body)
				}
			}
			for _, excluded := range []string{"old summary", `"checks"`} {
				if strings.Contains(body, excluded) {
					t.Fatalf("follow-up request contains %q:\n%s", excluded, body)
				}
			}
			return responseWithText("resp_follow_up", `{"summary":"Fixed","recommendation":"APPROVE","findings":[]}`)
		},
	})
	defer server.Close()
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv(detachedChildEnv, "1")
	t.Setenv(detachedTaskIDEnv, cliWaitTaskID)

	var stderr bytes.Buffer
	app := &App{stdout: &bytes.Buffer{}, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"review", "--follow-up", followUpParentTaskID, "re-check", "the", "fix"}); err != nil {
		t.Fatal(err)
	}
	child, err := store.Read(cliWaitTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Turn == nil || child.Turn.ParentID != followUpParentTaskID || child.Turn.Mode != "staged" ||
		child.Terminal == nil || child.Terminal.Kind != "final" {
		t.Fatalf("follow-up child = %#v", child)
	}
}
