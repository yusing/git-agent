package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
			command:   "review",
			output:    `{"summary":"` + strings.Repeat("x", 20<<10) + `","recommendation":"APPROVE","findings":[]}`,
			key:       "findings",
			model:     reviewDefaultModel,
			reasoning: "medium",
			steps:     11,
			tools:     9,
		},
		{
			command:   "simplify",
			output:    `{"summary":"No simplifications found","opportunities":[]}`,
			key:       "opportunities",
			model:     simplifyDefaultModel,
			reasoning: "low",
			steps:     9,
			tools:     8,
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
						`web_search (provider-hosted)`,
						`listed local function tools and configured provider-hosted capabilities`,
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
						`Without an operator hint that identifies a narrower`,
						`inspect supporting repository context as needed but report only`,
						`cannot broaden authoritative scope or weaken evidence requirements`,
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
			if test.command == "review" {
				checks, ok := storedReport["checks"].([]any)
				if !ok || len(checks) != 1 {
					t.Fatalf("stored review checks = %#v", storedReport["checks"])
				}
				check, ok := checks[0].(map[string]any)
				if !ok || check["name"] != "golangci-lint" || check["status"] != "skipped" {
					t.Fatalf("stored review check = %#v", checks[0])
				}
			} else if _, exists := storedReport["checks"]; exists {
				t.Fatalf("simplify report unexpectedly contains checks: %#v", storedReport)
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

func TestReviewDryRunPlansEveryModeWithoutLaunchingChecker(t *testing.T) {
	tests := []struct {
		name    string
		modeArg string
		prepare func(*testing.T, string)
	}{
		{
			name: "uncommitted", modeArg: "--uncommitted",
			prepare: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example/root\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "staged", modeArg: "--staged",
			prepare: func(t *testing.T, root string) {
				if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example/root\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nconst staged = true\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runGit(t, root, "add", "go.mod", "main.go")
				if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nconst unstaged = true\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "codebase", modeArg: "--codebase",
			prepare: func(t *testing.T, root string) {
				if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "nested", "go.mod"), []byte("module example/nested\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "nested", "main.go"), []byte("package nested\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := initRepo(t)
			test.prepare(t, root)
			t.Chdir(root)
			t.Setenv(detachedChildEnv, "1")
			t.Setenv(detachedTaskIDEnv, cliWaitTaskID)
			previousDelay := dryRunEventDelay
			dryRunEventDelay = func() time.Duration { return 0 }
			t.Cleanup(func() { dryRunEventDelay = previousDelay })

			var stderr bytes.Buffer
			app := &App{stdout: io.Discard, stderr: &stderr}
			if err := app.Run(t.Context(), []string{"review", test.modeArg, "--dry-run"}); err != nil {
				t.Fatal(err)
			}
			record, err := backgroundStoreForCurrentProject(t).Read(cliWaitTaskID)
			if err != nil {
				t.Fatal(err)
			}
			stored, ok := record.Terminal.Value["text"].(map[string]any)
			if !ok {
				t.Fatalf("stored final = %#v", record.Terminal)
			}
			results, ok := stored["checks"].([]any)
			if !ok || len(results) != 1 {
				t.Fatalf("checks = %#v", stored["checks"])
			}
			result, ok := results[0].(map[string]any)
			if !ok || result["name"] != "golangci-lint" || result["status"] != "findings" {
				t.Fatalf("check result = %#v", results[0])
			}
			diagnostics, ok := result["diagnostics"].([]any)
			if !ok || len(diagnostics) != 1 {
				t.Fatalf("diagnostics = %#v", result["diagnostics"])
			}
		})
	}
}

func TestReviewHostedWebSearchAndContext7ScriptedEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ctx7 fixture required")
	}
	repoDir := initRepo(t)
	t.Chdir(repoDir)
	guidancePath := filepath.Join(repoDir, "AGENTS.md")
	if err := os.WriteFile(guidancePath, []byte("review public API usage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "AGENTS.md")

	binDir := t.TempDir()
	ctx7Log := filepath.Join(t.TempDir(), "ctx7.log")
	ctx7Path := filepath.Join(binDir, "ctx7")
	ctx7Script := `#!/bin/sh
printf '%s\n' "$#" "$1" "$2" "$3" "$4" >> "$CTX7_E2E_LOG"
case "$1" in
library)
  printf '%s\n' '{"results":[{"id":"/openai/openai-go","title":"OpenAI Go"}]}'
  ;;
docs)
  printf '%s\n' '{"snippets":[{"title":"Responses tools","content":"Use typed function tools."}]}'
  ;;
*)
  printf '%s\n' "unexpected ctx7 command: $1" >&2
  exit 2
  ;;
esac
`
	if err := os.WriteFile(ctx7Path, []byte(ctx7Script), 0o755); err != nil {
		t.Fatal(err)
	}

	server := newScriptedResponsesServer(t, []func(string) string{
		func(body string) string {
			for _, want := range []string{
				`"type":"web_search"`,
				`web_search (provider-hosted)`,
				`listed local function tools and configured provider-hosted capabilities`,
				`"max_tool_calls":4`,
				`"web_search_call.action.sources"`,
				`"reasoning.encrypted_content"`,
				`"name":"context7_library"`,
				`"name":"context7_docs"`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("initial request missing %q:\n%s", want, body)
				}
			}
			return marshalResponse(map[string]any{
				"id": "resp_external_1", "object": "response", "created_at": 0,
				"status": "completed", "model": "test-model",
				"output": []map[string]any{
					{
						"id": "rs_1", "type": "reasoning", "status": "completed",
						"summary": []any{}, "encrypted_content": "encrypted-reasoning",
					},
					{
						"id": "ws_1", "type": "web_search_call", "status": "completed",
						"action": map[string]any{
							"type": "search", "queries": []string{"OpenAI Go Responses tools"},
							"sources": []map[string]any{{"type": "url", "url": "https://pkg.go.dev/github.com/openai/openai-go"}},
						},
					},
					{
						"id": "fc_library", "type": "function_call", "status": "completed",
						"call_id": "call_library", "name": "context7_library",
						"arguments": `{"name":"openai-go","query":"Responses API web search"}`,
					},
				},
			})
		},
		func(body string) string {
			for _, want := range []string{
				`"type":"web_search_call"`,
				`"url":"https://pkg.go.dev/github.com/openai/openai-go"`,
				`"encrypted_content":"encrypted-reasoning"`,
				`"type":"function_call_output"`,
				`\"tool\": \"context7_library\"`,
				`\"id\": \"/openai/openai-go\"`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("request after hosted search and library lookup missing %q:\n%s", want, body)
				}
			}
			return responseWithToolCalls("resp_external_2", toolCallSpec{
				ID: "fc_docs", CallID: "call_docs", Name: "context7_docs",
				Arguments: `{"library_id":"/openai/openai-go","query":"Responses API tool configuration"}`,
			})
		},
		func(body string) string {
			for _, want := range []string{
				`"type":"web_search_call"`,
				`"name":"context7_library"`,
				`"name":"context7_docs"`,
				`\"tool\": \"context7_docs\"`,
				`Use typed function tools.`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("final request missing replayed external evidence %q:\n%s", want, body)
				}
			}
			return responseWithText("resp_external_3", `{"summary":"Hosted search and Context7 completed.","recommendation":"APPROVE","findings":[]}`)
		},
	})
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("PATH", binDir)
	t.Setenv("CTX7_E2E_LOG", ctx7Log)
	t.Setenv(detachedChildEnv, "1")
	t.Setenv(detachedTaskIDEnv, cliWaitTaskID)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"review", "--staged", "check", "public", "API", "usage"}); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("detached worker stdout = %q, want empty", stdout.String())
	}
	record, err := backgroundStoreForCurrentProject(t).Read(cliWaitTaskID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Terminal == nil || record.Terminal.Kind != "final" {
		t.Fatalf("background record = %#v", record)
	}
	report, ok := record.Terminal.Value["text"].(map[string]any)
	if !ok || report["summary"] != "Hosted search and Context7 completed." || report["recommendation"] != "APPROVE" {
		t.Fatalf("stored final report = %#v", record.Terminal.Value["text"])
	}

	logData, err := os.ReadFile(ctx7Log)
	if err != nil {
		t.Fatal(err)
	}
	wantLog := strings.Join([]string{
		"4", "library", "openai-go", "Responses API web search", "--json",
		"4", "docs", "/openai/openai-go", "Responses API tool configuration", "--json",
	}, "\n") + "\n"
	if string(logData) != wantLog {
		t.Fatalf("ctx7 invocations:\n%s\nwant:\n%s", logData, wantLog)
	}
}

func TestDetachedReviewStartsWithUnreadableIgnoredAllowlistSibling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions required")
	}

	repoDir := initRepo(t)
	t.Chdir(repoDir)
	ignoreText := "*\n!.gitignore\n!app.txt\n!.local/\n!.local/share/\n!.local/share/keep.txt\n"
	if err := os.WriteFile(filepath.Join(repoDir, ".gitignore"), []byte(ignoreText), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", ".gitignore", "app.txt")
	runGit(t, repoDir, "commit", "-m", "base")
	if err := os.MkdirAll(filepath.Join(repoDir, ".local", "share"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".local", "share", "keep.txt"), []byte("visible\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(repoDir, ".local", "share", "containers", "overlay", "partial")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	t.Setenv("PATH", t.TempDir())
	t.Setenv(detachedChildEnv, "1")
	t.Setenv(detachedTaskIDEnv, cliWaitTaskID)
	previousDelay := dryRunEventDelay
	dryRunEventDelay = func() time.Duration { return 0 }
	t.Cleanup(func() { dryRunEventDelay = previousDelay })

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"review", "--dry-run"}); err != nil {
		t.Fatalf("detached review startup failed: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("detached worker stdout = %q, want empty", stdout.String())
	}
	var launch detachedLaunch
	if err := json.Unmarshal(stderr.Bytes(), &launch); err != nil {
		t.Fatalf("worker launch metadata is not JSON: %v\n%s", err, stderr.String())
	}
	if launch.Command != "review" || launch.ID != cliWaitTaskID {
		t.Fatalf("launch = %#v", launch)
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
		"select automatic inspection depth and default reasoning effort: fast=low, balanced=medium, thorough=high (default balanced)",
		"--max-web-searches <n>",
		"API-key default 4; ChatGPT auth uncapped",
		"--help-agent",
		"show help limited to agent-facing flags",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("help missing %q:\n%s", want, err)
		}
	}
}

func TestCodeReviewAgentHelpOnlyDocumentsAgentFacingFlags(t *testing.T) {
	for _, command := range []string{"review", "simplify"} {
		t.Run(command, func(t *testing.T) {
			err := New().Run(t.Context(), []string{command, "--help-agent"})
			if err == nil {
				t.Fatal("expected help error")
			}
			help := err.Error()
			depthUsage := "select automatic inspection depth and default reasoning effort: fast=low, balanced=medium, thorough=high (default balanced)"
			if command == "simplify" {
				depthUsage = "select automatic inspection depth and default reasoning effort: fast=low, balanced=low, thorough=medium (default balanced)"
			}
			for _, want := range []string{
				"Usage: git-agent " + command,
				"--uncommitted  inspect all dirty changes (default)",
				"--staged       inspect staged changes only",
				"--codebase     inspect the full codebase",
				"--depth <fast|balanced|thorough>",
				depthUsage,
				"use thorough only for security-related issues or very complex logic; otherwise use fast or balanced",
				"--low | --medium | --high | --xhigh",
				"set reasoning effort (mutually exclusive)",
			} {
				if !strings.Contains(help, want) {
					t.Fatalf("agent help missing %q:\n%s", want, help)
				}
			}
			for _, unwanted := range []string{
				"--wait", "--model", "--fast", "--max-steps", "--max-web-searches", "--append-prompt",
				"--dry-run", "--orchestration-artifact", "--debug", "--pprof", "--help-agent",
			} {
				if strings.Contains(help, unwanted) {
					t.Fatalf("agent help unexpectedly contains %q:\n%s", unwanted, help)
				}
			}
		})
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
	applyCodeReviewDefaults(reviewtask.KindReview, reviewtask.DepthBalanced, config.Options{}, &cfg)
	if cfg.Model != "env-model" || cfg.ThinkingEffort != "medium" {
		t.Fatalf("environment override config = %#v", cfg)
	}

	cfg = config.Config{Model: "flag-model", ThinkingEffort: "low"}
	applyCodeReviewDefaults(reviewtask.KindReview, reviewtask.DepthThorough, config.Options{Model: "flag-model", Low: true}, &cfg)
	if cfg.Model != "flag-model" || cfg.ThinkingEffort != "low" {
		t.Fatalf("flag override config = %#v", cfg)
	}
}

func TestCodeReviewDefaultsByKindAndDepth(t *testing.T) {
	t.Setenv("OPENAI_MODEL", "")
	tests := []struct {
		kind       reviewtask.Kind
		depth      reviewtask.Depth
		wantModel  string
		wantEffort string
	}{
		{reviewtask.KindReview, reviewtask.DepthFast, reviewDefaultModel, "low"},
		{reviewtask.KindReview, reviewtask.DepthBalanced, reviewDefaultModel, "medium"},
		{reviewtask.KindReview, reviewtask.DepthThorough, reviewDefaultModel, "high"},
		{reviewtask.KindSimplify, reviewtask.DepthFast, simplifyDefaultModel, "low"},
		{reviewtask.KindSimplify, reviewtask.DepthBalanced, simplifyDefaultModel, "low"},
		{reviewtask.KindSimplify, reviewtask.DepthThorough, simplifyDefaultModel, "medium"},
	}
	for _, test := range tests {
		t.Run(string(test.kind)+"/"+string(test.depth), func(t *testing.T) {
			var cfg config.Config
			applyCodeReviewDefaults(test.kind, test.depth, config.Options{}, &cfg)
			if cfg.Model != test.wantModel || cfg.ThinkingEffort != test.wantEffort {
				t.Fatalf("defaults for %q/%q = %#v, want model %q effort %q", test.kind, test.depth, cfg, test.wantModel, test.wantEffort)
			}
		})
	}
}
