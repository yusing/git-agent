package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/gitctx"
)

func TestRunWithoutArgsReturnsUsage(t *testing.T) {
	err := New().Run(t.Context(), nil)
	if err == nil {
		t.Fatal("expected usage error")
	}
}

func TestRunCommitMsgRequiresAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "")
	t.Setenv("HOME", t.TempDir())

	app := &App{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	err := app.Run(t.Context(), []string{"commit-msg"})
	if err == nil || !strings.Contains(err.Error(), "missing ~/.codex/auth.json and OPENAI_API_KEY") {
		t.Fatalf("expected missing auth error, got %v", err)
	}
}

func TestRunReleaseNoteRequiresRange(t *testing.T) {
	err := New().Run(t.Context(), []string{"release-note"})
	if err == nil {
		t.Fatal("expected argument error")
	}
}

func TestCommitMsgPrintsOnlyProviderArtifact(t *testing.T) {
	repoDir := initRepo(t)
	t.Chdir(repoDir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ")
		fmt.Fprint(w, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"test-model","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Add parser","annotations":[]}]}]}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "test-model")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"commit-msg"}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Add parser\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	sessions, err := filepath.Glob(filepath.Join(repoDir, ".git-agent", "sessions", "*-commit-msg"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %#v, want one", sessions)
	}
	for _, name := range []string{"events.ndjson", "session.json"} {
		if _, err := os.Stat(filepath.Join(sessions[0], name)); err != nil {
			t.Fatalf("missing trace file %s: %v", name, err)
		}
	}
}

func TestCommitStreamsTraceThenPrintsGitSummary(t *testing.T) {
	repoDir := initRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, ".git", "hooks", "post-commit"), []byte("#!/bin/sh\necho hook stderr >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoDir)
	server := commitMessageServer(t, "feat: add app")
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "test-model")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"commit"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stderr.String()); got != "hook stderr" {
		t.Fatalf("stderr = %q", stderr.String())
	}

	events, rawOutput := decodeCommitOutput(t, stdout.Bytes())
	if !strings.Contains(rawOutput, "(root-commit)") || !strings.Contains(rawOutput, "feat: add app") {
		t.Fatalf("raw git output = %q", rawOutput)
	}
	session := eventValue(t, events, "session")
	if got := session["command"]; got != "commit" {
		t.Fatalf("trace command = %#v", got)
	}
	final := eventValue(t, events, "final")
	if got := final["text"]; got != "feat: add app" {
		t.Fatalf("trace final text = %#v", got)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".git-agent")); !os.IsNotExist(err) {
		t.Fatalf(".git-agent stat err = %v, want not exist", err)
	}
	if got := gitOutputString(t, repoDir, "log", "-1", "--pretty=%s"); strings.TrimSpace(got) != "feat: add app" {
		t.Fatalf("commit subject = %q", got)
	}
}

func TestCommitAmendAmendsHead(t *testing.T) {
	repoDir := initRepo(t)
	runGit(t, repoDir, "commit", "-m", "feat: amend app")
	if err := os.WriteFile(filepath.Join(repoDir, "app.txt"), []byte("amended\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "app.txt")
	t.Chdir(repoDir)
	server := commitMessageServer(t, "feat: amend app")
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "test-model")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"commit", "--amend"}); err != nil {
		t.Fatal(err)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	events, rawOutput := decodeCommitOutput(t, stdout.Bytes())
	if !strings.Contains(rawOutput, "feat: amend app") {
		t.Fatalf("raw git output = %q", rawOutput)
	}
	session := eventValue(t, events, "session")
	if got := session["mode"]; got != "amend" {
		t.Fatalf("trace mode = %#v", got)
	}
	if got := strings.TrimSpace(gitOutputString(t, repoDir, "rev-list", "--count", "HEAD")); got != "1" {
		t.Fatalf("commit count = %q", got)
	}
	if got := strings.TrimSpace(gitOutputString(t, repoDir, "log", "-1", "--pretty=%s")); got != "feat: amend app" {
		t.Fatalf("commit subject = %q", got)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".git-agent")); !os.IsNotExist(err) {
		t.Fatalf(".git-agent stat err = %v, want not exist", err)
	}
}

func TestCommitAmendRepairsMessageThatDropsOriginalSubject(t *testing.T) {
	repoDir := initRepo(t)
	runGit(t, repoDir, "commit", "-m", "feat(cli): add commit command", "-m", "Add commit creation after message generation.")
	if err := os.WriteFile(filepath.Join(repoDir, "app.txt"), []byte("amended\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "app.txt")
	t.Chdir(repoDir)
	server := commitMessageSequenceServer(t,
		"feat(trace): switch streamed commit traces to custom console output\n\nRewrite console trace formatting.",
		"feat(cli): add commit command\n\nAdd commit creation after message generation and keep trace output readable.",
	)
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "test-model")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"commit", "--amend"}); err != nil {
		t.Fatal(err)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if got := strings.TrimSpace(gitOutputString(t, repoDir, "log", "-1", "--pretty=%s")); got != "feat(cli): add commit command" {
		t.Fatalf("commit subject = %q", got)
	}
	if body := gitOutputString(t, repoDir, "log", "-1", "--pretty=%b"); !strings.Contains(strings.Join(strings.Fields(body), " "), "keep trace output readable") {
		t.Fatalf("commit body = %q", body)
	}
}

func TestCommitAmendPreservesOriginalAuthor(t *testing.T) {
	repoDir := initRepo(t)
	runGit(t, repoDir, "config", "user.name", "Original Author")
	runGit(t, repoDir, "config", "user.email", "original@example.com")
	runGit(t, repoDir, "commit", "-m", "feat: amend app")
	runGit(t, repoDir, "config", "user.name", "Current Committer")
	runGit(t, repoDir, "config", "user.email", "current@example.com")
	if err := os.WriteFile(filepath.Join(repoDir, "app.txt"), []byte("amended\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "app.txt")
	t.Chdir(repoDir)
	server := commitMessageServer(t, "feat: amend app")
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "test-model")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"commit", "--amend"}); err != nil {
		t.Fatal(err)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	got := strings.TrimSpace(gitOutputString(t, repoDir, "log", "-1", "--format=%an <%ae>|%cn <%ce>"))
	want := "Original Author <original@example.com>|Current Committer <current@example.com>"
	if got != want {
		t.Fatalf("author/committer = %q, want %q", got, want)
	}
}

func TestCommitFailureReturnsGeneratedMessage(t *testing.T) {
	repoDir := initRepo(t)
	runGit(t, repoDir, "config", "commit.gpgSign", "true")
	fakeGPG := filepath.Join(t.TempDir(), "fake-gpg")
	if err := os.WriteFile(fakeGPG, []byte("#!/bin/sh\necho fake gpg locked >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "config", "gpg.program", fakeGPG)
	t.Chdir(repoDir)
	server := commitMessageServer(t, "feat: signed commit")
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "test-model")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	err := app.Run(t.Context(), []string{"commit"})
	if err == nil {
		t.Fatal("expected commit failure")
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	events, gitOutput := decodeCommitOutput(t, stdout.Bytes())
	if strings.TrimSpace(gitOutput) != "" {
		t.Fatalf("git output = %q", gitOutput)
	}
	final := eventValue(t, events, "final")
	if got := final["text"]; got != "feat: signed commit" {
		t.Fatalf("trace final text = %#v", got)
	}
	traceError := eventValue(t, events, "error")
	if traceError["generated_commit_message"] != "feat: signed commit" || !strings.Contains(stdout.String(), "fake gpg locked") {
		t.Fatalf("trace error = %#v", traceError)
	}
	for _, want := range []string{
		"commit failed after message generation",
		"git commit failed",
		"fake gpg locked",
		"Generated commit message:\nfeat: signed commit",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q:\n%s", want, err)
		}
	}
}

func TestCommitMsgUsesChatGPTAuthFileByDefault(t *testing.T) {
	repoDir := initRepo(t)
	t.Chdir(repoDir)
	writeCodexAuth(t, `{
		"auth_mode": "chatgpt",
		"tokens": {
			"access_token": "access-token",
			"account_id": "workspace-123"
		}
	}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "workspace-123" {
			t.Fatalf("ChatGPT-Account-ID = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ")
		fmt.Fprint(w, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"test-model","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Add parser","annotations":[]}]}]}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "http://legacy.example/v1")
	t.Setenv("OPENAI_MODEL", "test-model")

	var stdout bytes.Buffer
	app := &App{stdout: &stdout, stderr: &bytes.Buffer{}}
	if err := app.Run(t.Context(), []string{"commit-msg", "--base-url", server.URL}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "Add parser\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestCommitMsgRequiresStagedChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "normal", args: []string{"commit-msg"}},
		{name: "amend", args: []string{"commit-msg", "--amend"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := initRepo(t)
			runGit(t, repoDir, "commit", "-m", "base")
			t.Chdir(repoDir)

			t.Setenv("OPENAI_API_KEY", "test-key")
			t.Setenv("OPENAI_BASE_URL", "")
			t.Setenv("OPENAI_MODEL", "")

			var stdout bytes.Buffer
			var stderr bytes.Buffer
			app := &App{stdout: &stdout, stderr: &stderr}
			err := app.Run(t.Context(), tc.args)
			if err == nil || !strings.Contains(err.Error(), "commit-msg requires staged changes") {
				t.Fatalf("expected staged changes error, got %v", err)
			}
			if stdout.String() != "" {
				t.Fatalf("stdout = %q", stdout.String())
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestCommitRequiresStagedChanges(t *testing.T) {
	repoDir := initRepo(t)
	runGit(t, repoDir, "commit", "-m", "base")
	t.Chdir(repoDir)

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	err := app.Run(t.Context(), []string{"commit"})
	if err == nil || !strings.Contains(err.Error(), "commit requires staged changes") {
		t.Fatalf("expected staged changes error, got %v", err)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestCommitMsgRestoresMatchingRecentTaskIDSuffix(t *testing.T) {
	repoDir := initRepo(t)
	runGit(t, repoDir, "commit", "-m", "fix(schedtask): log skipped duplicate task creation (T46571)")
	if err := os.WriteFile(filepath.Join(repoDir, "app.txt"), []byte("updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "app.txt")
	t.Chdir(repoDir)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ")
		fmt.Fprint(w, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"test-model","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"fix(schedtask): log skipped duplicate task creation\n\nLog duplicate task payloads before returning.","annotations":[]}]}]}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "test-model")

	var stdout bytes.Buffer
	app := &App{stdout: &stdout, stderr: &bytes.Buffer{}}
	if err := app.Run(t.Context(), []string{"commit-msg"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "fix(schedtask): log skipped duplicate task creation (T46571)\n\n") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestPRMessageUsesPreparedOriginHeadContextAndPrintsArtifact(t *testing.T) {
	repoDir := initRepo(t)
	runGit(t, repoDir, "commit", "-m", "base")
	runGit(t, repoDir, "update-ref", "refs/remotes/origin/HEAD", gitHead(t, repoDir))
	if err := os.WriteFile(filepath.Join(repoDir, "app.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoDir, "add", "app.txt")
	runGit(t, repoDir, "commit", "-m", "feat: branch change")
	t.Chdir(repoDir)

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		requests = append(requests, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ")
		fmt.Fprint(w, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"test-model","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"feat: update app from branch","annotations":[]}]}]}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "test-model")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"pr-message"}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "feat: update app from branch\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(requests) != 1 {
		t.Fatalf("request count = %d", len(requests))
	}
	for _, want := range []string{"prepared_pr_context", "origin/HEAD", "app.txt", "-content", "+branch", "<command>pr-message</command>", "<mode>origin/HEAD..HEAD</mode>"} {
		if !strings.Contains(requests[0], want) {
			t.Fatalf("pr-message request missing %q:\n%s", want, requests[0])
		}
	}
	if strings.Contains(requests[0], "git_pr_") {
		t.Fatalf("pr-message request should not expose PR tools:\n%s", requests[0])
	}
	sessions, err := filepath.Glob(filepath.Join(repoDir, ".git-agent", "sessions", "*-pr-message"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %#v, want one", sessions)
	}
}

func TestReleaseNoteRaisesStepAndTimeoutFloor(t *testing.T) {
	repoDir := initRepo(t)
	t.Chdir(repoDir)
	runGit(t, repoDir, "commit", "--allow-empty", "-m", "base")
	runGit(t, repoDir, "tag", "-m", "v1.0.0", "v1.0.0")
	runGit(t, repoDir, "commit", "--allow-empty", "-m", "release")

	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, payload)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ")
		fmt.Fprint(w, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"test-model","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"{\"sections\":[]}","annotations":[]}]}]}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "test-model")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"release-note", "--timeout", "30s", "--max-steps", "3", "v1.0.0", "HEAD"}); err != nil {
		t.Fatal(err)
	}
	if len(requests) == 0 {
		t.Fatal("expected at least one request")
	}
}

func TestEnvironmentContextIncludesCurrentStepLimit(t *testing.T) {
	repoDir := initRepo(t)
	repo, err := gitctx.Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}

	got := environmentContext(repo, "commit-msg", "normal", "auto", 30, 24)
	if !strings.Contains(got, "<max_model_steps>30</max_model_steps>") {
		t.Fatalf("environment context missing max steps: %s", got)
	}
	if !strings.Contains(got, "<max_tool_calls>24</max_tool_calls>") {
		t.Fatalf("environment context missing max tool calls: %s", got)
	}
}

func TestCommitMsgForwardsFastAndThinkingFlags(t *testing.T) {
	repoDir := initRepo(t)
	t.Chdir(repoDir)

	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, payload)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ")
		fmt.Fprint(w, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"test-model","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Add parser","annotations":[]}]}]}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "test-model")

	app := &App{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	if err := app.Run(t.Context(), []string{"commit-msg", "--fast", "--medium"}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("request count = %d", len(requests))
	}
	if got := requests[0]["service_tier"]; got != "priority" {
		t.Fatalf("service_tier = %#v", got)
	}
	reasoning, ok := requests[0]["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %#v", requests[0]["reasoning"])
	}
	if got := reasoning["effort"]; got != "medium" {
		t.Fatalf("reasoning.effort = %#v", got)
	}
}

func TestRunRejectsConflictingThinkingFlags(t *testing.T) {
	app := &App{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	err := app.Run(t.Context(), []string{"commit-msg", "--high", "--xhigh"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got %v", err)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "app.txt")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func gitOutputString(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

func commitMessageServer(t *testing.T, message string) *httptest.Server {
	t.Helper()
	return commitMessageSequenceServer(t, message)
}

func commitMessageSequenceServer(t *testing.T, messages ...string) *httptest.Server {
	t.Helper()
	if len(messages) == 0 {
		t.Fatal("commit message sequence must not be empty")
	}
	var responses []string
	for _, message := range messages {
		escaped, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, string(escaped))
	}
	var requestCount int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		idx := min(requestCount, len(responses)-1)
		requestCount++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ")
		fmt.Fprintf(w, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"test-model","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":%s,"annotations":[]}]}]}}`, responses[idx])
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

func decodeCommitOutput(t *testing.T, data []byte) ([]map[string]any, string) {
	t.Helper()
	var events []map[string]any
	var rest bytes.Buffer
	inRest := false
	lines := bytes.Split(data, []byte("\n"))
	for idx, line := range lines {
		if len(line) == 0 {
			if inRest && idx < len(lines)-1 {
				rest.WriteByte('\n')
			}
			continue
		}
		text := string(line)
		if !inRest {
			if event, ok := parseTextTraceEvent(t, text); ok {
				events = append(events, event)
				continue
			}
			if len(events) > 0 && strings.HasPrefix(text, "  ") && !strings.HasPrefix(text, "    ") {
				parseContinuationFields(events[len(events)-1], strings.TrimSpace(text))
				continue
			}
			if strings.HasPrefix(text, "    ") {
				continue
			}
		}
		inRest = true
		rest.Write(line)
		if idx < len(lines)-1 {
			rest.WriteByte('\n')
		}
	}
	if len(events) == 0 {
		t.Fatalf("no trace events in output:\n%s", data)
	}
	return events, rest.String()
}

func parseTextTraceEvent(t *testing.T, line string) (map[string]any, bool) {
	t.Helper()
	fields := splitTextFields(line)
	if len(fields) < 3 || !consoleTimeField(fields[0]) || !consoleLevelField(fields[1]) {
		return nil, false
	}
	event := map[string]any{
		"time":  fields[0],
		"level": fields[1],
		"msg":   fields[2],
	}
	parseFields(event, fields[3:])
	return event, true
}

func parseContinuationFields(event map[string]any, line string) {
	if strings.HasSuffix(line, ":") {
		return
	}
	parseFields(event, splitTextFields(line))
}

func parseFields(event map[string]any, fields []string) {
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		}
		if strings.Contains(key, ".") {
			setNested(event, key, value)
			continue
		}
		event[key] = value
	}
}

func consoleTimeField(value string) bool {
	parts := strings.Split(value, ":")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if len(part) != 2 {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func consoleLevelField(value string) bool {
	switch value {
	case "DBG", "INF", "WRN", "ERR":
		return true
	default:
		return false
	}
}

func splitTextFields(line string) []string {
	var fields []string
	start := -1
	inQuote := false
	escaped := false
	for idx, r := range line {
		if start < 0 {
			if r == ' ' {
				continue
			}
			start = idx
		}
		if escaped {
			escaped = false
			continue
		}
		switch r {
		case '\\':
			if inQuote {
				escaped = true
			}
		case '"':
			inQuote = !inQuote
		case ' ':
			if !inQuote {
				fields = append(fields, line[start:idx])
				start = -1
			}
		}
	}
	if start >= 0 {
		fields = append(fields, line[start:])
	}
	return fields
}

func setNested(root map[string]any, dotted string, value string) {
	parts := strings.Split(dotted, ".")
	for _, part := range parts[:len(parts)-1] {
		next, ok := root[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			root[part] = next
		}
		root = next
	}
	root[parts[len(parts)-1]] = value
}

func eventValue(t *testing.T, events []map[string]any, kind string) map[string]any {
	t.Helper()
	for _, event := range events {
		if event["msg"] != kind {
			continue
		}
		return event
	}
	t.Fatalf("missing trace event %q in %#v", kind, events)
	return nil
}

func writeCodexAuth(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
