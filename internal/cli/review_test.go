package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yusing/git-agent/internal/config"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
)

func TestReviewAndSimplifyEmitStrictJSONAndAgentEventURL(t *testing.T) {
	tests := []struct {
		command   string
		output    string
		key       string
		model     string
		reasoning string
	}{
		{
			command: "review",
			output:  `{"summary":"No defects found","recommendation":"APPROVE","findings":[]}`,
			key:     "findings",
			model:   reviewDefaultModel,
		},
		{
			command: "simplify",
			output:  `{"summary":"No simplifications found","opportunities":[]}`,
			key:     "opportunities",
			model:   simplifyDefaultModel,
		},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			repoDir := initRepo(t)
			t.Chdir(repoDir)
			guidancePath := filepath.Join(repoDir, "AGENTS.md")
			if err := os.WriteFile(guidancePath, []byte("staged guidance\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGit(t, repoDir, "add", "AGENTS.md")
			if err := os.WriteFile(guidancePath, []byte("unstaged injection\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			server := newScriptedResponsesServer(t, []func(string) string{
				func(body string) string {
					for _, want := range []string{
						`"type":"json_schema"`,
						`"name":"` + test.command + `"`,
						`"review_diff"`,
						`"review_diff_for_paths"`,
						`"read_file"`,
						`"grep"`,
						`"find"`,
						`operator focus`,
						fmt.Sprintf(`<max_model_steps>%d</max_model_steps>`, map[string]int{"review": reviewDefaultMaxSteps, "simplify": simplifyDefaultSteps}[test.command]),
						fmt.Sprintf(`<max_tool_calls>%d</max_tool_calls>`, map[string]int{"review": reviewDefaultMaxTools, "simplify": simplifyDefaultTools}[test.command]),
					} {
						if !strings.Contains(body, want) {
							t.Fatalf("request missing %q:\n%s", want, body)
						}
					}
					if !strings.Contains(body, `"model":"`+test.model+`"`) {
						t.Fatalf("request missing default model %q:\n%s", test.model, body)
					}
					if !strings.Contains(body, "staged guidance") || strings.Contains(body, "unstaged injection") {
						t.Fatalf("request did not isolate staged guidance:\n%s", body)
					}
					if !strings.Contains(body, `"summary":"auto"`) {
						t.Fatalf("request missing reasoning summary:\n%s", body)
					}
					if test.reasoning == "" {
						if strings.Contains(body, `"effort":`) {
							t.Fatalf("request unexpectedly sets reasoning effort:\n%s", body)
						}
					} else if !strings.Contains(body, `"effort":"`+test.reasoning+`"`) {
						t.Fatalf("request missing reasoning effort %q:\n%s", test.reasoning, body)
					}
					return responseWithText("resp_"+test.command, test.output)
				},
			})
			defer server.Close()
			t.Setenv("OPENAI_API_KEY", "test-key")
			t.Setenv("OPENAI_BASE_URL", server.URL)
			t.Setenv("OPENAI_MODEL", "")

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			app := &App{stdout: &stdout, stderr: &stderr}
			if err := app.Run(t.Context(), []string{test.command, "--staged", "operator", "focus"}); err != nil {
				t.Fatal(err)
			}
			var report map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
			}
			if _, ok := report[test.key].([]any); !ok {
				t.Fatalf("report = %#v", report)
			}
			wantPrefix := test.command + ": agent events listening on "
			eventURL, err := url.Parse(strings.TrimSpace(strings.TrimPrefix(stderr.String(), wantPrefix)))
			if err != nil || !strings.HasPrefix(stderr.String(), wantPrefix) || eventURL.Hostname() != "127.0.0.1" || eventURL.Path != "/events" || eventURL.Query().Get("token") == "" {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestReviewBudgetExhaustionForcesJSONWithoutPrompt(t *testing.T) {
	repoDir := initRepo(t)
	t.Chdir(repoDir)
	server := newScriptedResponsesServer(t, []func(string) string{
		func(string) string {
			return responseWithToolCalls("resp_tool", toolCallSpec{ID: "fc_1", CallID: "call_1", Name: "repo_summary", Arguments: `{}`})
		},
		func(body string) string {
			var request struct {
				Tools []any `json:"tools"`
			}
			if err := json.Unmarshal([]byte(body), &request); err != nil {
				t.Fatal(err)
			}
			if len(request.Tools) != 0 {
				t.Fatalf("forced finalization still exposes tools: %#v", request.Tools)
			}
			return responseWithText("resp_final", `{"summary":"Budget-bounded review","recommendation":"APPROVE","findings":[]}`)
		},
	})
	defer server.Close()
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdin: strings.NewReader("y\n"), stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"review", "--staged", "--max-steps", "1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "Budget reached") || strings.Count(stderr.String(), "\n") != 1 {
		t.Fatalf("stderr contains non-contract output: %q", stderr.String())
	}
	var report reviewtask.ReviewReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not review JSON: %v\n%s", err, stdout.String())
	}
}

func TestReviewModeValidationHappensBeforeProviderResolution(t *testing.T) {
	app := &App{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	err := app.Run(t.Context(), []string{"review", "--codebase", "--staged"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v", err)
	}
}

func TestReviewHelpDocumentsDefaultMode(t *testing.T) {
	err := New().Run(t.Context(), []string{"review", "--help"})
	if err == nil {
		t.Fatal("expected help error")
	}
	for _, want := range []string{
		"Usage: git-agent review",
		"--uncommitted  inspect all dirty changes (default)",
		"--staged       inspect staged changes only",
		"--codebase     inspect the full codebase",
		"--timeout <duration>",
		"set request timeout (disabled by default)",
		"override model (default gpt-5.6-sol)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("help missing %q:\n%s", want, err)
		}
	}
}

func TestCodeReviewContextHasNoDefaultDeadline(t *testing.T) {
	ctx, cancel := contextWithOptionalTimeout(context.Background(), 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("default review context has a deadline")
	}

	ctx, cancel = contextWithOptionalTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("explicit review timeout did not set a deadline")
	}
}

func TestCodeReviewDefaultsPreserveExplicitOverrides(t *testing.T) {
	t.Setenv("OPENAI_MODEL", "env-model")
	cfg := config.Config{Model: "env-model"}
	applyCodeReviewDefaults(reviewtask.KindReview, config.Options{}, &cfg)
	if cfg.Model != "env-model" || cfg.ThinkingEffort != "" {
		t.Fatalf("environment override config = %#v", cfg)
	}

	cfg = config.Config{Model: "flag-model", ThinkingEffort: "low"}
	applyCodeReviewDefaults(reviewtask.KindReview, config.Options{Model: "flag-model", Low: true}, &cfg)
	if cfg.Model != "flag-model" || cfg.ThinkingEffort != "low" {
		t.Fatalf("flag override config = %#v", cfg)
	}
}
