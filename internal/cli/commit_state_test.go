package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/gitctx"
)

func TestCommitRejectsRepositoryDrift(t *testing.T) {
	for _, command := range []string{"commit", "commit-msg"} {
		for _, drift := range []string{"index", "same-tree-head"} {
			t.Run(command+"/"+drift, func(t *testing.T) {
				dir := initRepo(t)
				runGit(t, dir, "commit", "-m", "base")
				if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte("staged\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				runGit(t, dir, "add", "app.txt")
				originalHead := gitHead(t, dir)
				replacement := make(chan string, 1)
				t.Chdir(dir)
				server := commitMessageServer(t, "fix: update app")
				defer server.Close()
				handler := server.Config.Handler
				server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if drift == "index" {
						if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("extra\n"), 0o644); err != nil {
							t.Error(err)
							w.WriteHeader(500)
							return
						}
						runGit(t, dir, "add", "extra.txt")
					} else {
						tree := strings.TrimSpace(gitOutputString(t, dir, "rev-parse", "HEAD^{tree}"))
						expectedHead := strings.TrimSpace(gitOutputString(t, dir, "commit-tree", tree, "-m", "replacement metadata"))
						runGit(t, dir, "update-ref", "HEAD", expectedHead)
						replacement <- expectedHead
					}
					handler.ServeHTTP(w, r)
				})
				t.Setenv("OPENAI_API_KEY", "test-key")
				t.Setenv("OPENAI_BASE_URL", server.URL)
				t.Setenv("OPENAI_MODEL", "test-model")
				var stdout bytes.Buffer
				app := &App{stdout: &stdout, stderr: &bytes.Buffer{}}
				err := app.Run(t.Context(), []string{command})
				if !errors.Is(err, gitctx.ErrChangeSnapshotStale) {
					t.Fatalf("error = %v", err)
				}
				expectedHead := originalHead
				if drift == "same-tree-head" {
					expectedHead = <-replacement
				}
				if got := gitHead(t, dir); got != expectedHead {
					t.Fatalf("HEAD = %s, want %s", got, expectedHead)
				}
				if command == "commit-msg" && stdout.Len() != 0 {
					t.Fatalf("stale artifact printed: %s", &stdout)
				}
				if strings.Contains(stdout.String(), "INF final") {
					t.Fatalf("stale final trace printed: %s", &stdout)
				}
			})
		}
	}
}

func TestCommitRejectsRedirectingEnvironment(t *testing.T) {
	for _, command := range []string{"commit", "commit-msg"} {
		for _, name := range []string{"GIT_INDEX_FILE", "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_SHALLOW_FILE"} {
			t.Run(command+"/"+name, func(t *testing.T) {
				dir := initRepo(t)
				t.Chdir(dir)
				t.Setenv(name, "private-setting-value")
				app := &App{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
				err := app.Run(t.Context(), []string{command})
				if err == nil || !strings.Contains(err.Error(), name) || strings.Contains(err.Error(), "private-setting-value") {
					t.Fatalf("error = %v", err)
				}
			})
		}
	}
}

func TestCommitBindsWorktreeWithConfigOverride(t *testing.T) {
	dir := initRepo(t)
	t.Chdir(dir)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.worktree")
	t.Setenv("GIT_CONFIG_VALUE_0", t.TempDir())
	server := commitMessageServer(t, "feat: add app")
	defer server.Close()
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)
	t.Setenv("OPENAI_MODEL", "test-model")
	app := &App{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	if err := app.Run(t.Context(), []string{"commit"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(gitOutputString(t, dir, "show", "HEAD:app.txt")); got != "content" {
		t.Fatalf("committed content = %q", got)
	}
}

func TestCommitProviderReceivesOnlyIndexEvidence(t *testing.T) {
	for _, amend := range []bool{false, true} {
		t.Run(fmt.Sprint("amend=", amend), func(t *testing.T) {
			dir := initRepo(t)
			runGit(t, dir, "commit", "-m", "base")
			for name, content := range map[string]string{
				"app.go":     "package app\nfunc StagedEvidence() {}\n",
				"AGENTS.md":  "Prefer staged-guidance-marker when describing changes.\n",
				".gitignore": ".env\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			runGit(t, dir, "add", "app.go", "AGENTS.md", ".gitignore")
			for name, content := range map[string]string{
				"app.go":                "package app\nfunc PRIVATE_WORKTREE() {}\n",
				"AGENTS.md":             "PRIVATE_GUIDANCE\n",
				".env":                  "PRIVATE_IGNORED\n",
				"private-untracked.txt": "PRIVATE_UNTRACKED\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(dir)
			requests := make(chan string, 3)
			requestCount := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				requests <- string(raw)
				requestCount++
				w.Header().Set("Content-Type", "text/event-stream")
				if requestCount == 1 {
					fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"app.go\"}"},{"type":"function_call","id":"fc_2","call_id":"call_2","name":"inspect_file","arguments":"{\"path\":\"app.go\"}"},{"type":"function_call","id":"fc_3","call_id":"call_3","name":"grep","arguments":"{\"pattern\":\"StagedEvidence|PRIVATE\"}"},{"type":"function_call","id":"fc_4","call_id":"call_4","name":"list_files","arguments":"{}"},{"type":"function_call","id":"fc_5","call_id":"call_5","name":"read_file","arguments":"{\"path\":\"app.go\",\"source\":\"worktree\"}"}]}}`)
				} else {
					fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp_2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"base"}]}]}}`)
				}
				fmt.Fprint(w, "\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			t.Setenv("OPENAI_API_KEY", "test-key")
			t.Setenv("OPENAI_BASE_URL", server.URL)
			t.Setenv("OPENAI_MODEL", "test-model")
			var stdout bytes.Buffer
			app := &App{stdout: &stdout, stderr: &bytes.Buffer{}}
			args := []string{"commit-msg"}
			if amend {
				args = append(args, "--amend")
			}
			if err := app.Run(t.Context(), args); err != nil {
				t.Fatal(err)
			}
			first, second := <-requests, <-requests
			for _, request := range []string{first, second} {
				for _, secret := range []string{"PRIVATE_WORKTREE", "PRIVATE_GUIDANCE", "PRIVATE_IGNORED", "PRIVATE_UNTRACKED", "private-untracked.txt"} {
					if strings.Contains(request, secret) {
						t.Fatalf("provider received %s", secret)
					}
				}
			}
			if !strings.Contains(first, "staged-guidance-marker") {
				t.Fatal("provider missing indexed guidance")
			}
			if !strings.Contains(second, "function_call_output") || !strings.Contains(second, "StagedEvidence") || !strings.Contains(second, "unavailable") {
				t.Fatalf("provider missing index tool evidence or worktree rejection: %s", second)
			}
			if stdout.String() != "base\n" {
				t.Fatalf("stdout = %q", stdout.String())
			}
		})
	}
}
