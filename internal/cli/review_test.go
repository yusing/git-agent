package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yusing/git-agent/internal/config"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
)

func TestDetachedReviewAndSimplifyPersistStrictFinalWithoutStdout(t *testing.T) {
	tests := []struct {
		command   string
		output    string
		key       string
		model     string
		reasoning string
		steps     int
		tools     int
	}{
		{
			command: "review",
			output:  `{"summary":"` + strings.Repeat("x", 20<<10) + `","recommendation":"APPROVE","findings":[]}`,
			key:     "findings",
			model:   reviewDefaultModel,
			steps:   11,
			tools:   9,
		},
		{
			command: "simplify",
			output:  `{"summary":"No simplifications found","opportunities":[]}`,
			key:     "opportunities",
			model:   simplifyDefaultModel,
			steps:   9,
			tools:   8,
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
						`"type":"web_search"`,
						`"max_tool_calls":4`,
						`"web_search_call.action.sources"`,
						`"reasoning.encrypted_content"`,
						`"type":"json_schema"`,
						`"name":"` + test.command + `"`,
						`"review_diff"`,
						`"review_diff_for_paths"`,
						`"read_file"`,
						`"grep"`,
						`"find"`,
						`operator focus`,
						`Never send secrets, source code, diffs, credentials, personal data`,
						`deduplicated material source URLs or local documentation locators`,
						fmt.Sprintf(`<max_model_steps>%d</max_model_steps>`, test.steps),
						fmt.Sprintf(`<max_tool_calls>%d</max_tool_calls>`, test.tools),
					} {
						if !strings.Contains(body, want) {
							t.Fatalf("request missing %q:\n%s", want, body)
						}
					}
					for _, unavailable := range []string{"go_doc", "rust_doc", "context7_library", "context7_docs", "go doc", "rustup doc --path", "Context7 library/docs"} {
						if strings.Contains(body, unavailable) {
							t.Fatalf("request advertises unavailable documentation tool %q:\n%s", unavailable, body)
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
			t.Setenv("PATH", t.TempDir())
			t.Setenv(detachedChildEnv, "1")
			t.Setenv(detachedTaskIDEnv, cliWaitTaskID)

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			app := &App{stdout: &stdout, stderr: &stderr}
			if err := app.Run(t.Context(), []string{test.command, "--staged", "operator", "focus"}); err != nil {
				t.Fatal(err)
			}
			if stdout.Len() != 0 {
				t.Fatalf("detached worker stdout = %q, want empty", stdout.String())
			}
			var expectedReport map[string]any
			if err := json.Unmarshal([]byte(test.output), &expectedReport); err != nil {
				t.Fatal(err)
			}
			if _, ok := expectedReport[test.key].([]any); !ok {
				t.Fatalf("expected report = %#v", expectedReport)
			}
			record, err := backgroundStoreForCurrentProject(t).Read(cliWaitTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Command != test.command || record.Terminal == nil || record.Terminal.Kind != "final" {
				t.Fatalf("background record = %#v", record)
			}
			storedReport, ok := record.Terminal.Value["text"].(map[string]any)
			if !ok || storedReport["summary"] != expectedReport["summary"] {
				t.Fatalf("stored final report = %#v", record.Terminal.Value["text"])
			}
			var launch detachedLaunch
			if err := json.Unmarshal(stderr.Bytes(), &launch); err != nil {
				t.Fatalf("worker launch metadata is not JSON: %v\n%s", err, stderr.String())
			}
			if launch.Command != test.command || launch.ID != cliWaitTaskID || launch.PID != os.Getpid() || launch.Endpoint.Network != localHTTPNetwork || !filepath.IsAbs(launch.Endpoint.Address) || !strings.HasPrefix(launch.Endpoint.URL, "http://localhost/events?token=") {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestDetachedReviewPersistsBoundedFailureDiagnostics(t *testing.T) {
	repoDir := initRepo(t)
	t.Chdir(repoDir)
	path := filepath.Join(repoDir, "app.go")
	if err := os.WriteFile(path, []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "app.go")
	server := newScriptedResponsesServer(t, []func(string) string{
		func(string) string {
			return responseWithToolCalls("resp_tool", toolCallSpec{ID: "fc_1", CallID: "call_1", Name: "repo_summary", Arguments: `{}`})
		},
		func(string) string { return responseWithText("resp_empty", "") },
	})
	defer server.Close()
	t.Setenv("OPENAI_API_KEY", "diagnostic-secret")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv(detachedChildEnv, "1")
	t.Setenv(detachedTaskIDEnv, cliWaitTaskID)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"review", "--staged"}); err == nil {
		t.Fatal("review unexpectedly succeeded")
	}
	record, err := backgroundStoreForCurrentProject(t).Read(cliWaitTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Terminal == nil || record.Terminal.Kind != "error" || record.Failure == nil {
		t.Fatalf("failed background record = %#v", record)
	}
	if record.Failure.Model != reviewDefaultModel || record.Failure.Mode != "staged" || record.Failure.RepositoryFingerprint == nil {
		t.Fatalf("failure identity = %#v", record.Failure)
	}
	if len(record.Failure.ToolEvents) != 2 || record.Failure.ToolEvents[0].Kind != "tool-call" || record.Failure.ToolEvents[1].Kind != "tool-output" {
		t.Fatalf("failure tool events = %#v", record.Failure.ToolEvents)
	}
	data, err := json.Marshal(record.Failure)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "diagnostic-secret") || strings.Contains(string(data), server.URL) {
		t.Fatalf("failure diagnostic contains provider credentials or endpoint: %s", data)
	}
}

func TestDetachedReviewPersistsEarlyFailure(t *testing.T) {
	repoDir := initRepo(t)
	t.Chdir(repoDir)
	t.Setenv(detachedChildEnv, "1")
	t.Setenv(detachedTaskIDEnv, cliWaitTaskID)

	app := &App{stdout: io.Discard, stderr: io.Discard}
	if err := app.Run(t.Context(), []string{"review", "--staged"}); err == nil {
		t.Fatal("empty staged review unexpectedly succeeded")
	}
	record, err := backgroundStoreForCurrentProject(t).Read(cliWaitTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Terminal == nil || record.Terminal.Kind != "error" || record.Failure == nil || record.Failure.Mode != "staged" {
		t.Fatalf("early failure record = %#v", record)
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
	t.Setenv(detachedChildEnv, "1")
	t.Setenv(detachedTaskIDEnv, cliWaitTaskID)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdin: strings.NewReader("y\n"), stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"review", "--staged", "--max-steps", "1"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "Budget reached") || strings.Count(stderr.String(), "\n") != 1 {
		t.Fatalf("stderr contains non-contract output: %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("detached worker stdout = %q, want empty", stdout.String())
	}
	record, err := backgroundStoreForCurrentProject(t).Read(cliWaitTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Terminal == nil || record.Terminal.Kind != "final" {
		t.Fatalf("detached record = %#v", record)
	}
	var report reviewtask.ReviewReport
	reportJSON, err := json.Marshal(record.Terminal.Value["text"])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reportJSON, &report); err != nil {
		t.Fatalf("stored final is not review JSON: %v\n%s", err, reportJSON)
	}
}

func TestReviewModeValidationHappensBeforeProviderResolution(t *testing.T) {
	t.Setenv(detachedChildEnv, "1")
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
		"git-agent review --wait <id>",
		"wait for a detached task and print its report",
		"--uncommitted  inspect all dirty changes (default)",
		"--staged       inspect staged changes only",
		"--codebase     inspect the full codebase",
		"--timeout <duration>",
		"set request timeout (disabled by default)",
		"override model (default gpt-5.6-sol)",
		"--depth <fast|balanced|thorough>",
		"select automatic inspection depth: fast, balanced, or thorough (default balanced)",
		"--max-web-searches <n>",
		"API-key default 4; ChatGPT auth uncapped",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("help missing %q:\n%s", want, err)
		}
	}
}

func TestReviewDepthValidationHappensBeforeDetach(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"invalid", []string{"review", "--depth", "exhaustive"}, "--depth must be one of"},
		{"explicit conflict", []string{"review", "--depth", "fast", "--max-steps", "12"}, "mutually exclusive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := New().Run(t.Context(), test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReviewRejectsNonPositiveWebSearchCapBeforeDetach(t *testing.T) {
	for _, value := range []string{"0", "-1"} {
		err := New().Run(t.Context(), []string{"review", "--max-web-searches", value})
		if err == nil || !strings.Contains(err.Error(), "--max-web-searches must be positive") {
			t.Fatalf("value %s error = %v", value, err)
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
	applyCodeReviewModelDefault(reviewtask.KindReview, config.Options{}, &cfg)
	if cfg.Model != "env-model" || cfg.ThinkingEffort != "" {
		t.Fatalf("environment override config = %#v", cfg)
	}

	cfg = config.Config{Model: "flag-model", ThinkingEffort: "low"}
	applyCodeReviewModelDefault(reviewtask.KindReview, config.Options{Model: "flag-model", Low: true}, &cfg)
	if cfg.Model != "flag-model" || cfg.ThinkingEffort != "low" {
		t.Fatalf("flag override config = %#v", cfg)
	}
}
