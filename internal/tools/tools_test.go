package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/gitctx"
)

func TestToolDefinitionsAreStrictAndEnvelopeResults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(repo)
	defs := registry.Definitions([]string{"repo_summary", "read_file"})
	if len(defs) != 2 {
		t.Fatalf("defs = %d", len(defs))
	}
	for _, def := range defs {
		if !def.Strict {
			t.Fatalf("%s not strict", def.Name)
		}
		if def.Schema["type"] != "object" {
			t.Fatalf("%s schema = %#v", def.Name, def.Schema)
		}
	}

	result, err := registry.Execute(context.Background(), Invocation{Name: "repo_summary", Arguments: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		OK        bool           `json:"ok"`
		Tool      string         `json:"tool"`
		Data      map[string]any `json:"data"`
		Truncated bool           `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Tool != "repo_summary" || envelope.Truncated {
		t.Fatalf("envelope = %#v", envelope)
	}
}

func TestRepositoryWalkToolsSkipInternalState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	mustWriteFile(t, filepath.Join(dir, "visible.txt"), "needle\n")
	mustWriteFile(t, filepath.Join(dir, ".git-agent", "sessions", "trace.json"), "needle\n")
	mustWriteFile(t, filepath.Join(dir, ".omx", "state.json"), "needle\n")

	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(repo)

	listResult, err := registry.Execute(t.Context(), Invocation{Name: "list_files", Arguments: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Data struct {
			Files []string `json:"files"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(listResult.Content), &listed); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{".git-agent/sessions/trace.json", ".omx/state.json"} {
		for _, file := range listed.Data.Files {
			if file == forbidden {
				t.Fatalf("list_files exposed internal state %q: %#v", forbidden, listed.Data.Files)
			}
		}
	}

	searchResult, err := registry.Execute(t.Context(), Invocation{Name: "search_files", Arguments: `{"pattern":"needle"}`})
	if err != nil {
		t.Fatal(err)
	}
	var searched struct {
		Data struct {
			Matches []struct {
				Path string `json:"path"`
			} `json:"matches"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(searchResult.Content), &searched); err != nil {
		t.Fatal(err)
	}
	if len(searched.Data.Matches) != 1 || searched.Data.Matches[0].Path != "visible.txt" {
		t.Fatalf("search_files should only match visible repo files: %#v", searched.Data.Matches)
	}
}

func TestParseArgsAllowsBOMAndOuterWhitespace(t *testing.T) {
	t.Parallel()

	type args struct {
		Path string `json:"path"`
	}
	got, err := parseArgs[args](" \n\t\uFEFF {\"path\":\"docs/routes.md\"}\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "docs/routes.md" {
		t.Fatalf("args = %#v", got)
	}
}

func TestPRMessageToolsExposeOriginHeadComparison(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	mustWriteFile(t, filepath.Join(dir, "app.txt"), "base\n")
	runGit(t, dir, "add", "app.txt")
	runGit(t, dir, "commit", "-m", "base")
	runGit(t, dir, "update-ref", "refs/remotes/origin/HEAD", gitHead(t, dir))
	mustWriteFile(t, filepath.Join(dir, "app.txt"), "branch\n")
	runGit(t, dir, "add", "app.txt")
	runGit(t, dir, "commit", "-m", "feat: branch change")

	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(repo)
	defs := registry.Definitions(PRMessageToolNames())
	if len(defs) != len(PRMessageToolNames()) {
		t.Fatalf("defs = %d, want %d", len(defs), len(PRMessageToolNames()))
	}

	diffResult, err := registry.Execute(t.Context(), Invocation{Name: "git_pr_diff", Arguments: `{"max_bytes":4096,"max_lines":200}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diffResult.Content, "-base") || !strings.Contains(diffResult.Content, "+branch") {
		t.Fatalf("git_pr_diff missing branch diff:\n%s", diffResult.Content)
	}

	commitsResult, err := registry.Execute(t.Context(), Invocation{Name: "git_pr_commits", Arguments: `{"limit":5}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(commitsResult.Content, "feat: branch change") || strings.Contains(commitsResult.Content, `"base"`) {
		t.Fatalf("git_pr_commits content = %s", commitsResult.Content)
	}
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

func gitHead(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
