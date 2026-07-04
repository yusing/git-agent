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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yusing/git-agent/internal/tasks/commitmsg"
)

func TestSearchEndToEndIndexesRanksAndReplays(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	useGeneralEmbeddingProvider(t)
	runGit(t, root, "init")
	if err := os.MkdirAll(filepath.Join(root, "internal", "cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "internal", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(root, "internal", "cli", "app.go"), `package cli

func runSearch() {
	// semantic search flags are parsed here
	_ = "search parser target"
}
`)
	writeFixtureFile(t, filepath.Join(root, "internal", "agent", "runner.go"), `package agent

func enforceBudget() {
	_ = "tool call budget enforcement"
}
`)
	writeFixtureFile(t, filepath.Join(root, "README.md"), "release note docs\n")
	t.Chdir(root)

	server, stats := newSearchEmbeddingsServer(t)
	defer server.Close()
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_BASE_URL", server.URL)

	first := runSearchE2E(t, []string{"search", "--min-relatedness", "0.70", "where are search flags parsed"})
	if first.Source.Root != root {
		t.Fatalf("search root = %q, want %q", first.Source.Root, root)
	}
	assertSearchResult(t, first, "internal/cli/app.go:", "search parser target")
	if first.Retrieval.Index != "miss" || first.Replay.Mode != "none" {
		t.Fatalf("first retrieval/replay = %#v %#v", first.Retrieval, first.Replay)
	}
	if first.Retrieval.Dimensions != 1024 {
		t.Fatalf("first dimensions = %d", first.Retrieval.Dimensions)
	}
	if got := stats.calls(); got != 2 {
		t.Fatalf("first run embedding calls = %d, want chunks batch + query", got)
	}

	second := runSearchE2E(t, []string{"search", "--min-relatedness", "0.70", "where are search flags parsed"})
	assertSearchResult(t, second, "internal/cli/app.go:", "search parser target")
	if second.Retrieval.Index != "hit" || second.Replay.Mode != "hit" {
		t.Fatalf("second retrieval/replay = %#v %#v", second.Retrieval, second.Replay)
	}
	if got := stats.calls(); got != 2 {
		t.Fatalf("second exact query should reuse chunks and cached query embedding; calls = %d", got)
	}

	similar := runSearchE2E(t, []string{"search", "--min-relatedness", "0.70", "find the search flag parser"})
	assertSearchResult(t, similar, "internal/cli/app.go:", "search parser target")
	if similar.Replay.Mode != "similar" || similar.Replay.From == nil || *similar.Replay.From != "where are search flags parsed" {
		t.Fatalf("similar replay = %#v", similar.Replay)
	}

	writeFixtureFile(t, filepath.Join(root, "internal", "cli", "app.go"), `package cli

func runSearch() {
	// semantic search flags are parsed here
	_ = "fresh indexed marker"
}
`)
	changed := runSearchE2E(t, []string{"search", "--min-relatedness", "0.70", "where are search flags parsed"})
	assertSearchResult(t, changed, "internal/cli/app.go:", "fresh indexed marker")
	if changed.Replay.Mode != "hit" {
		t.Fatalf("changed replay = %#v", changed.Replay)
	}
	if got := stats.calls(); got != 4 {
		t.Fatalf("changed run should re-embed changed chunks and reuse cached query embedding; calls = %d", got)
	}

	manifests, err := filepath.Glob(filepath.Join(projectMetadataDir(t, root), "search", "fs", "*", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) == 0 {
		t.Fatal("search index manifest was not written")
	}
	vectorFiles, err := filepath.Glob(filepath.Join(projectMetadataDir(t, root), "search", "fs", "*", "vectors.f32"))
	if err != nil {
		t.Fatal(err)
	}
	if len(vectorFiles) == 0 {
		t.Fatal("search binary vector cache was not written")
	}
	if _, err := os.Stat(filepath.Join(projectMetadataDir(t, root), "sessions")); !os.IsNotExist(err) {
		t.Fatalf("search should not create model sessions, stat err = %v", err)
	}
}

func TestSearchRealProviderEndToEndOnThisRepo(t *testing.T) {
	if strings.ToLower(strings.TrimSpace(os.Getenv("GIT_AGENT_SEARCH_E2E_PROVIDER"))) != "real" {
		t.Skip("set GIT_AGENT_SEARCH_E2E_PROVIDER=real to run real embeddings e2e")
	}
	if os.Getenv("OPENAI_EMBEDDING_API_KEY") == "" && os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("skipping real search e2e: neither OPENAI_EMBEDDING_API_KEY nor OPENAI_API_KEY is set")
	}

	sourceRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	runGit(t, root, "init")
	for _, path := range []string{
		"internal/tasks/search/task.go",
		"internal/cli/app.go",
		"README.md",
	} {
		data, err := os.ReadFile(filepath.Join(sourceRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		writeFixtureFile(t, filepath.Join(root, filepath.FromSlash(path)), string(data))
	}
	t.Chdir(root)

	query := "where is semantic search replay history vector cache and chunk ranking implemented"
	first := runSearchE2E(t, []string{"search", "--reindex", "--min-relatedness", "0.55", "--limit", "5", query})
	assertSearchHasRange(t, first, "internal/tasks/search/task.go:")
	if first.Retrieval.Index != "miss" || first.Replay.Mode != "none" {
		t.Fatalf("first retrieval/replay = %#v %#v", first.Retrieval, first.Replay)
	}

	second := runSearchE2E(t, []string{"search", "--min-relatedness", "0.55", "--limit", "5", query})
	assertSearchHasRange(t, second, "internal/tasks/search/task.go:")
	if second.Retrieval.Index != "hit" || second.Replay.Mode != "hit" {
		t.Fatalf("second retrieval/replay = %#v %#v", second.Retrieval, second.Replay)
	}
}

func TestCommitMsgEndToEndWithRealisticFixture(t *testing.T) {
	fixture := buildCommitMsgFixture(t)
	t.Chdir(fixture.repoDir)

	mode, cleanup := configureE2EProvider(t, newScriptedResponsesServer(t, []func(string) string{
		func(body string) string {
			for _, want := range []string{
				`prepared_commit_context`,
				`staged_stats`,
				`previous_head_paths`,
				`previous_head_stats`,
				`previous_head_diff`,
				`diff_truncated`,
				`"git_staged_paths"`,
				`"git_staged_status"`,
				`"git_staged_stat"`,
				`"git_staged_diff"`,
				`"git_recent_commits"`,
				`"read_file"`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("first request missing tool %s\n%s", want, body)
				}
			}
			return responseWithToolCalls("resp_commit_1",
				toolCallSpec{ID: "fc_1", CallID: "call_1", Name: "git_staged_paths", Arguments: `{}`},
				toolCallSpec{ID: "fc_2", CallID: "call_2", Name: "git_staged_status", Arguments: `{}`},
				toolCallSpec{ID: "fc_3", CallID: "call_3", Name: "git_staged_stat", Arguments: `{}`},
				toolCallSpec{ID: "fc_4", CallID: "call_4", Name: "git_staged_diff", Arguments: `{"max_bytes":32768,"max_lines":800}`},
				toolCallSpec{ID: "fc_5", CallID: "call_5", Name: "git_recent_commits", Arguments: `{"limit":6}`},
				toolCallSpec{ID: "fc_6", CallID: "call_6", Name: "read_file", Arguments: `{"path":"internal/route/do_parser.go","max_bytes":8192,"max_lines":260}`},
				toolCallSpec{ID: "fc_7", CallID: "call_7", Name: "read_file", Arguments: `{"path":"internal/route/do_types.go","max_bytes":8192,"max_lines":260}`},
				toolCallSpec{ID: "fc_8", CallID: "call_8", Name: "read_file", Arguments: `{"path":"docs/routing.md","max_bytes":8192,"max_lines":260}`},
			)
		},
		func(body string) string {
			for _, want := range []string{
				`"type":"function_call_output"`,
				`internal/route/do_parser.go`,
				`internal/route/do_types.go`,
				`README.md`,
				`docs/routing.md`,
				`feat(route): add parser for do actions`,
				`refactor(cli): preserve ordered help output`,
				`feat(schema): add route rule structs`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("second request missing %q\n%s", want, body)
				}
			}
			return responseWithText("resp_commit_2", `feat(route): add named do option blocks

Accept braced do-option maps while preserving positional help order and validation, then update parser coverage, README examples, and routing docs so generated messages can rely on formatter-owned wrapping.`)
		},
	}))
	defer cleanup()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"commit-msg"}); err != nil {
		t.Fatal(err)
	}
	output := strings.TrimSpace(stdout.String())
	t.Logf("provider=%s commit-msg output:\n%s", mode, output)
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if errs := commitmsg.Validate(commitmsg.ModeNormal, output); len(errs) > 0 {
		t.Fatalf("commit message validation failed: %v\n%s", errs, output)
	}
	if mode == providerModeFake && !strings.Contains(output, "feat(route): add named do option blocks") {
		t.Fatalf("unexpected fake-provider commit message:\n%s", output)
	}
	for _, line := range strings.Split(output, "\n")[2:] {
		if len(line) > 72 {
			t.Fatalf("body line too long after shaping: %d %q\n%s", len(line), line, output)
		}
	}
	assertTraceArtifacts(t, fixture.repoDir, "*-commit-msg", 1)
}

func TestCommitMsgSubmoduleOnlySkipsProviderAuth(t *testing.T) {
	fixture := buildStagedSubmoduleOnlyFixture(t, "feat: add webui submodule")
	t.Chdir(fixture.repoDir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "")
	legacyMarker := filepath.Join(fixture.repoDir, ".git-agent", "search", "legacy.txt")
	if err := os.MkdirAll(filepath.Dir(legacyMarker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyMarker, []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"commit-msg"}); err != nil {
		t.Fatal(err)
	}
	output := strings.TrimSpace(stdout.String())
	want := `chore(deps): update webui submodule

webui
  - ` + fixture.submoduleReleaseSHA[:7] + `: fix(webui): refresh login`
	if output != want {
		t.Fatalf("output:\n%s\nwant:\n%s", output, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(projectMetadataDir(t, fixture.repoDir), "sessions")); !os.IsNotExist(err) {
		t.Fatalf("deterministic commit-msg should not create trace sessions, stat err = %v", err)
	}
	migratedMarker := filepath.Join(projectMetadataDir(t, fixture.repoDir), "search", "legacy.txt")
	if data, err := os.ReadFile(migratedMarker); err != nil || string(data) != "legacy\n" {
		t.Fatalf("legacy metadata marker = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.repoDir, ".git-agent")); !os.IsNotExist(err) {
		t.Fatalf("legacy metadata dir stat err = %v, want migrated", err)
	}
}

func TestCommitSubmoduleOnlySkipsProviderAuthAndCreatesCommit(t *testing.T) {
	fixture := buildStagedSubmoduleOnlyFixture(t, "Add webui submodule")
	t.Chdir(fixture.repoDir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"commit"}); err != nil {
		t.Fatal(err)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Update webui submodule") {
		t.Fatalf("stdout missing git commit summary:\n%s", stdout.String())
	}

	want := `Update webui submodule

webui
  - ` + fixture.submoduleReleaseSHA[:7] + `: fix(webui): refresh login`
	if got := gitHeadMessage(t, fixture.repoDir); got != want {
		t.Fatalf("HEAD message:\n%s\nwant:\n%s", got, want)
	}
	if _, err := os.Stat(filepath.Join(projectMetadataDir(t, fixture.repoDir), "sessions")); !os.IsNotExist(err) {
		t.Fatalf("deterministic commit should not create trace sessions, stat err = %v", err)
	}
}

func TestCommitMsgAmendEndToEndWithRealisticFixture(t *testing.T) {
	fixture := buildCommitMsgAmendFixture(t)
	t.Chdir(fixture.repoDir)

	mode, cleanup := configureE2EProvider(t, newScriptedResponsesServer(t, []func(string) string{
		func(body string) string {
			for _, want := range []string{
				`prepared_amend_context`,
				`original_head_message`,
				`final_diff`,
				`head_diff`,
				`head_paths`,
				`staged_paths`,
				`internal/agent/verify.go`,
				`docs/verify.md`,
				`fix(agent): persist verified providers`,
				`"git_final_amended_diff"`,
				`"git_head_show"`,
				`"git_diff_against_parent"`,
				`"git_amend_delta"`,
				`"git_recent_commits"`,
				`"read_file"`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("first request missing tool %s\n%s", want, body)
				}
			}
			return responseWithToolCalls("resp_amend_1",
				toolCallSpec{ID: "fc_1", CallID: "call_1", Name: "git_final_amended_diff", Arguments: `{"max_bytes":32768,"max_lines":900}`},
				toolCallSpec{ID: "fc_2", CallID: "call_2", Name: "git_head_show", Arguments: `{"max_bytes":16384,"max_lines":500}`},
				toolCallSpec{ID: "fc_3", CallID: "call_3", Name: "git_diff_against_parent", Arguments: `{"max_bytes":16384,"max_lines":500}`},
				toolCallSpec{ID: "fc_4", CallID: "call_4", Name: "git_amend_delta", Arguments: `{"max_bytes":16384,"max_lines":500}`},
				toolCallSpec{ID: "fc_5", CallID: "call_5", Name: "git_recent_commits", Arguments: `{"limit":6}`},
				toolCallSpec{ID: "fc_6", CallID: "call_6", Name: "read_file", Arguments: `{"path":"internal/agent/verify.go","max_bytes":8192,"max_lines":260}`},
			)
		},
		func(body string) string {
			for _, want := range []string{
				`"type":"function_call_output"`,
				`internal/agent/verify.go`,
				`fix(agent): persist verified providers`,
				`feat(agent): verify provider config`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("second request missing %q\n%s", want, body)
				}
			}
			return responseWithText("resp_amend_2", `feat(agent): persist verified providers on verify

Store verified providers in config and suppress the matching reload while the
write is in flight.

Return the updated provider list and refresh API docs for the new verify
response shape.`)
		},
		func(body string) string {
			for _, want := range []string{
				`Repair the output`,
				`preserve original HEAD subject`,
				`fix(agent): persist verified providers`,
				`feat(agent): persist verified providers on verify`,
				`"type":"function_call_output"`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("repair request missing %q\n%s", want, body)
				}
			}
			return responseWithText("resp_amend_3", `fix(agent): persist verified providers

Store verified providers in config and suppress the matching reload while the
write is in flight.

Return the updated provider list and refresh API docs for the new verify
response shape.`)
		},
	}))
	defer cleanup()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"commit-msg", "--amend"}); err != nil {
		t.Fatal(err)
	}
	output := strings.TrimSpace(stdout.String())
	t.Logf("provider=%s commit-msg --amend output:\n%s", mode, output)
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if errs := commitmsg.Validate(commitmsg.ModeAmend, output); len(errs) > 0 {
		t.Fatalf("commit amend validation failed: %v\n%s", errs, output)
	}
	if mode == providerModeFake && strings.Contains(strings.ToLower(output), "also") {
		t.Fatalf("fake-provider amend output regressed to delta narration:\n%s", output)
	}
	if mode == providerModeFake && !strings.HasPrefix(output, "fix(agent): persist verified providers\n") {
		t.Fatalf("fake-provider amend output did not preserve original subject:\n%s", output)
	}
	minToolCalls := 1
	if mode == providerModeReal {
		// Prepared amend context should be sufficient for a real model to finish
		// without extra tools; the fake provider path still exercises tool
		// round-trips.
		minToolCalls = 0
	}
	assertTraceArtifacts(t, fixture.repoDir, "*-commit-msg", minToolCalls)
}

func TestPRMessageEndToEndWithRealisticFixture(t *testing.T) {
	fixture := buildPRMessageFixture(t)
	t.Chdir(fixture.repoDir)

	mode, cleanup := configureE2EProvider(t, newScriptedResponsesServer(t, []func(string) string{
		func(body string) string {
			for _, want := range []string{
				`prepared_pr_context`,
				`origin/HEAD`,
				`internal/auth/policy.go`,
				`internal/auth/session.go`,
				`docs/policies.md`,
				`feat(auth): include decision reasons in policy checks`,
				`test(auth): cover strict session policy outcomes`,
				`+type DecisionReason string`,
				`+func EvaluateSessionPolicy`,
				`<command>pr-message</command>`,
				`<mode>origin/HEAD..HEAD</mode>`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("first request missing %s\n%s", want, body)
				}
			}
			if strings.Contains(body, `"git_pr_`) || strings.Contains(body, `"read_file"`) {
				t.Fatalf("pr-message request should not expose tools\n%s", body)
			}
			return responseWithText("resp_pr_1", `feat(auth): add strict session policy reasons

Return structured allow/deny reasons from policy evaluation and apply them to
session checks.

Document the strict-mode outcomes and cover denied, expired, and missing-session
cases in tests.`)
		},
	}))
	defer cleanup()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	args := []string{"pr-message"}
	if mode == providerModeReal {
		args = append(args, "--timeout", "6m")
	}
	start := time.Now()
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	output := strings.TrimSpace(stdout.String())
	t.Logf("provider=%s pr-message elapsed=%s output:\n%s", mode, elapsed, output)
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if errs := commitmsg.Validate(commitmsg.ModePR, output); len(errs) > 0 {
		t.Fatalf("pr message validation failed: %v\n%s", errs, output)
	}
	if mode == providerModeFake && !strings.Contains(output, "feat(auth): add strict session policy reasons") {
		t.Fatalf("unexpected fake-provider pr message:\n%s", output)
	}
	assertTraceArtifacts(t, fixture.repoDir, "*-pr-message", 0)
}

func TestReleaseNoteEndToEndWithRealisticFixture(t *testing.T) {
	fixture := buildReleaseNoteFixture(t)
	t.Chdir(fixture.repoDir)

	modelJSON := fmt.Sprintf(`{
  "sections": [
    {
      "heading": "Breaking Changes",
      "bullets": [
        {
          "label": null,
          "summary": "Route configuration and API payloads no longer support path_patterns",
          "refs": [{"type":"commit","value":"%s"}]
        }
      ]
    },
    {
      "heading": "Bug Fixes",
      "bullets": [
        {
          "label": "Core/Middleware",
          "summary": "FileServer routes now apply middleware after routing rules settle, preserving operator-visible handler behavior for static file routes",
          "refs": [{"type":"commit","value":"%s"}]
        }
      ]
    },
    {
      "heading": "Improvements",
      "bullets": [
        {
          "label": null,
          "summary": "Web UI docs and screenshots now align better with current operator flows",
          "refs": [{"type":"commit","value":"%s"}]
        }
      ]
    }
  ]
}`, fixture.parentRefactorSHA, fixture.parentFixSHA, fixture.webuiReleaseSHA)

	mode, cleanup := configureE2EProvider(t, newScriptedResponsesServer(t, []func(string) string{
		func(body string) string {
			var payload map[string]any
			if err := json.Unmarshal([]byte(body), &payload); err != nil {
				t.Fatalf("request body is not valid json: %v\n%s", err, body)
			}
			text, ok := payload["text"].(map[string]any)
			if !ok {
				t.Fatalf("request missing text config\n%s", body)
			}
			format, ok := text["format"].(map[string]any)
			if !ok {
				t.Fatalf("request missing text.format\n%s", body)
			}
			if format["type"] != "json_schema" {
				t.Fatalf("text.format.type = %#v\n%s", format["type"], body)
			}
			if format["name"] != "release_note" {
				t.Fatalf("text.format.name = %#v\n%s", format["name"], body)
			}
			for _, want := range []string{
				`"repo_summary"`,
				`\"required_submodule_groups\": [`,
				`\"goutils\"`,
				`\"webui\"`,
				`\"parent_commits\": [`,
				`\"sha\": \"` + fixture.parentFixSHA,
				`\"path\": \"webui\"`,
				`\"path\": \"goutils\"`,
				`\"release_sha\": \"` + fixture.webuiReleaseSHA,
				`\"release_sha\": \"` + fixture.goutilsReleaseSHA,
				`\"group_heading\": \"[**webui**](https://github.com/example/webui)\"`,
				`\"group_heading\": \"[**goutils**](https://github.com/example/goutils)\"`,
				`\"local_history_available\": true`,
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("first request missing %q\n%s", want, body)
				}
			}
			if strings.Contains(body, `"resolve_ref"`) ||
				strings.Contains(body, `"git_log_range"`) ||
				strings.Contains(body, `"submodule_log_range"`) ||
				strings.Contains(body, `"submodule_gitlink_range"`) ||
				strings.Contains(body, `"gitmodules_table"`) {
				t.Fatalf("first request should not expose legacy release-note tools\n%s", body)
			}
			return responseWithText("resp_release_1", modelJSON)
		},
	}))
	defer cleanup()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), []string{"release-note", fixture.baseTag, fixture.releaseTag}); err != nil {
		t.Fatal(err)
	}
	output := strings.TrimSpace(stdout.String())
	t.Logf("provider=%s release-note output:\n%s", mode, output)
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if mode == providerModeFake {
		for _, want := range []string{
			"### Breaking Changes",
			"- Route configuration and API payloads no longer support path_patterns (" + fixture.parentRefactorSHA[:7] + ")",
			"### Bug Fixes",
			"- **Core/Middleware**: FileServer routes now apply middleware after routing rules settle, preserving operator-visible handler behavior for static file routes (" + fixture.parentFixSHA[:7] + ")",
			"### Improvements",
			"- Web UI docs and screenshots now align better with current operator flows (https://github.com/example/webui/commit/" + fixture.webuiReleaseSHA + ")",
			"### Full Changelog",
			"[**webui**](https://github.com/example/webui)",
			"[**goutils**](https://github.com/example/goutils)",
			fixture.parentRefactorSHA[:7],
			fixture.parentFixSHA[:7],
			fixture.goutilsReleaseSHA[:7],
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("fake-provider release note missing %q:\n%s", want, output)
			}
		}
	}
	assertTraceArtifacts(t, fixture.repoDir, "*-release-note", 0)
}

type providerMode string

const (
	providerModeFake providerMode = "fake"
	providerModeReal providerMode = "real"
)

type searchE2EOutput struct {
	Source struct {
		Mode string `json:"mode"`
		Root string `json:"root"`
	} `json:"source"`
	Retrieval struct {
		Index      string `json:"index"`
		Dimensions int    `json:"dimensions"`
	} `json:"retrieval"`
	Results []struct {
		Relatedness float64 `json:"relatedness"`
		Range       string  `json:"range"`
		Excerpt     string  `json:"excerpt"`
	} `json:"results"`
	Replay struct {
		Mode string  `json:"mode"`
		From *string `json:"from"`
	} `json:"replay"`
}

type searchEmbeddingStats struct {
	mu    sync.Mutex
	count int
}

func (s *searchEmbeddingStats) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func (s *searchEmbeddingStats) addCall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
}

func runSearchE2E(t *testing.T, args []string) searchE2EOutput {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := &App{stdout: &stdout, stderr: &stderr}
	if err := app.Run(t.Context(), args); err != nil {
		t.Fatalf("search failed: %v\nstderr:\n%s", err, stderr.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	var out searchE2EOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not search JSON: %q: %v", stdout.String(), err)
	}
	return out
}

func assertSearchResult(t *testing.T, out searchE2EOutput, rangePrefix, excerpt string) {
	t.Helper()

	if out.Source.Mode != "filesystem" {
		t.Fatalf("source = %#v", out.Source)
	}
	if len(out.Results) == 0 {
		t.Fatalf("no search results: %#v", out)
	}
	top := out.Results[0]
	if !strings.HasPrefix(top.Range, rangePrefix) {
		t.Fatalf("top range = %q, want prefix %q", top.Range, rangePrefix)
	}
	if !strings.Contains(top.Excerpt, excerpt) {
		t.Fatalf("top excerpt missing %q:\n%s", excerpt, top.Excerpt)
	}
	if top.Relatedness <= 0 || top.Relatedness > 1 {
		t.Fatalf("relatedness = %v", top.Relatedness)
	}
}

func assertSearchHasRange(t *testing.T, out searchE2EOutput, rangePrefix string) {
	t.Helper()

	if len(out.Results) == 0 {
		t.Fatalf("no search results: %#v", out)
	}
	for _, result := range out.Results {
		if strings.HasPrefix(result.Range, rangePrefix) {
			if result.Relatedness <= 0 || result.Relatedness > 1 {
				t.Fatalf("relatedness = %v", result.Relatedness)
			}
			return
		}
	}
	t.Fatalf("results do not include range prefix %q: %#v", rangePrefix, out.Results)
}

func newSearchEmbeddingsServer(t *testing.T) (*httptest.Server, *searchEmbeddingStats) {
	t.Helper()

	stats := &searchEmbeddingStats{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stats.addCall()
		if r.URL.Path != "/embeddings" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		var payload struct {
			Input      any    `json:"input"`
			Model      string `json:"model"`
			Dimensions int    `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Dimensions != 1024 {
			t.Fatalf("dimensions = %d", payload.Dimensions)
		}
		inputs := embeddingInputs(t, payload.Input)
		data := make([]map[string]any, len(inputs))
		for i, input := range inputs {
			data[i] = map[string]any{
				"object":    "embedding",
				"index":     i,
				"embedding": searchE2EVector(input, payload.Dimensions),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"model":  payload.Model,
			"data":   data,
			"usage":  map[string]any{"prompt_tokens": 1, "total_tokens": 1},
		}); err != nil {
			t.Fatal(err)
		}
	}))
	return server, stats
}

func embeddingInputs(t *testing.T, input any) []string {
	t.Helper()

	switch value := input.(type) {
	case string:
		return []string{value}
	case []any:
		inputs := make([]string, 0, len(value))
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				t.Fatalf("embedding input item = %T, want string", item)
			}
			inputs = append(inputs, text)
		}
		return inputs
	default:
		t.Fatalf("embedding input = %T, want string or []string", input)
		return nil
	}
}

func searchE2EVector(input string, dimensions int) []float64 {
	vector := make([]float64, dimensions)
	input = strings.ToLower(input)
	switch {
	case strings.Contains(input, "search") || strings.Contains(input, "parser") || strings.Contains(input, "flag"):
		vector[0] = 1
	case strings.Contains(input, "budget"):
		if dimensions > 1 {
			vector[1] = 1
		}
	case strings.Contains(input, "release"):
		if dimensions > 2 {
			vector[2] = 1
		}
	default:
		vector[0] = -1
	}
	return vector
}

func configureE2EProvider(t *testing.T, fakeServer *httptest.Server) (providerMode, func()) {
	t.Helper()

	switch mode := normalizeProviderMode(os.Getenv("GIT_AGENT_E2E_PROVIDER")); mode {
	case providerModeFake:
		t.Setenv("HOME", t.TempDir())
		t.Setenv("OPENAI_API_KEY", "test-key")
		t.Setenv("OPENAI_BASE_URL", fakeServer.URL)
		t.Setenv("OPENAI_MODEL", "test-model")
		return mode, fakeServer.Close
	case providerModeReal:
		fakeServer.Close()
		requireRealProviderEnv(t)
		t.Logf("using real provider base_url=%s model=%s", os.Getenv("OPENAI_BASE_URL"), os.Getenv("OPENAI_MODEL"))
		return mode, func() {}
	default:
		t.Fatalf("unsupported GIT_AGENT_E2E_PROVIDER=%q", os.Getenv("GIT_AGENT_E2E_PROVIDER"))
		return "", func() {}
	}
}

func normalizeProviderMode(raw string) providerMode {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "fake":
		return providerModeFake
	case "real", "live":
		return providerModeReal
	default:
		return providerMode(raw)
	}
}

func requireRealProviderEnv(t *testing.T) {
	t.Helper()

	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("skipping real-provider e2e: OPENAI_API_KEY is not set")
	}
	if os.Getenv("OPENAI_MODEL") == "" {
		t.Skip("skipping real-provider e2e: OPENAI_MODEL is not set")
	}
}

type commitMsgFixture struct {
	repoDir string
}

type stagedSubmoduleOnlyFixture struct {
	repoDir             string
	submoduleReleaseSHA string
}

func buildCommitMsgFixture(t *testing.T) commitMsgFixture {
	t.Helper()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.name", "Test User")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/example/route-agent.git")

	writeFixtureFile(t, filepath.Join(repoDir, "README.md"), strings.Join([]string{
		"# Route Agent",
		"",
		"CLI helpers for route parsing, validation, and generated cheatsheets.",
		"",
		"## Actions",
		"",
		"- `notify target template`",
		"- `pass upstream timeout`",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "internal", "route", "do_parser.go"), strings.Join([]string{
		"package route",
		"",
		"type DoCommand struct {",
		"\tName string",
		"\tArgs []string",
		"}",
		"",
		"func ParseDo(tokens []string) DoCommand {",
		"\treturn DoCommand{Name: tokens[0], Args: tokens[1:]}",
		"}",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "internal", "route", "do_types.go"), strings.Join([]string{
		"package route",
		"",
		"type DoArg struct {",
		"\tName string",
		"\tHelp string",
		"}",
		"",
		"type DoHelp struct {",
		"\tCommand string",
		"\tArgs    []DoArg",
		"}",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "docs", "routing.md"), strings.Join([]string{
		"# Routing",
		"",
		"Route actions currently use positional arguments.",
		"",
		"```yaml",
		"do: notify ops '#deploys'",
		"```",
	}, "\n"))
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "feat(route): add parser for do actions")

	writeFixtureFile(t, filepath.Join(repoDir, "internal", "cli", "help_order.go"), strings.Join([]string{
		"package cli",
		"",
		"var helpOrder = []string{\"notify\", \"pass\", \"bypass\"}",
		"",
		"func orderedHelp() []string {",
		"\treturn append([]string(nil), helpOrder...)",
		"}",
	}, "\n"))
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "refactor(cli): preserve ordered help output")

	writeFixtureFile(t, filepath.Join(repoDir, "internal", "route", "schema.go"), strings.Join([]string{
		"package route",
		"",
		"type Rule struct {",
		"\tName string `json:\"name\"`",
		"\tDo   []DoCommand `json:\"do\"`",
		"}",
	}, "\n"))
	runGit(t, repoDir, "add", "internal/route/schema.go")
	runGit(t, repoDir, "commit", "-m", "feat(schema): add route rule structs")

	writeFixtureFile(t, filepath.Join(repoDir, "docs", "routing.md"), strings.Join([]string{
		"# Routing",
		"",
		"Named routes and action docs.",
		"",
		"Rules may use multiline action blocks in YAML examples.",
	}, "\n"))
	runGit(t, repoDir, "add", "docs/routing.md")
	runGit(t, repoDir, "commit", "-m", "docs(actions): expand route action guide")

	writeFixtureFile(t, filepath.Join(repoDir, "internal", "route", "do_parser.go"), strings.Join([]string{
		"package route",
		"",
		"type DoOptionBlock struct {",
		"\tName   string",
		"\tValues map[string]string",
		"}",
		"",
		"type DoCommand struct {",
		"\tName   string",
		"\tArgs   []string",
		"\tBlock  *DoOptionBlock",
		"}",
		"",
		"func ParseDo(tokens []string) DoCommand {",
		"\treturn DoCommand{Name: tokens[0], Args: tokens[1:]}",
		"}",
		"",
		"func ParseDoBlock(name string, values map[string]string) DoCommand {",
		"\treturn DoCommand{Name: name, Block: &DoOptionBlock{Name: name, Values: values}}",
		"}",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "internal", "route", "do_types.go"), strings.Join([]string{
		"package route",
		"",
		"type DoArg struct {",
		"\tName     string",
		"\tHelp     string",
		"\tRequired bool",
		"}",
		"",
		"type DoHelp struct {",
		"\tCommand string",
		"\tArgs    []DoArg",
		"}",
		"",
		"func notifyHelp() DoHelp {",
		"\treturn DoHelp{",
		"\t\tCommand: \"notify\",",
		"\t\tArgs: []DoArg{{Name: \"target\", Help: \"who to notify\", Required: true}, {Name: \"template\", Help: \"message template\", Required: true}},",
		"\t}",
		"}",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "README.md"), strings.Join([]string{
		"# Route Agent",
		"",
		"CLI helpers for route parsing, validation, and generated cheatsheets.",
		"",
		"## Actions",
		"",
		"- `notify target template`",
		"- `pass upstream timeout`",
		"",
		"Supports named do blocks in YAML and documentation examples.",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "docs", "routing.md"), strings.Join([]string{
		"# Routing",
		"",
		"Named routes and action docs.",
		"",
		"Option blocks mirror positional args and stay in the same order as CLI help output.",
		"",
		"```yaml",
		"do:",
		"  notify:",
		"    target: ops",
		"    template: deploy-complete",
		"```",
	}, "\n"))
	runGit(t, repoDir, "add", "internal/route/do_parser.go", "internal/route/do_types.go", "README.md", "docs/routing.md")

	return commitMsgFixture{repoDir: repoDir}
}

func buildStagedSubmoduleOnlyFixture(t *testing.T, parentInitialMessage string) stagedSubmoduleOnlyFixture {
	t.Helper()

	submoduleDir := t.TempDir()
	runGit(t, submoduleDir, "init")
	runGit(t, submoduleDir, "config", "user.name", "Test User")
	runGit(t, submoduleDir, "config", "user.email", "test@example.com")
	writeFixtureFile(t, filepath.Join(submoduleDir, "ui.txt"), "v1\n")
	runGit(t, submoduleDir, "add", "ui.txt")
	runGit(t, submoduleDir, "commit", "-m", "feat(webui): initial")
	baseSHA := gitHead(t, submoduleDir)
	writeFixtureFile(t, filepath.Join(submoduleDir, "ui.txt"), "v2\n")
	runGit(t, submoduleDir, "add", "ui.txt")
	runGit(t, submoduleDir, "commit", "-m", "fix(webui): refresh login")
	releaseSHA := gitHead(t, submoduleDir)

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.name", "Test User")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "-c", "protocol.file.allow=always", "submodule", "add", submoduleDir, "webui")
	runGit(t, filepath.Join(repoDir, "webui"), "checkout", baseSHA)
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", parentInitialMessage)

	runGit(t, filepath.Join(repoDir, "webui"), "checkout", releaseSHA)
	runGit(t, repoDir, "add", "webui")

	return stagedSubmoduleOnlyFixture{repoDir: repoDir, submoduleReleaseSHA: releaseSHA}
}

type prMessageFixture struct {
	repoDir string
}

func buildPRMessageFixture(t *testing.T) prMessageFixture {
	t.Helper()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.name", "Test User")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/example/auth-gateway.git")

	writeFixtureFile(t, filepath.Join(repoDir, "README.md"), strings.Join([]string{
		"# Auth Gateway",
		"",
		"HTTP middleware for tenant session validation and policy checks.",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "internal", "auth", "policy.go"), strings.Join([]string{
		"package auth",
		"",
		"type Decision struct {",
		"\tAllowed bool",
		"}",
		"",
		"func EvaluatePolicy(token string) Decision {",
		"\treturn Decision{Allowed: token != \"\"}",
		"}",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "internal", "auth", "middleware.go"), strings.Join([]string{
		"package auth",
		"",
		"type Request struct {",
		"\tToken string",
		"}",
		"",
		"func Authorize(request Request) Decision {",
		"\treturn EvaluatePolicy(request.Token)",
		"}",
	}, "\n"))
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "feat(auth): add policy evaluator")

	writeFixtureFile(t, filepath.Join(repoDir, "docs", "policies.md"), strings.Join([]string{
		"# Policies",
		"",
		"Session policy checks currently return allow/deny only.",
	}, "\n"))
	runGit(t, repoDir, "add", "docs/policies.md")
	runGit(t, repoDir, "commit", "-m", "docs(auth): describe policy decisions")
	baseSHA := gitHead(t, repoDir)
	runGit(t, repoDir, "update-ref", "refs/remotes/origin/HEAD", baseSHA)

	writeFixtureFile(t, filepath.Join(repoDir, "internal", "auth", "policy.go"), strings.Join([]string{
		"package auth",
		"",
		"type DecisionReason string",
		"",
		"const (",
		"\tDecisionReasonAllowed DecisionReason = \"allowed\"",
		"\tDecisionReasonMissing DecisionReason = \"missing_token\"",
		"\tDecisionReasonExpired DecisionReason = \"expired_session\"",
		")",
		"",
		"type Decision struct {",
		"\tAllowed bool",
		"\tReason  DecisionReason",
		"}",
		"",
		"func EvaluatePolicy(token string) Decision {",
		"\tif token == \"\" {",
		"\t\treturn Decision{Allowed: false, Reason: DecisionReasonMissing}",
		"\t}",
		"\treturn Decision{Allowed: true, Reason: DecisionReasonAllowed}",
		"}",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "internal", "auth", "session.go"), strings.Join([]string{
		"package auth",
		"",
		"type Session struct {",
		"\tToken   string",
		"\tExpired bool",
		"}",
		"",
		"func EvaluateSessionPolicy(session Session, strict bool) Decision {",
		"\tif session.Expired && strict {",
		"\t\treturn Decision{Allowed: false, Reason: DecisionReasonExpired}",
		"\t}",
		"\treturn EvaluatePolicy(session.Token)",
		"}",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "docs", "policies.md"), strings.Join([]string{
		"# Policies",
		"",
		"Session policy checks return allow/deny plus a machine-readable reason.",
		"",
		"Strict mode denies expired sessions before token checks.",
	}, "\n"))
	runGit(t, repoDir, "add", "internal/auth/policy.go", "internal/auth/session.go", "docs/policies.md")
	runGit(t, repoDir, "commit", "-m", "feat(auth): include decision reasons in policy checks")

	writeFixtureFile(t, filepath.Join(repoDir, "internal", "auth", "policy_test.go"), strings.Join([]string{
		"package auth",
		"",
		"import \"testing\"",
		"",
		"func TestEvaluateSessionPolicyStrictOutcomes(t *testing.T) {",
		"\tcases := []struct {",
		"\t\tname string",
		"\t\tsession Session",
		"\t\tstrict bool",
		"\t\twant DecisionReason",
		"\t}{",
		"\t\t{name: \"missing\", strict: true, want: DecisionReasonMissing},",
		"\t\t{name: \"expired\", session: Session{Token: \"ok\", Expired: true}, strict: true, want: DecisionReasonExpired},",
		"\t\t{name: \"allowed\", session: Session{Token: \"ok\"}, strict: true, want: DecisionReasonAllowed},",
		"\t}",
		"\tfor _, tc := range cases {",
		"\t\tt.Run(tc.name, func(t *testing.T) {",
		"\t\t\tgot := EvaluateSessionPolicy(tc.session, tc.strict)",
		"\t\t\tif got.Reason != tc.want {",
		"\t\t\t\tt.Fatalf(\"reason = %s, want %s\", got.Reason, tc.want)",
		"\t\t\t}",
		"\t\t})",
		"\t}",
		"}",
	}, "\n"))
	runGit(t, repoDir, "add", "internal/auth/policy_test.go")
	runGit(t, repoDir, "commit", "-m", "test(auth): cover strict session policy outcomes")

	return prMessageFixture{repoDir: repoDir}
}

func buildCommitMsgAmendFixture(t *testing.T) commitMsgFixture {
	t.Helper()

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.name", "Test User")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/example/git-agent.git")

	writeFixtureFile(t, filepath.Join(repoDir, "internal", "agent", "verify.go"), strings.Join([]string{
		"package agent",
		"",
		"type VerifyResponse struct {",
		"\tSuccess bool",
		"}",
		"",
		"func VerifyProvider(host string) VerifyResponse {",
		"\treturn VerifyResponse{Success: host != \"\"}",
		"}",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "docs", "verify.md"), "Provider verification returns success only.\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "feat(agent): verify provider config")

	writeFixtureFile(t, filepath.Join(repoDir, "internal", "config", "reload.go"), strings.Join([]string{
		"package config",
		"",
		"func ReloadConfig() error {",
		"\treturn nil",
		"}",
	}, "\n"))
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "refactor(config): isolate reload entrypoint")

	writeFixtureFile(t, filepath.Join(repoDir, "internal", "agent", "verify.go"), strings.Join([]string{
		"package agent",
		"",
		"type VerifyResponse struct {",
		"\tSuccess bool",
		"}",
		"",
		"func VerifyProvider(host string) VerifyResponse {",
		"\treturn VerifyResponse{Success: host != \"\",}",
		"}",
	}, "\n"))
	runGit(t, repoDir, "add", "internal/agent/verify.go")
	runGit(t, repoDir, "commit", "-m", "fix(agent): persist verified providers")

	writeFixtureFile(t, filepath.Join(repoDir, "internal", "agent", "verify.go"), strings.Join([]string{
		"package agent",
		"",
		"type VerifyResponse struct {",
		"\tSuccess   bool",
		"\tProviders []string",
		"}",
		"",
		"func VerifyProvider(host string) VerifyResponse {",
		"\tif host == \"\" {",
		"\t\treturn VerifyResponse{Success: false}",
		"\t}",
		"\treturn VerifyResponse{Success: true, Providers: []string{host}}",
		"}",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "docs", "verify.md"), strings.Join([]string{
		"# Verify API",
		"",
		"Verification now returns the current provider list.",
	}, "\n"))
	runGit(t, repoDir, "add", "internal/agent/verify.go", "docs/verify.md")

	return commitMsgFixture{repoDir: repoDir}
}

type releaseNoteFixture struct {
	repoDir           string
	baseTag           string
	releaseTag        string
	parentRefactorSHA string
	parentFixSHA      string
	webuiBaseSHA      string
	webuiReleaseSHA   string
	goutilsBaseSHA    string
	goutilsReleaseSHA string
}

func buildReleaseNoteFixture(t *testing.T) releaseNoteFixture {
	t.Helper()

	webuiRepo := buildSubmoduleRepo(t, "webui", []submoduleCommit{
		{message: "feat(webui): initial dashboard shell", files: map[string]string{
			"ui/dashboard.tsx": "export const dashboard = 'v1'\n",
			"ui/routes.ts":     "export const routeFields = ['path_patterns']\n",
		}},
		{message: "chore(webui): sync route docs and screenshots", files: map[string]string{
			"docs/routes.md":    "Route docs refreshed.\n",
			"docs/gallery.md":   "Screenshots refreshed.\n",
			"ui/routes.ts":      "export const routeFields = ['mode']\n",
			"ui/file_server.ts": "export function runMiddlewareAfterRules() { return true }\n",
		}},
	})
	goutilsRepo := buildSubmoduleRepo(t, "goutils", []submoduleCommit{
		{message: "feat(http): initial middleware chain", files: map[string]string{
			"http/middleware.go": "package http\n\nfunc ApplyChain() {}\n",
		}},
		{message: "feat(http): run ModifyResponse before lazy buffering decision", files: map[string]string{
			"http/middleware.go": "package http\n\nfunc ApplyChain() {}\n\nfunc ModifyResponseFirst() {}\n",
		}},
	})

	repoDir := t.TempDir()
	runGit(t, repoDir, "init")
	runGit(t, repoDir, "config", "user.name", "Test User")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "remote", "add", "origin", "https://github.com/example/godoxy.git")

	writeFixtureFile(t, filepath.Join(repoDir, "README.md"), "# GoDoxy\n\nParent repository.\n\nDeclarative file-server routes.\n")
	writeFixtureFile(t, filepath.Join(repoDir, "routes.yaml"), strings.Join([]string{
		"routes:",
		"  - name: files",
		"    path_patterns:",
		"      - /static/**",
		"    middleware:",
		"      - auth",
		"      - gzip",
	}, "\n"))
	runGit(t, repoDir, "-c", "protocol.file.allow=always", "submodule", "add", webuiRepo.dir, "webui")
	runGit(t, repoDir, "-c", "protocol.file.allow=always", "submodule", "add", goutilsRepo.dir, "goutils")
	runGit(t, filepath.Join(repoDir, "webui"), "remote", "set-url", "origin", "https://github.com/example/webui.git")
	runGit(t, filepath.Join(repoDir, "goutils"), "remote", "set-url", "origin", "https://github.com/example/goutils.git")
	runGit(t, filepath.Join(repoDir, "webui"), "checkout", webuiRepo.baseSHA)
	runGit(t, filepath.Join(repoDir, "goutils"), "checkout", goutilsRepo.baseSHA)
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-m", "feat(route): add path_patterns support")
	runGit(t, repoDir, "tag", "-m", "v1.0.0", "v1.0.0")

	writeFixtureFile(t, filepath.Join(repoDir, "routes.yaml"), strings.Join([]string{
		"routes:",
		"  - name: files",
		"    mode: static",
		"    middleware:",
		"      - auth",
		"      - gzip",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "internal", "route", "fileserver.go"), strings.Join([]string{
		"package route",
		"",
		"type Route struct {",
		"\tName       string",
		"\tMode       string",
		"\tMiddleware []string",
		"}",
		"",
		"func FileServerMode() string {",
		"\treturn \"rules-first\"",
		"}",
	}, "\n"))
	runGit(t, repoDir, "add", "routes.yaml", "internal/route/fileserver.go")
	runGit(t, repoDir, "commit", "-m", "refactor(route): remove path_patterns")
	parentRefactorSHA := gitHead(t, repoDir)

	runGit(t, filepath.Join(repoDir, "webui"), "checkout", webuiRepo.releaseSHA)
	runGit(t, filepath.Join(repoDir, "goutils"), "checkout", goutilsRepo.releaseSHA)
	writeFixtureFile(t, filepath.Join(repoDir, "README.md"), strings.Join([]string{
		"# GoDoxy",
		"",
		"Parent repository.",
		"",
		"Middleware ordering fixed for FileServer routes.",
	}, "\n"))
	writeFixtureFile(t, filepath.Join(repoDir, "internal", "route", "fileserver.go"), strings.Join([]string{
		"package route",
		"",
		"type Route struct {",
		"\tName       string",
		"\tMode       string",
		"\tMiddleware []string",
		"}",
		"",
		"func FileServerMode() string {",
		"\treturn \"middleware-after-rules\"",
		"}",
		"",
		"func WrapAfterRules(route Route) Route {",
		"\treturn route",
		"}",
	}, "\n"))
	runGit(t, repoDir, "add", "README.md", "internal/route/fileserver.go", "webui", "goutils")
	runGit(t, repoDir, "commit", "-m", "fix(route): wrap FileServer handlers with middleware after rules")
	parentFixSHA := gitHead(t, repoDir)
	runGit(t, repoDir, "tag", "-m", "v1.1.0", "v1.1.0")

	return releaseNoteFixture{
		repoDir:           repoDir,
		baseTag:           "v1.0.0",
		releaseTag:        "v1.1.0",
		parentRefactorSHA: parentRefactorSHA,
		parentFixSHA:      parentFixSHA,
		webuiBaseSHA:      webuiRepo.baseSHA,
		webuiReleaseSHA:   webuiRepo.releaseSHA,
		goutilsBaseSHA:    goutilsRepo.baseSHA,
		goutilsReleaseSHA: goutilsRepo.releaseSHA,
	}
}

type submoduleCommit struct {
	message string
	files   map[string]string
}

type submoduleFixture struct {
	dir        string
	baseSHA    string
	releaseSHA string
}

func buildSubmoduleRepo(t *testing.T, name string, commits []submoduleCommit) submoduleFixture {
	t.Helper()

	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "remote", "add", "origin", fmt.Sprintf("https://github.com/example/%s.git", name))

	var shas []string
	for _, commit := range commits {
		for path, content := range commit.files {
			writeFixtureFile(t, filepath.Join(dir, path), content)
		}
		runGit(t, dir, "add", ".")
		runGit(t, dir, "commit", "-m", commit.message)
		shas = append(shas, gitHead(t, dir))
	}

	return submoduleFixture{
		dir:        dir,
		baseSHA:    shas[0],
		releaseSHA: shas[len(shas)-1],
	}
}

type toolCallSpec struct {
	ID        string
	CallID    string
	Name      string
	Arguments string
}

func newScriptedResponsesServer(t *testing.T, steps []func(string) string) *httptest.Server {
	t.Helper()

	var mu sync.Mutex
	next := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if next >= len(steps) {
			t.Fatalf("unexpected extra provider request %s", r.URL.Path)
		}
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		response := steps[next](string(body))
		next++

		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", streamCompletedEvent(response))
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, response)
	}))
}

func responseWithToolCalls(id string, calls ...toolCallSpec) string {
	output := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		output = append(output, map[string]any{
			"id":        call.ID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   call.CallID,
			"name":      call.Name,
			"arguments": call.Arguments,
		})
	}
	return marshalResponse(map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": 0,
		"status":     "completed",
		"model":      "test-model",
		"output":     output,
	})
}

func responseWithText(id, text string) string {
	return marshalResponse(map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": 0,
		"status":     "completed",
		"model":      "test-model",
		"output": []map[string]any{{
			"id":     id + "_msg",
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]any{{
				"type":        "output_text",
				"text":        text,
				"annotations": []any{},
			}},
		}},
	})
}

func marshalResponse(payload map[string]any) string {
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func streamCompletedEvent(responseJSON string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(responseJSON), &payload); err != nil {
		panic(err)
	}
	return marshalResponse(map[string]any{
		"type":            "response.completed",
		"sequence_number": 1,
		"response":        payload,
	})
}

func assertTraceArtifacts(t *testing.T, repoDir, pattern string, minToolCalls int) {
	t.Helper()

	sessions, err := filepath.Glob(filepath.Join(projectMetadataDir(t, repoDir), "sessions", pattern))
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
	data, err := os.ReadFile(filepath.Join(sessions[0], "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	var toolCalls, toolOutputs int
	for _, item := range session.Items {
		switch item["type"] {
		case "function_call":
			toolCalls++
		case "function_call_output":
			toolOutputs++
		}
	}
	if toolCalls < minToolCalls || toolOutputs < minToolCalls {
		t.Fatalf("trace missing tool artifacts: calls=%d outputs=%d session=%s", toolCalls, toolOutputs, sessions[0])
	}
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
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

func gitHeadMessage(t *testing.T, dir string) string {
	t.Helper()

	cmd := exec.Command("git", "log", "-1", "--format=%B")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log HEAD failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}
