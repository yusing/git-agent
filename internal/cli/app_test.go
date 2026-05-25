package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/gitctx"
)

func TestRunWithoutArgsReturnsUsage(t *testing.T) {
	err := New().Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected usage error")
	}
}

func TestRunCommitMsgRequiresAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "")

	app := &App{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	err := app.Run(context.Background(), []string{"commit-msg"})
	if err == nil || !strings.Contains(err.Error(), "missing OPENAI_API_KEY") {
		t.Fatalf("expected missing API key error, got %v", err)
	}
}

func TestRunReleaseNoteRequiresRange(t *testing.T) {
	err := New().Run(context.Background(), []string{"release-note"})
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
	if err := app.Run(context.Background(), []string{"commit-msg"}); err != nil {
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
	if err := app.Run(context.Background(), []string{"pr-message"}); err != nil {
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
	if err := app.Run(context.Background(), []string{"release-note", "--timeout", "30s", "--max-steps", "3", "v1.0.0", "HEAD"}); err != nil {
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
	if err := app.Run(context.Background(), []string{"commit-msg", "--fast", "--medium"}); err != nil {
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
	err := app.Run(context.Background(), []string{"commit-msg", "--high", "--xhigh"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually exclusive error, got %v", err)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
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
