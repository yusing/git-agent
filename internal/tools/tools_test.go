package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/gitctx"
	skillctx "github.com/yusing/git-agent/internal/skills"
)

func TestErrorResultUsesStableBoundedEnvelope(t *testing.T) {
	t.Parallel()

	result, err := ErrorResult("read_file", errors.New(strings.Repeat("missing evidence\n", 500)))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		OK        bool   `json:"ok"`
		Tool      string `json:"tool"`
		Error     string `json:"error"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Tool != "read_file" || !envelope.Truncated || !result.Truncated {
		t.Fatalf("error envelope = %#v, result = %#v", envelope, result)
	}
	if len(envelope.Error) > maxErrorBytes+len("\n[truncated]\n") {
		t.Fatalf("error output has %d bytes", len(envelope.Error))
	}
}

func TestToolDefinitionsAreStrictAndEnvelopeResults(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistryWithSkills(repo, nil)
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
	mustWriteFile(t, filepath.Join(dir, ".omx", "tracked.txt"), "tracked-needle\n")
	runGit(t, dir, "add", "-f", ".omx/tracked.txt")

	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistryWithSkills(repo, nil)

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
	if !slices.Contains(listed.Data.Files, ".omx/tracked.txt") {
		t.Fatalf("list_files hid tracked internal path: %#v", listed.Data.Files)
	}

	searchResult, err := registry.Execute(t.Context(), Invocation{Name: "grep", Arguments: `{"pattern":"needle"}`})
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
	if len(searched.Data.Matches) != 2 || searched.Data.Matches[0].Path != ".omx/tracked.txt" || searched.Data.Matches[1].Path != "visible.txt" {
		t.Fatalf("grep should include visible and tracked internal files: %#v", searched.Data.Matches)
	}
}

func TestStagedReviewToolsNeverReadUnstagedWorktree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	mustWriteFile(t, filepath.Join(dir, "app.txt"), "base\n")
	runGit(t, dir, "add", "app.txt")
	runGit(t, dir, "commit", "-m", "base")
	mustWriteFile(t, filepath.Join(dir, "app.txt"), "staged-value\n")
	runGit(t, dir, "add", "app.txt")
	mustWriteFile(t, filepath.Join(dir, "app.txt"), "unstaged-secret\n")
	mustWriteFile(t, filepath.Join(dir, "untracked-secret.txt"), "unstaged-secret\n")

	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewReviewRegistryWithSkills(repo, nil, ReviewModeStaged, ReviewScope{})
	if _, err := registry.Execute(t.Context(), Invocation{Name: "git_staged_status", Arguments: `{}`}); err == nil {
		t.Fatal("review registry exposed a non-review tool")
	}

	readResult, err := registry.Execute(t.Context(), Invocation{Name: "read_file", Arguments: `{"path":"app.txt"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readResult.Content, "staged-value") || strings.Contains(readResult.Content, "unstaged-secret") {
		t.Fatalf("read_file leaked worktree content: %s", readResult.Content)
	}
	if _, err := registry.Execute(t.Context(), Invocation{Name: "read_file", Arguments: `{"path":"app.txt","source":"worktree"}`}); err == nil {
		t.Fatal("read_file accepted worktree source in staged mode")
	}

	grepResult, err := registry.Execute(t.Context(), Invocation{Name: "grep", Arguments: `{"pattern":"staged-value|unstaged-secret"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(grepResult.Content, "staged-value") || strings.Contains(grepResult.Content, "unstaged-secret") {
		t.Fatalf("grep leaked worktree content: %s", grepResult.Content)
	}

	for _, invocation := range []Invocation{
		{Name: "list_files", Arguments: `{}`},
		{Name: "find", Arguments: `{"type":"file"}`},
	} {
		result, err := registry.Execute(t.Context(), invocation)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.Content, "app.txt") || strings.Contains(result.Content, "untracked-secret.txt") {
			t.Fatalf("%s leaked worktree paths: %s", invocation.Name, result.Content)
		}
	}
}

func TestReviewChangesPaginatesAuthoritativeScope(t *testing.T) {
	t.Parallel()

	paths := make([]string, 130)
	status := make([]gitctx.PathChange, len(paths))
	stats := make([]gitctx.FileStat, len(paths))
	for i := range paths {
		paths[i] = fmt.Sprintf("change-%03d.go", i)
		status[i] = gitctx.PathChange{Path: paths[i], Worktree: "M"}
		stats[i] = gitctx.FileStat{Path: paths[i], Adds: i}
	}
	scope := NewReviewScope(paths, status, stats)
	registry := NewReviewRegistryWithSkills(nil, nil, ReviewModeUncommitted, scope)
	result, err := registry.Execute(t.Context(), Invocation{Name: "review_changes", Arguments: `{"offset":128,"limit":1}`})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data struct {
			Changes    []ReviewChange `json:"changes"`
			Total      int            `json:"total"`
			NextOffset int            `json:"next_offset"`
			HasMore    bool           `json:"has_more"`
		} `json:"data"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Changes) != 1 || envelope.Data.Changes[0].Path != "change-128.go" || envelope.Data.Changes[0].Adds != 128 {
		t.Fatalf("changes = %#v", envelope.Data.Changes)
	}
	if envelope.Data.Total != 130 || envelope.Data.NextOffset != 129 || !envelope.Data.HasMore || !envelope.Truncated {
		t.Fatalf("page metadata = %#v", envelope)
	}
}

func TestRepositoryToolsDoNotFollowSymlinks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	mustWriteFile(t, outside, "outside-secret\n")
	if err := os.Symlink(outside, filepath.Join(dir, "leak.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".gitmodules")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistryWithSkills(repo, nil)
	if _, err := registry.Execute(t.Context(), Invocation{Name: "read_file", Arguments: `{"path":"leak.txt"}`}); err == nil {
		t.Fatal("read_file followed symlink outside repository")
	}

	listResult, err := registry.Execute(t.Context(), Invocation{Name: "list_files", Arguments: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listResult.Content, "leak.txt") {
		t.Fatalf("list_files exposed symlink: %s", listResult.Content)
	}

	searchResult, err := registry.Execute(t.Context(), Invocation{Name: "grep", Arguments: `{"pattern":"outside-secret"}`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(searchResult.Content, "outside-secret") {
		t.Fatalf("grep followed symlink: %s", searchResult.Content)
	}
	if _, err := registry.Execute(t.Context(), Invocation{Name: "gitmodules_table", Arguments: "{}"}); err == nil {
		t.Fatal("gitmodules_table followed symlink outside repository")
	}
}

func TestReadFileSelectsSnapshotAndInclusiveLineRange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	mustWriteFile(t, filepath.Join(dir, "app.txt"), "one\nbase\nthree\n")
	runGit(t, dir, "add", "app.txt")
	runGit(t, dir, "commit", "-m", "base")
	mustWriteFile(t, filepath.Join(dir, "app.txt"), "one\nstaged\nthree\n")
	runGit(t, dir, "add", "app.txt")
	mustWriteFile(t, filepath.Join(dir, "app.txt"), "one\nworktree\nthree\n")

	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistryWithSkills(repo, nil)
	for source, want := range map[string]string{"head": "base", "index": "staged", "worktree": "worktree"} {
		result, err := registry.Execute(t.Context(), Invocation{Name: "read_file", Arguments: fmt.Sprintf(`{"path":"app.txt","source":%q,"line_start":2,"line_end":2}`, source)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.Content, `"content": "`+want+`\n"`) {
			t.Fatalf("source %s missing %q: %s", source, want, result.Content)
		}
		if !strings.Contains(result.Content, `"line_start": 2`) || !strings.Contains(result.Content, `"line_end": 2`) {
			t.Fatalf("source %s missing selected range: %s", source, result.Content)
		}
	}
}

func TestFindAndGrepSupportBoundedDiscovery(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	mustWriteFile(t, filepath.Join(dir, "internal", "one.go"), "package internal\n\nfunc One() {}\n")
	mustWriteFile(t, filepath.Join(dir, "internal", "two.txt"), "func text\n")
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistryWithSkills(repo, nil)

	grepResult, err := registry.Execute(t.Context(), Invocation{Name: "grep", Arguments: `{"pattern":"^func [A-Z]","path":"internal","glob":"*.go","max_matches":10}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(grepResult.Content, "internal/one.go") || strings.Contains(grepResult.Content, "two.txt") {
		t.Fatalf("grep result = %s", grepResult.Content)
	}

	findResult, err := registry.Execute(t.Context(), Invocation{Name: "find", Arguments: `{"path":"internal","name":"*.go","type":"file","max_entries":10}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(findResult.Content, "internal/one.go") || strings.Contains(findResult.Content, "two.txt") {
		t.Fatalf("find result = %s", findResult.Content)
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

func TestLegacyReleaseNoteToolsAreMarkedDeprecated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	registry := NewRegistryWithSkills(repo, nil)
	for _, def := range registry.Definitions([]string{
		"resolve_ref",
		"git_log_range",
		"gitmodules_table",
		"submodule_gitlink_range",
		"submodule_log_range",
		"repo_kind",
	}) {
		if !strings.HasPrefix(def.Description, "Deprecated:") {
			t.Fatalf("%s description is not marked deprecated: %q", def.Name, def.Description)
		}
	}
}

func TestStagedDiffForPathsReturnsSelectedStagedPatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	mustWriteFile(t, filepath.Join(dir, "one.txt"), "old one\n")
	mustWriteFile(t, filepath.Join(dir, "two.txt"), "old two\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "base")
	mustWriteFile(t, filepath.Join(dir, "one.txt"), "new one\n")
	mustWriteFile(t, filepath.Join(dir, "two.txt"), "new two\n")
	runGit(t, dir, "add", ".")

	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistryWithSkills(repo, nil)
	defs := registry.Definitions(CommitMessageToolNames())
	if len(defs) != len(CommitMessageToolNames()) {
		t.Fatalf("defs = %d, want %d", len(defs), len(CommitMessageToolNames()))
	}

	result, err := registry.Execute(t.Context(), Invocation{Name: "git_staged_diff_for_paths", Arguments: `{"paths":["two.txt"],"max_bytes":4096,"max_lines":200}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "diff --git a/two.txt b/two.txt") || !strings.Contains(result.Content, "+new two") {
		t.Fatalf("selected patch missing two.txt diff:\n%s", result.Content)
	}
	if strings.Contains(result.Content, "one.txt") || strings.Contains(result.Content, "+new one") {
		t.Fatalf("selected patch leaked one.txt diff:\n%s", result.Content)
	}
}

func TestSkillsReadToolReadsDiscoveredSkillFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	skillDir := filepath.Join(dir, ".agents", "skills", "change-writer")
	mustWriteFile(t, filepath.Join(skillDir, "SKILL.md"), "---\nname: change-writer\ndescription: Draft change summaries.\n---\n\nUse evidence.\n")
	mustWriteFile(t, filepath.Join(skillDir, "references", "style.md"), "Prefer concise subjects.\n")
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := skillctx.Discover(skillctx.Options{RepoRoot: dir, WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if store.Len() != 1 {
		t.Fatalf("skills = %d, want 1", store.Len())
	}
	locator := store.Skills()[0].Locator
	registry := NewRegistryWithSkills(repo, store)
	defs := registry.Definitions(SkillToolNames())
	if len(defs) != 1 || defs[0].Name != "skills_read" || !defs[0].Strict {
		t.Fatalf("skill defs = %#v", defs)
	}
	required, ok := defs[0].Schema["required"].([]string)
	if !ok || strings.Join(required, ",") != "max_bytes,max_lines,path,source_locator" {
		t.Fatalf("skills_read required fields = %#v", defs[0].Schema["required"])
	}
	properties, ok := defs[0].Schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("skills_read properties = %#v", defs[0].Schema["properties"])
	}
	sourceLocator, ok := properties["source_locator"].(map[string]any)
	if !ok {
		t.Fatalf("skills_read source_locator = %#v", properties["source_locator"])
	}
	locators, ok := sourceLocator["enum"].([]string)
	if !ok || len(locators) != 1 || locators[0] != locator {
		t.Fatalf("skills_read source_locator enum = %#v, want [%q]", sourceLocator["enum"], locator)
	}

	result, err := registry.Execute(t.Context(), Invocation{Name: "skills_read", Arguments: `{"source_locator":"` + locator + `","path":"SKILL.md","max_bytes":4096,"max_lines":200}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Use evidence.") {
		t.Fatalf("skill read missing SKILL.md content:\n%s", result.Content)
	}
	result, err = registry.Execute(t.Context(), Invocation{Name: "skills_read", Arguments: `{"source_locator":"` + locator + `","path":"references/style.md","max_bytes":4096,"max_lines":200}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Prefer concise subjects.") {
		t.Fatalf("skill read missing reference content:\n%s", result.Content)
	}
}

func TestSkillsReadToolIsNotRegisteredWithoutDiscoveredSkills(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := skillctx.Discover(skillctx.Options{
		RepoRoot:  dir,
		WorkDir:   dir,
		Home:      "",
		CodexHome: "",
		AdminRoot: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if store.Len() != 0 {
		t.Fatalf("skills = %d, want 0", store.Len())
	}

	defs := NewRegistryWithSkills(repo, store).Definitions(SkillToolNames())
	encoded, err := json.Marshal(defs)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("serialized empty skill definitions = %s, want []", encoded)
	}
}

func TestSkillsReadToolRejectsUnknownAndEscapingPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	skillDir := filepath.Join(dir, ".agents", "skills", "change-writer")
	mustWriteFile(t, filepath.Join(skillDir, "SKILL.md"), "---\nname: change-writer\ndescription: Draft change summaries.\n---\n")
	mustWriteFile(t, filepath.Join(skillDir, "scripts", "helper.sh"), "#!/bin/sh\n")
	mustWriteFile(t, filepath.Join(skillDir, "references", "binary.bin"), "ok\x00no\n")
	mustWriteFile(t, filepath.Join(dir, "secret.txt"), "secret\n")
	if err := os.Symlink(filepath.Join(dir, "secret.txt"), filepath.Join(skillDir, "secret-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "scripts", "helper.sh"), filepath.Join(skillDir, "references", "helper-link")); err != nil {
		t.Fatal(err)
	}
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := skillctx.Discover(skillctx.Options{RepoRoot: dir, WorkDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	locator := store.Skills()[0].Locator
	registry := NewRegistryWithSkills(repo, store)
	for _, tc := range []struct {
		name string
		args string
	}{
		{name: "unknown locator", args: `{"source_locator":"/missing/SKILL.md"}`},
		{name: "absolute path", args: `{"source_locator":"` + locator + `","path":"/etc/passwd"}`},
		{name: "traversal", args: `{"source_locator":"` + locator + `","path":"../other.md"}`},
		{name: "normalized traversal", args: `{"source_locator":"` + locator + `","path":"references/../SKILL.md"}`},
		{name: "symlink escape", args: `{"source_locator":"` + locator + `","path":"secret-link"}`},
		{name: "reference symlink to non-reference file", args: `{"source_locator":"` + locator + `","path":"references/helper-link"}`},
		{name: "script", args: `{"source_locator":"` + locator + `","path":"scripts/helper.sh"}`},
		{name: "binary", args: `{"source_locator":"` + locator + `","path":"references/binary.bin"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := registry.Execute(t.Context(), Invocation{Name: "skills_read", Arguments: tc.args})
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
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
