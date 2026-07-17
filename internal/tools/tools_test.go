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

func TestDocumentationToolsRegisterOnlyForReviewAndSimplifyRegistry(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"go", "rustup", "ctx7"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)

	names := []string{"go_doc", "rust_doc", "context7_library", "context7_docs"}
	reviewRegistry := NewReviewRegistryWithSkills(nil, nil, ReviewModeCodebase, ReviewScope{}, gitctx.ChangeFingerprint{})
	definitions := reviewRegistry.Definitions(names)
	if len(definitions) != len(names) {
		t.Fatalf("review documentation definitions = %#v", definitions)
	}
	for _, definition := range definitions {
		if !definition.Strict || definition.Schema["additionalProperties"] != false {
			t.Fatalf("definition is not strict: %#v", definition)
		}
	}

	normalRegistry := NewRegistryWithSkills(nil, nil)
	if definitions := normalRegistry.Definitions(names); len(definitions) != 0 {
		t.Fatalf("non-review registry exposes documentation tools: %#v", definitions)
	}
}

func TestDocumentationToolsOmitMissingExecutables(t *testing.T) {
	tests := []struct {
		name        string
		executables []string
		want        []string
	}{
		{name: "none"},
		{name: "go", executables: []string{"go"}, want: []string{"go_doc"}},
		{name: "rustup", executables: []string{"rustup"}, want: []string{"rust_doc"}},
		{name: "ctx7", executables: []string{"ctx7"}, want: []string{"context7_library", "context7_docs"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bin := t.TempDir()
			for _, name := range test.executables {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("fixture"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)

			registry := NewReviewRegistryWithSkills(nil, nil, ReviewModeCodebase, ReviewScope{}, gitctx.ChangeFingerprint{})
			definitions := registry.Definitions([]string{"go_doc", "rust_doc", "context7_library", "context7_docs"})
			got := make([]string, 0, len(definitions))
			for _, definition := range definitions {
				got = append(got, definition.Name)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("documentation tools = %v, want %v", got, test.want)
			}
		})
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
	readFileProperties := defs[1].Schema["properties"].(map[string]any)
	if readFileProperties["with_line_number"].(map[string]any)["type"] != "boolean" {
		t.Fatalf("read_file with_line_number schema = %#v", readFileProperties["with_line_number"])
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
	mustWriteFile(t, filepath.Join(dir, ".git-agent", "search", "cache.json"), "needle\n")
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
	for _, forbidden := range []string{".git-agent/search/cache.json", ".omx/state.json"} {
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
	fingerprint, err := repo.StagedFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	registry := NewReviewRegistryWithSkills(repo, nil, ReviewModeStaged, ReviewScope{}, fingerprint)
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

func TestUncommittedReviewReadFileRoutesNestedHeadSource(t *testing.T) {
	t.Parallel()

	sub := t.TempDir()
	runGit(t, sub, "init")
	runGit(t, sub, "config", "user.name", "Test User")
	runGit(t, sub, "config", "user.email", "test@example.com")
	mustWriteFile(t, filepath.Join(sub, "removed.txt"), "nested base evidence\n")
	runGit(t, sub, "add", "removed.txt")
	runGit(t, sub, "commit", "-m", "base")

	root := t.TempDir()
	runGit(t, root, "init")
	runGit(t, root, "config", "user.name", "Test User")
	runGit(t, root, "config", "user.email", "test@example.com")
	mustWriteFile(t, filepath.Join(root, "app.txt"), "root\n")
	runGit(t, root, "add", "app.txt")
	runGit(t, root, "commit", "-m", "root")
	runGit(t, root, "-c", "protocol.file.allow=always", "submodule", "add", sub, "webui")
	runGit(t, root, "commit", "-m", "add webui")
	if err := os.Remove(filepath.Join(root, "webui", "removed.txt")); err != nil {
		t.Fatal(err)
	}

	repo, err := gitctx.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := repo.UncommittedFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	registry := NewReviewRegistryWithSkills(repo, nil, ReviewModeUncommitted, ReviewScope{}, fingerprint)
	result, err := registry.Execute(t.Context(), Invocation{Name: "read_file", Arguments: `{"path":"webui/removed.txt","source":"head"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "nested base evidence") {
		t.Fatalf("nested head read = %s", result.Content)
	}
	if _, err := registry.Execute(t.Context(), Invocation{Name: "read_file", Arguments: `{"path":"webui/removed.txt","source":"worktree"}`}); err == nil {
		t.Fatal("deleted nested worktree file unexpectedly readable")
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
	registry := NewReviewRegistryWithSkills(nil, nil, ReviewModeUncommitted, scope, gitctx.ChangeFingerprint{})
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

func TestDiffReviewToolsRejectRepositoryDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		mode        ReviewMode
		prepare     func(*testing.T, string)
		fingerprint func(*gitctx.Repository) (gitctx.ChangeFingerprint, error)
		drift       func(*testing.T, string)
	}{
		{
			name: "worktree",
			mode: ReviewModeUncommitted,
			prepare: func(t *testing.T, dir string) {
				mustWriteFile(t, filepath.Join(dir, "app.txt"), "launch\n")
			},
			fingerprint: (*gitctx.Repository).UncommittedFingerprint,
			drift: func(t *testing.T, dir string) {
				mustWriteFile(t, filepath.Join(dir, "app.txt"), "later\n")
			},
		},
		{
			name: "index",
			mode: ReviewModeStaged,
			prepare: func(t *testing.T, dir string) {
				mustWriteFile(t, filepath.Join(dir, "app.txt"), "launch\n")
				runGit(t, dir, "add", "app.txt")
			},
			fingerprint: (*gitctx.Repository).StagedFingerprint,
			drift: func(t *testing.T, dir string) {
				mustWriteFile(t, filepath.Join(dir, "app.txt"), "later\n")
				runGit(t, dir, "add", "app.txt")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			runGit(t, dir, "init")
			runGit(t, dir, "config", "user.name", "Test User")
			runGit(t, dir, "config", "user.email", "test@example.com")
			mustWriteFile(t, filepath.Join(dir, "app.txt"), "base\n")
			runGit(t, dir, "add", "app.txt")
			runGit(t, dir, "commit", "-m", "base")
			test.prepare(t, dir)

			repo, err := gitctx.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			fingerprint, err := test.fingerprint(repo)
			if err != nil {
				t.Fatal(err)
			}
			registry := NewReviewRegistryWithSkills(repo, nil, test.mode, ReviewScope{}, fingerprint)
			test.drift(t, dir)

			_, err = registry.Execute(t.Context(), Invocation{Name: "read_file", Arguments: `{"path":"app.txt"}`})
			if !errors.Is(err, gitctx.ErrChangeSnapshotStale) {
				t.Fatalf("read_file error = %v, want stale snapshot", err)
			}
		})
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
	for _, name := range []string{"read_file", "inspect_file"} {
		if _, err := registry.Execute(t.Context(), Invocation{Name: name, Arguments: `{"path":"leak.txt"}`}); err == nil {
			t.Fatalf("%s followed symlink outside repository", name)
		}
		if _, err := registry.Execute(t.Context(), Invocation{Name: name, Arguments: `{"path":"."}`}); err == nil {
			t.Fatalf("%s accepted a directory", name)
		}
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
	result, err := registry.Execute(t.Context(), Invocation{Name: "read_file", Arguments: `{"path":"app.txt","source":"worktree","line_start":2,"line_end":3,"with_line_number":true}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"content": "     2\tworktree\n     3\tthree\n"`) {
		t.Fatalf("numbered content = %s", result.Content)
	}
}

func TestInspectFileReportsMetadataAndUsesReadFilePolicy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	mustWriteFile(t, filepath.Join(dir, "app.go"), "package app\n\ntype Service struct{}\n\nfunc (Service) Run() {}\n")
	runGit(t, dir, "add", "app.go")
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := repo.StagedFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	registry := NewReviewRegistryWithSkills(repo, nil, ReviewModeStaged, ReviewScope{}, fingerprint)
	result, err := registry.Execute(t.Context(), Invocation{Name: "inspect_file", Arguments: `{"path":"app.go"}`})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"source": "index"`, `"size_bytes": 60`, `"lines": 5`, `"outline_kind": "code"`, `"kind": "type"`, `"name": "Service"`, `"name": "Service.Run"`} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("inspect_file missing %s: %s", want, result.Content)
		}
	}
	if _, err := registry.Execute(t.Context(), Invocation{Name: "inspect_file", Arguments: `{"path":"app.go","source":"worktree"}`}); err == nil {
		t.Fatal("inspect_file accepted worktree source in staged mode")
	}
}

func TestInspectFileBoundsContentAndHandlesLongMarkdownLines(t *testing.T) {
	t.Parallel()

	content, size, lines, truncated, err := inspectContent(t.Context(), strings.NewReader(strings.Repeat("x", inspectMaxContent)+"\n# End\n\x00"), true)
	if err != nil {
		t.Fatal(err)
	}
	if size != inspectMaxContent+8 || lines != 3 || !truncated || len(content) != inspectMaxContent {
		t.Fatalf("inspectContent = size %d, lines %d, truncated %v, retained %d", size, lines, truncated, len(content))
	}
	outline, outlineTruncated := markdownOutline([]byte(strings.Repeat("x", 70_000) + "\n# End\n"))
	if outlineTruncated || len(outline) != 1 || outline[0].Name != "End" || outline[0].Line != 2 {
		t.Fatalf("markdown outline = %#v, truncated %v", outline, outlineTruncated)
	}
}

func TestInspectFileOutlinesJSONAndUnsupportedFiles(t *testing.T) {
	t.Parallel()

	outline, truncated := jsonOutline([]byte(`{"items":[{"name":"x"}]}`))
	if truncated || len(outline) != 3 || outline[0].Kind != "array" || outline[0].Name != "/items" || outline[1].Kind != "object" || outline[1].Name != "/items/0" || outline[2].Kind != "string" || outline[2].Name != "/items/0/name" {
		t.Fatalf("json outline = %#v, truncated %v", outline, truncated)
	}
	outline, truncated = inspectOutline(fileOutlineKind("notes.txt"), "notes.txt", []byte("plain text\n"))
	if truncated || len(outline) != 0 || fileOutlineKind("notes.txt") != "none" {
		t.Fatalf("text outline = %#v, truncated %v", outline, truncated)
	}
}

func TestInspectFileBoundsOutlineAndSkipsFencedMarkdown(t *testing.T) {
	t.Parallel()

	var document strings.Builder
	for range 100 {
		document.WriteString(`{"`)
		document.WriteString(strings.Repeat("x", 1024))
		document.WriteString(`":`)
	}
	document.WriteString(`null`)
	document.WriteString(strings.Repeat("}", 100))
	outline, truncated := jsonOutline([]byte(document.String()))
	encoded, err := json.Marshal(outline)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(encoded) > inspectMaxOutlineBytes+10_000 {
		t.Fatalf("JSON outline bytes = %d, entries = %d, truncated = %v", len(encoded), len(outline), truncated)
	}

	outline, truncated = markdownOutline([]byte("# Before\n```go\n# Example\n```\n~~~\n# Also example\n~~~\n# After\n"))
	if truncated || len(outline) != 2 || outline[0].Name != "Before" || outline[1].Name != "After" {
		t.Fatalf("Markdown outline = %#v, truncated = %v", outline, truncated)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, _, _, err := inspectContent(ctx, strings.NewReader("content"), true); !errors.Is(err, context.Canceled) {
		t.Fatalf("inspectContent error = %v, want context cancellation", err)
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
	gitArgs := append([]string{"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false", "-c", "tag.forceSignAnnotated=false"}, args...)
	cmd := exec.Command("git", gitArgs...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
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
