package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yusing/git-agent/internal/doccmd"
)

type documentationRunner struct {
	commands []doccmd.Command
	output   doccmd.CommandOutput
}

func (r *documentationRunner) Run(_ context.Context, command doccmd.Command) (doccmd.CommandOutput, error) {
	r.commands = append(r.commands, command)
	return r.output, nil
}

func TestDocumentationToolsExecuteAndRenderCommandOutput(t *testing.T) {
	t.Run("go_doc", func(t *testing.T) {
		root := t.TempDir()
		runner := &documentationRunner{output: doccmd.CommandOutput{
			Stdout:    "package strings\n\ntype Builder struct",
			Truncated: true,
		}}
		tool := documentationTool{kind: doccmd.GoDoc, commands: documentationCommandsForTest(t, root, runner)}

		result, err := tool.Execute(t.Context(), Invocation{
			Name: "go_doc", Arguments: `{"target":"strings","symbol":"Builder","flags":["src"]}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(runner.commands) != 1 || !slices.Equal(runner.commands[0].Args, []string{"doc", "-src", "strings", "Builder"}) {
			t.Fatalf("commands = %#v", runner.commands)
		}
		envelope := decodeDocumentationEnvelope(t, result)
		if !envelope.OK || envelope.Tool != "go_doc" || !envelope.Truncated || !result.Truncated {
			t.Fatalf("envelope = %#v, result = %#v", envelope, result)
		}
		if envelope.Data["text"] != "package strings\n\ntype Builder struct" || envelope.Data["locator"] != "go doc -src strings Builder" {
			t.Fatalf("data = %#v", envelope.Data)
		}
	})

	t.Run("rust_doc", func(t *testing.T) {
		root := t.TempDir()
		rustupHome := filepath.Join(root, "rustup")
		docPath := filepath.Join(rustupHome, "toolchains", "stable", "share", "doc", "rust", "html", "std", "fs", "index.html")
		if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(docPath, []byte(`<html><body><nav>hidden</nav><main id="main-content"><h1>std::fs</h1><p>Filesystem operations.</p><script>hidden()</script></main></body></html>`), 0o600); err != nil {
			t.Fatal(err)
		}
		runner := &documentationRunner{output: doccmd.CommandOutput{Stdout: docPath}}
		tool := documentationTool{kind: doccmd.RustDoc, commands: documentationCommandsForTestWithRustup(t, root, rustupHome, runner)}

		result, err := tool.Execute(t.Context(), Invocation{Name: "rust_doc", Arguments: `{"topic":"std::fs"}`})
		if err != nil {
			t.Fatal(err)
		}
		if len(runner.commands) != 1 || !slices.Equal(runner.commands[0].Args, []string{"doc", "--path", "std::fs"}) {
			t.Fatalf("commands = %#v", runner.commands)
		}
		envelope := decodeDocumentationEnvelope(t, result)
		if !envelope.OK || envelope.Tool != "rust_doc" || envelope.Truncated || result.Truncated {
			t.Fatalf("envelope = %#v, result = %#v", envelope, result)
		}
		if envelope.Data["text"] != "std::fs Filesystem operations." || envelope.Data["locator"] != filepath.ToSlash(docPath) {
			t.Fatalf("data = %#v", envelope.Data)
		}
	})

	t.Run("context7_library", func(t *testing.T) {
		root := t.TempDir()
		runner := &documentationRunner{output: doccmd.CommandOutput{Stdout: `{"results":[{"id":"/openai/openai-go","title":"OpenAI Go"}]}`}}
		tool := documentationTool{kind: doccmd.Context7Library, commands: documentationCommandsForTest(t, root, runner)}

		result, err := tool.Execute(t.Context(), Invocation{
			Name: "context7_library", Arguments: `{"name":"openai-go","query":"Responses API tools"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(runner.commands) != 1 || !slices.Equal(runner.commands[0].Args, []string{"library", "openai-go", "Responses API tools", "--json"}) {
			t.Fatalf("commands = %#v", runner.commands)
		}
		envelope := decodeDocumentationEnvelope(t, result)
		results, ok := envelope.Data["results"].([]any)
		if !envelope.OK || envelope.Tool != "context7_library" || !ok || len(results) != 1 {
			t.Fatalf("envelope = %#v", envelope)
		}
		entry, ok := results[0].(map[string]any)
		if !ok || entry["id"] != "/openai/openai-go" || entry["title"] != "OpenAI Go" {
			t.Fatalf("results = %#v", results)
		}
	})

	t.Run("context7_docs", func(t *testing.T) {
		root := t.TempDir()
		runner := &documentationRunner{output: doccmd.CommandOutput{Stdout: `{"snippets":[{"title":"Tools","content":"Use typed tools."}]}`}}
		tool := documentationTool{kind: doccmd.Context7Docs, commands: documentationCommandsForTest(t, root, runner)}

		result, err := tool.Execute(t.Context(), Invocation{
			Name: "context7_docs", Arguments: `{"library_id":"/openai/openai-go","query":"typed tools"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(runner.commands) != 1 || !slices.Equal(runner.commands[0].Args, []string{"docs", "/openai/openai-go", "typed tools", "--json"}) {
			t.Fatalf("commands = %#v", runner.commands)
		}
		envelope := decodeDocumentationEnvelope(t, result)
		snippets, ok := envelope.Data["snippets"].([]any)
		if !envelope.OK || envelope.Tool != "context7_docs" || !ok || len(snippets) != 1 {
			t.Fatalf("envelope = %#v", envelope)
		}
		entry, ok := snippets[0].(map[string]any)
		if !ok || entry["title"] != "Tools" || entry["content"] != "Use typed tools." {
			t.Fatalf("snippets = %#v", snippets)
		}
	})
}

func documentationCommandsForTest(t *testing.T, root string, runner doccmd.Runner) *doccmd.Commands {
	t.Helper()
	return documentationCommandsForTestWithRustup(t, root, filepath.Join(root, "rustup"), runner)
}

func documentationCommandsForTestWithRustup(t *testing.T, root, rustupHome string, runner doccmd.Runner) *doccmd.Commands {
	t.Helper()
	return doccmd.DiscoverWithOptions(doccmd.Options{
		Root: root, RustupHome: rustupHome, Runner: runner,
		Lookup: func(name string) (string, error) { return filepath.Join(root, "bin", name), nil },
	})
}

func decodeDocumentationEnvelope(t *testing.T, result Result) struct {
	OK        bool           `json:"ok"`
	Tool      string         `json:"tool"`
	Data      map[string]any `json:"data"`
	Truncated bool           `json:"truncated"`
} {
	t.Helper()
	var envelope struct {
		OK        bool           `json:"ok"`
		Tool      string         `json:"tool"`
		Data      map[string]any `json:"data"`
		Truncated bool           `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}
