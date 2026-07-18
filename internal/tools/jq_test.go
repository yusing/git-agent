package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/gitctx"
)

func TestJQRegistersOnlyForReviewAndSimplify(t *testing.T) {
	t.Parallel()

	for _, mode := range []ReviewMode{ReviewModeCodebase, ReviewModeUncommitted, ReviewModeStaged} {
		if !slices.Contains(ReviewToolCandidates(mode), jqToolName) {
			t.Fatalf("%s review candidates omit %s", mode, jqToolName)
		}
		definitions := NewReviewRegistryWithSkills(nil, nil, mode, ReviewScope{}, gitctx.ChangeFingerprint{}).Definitions([]string{jqToolName})
		if len(definitions) != 1 || !definitions[0].Strict || definitions[0].Schema["additionalProperties"] != false {
			t.Fatalf("%s jq definition = %#v", mode, definitions)
		}
	}
	if definitions := NewRegistryWithSkills(nil, nil).Definitions([]string{jqToolName}); len(definitions) != 0 {
		t.Fatalf("non-review registry exposes jq: %#v", definitions)
	}
}

func TestJQRetrievesJSONPointerValuesWithoutRoundingNumbers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	mustWriteFile(t, filepath.Join(dir, "config.json"), `{"a/b":{"~key":[{"number":9007199254740993,"enabled":true,"nothing":null}]}}`)
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewReviewRegistryWithSkills(repo, nil, ReviewModeCodebase, ReviewScope{}, gitctx.ChangeFingerprint{})

	tests := []struct {
		name    string
		pointer string
		want    string
	}{
		{name: "escaped object keys and array index", pointer: "/a~1b/~0key/0/number", want: `"value": 9007199254740993`},
		{name: "boolean", pointer: "/a~1b/~0key/0/enabled", want: `"value": true`},
		{name: "null", pointer: "/a~1b/~0key/0/nothing", want: `"value": null`},
		{name: "document root", pointer: "", want: `"a/b"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := registry.Execute(t.Context(), Invocation{
				Name:      jqToolName,
				Arguments: `{"path":"config.json","source":"","pointer":` + mustJSONQuote(t, test.pointer) + `,"max_bytes":4096,"max_lines":200}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(result.Content, test.want) || !strings.Contains(result.Content, `"source": "worktree"`) {
				t.Fatalf("jq result missing %s: %s", test.want, result.Content)
			}
		})
	}
}

func TestJQUsesStagedSourcePolicy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.name", "Test User")
	runGit(t, dir, "config", "user.email", "test@example.com")
	mustWriteFile(t, filepath.Join(dir, "config.json"), `{"value":"base"}`)
	runGit(t, dir, "add", "config.json")
	runGit(t, dir, "commit", "-m", "base")
	mustWriteFile(t, filepath.Join(dir, "config.json"), `{"value":"staged"}`)
	runGit(t, dir, "add", "config.json")
	mustWriteFile(t, filepath.Join(dir, "config.json"), `{"value":"unstaged-secret"}`)

	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := repo.StagedFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	registry := NewReviewRegistryWithSkills(repo, nil, ReviewModeStaged, ReviewScope{}, fingerprint)
	result, err := registry.Execute(t.Context(), Invocation{Name: jqToolName, Arguments: `{"path":"config.json","pointer":"/value"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, `"source": "index"`) || !strings.Contains(result.Content, `"value": "staged"`) || strings.Contains(result.Content, "unstaged-secret") {
		t.Fatalf("staged jq leaked worktree content: %s", result.Content)
	}
	if _, err := registry.Execute(t.Context(), Invocation{Name: jqToolName, Arguments: `{"path":"config.json","source":"worktree","pointer":"/value"}`}); err == nil {
		t.Fatal("jq accepted worktree source in staged mode")
	}
}

func TestJQRejectsInvalidPointersAndDocuments(t *testing.T) {
	t.Parallel()

	value := map[string]any{"array": []any{"first"}, "scalar": true}
	for _, test := range []struct {
		name    string
		pointer string
		want    string
	}{
		{name: "missing leading slash", pointer: "array/0", want: "must be empty or start with /"},
		{name: "invalid escape", pointer: "/bad~2key", want: "invalid escape"},
		{name: "missing key", pointer: "/missing", want: "does not exist"},
		{name: "leading zero", pointer: "/array/00", want: "leading zero"},
		{name: "out of range", pointer: "/array/1", want: "out of range"},
		{name: "scalar traversal", pointer: "/scalar/child", want: "cannot apply token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := resolveJSONPointer(value, test.pointer)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("resolveJSONPointer error = %v, want %q", err, test.want)
			}
		})
	}

	for _, document := range []string{`{"broken":`, `{"valid":true} {"extra":true}`} {
		if _, err := decodeJQDocument([]byte(document)); err == nil {
			t.Fatalf("decodeJQDocument accepted %q", document)
		}
	}
}

func TestJQMarksOversizedSelectedValuesTruncated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	runGit(t, dir, "init")
	mustWriteFile(t, filepath.Join(dir, "large.json"), `{"large":`+mustJSONQuote(t, strings.Repeat("x", 200))+`}`)
	repo, err := gitctx.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewReviewRegistryWithSkills(repo, nil, ReviewModeCodebase, ReviewScope{}, gitctx.ChangeFingerprint{})
	result, err := registry.Execute(t.Context(), Invocation{Name: jqToolName, Arguments: `{"path":"large.json","pointer":"/large","max_bytes":32,"max_lines":10}`})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data      map[string]any `json:"data"`
		Truncated bool           `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.Truncated || !result.Truncated || envelope.Data["value_preview"] == nil {
		t.Fatalf("jq truncated envelope = %#v, result = %#v", envelope, result)
	}
	if _, exists := envelope.Data["value"]; exists {
		t.Fatalf("truncated jq result exposed uncapped value: %s", result.Content)
	}
}

func TestReadJQDocumentHonorsContextAndDocumentCap(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readJQDocument(ctx, strings.NewReader(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("readJQDocument canceled error = %v", err)
	}
	if _, err := readJQDocument(t.Context(), io.LimitReader(strings.NewReader(strings.Repeat("x", jqMaxDocumentBytes+1)), int64(jqMaxDocumentBytes+1))); err == nil || !strings.Contains(err.Error(), "document exceeds") {
		t.Fatalf("readJQDocument oversized error = %v", err)
	}
}

func mustJSONQuote(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
