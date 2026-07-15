package doccmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	commands []Command
	output   CommandOutput
	err      error
}

func (f *fakeRunner) Run(_ context.Context, command Command) (CommandOutput, error) {
	f.commands = append(f.commands, command)
	return f.output, f.err
}

func TestDiscoverRegistersOnlyResolvedCommands(t *testing.T) {
	t.Parallel()

	lookup := func(name string) (string, error) {
		if name == "go" {
			return filepath.Join(t.TempDir(), "go"), nil
		}
		return "", errors.New("missing")
	}
	commands := DiscoverWithOptions(Options{Root: t.TempDir(), Lookup: lookup, Runner: &fakeRunner{}})
	if !commands.Available(GoDoc) {
		t.Fatal("go_doc is unavailable")
	}
	for _, kind := range []Kind{RustDoc, Context7Library, Context7Docs} {
		if commands.Available(kind) {
			t.Fatalf("%s unexpectedly available", kind)
		}
	}
}

func TestGoDocUsesExactArgvAndSanitizedEnvironment(t *testing.T) {
	t.Setenv("GOENV", "host-goenv")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOTOOLCHAIN", "auto")
	t.Setenv("GOPROXY", "https://proxy.example")
	runner := &fakeRunner{output: CommandOutput{Stdout: "package strings"}}
	root := t.TempDir()
	commands := commandsForTest(t, root, runner)

	output, err := commands.Go(t.Context(), "strings", "Builder.WriteString", []string{"all", "src"})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("commands = %d", len(runner.commands))
	}
	command := runner.commands[0]
	if command.Path != filepath.Join(root, "bin", "go") {
		t.Fatalf("path = %q", command.Path)
	}
	if !slices.Equal(command.Args, []string{"doc", "-all", "-src", "strings", "Builder.WriteString"}) {
		t.Fatalf("args = %#v", command.Args)
	}
	if command.Dir != root {
		t.Fatalf("dir = %q", command.Dir)
	}
	environment := environmentMap(command.Env)
	for key, want := range map[string]string{"GOENV": "off", "GOFLAGS": "", "GOTOOLCHAIN": "local", "GOPROXY": "off"} {
		if environment[key] != want {
			t.Fatalf("%s = %q, want %q", key, environment[key], want)
		}
	}
	data := output.Data.(map[string]any)
	if data["text"] != "package strings" {
		t.Fatalf("output = %#v", output)
	}
}

func TestGoDocRejectsOptionTargetsFlagsAndMetacharacters(t *testing.T) {
	t.Parallel()

	commands := commandsForTest(t, t.TempDir(), &fakeRunner{})
	for _, target := range []string{"-http", "strings;touch", "../secret", "net/http value"} {
		if _, err := commands.Go(t.Context(), target, "", nil); err == nil {
			t.Fatalf("target %q accepted", target)
		}
	}
	if _, err := commands.Go(t.Context(), "strings", "", []string{"http"}); err == nil {
		t.Fatal("unsupported -http flag accepted")
	}
}

func TestRustDocValidatesContainmentAndExtractsMainContent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rustupHome := filepath.Join(root, "rustup")
	docPath := filepath.Join(rustupHome, "toolchains", "stable", "share", "doc", "rust", "html", "std", "index.html")
	if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(docPath, []byte(`<html><body><nav>ignore</nav><main id="main-content"><h1>std</h1><p>Installed docs.</p><script>ignore()</script></main></body></html>`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{output: CommandOutput{Stdout: docPath}}
	commands := DiscoverWithOptions(Options{
		Root: root, RustupHome: rustupHome, Runner: runner,
		Lookup: testLookup(root),
	})

	output, err := commands.Rust(t.Context(), "std")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(runner.commands[0].Args, []string{"doc", "--path", "std"}) {
		t.Fatalf("args = %#v", runner.commands[0].Args)
	}
	text := output.Data.(map[string]any)["text"].(string)
	if text != "std Installed docs." {
		t.Fatalf("text = %q", text)
	}

	outside := filepath.Join(root, "outside.html")
	if err := os.WriteFile(outside, []byte(`<main id="main-content">outside</main>`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner.output.Stdout = outside
	if _, err := commands.Rust(t.Context(), "std"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("outside path error = %v", err)
	}
}

func TestContext7UsesExactCommandsAndParsesJSON(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	runner := &fakeRunner{output: CommandOutput{Stdout: `{"results":[{"id":"/openai/openai-go"}]}`}}
	commands := commandsForTest(t, root, runner)
	query := `function calls; $(touch nope)`
	if _, err := commands.Context7LibraryLookup(t.Context(), "openai-go", query); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(runner.commands[0].Args, []string{"library", "openai-go", query, "--json"}) {
		t.Fatalf("library args = %#v", runner.commands[0].Args)
	}

	runner.output.Stdout = `{"snippets":[]}`
	if _, err := commands.Context7Documentation(t.Context(), "/openai/openai-go", query); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(runner.commands[1].Args, []string{"docs", "/openai/openai-go", query, "--json"}) {
		t.Fatalf("docs args = %#v", runner.commands[1].Args)
	}
	for _, test := range []struct{ name, query string }{{"--base-url", "x"}, {"https://private.example", "x"}} {
		if _, err := commands.Context7LibraryLookup(t.Context(), test.name, test.query); err == nil {
			t.Fatalf("custom target %q accepted", test.name)
		}
	}
	if _, err := commands.Context7Documentation(t.Context(), "https://private.example", "x"); err == nil {
		t.Fatal("custom Context7 URL accepted")
	}
}

func TestContext7RejectsMalformedOrMultipleJSONValues(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{output: CommandOutput{Stdout: `{`}}
	commands := commandsForTest(t, t.TempDir(), runner)
	if _, err := commands.Context7LibraryLookup(t.Context(), "react", "hooks"); err == nil || !strings.Contains(err.Error(), "malformed JSON") {
		t.Fatalf("malformed JSON error = %v", err)
	}
	runner.output.Stdout = `{} {}`
	if _, err := commands.Context7LibraryLookup(t.Context(), "react", "hooks"); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("multiple JSON error = %v", err)
	}
}

func TestCommandRunnerBoundsOutputAndDistinguishesTimeoutFromParentCancellation(t *testing.T) {
	t.Parallel()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := commandRunner{}
	result, err := runner.Run(t.Context(), Command{
		Path: executable, Args: []string{"-test.run=TestDocCommandHelperProcess"},
		Env: append(os.Environ(), "GO_DOC_COMMAND_HELPER=output"), Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || len(result.Stdout) != maxOutputBytes {
		t.Fatalf("bounded output bytes=%d truncated=%t", len(result.Stdout), result.Truncated)
	}

	_, err = runner.Run(t.Context(), Command{
		Path: executable, Args: []string{"-test.run=TestDocCommandHelperProcess"},
		Env: append(os.Environ(), "GO_DOC_COMMAND_HELPER=wait"), Timeout: 10 * time.Millisecond,
	})
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("internal timeout error = %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = runner.Run(ctx, Command{Path: executable, Args: []string{"-test.run=TestDocCommandHelperProcess"}, Timeout: time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation error = %v", err)
	}
}

func TestDocCommandHelperProcess(t *testing.T) {
	switch os.Getenv("GO_DOC_COMMAND_HELPER") {
	case "output":
		fmt.Print(strings.Repeat("x", maxOutputBytes+1024))
		os.Exit(0)
	case "wait":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func commandsForTest(t *testing.T, root string, runner Runner) *Commands {
	t.Helper()
	return DiscoverWithOptions(Options{Root: root, RustupHome: filepath.Join(root, "rustup"), Runner: runner, Lookup: testLookup(root)})
}

func testLookup(root string) LookupFunc {
	return func(name string) (string, error) {
		return filepath.Join(root, "bin", name), nil
	}
}

func environmentMap(environment []string) map[string]string {
	result := make(map[string]string, len(environment))
	for _, entry := range environment {
		key, value, _ := strings.Cut(entry, "=")
		result[key] = value
	}
	return result
}
