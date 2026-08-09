package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	backgroundtask "github.com/yusing/git-agent/internal/background"
	"github.com/yusing/git-agent/internal/checks"
	"github.com/yusing/git-agent/internal/followup"
	"github.com/yusing/git-agent/internal/openai"
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
	if err := store.AttachTurn(followUpParentTaskID, backgroundtask.TurnMetadata{
		Mode: "staged", Workspace: "/workspace", ReviewDepth: "balanced",
		PromptCacheKey: "review:parent",
		Tree: followup.Tree{
			Input:  []openai.Item{openai.NewMessage("user", "parent input")},
			Leaves: []followup.Leaf{{Model: "gpt-5.6-sol"}},
		},
	}); err != nil {
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

	parent, err := readFollowUpParent(store, reviewtask.KindReview, followUpParentTaskID, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if parent.Mode != reviewtask.ModeStaged {
		t.Fatalf("mode = %q", parent.Mode)
	}
	gotReport, ok := parent.Report.(map[string]any)
	if !ok || gotReport["summary"] != "done" {
		t.Fatalf("report = %#v", parent.Report)
	}
	if _, err := readFollowUpParent(store, reviewtask.KindSimplify, followUpParentTaskID, "/workspace"); err == nil {
		t.Fatal("simplify accepted a review parent")
	}
}

func TestFollowUpRequiresAnIsolatedPrompt(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing review prompt with fast", args: []string{"review", "--fast", "--follow-up", followUpParentTaskID}, want: "nonempty re-review prompt"},
		{name: "missing review prompt with debug", args: []string{"review", "--debug", "--follow-up", followUpParentTaskID}, want: "nonempty re-review prompt"},
		{name: "missing simplify prompt with fast", args: []string{"simplify", "--fast", "--follow-up", followUpParentTaskID}, want: "nonempty re-review prompt"},
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
	if err := store.AttachTurn(followUpParentTaskID, backgroundtask.TurnMetadata{
		Mode: "staged", Workspace: repoDir, ReviewDepth: "balanced",
		PromptCacheKey: "review:parent",
		Tree: followup.Tree{
			Input:  []openai.Item{openai.NewMessage("user", "parent input sentinel")},
			Leaves: []followup.Leaf{{Model: "gpt-5.6-sol", ReasoningEffort: "medium"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	check := checks.Result{
		Name: "golangci-lint", Status: checks.StatusSkipped,
		Diagnostics: []checks.Diagnostic{}, Reason: "not run",
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
			for _, want := range []string{
				"previous_report", "re-check the fix", "missing case", "old summary",
				"checks", "parent input sentinel", `"service_tier":"priority"`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("follow-up request missing %q:\n%s", want, body)
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
	if err := app.Run(t.Context(), []string{"review", "--debug", "--fast", "--follow-up", followUpParentTaskID, "re-check", "the", "fix"}); err != nil {
		t.Fatal(err)
	}
	child, err := store.Read(cliWaitTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Turn == nil || child.Turn.ParentID != followUpParentTaskID || child.Turn.Mode != "staged" ||
		child.Turn.Depth != 1 || child.Turn.PromptCacheKey != "review:parent" || len(child.Turn.Leaves) != 1 ||
		child.Terminal == nil || child.Terminal.Kind != "final" {
		t.Fatalf("follow-up child = %#v", child)
	}
}

func TestReviewFollowUpReplaysNonBranchedInputAndCacheKey(t *testing.T) {
	repoDir := initRepo(t)
	runGit(t, repoDir, "commit", "-m", "base")
	t.Chdir(repoDir)
	if err := os.WriteFile(filepath.Join(repoDir, "reviewed.go"), []byte("package reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "reviewed.go")

	var mu sync.Mutex
	var requests []openai.Request
	client := openaiClientFunc(func(_ context.Context, request openai.Request) (openai.Response, error) {
		mu.Lock()
		requests = append(requests, request)
		mu.Unlock()
		return openai.Response{TurnState: "sticky-route", Text: `{"summary":"nonbranched report","recommendation":"APPROVE","findings":[]}`}, nil
	})
	originalPath := os.Getenv("PATH")
	parentID := backgroundtask.NewID()
	runReviewWorker(t, client, parentID, "review", "--staged", "--depth", "balanced")
	t.Setenv("PATH", originalPath)
	store := backgroundStoreForCurrentProject(t)
	parent, err := store.Read(parentID)
	if err != nil {
		t.Fatal(err)
	}
	parentLeaves := parent.Turn.Expanded()
	if len(parentLeaves) != 1 {
		t.Fatalf("parent leaves = %d, want 1", len(parentLeaves))
	}

	if err := os.Remove(filepath.Join(repoDir, "reviewed.go")); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "-u", "reviewed.go")
	childID := backgroundtask.NewID()
	runReviewWorker(t, client, childID, "review", "--follow-up", parentID, "re-check", "everything")
	mu.Lock()
	captured := slices.Clone(requests)
	mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(captured))
	}
	if !inputHasPrefix(captured[1].Input, parentLeaves[0].Input) {
		t.Fatal("follow-up request did not preserve the complete parent input prefix")
	}
	if captured[0].PromptCacheKey == "" || captured[1].PromptCacheKey != captured[0].PromptCacheKey {
		t.Fatalf("prompt cache keys = %q / %q", captured[0].PromptCacheKey, captured[1].PromptCacheKey)
	}
	if captured[0].TurnState != "" || captured[1].TurnState != "sticky-route" {
		t.Fatalf("detached root turn states = %q / %q", captured[0].TurnState, captured[1].TurnState)
	}
	if captured[1].Instructions != captured[0].Instructions {
		t.Fatal("follow-up changed provider instructions before the inherited input")
	}
	if !reflect.DeepEqual(captured[1].Tools, captured[0].Tools) {
		t.Fatal("follow-up changed provider tools before the inherited input")
	}
	if captured[1].Model != captured[0].Model ||
		captured[1].ThinkingMode != captured[0].ThinkingMode ||
		captured[1].ReasoningSummary != captured[0].ReasoningSummary ||
		captured[1].ServiceTier != captured[0].ServiceTier {
		t.Fatal("follow-up changed provider model controls before the inherited input")
	}
	suffix := captured[1].Input[len(parentLeaves[0].Input):]
	for _, want := range []string{"current_repository_context", "previous_report", "re-check everything"} {
		if !requestInputContains(suffix, want) {
			t.Fatalf("follow-up suffix missing %q", want)
		}
	}
	if requestInputContains(suffix, "Review authoritative scope") {
		t.Fatal("follow-up suffix repeated the initial mission prompt")
	}
	for _, want := range []string{"nonbranched report", "previous_report", "re-check everything"} {
		if !requestInputContains(captured[1].Input, want) {
			t.Fatalf("follow-up input missing %q", want)
		}
	}
}

func TestReviewFollowUpContinuesBranchedAndExhaustedParents(t *testing.T) {
	for _, rebranch := range []bool{false, true} {
		name := "branched"
		if rebranch {
			name = "branched_exhausted"
		}
		t.Run(name, func(t *testing.T) {
			repoDir := initRepo(t)
			t.Chdir(repoDir)
			if err := os.WriteFile(filepath.Join(repoDir, "reviewed.go"), []byte("package reviewed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGit(t, repoDir, "add", "reviewed.go")

			var mu sync.Mutex
			phase := "initial"
			var followUpRequests []openai.Request
			client := openaiClientFunc(func(_ context.Context, request openai.Request) (openai.Response, error) {
				mu.Lock()
				currentPhase := phase
				if currentPhase == "follow-up" {
					followUpRequests = append(followUpRequests, request)
				}
				mu.Unlock()
				if currentPhase == "initial" {
					if countInputFunctionCalls(request.Input, reviewtask.BranchToolName) == 0 {
						return branchResponse("root", "first scope", "second scope"), nil
					}
					scope := branchIDFromInput(request.Input)
					return openai.Response{Text: fmt.Sprintf(
						`{"summary":"initial %s","recommendation":"APPROVE","findings":[]}`,
						scope,
					)}, nil
				}
				if rebranch && countInputFunctionCalls(request.Input, reviewtask.BranchToolName) == 1 {
					if !requestHasTool(request, reviewtask.BranchToolName) {
						t.Error("continued exhausted branch did not regain branch capability")
					}
					return branchResponse("continued", "recheck first", "recheck second"), nil
				}
				return openai.Response{Text: `{"summary":"follow-up leaf","recommendation":"APPROVE","findings":[]}`}, nil
			})

			parentID := backgroundtask.NewID()
			runReviewWorker(t, client, parentID, "review", "--staged", "--depth", "balanced")
			store := backgroundStoreForCurrentProject(t)
			parent, err := store.Read(parentID)
			if err != nil {
				t.Fatal(err)
			}
			parentLeaves := parent.Turn.Expanded()
			if len(parentLeaves) != 2 {
				t.Fatalf("parent leaves = %d, want 2", len(parentLeaves))
			}

			mu.Lock()
			phase = "follow-up"
			mu.Unlock()
			childID := backgroundtask.NewID()
			runReviewWorker(t, client, childID, "review", "--follow-up", parentID, "re-check", "branches")
			child, err := store.Read(childID)
			if err != nil {
				t.Fatal(err)
			}
			wantLeaves := 2
			if rebranch {
				wantLeaves = 4
			}
			if len(child.Turn.Leaves) != wantLeaves {
				t.Fatalf("child leaves = %d, want %d", len(child.Turn.Leaves), wantLeaves)
			}

			mu.Lock()
			captured := slices.Clone(followUpRequests)
			mu.Unlock()
			rootRequests := make([]openai.Request, 0, len(parentLeaves))
			for _, request := range captured {
				if countInputFunctionCalls(request.Input, reviewtask.BranchToolName) == 1 {
					rootRequests = append(rootRequests, request)
				}
			}
			if len(rootRequests) != len(parentLeaves) {
				t.Fatalf("continued root requests = %d, want %d", len(rootRequests), len(parentLeaves))
			}
			matched := make([]bool, len(parentLeaves))
			for _, request := range rootRequests {
				if request.PromptCacheKey != parent.Turn.PromptCacheKey {
					t.Fatalf("follow-up cache key = %q, want %q", request.PromptCacheKey, parent.Turn.PromptCacheKey)
				}
				for index, leaf := range parentLeaves {
					if inputHasPrefix(request.Input, leaf.Input) {
						matched[index] = true
					}
				}
				for _, want := range []string{"initial b1", "initial b2", "previous_report", "re-check branches"} {
					if !requestInputContains(request.Input, want) {
						t.Fatalf("continued branch input missing %q", want)
					}
				}
			}
			if slices.Contains(matched, false) {
				t.Fatalf("continued branch prefix matches = %v", matched)
			}
		})
	}
}

func runReviewWorker(t *testing.T, client openai.Client, taskID string, args ...string) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv(detachedChildEnv, "1")
	t.Setenv(detachedTaskIDEnv, taskID)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr, responseClient: client}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("%v\nstderr:\n%s", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("worker stdout = %q, want empty", stdout.String())
	}
}

func inputHasPrefix(input, prefix []openai.Item) bool {
	return len(input) >= len(prefix) && slices.Equal(input[:len(prefix)], prefix)
}

func requestInputContains(input []openai.Item, text string) bool {
	for _, item := range input {
		if strings.Contains(item.Content, text) ||
			strings.Contains(item.Output, text) ||
			strings.Contains(item.Arguments, text) ||
			strings.Contains(item.RawJSON, text) {
			return true
		}
	}
	return false
}

func countInputFunctionCalls(input []openai.Item, name string) int {
	count := 0
	for _, item := range input {
		if item.Type == "function_call" && item.Name == name {
			count++
		}
	}
	return count
}

func requestHasTool(request openai.Request, name string) bool {
	for _, tool := range request.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}
