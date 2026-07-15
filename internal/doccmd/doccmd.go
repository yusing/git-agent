// Package doccmd runs a fixed set of local documentation commands.
package doccmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds one documentation command invocation.
	DefaultTimeout = 20 * time.Second
	// MaxTargetBytes bounds documentation targets, symbols, topics, names, and IDs.
	MaxTargetBytes = 256
	// MaxQueryBytes bounds external documentation queries.
	MaxQueryBytes  = 2 * 1024
	maxOutputBytes = 64 * 1024
)

var (
	goTargetPattern  = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
	goSymbolPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
	rustTopicPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:::[A-Za-z_][A-Za-z0-9_]*)*!?$`)
	libraryIDPattern = regexp.MustCompile(`^/[^/\s]+/[^/\s]+(?:[/@][^/\s]+)?$`)
)

var allowedGoFlags = []string{"all", "short", "src", "u", "c", "cmd"}

// GoDocFlags returns the accepted go doc display flags.
func GoDocFlags() []string {
	return slices.Clone(allowedGoFlags)
}

type Kind string

const (
	GoDoc           Kind = "go_doc"
	RustDoc         Kind = "rust_doc"
	Context7Library Kind = "context7_library"
	Context7Docs    Kind = "context7_docs"
)

type Output struct {
	Data      any
	Truncated bool
}

type Command struct {
	Path    string
	Args    []string
	Dir     string
	Env     []string
	Timeout time.Duration
}

type CommandOutput struct {
	Stdout    string
	Stderr    string
	Truncated bool
}

type Runner interface {
	Run(context.Context, Command) (CommandOutput, error)
}

type LookupFunc func(string) (string, error)

type Options struct {
	Root       string
	RustupHome string
	Timeout    time.Duration
	Lookup     LookupFunc
	Runner     Runner
}

type Commands struct {
	root       string
	rustupHome string
	timeout    time.Duration
	goPath     string
	rustupPath string
	context7   string
	runner     Runner
}

func Discover(root string) *Commands {
	return DiscoverWithOptions(Options{Root: root})
}

func DiscoverWithOptions(options Options) *Commands {
	lookup := options.Lookup
	if lookup == nil {
		lookup = exec.LookPath
	}
	runner := options.Runner
	if runner == nil {
		runner = commandRunner{}
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	root, _ := filepath.Abs(options.Root)
	commands := &Commands{
		root:       root,
		rustupHome: resolveRustupHome(options.RustupHome),
		timeout:    timeout,
		runner:     runner,
	}
	commands.goPath = resolveExecutable(lookup, "go")
	commands.rustupPath = resolveExecutable(lookup, "rustup")
	commands.context7 = resolveExecutable(lookup, "ctx7")
	return commands
}

func resolveExecutable(lookup LookupFunc, name string) string {
	path, err := lookup(name)
	if err != nil || path == "" {
		return ""
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return filepath.Clean(absolute)
}

func resolveRustupHome(configured string) string {
	if configured == "" {
		configured = os.Getenv("RUSTUP_HOME")
	}
	if configured == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configured = filepath.Join(home, ".rustup")
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return ""
	}
	return filepath.Clean(absolute)
}

func (c *Commands) Available(kind Kind) bool {
	switch kind {
	case GoDoc:
		return c.goPath != ""
	case RustDoc:
		return c.rustupPath != ""
	case Context7Library, Context7Docs:
		return c.context7 != ""
	default:
		return false
	}
}

func (c *Commands) Go(ctx context.Context, target, symbol string, flags []string) (Output, error) {
	if !c.Available(GoDoc) {
		return Output{}, errors.New("go documentation command is unavailable")
	}
	if err := validateGoTarget(target); err != nil {
		return Output{}, err
	}
	if symbol != "" && (len(symbol) > MaxTargetBytes || !goSymbolPattern.MatchString(symbol)) {
		return Output{}, fmt.Errorf("invalid Go documentation symbol %q", symbol)
	}
	if len(flags) > len(allowedGoFlags) {
		return Output{}, errors.New("too many go doc flags")
	}
	args := []string{"doc"}
	for _, flag := range flags {
		if !slices.Contains(allowedGoFlags, flag) {
			return Output{}, fmt.Errorf("unsupported go doc flag %q", flag)
		}
		args = append(args, "-"+flag)
	}
	args = append(args, target)
	if symbol != "" {
		args = append(args, symbol)
	}
	result, err := c.runner.Run(ctx, Command{
		Path: c.goPath, Args: args, Dir: c.root,
		Env: sanitizedGoEnvironment(), Timeout: c.timeout,
	})
	if err != nil {
		return Output{}, err
	}
	return Output{
		Data: map[string]any{
			"target": target, "symbol": symbol,
			"locator": strings.Join(append([]string{"go"}, args...), " "),
			"text":    result.Stdout,
		},
		Truncated: result.Truncated,
	}, nil
}

func validateGoTarget(target string) error {
	if target == "" || len(target) > MaxTargetBytes || strings.HasPrefix(target, "-") || !goTargetPattern.MatchString(target) {
		return fmt.Errorf("invalid Go documentation target %q", target)
	}
	if target == "." || strings.Contains(target, "..") {
		return fmt.Errorf("invalid Go documentation target %q", target)
	}
	return nil
}

func sanitizedGoEnvironment() []string {
	return replaceEnvironment(os.Environ(), map[string]string{
		"GOENV":       "off",
		"GOFLAGS":     "",
		"GOTOOLCHAIN": "local",
		"GOPROXY":     "off",
	})
}

func replaceEnvironment(environment []string, replacements map[string]string) []string {
	result := make([]string, 0, len(environment)+len(replacements))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := replacements[key]; !replaced {
			result = append(result, entry)
		}
	}
	for key, value := range replacements {
		result = append(result, key+"="+value)
	}
	slices.Sort(result)
	return result
}

func (c *Commands) Rust(ctx context.Context, topic string) (Output, error) {
	if !c.Available(RustDoc) {
		return Output{}, errors.New("Rust documentation command is unavailable")
	}
	if topic == "" || len(topic) > MaxTargetBytes || strings.HasPrefix(topic, "-") || !rustTopicPattern.MatchString(topic) {
		return Output{}, fmt.Errorf("invalid Rust documentation topic %q", topic)
	}
	result, err := c.runner.Run(ctx, Command{
		Path: c.rustupPath, Args: []string{"doc", "--path", topic}, Timeout: c.timeout,
	})
	if err != nil {
		return Output{}, err
	}
	path, err := c.validRustDocPath(result.Stdout)
	if err != nil {
		return Output{}, err
	}
	text, truncated, err := extractMainContent(path)
	if err != nil {
		return Output{}, err
	}
	return Output{
		Data:      map[string]any{"topic": topic, "locator": filepath.ToSlash(path), "text": text},
		Truncated: result.Truncated || truncated,
	}, nil
}

func (c *Commands) validRustDocPath(stdout string) (string, error) {
	path := strings.TrimSpace(stdout)
	if path == "" || strings.ContainsAny(path, "\r\n") || filepath.Ext(path) != ".html" {
		return "", errors.New("rustup doc returned an invalid HTML path")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve rustup documentation path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve rustup documentation file: %w", err)
	}
	root, err := filepath.EvalSymlinks(filepath.Join(c.rustupHome, "toolchains"))
	if err != nil {
		return "", fmt.Errorf("resolve rustup toolchains directory: %w", err)
	}
	if !pathWithin(root, resolved) || !strings.Contains(filepath.ToSlash(resolved), "/share/doc/rust/html/") {
		return "", errors.New("rustup documentation path escapes installed toolchain documentation")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("rustup documentation path is not a regular local HTML file")
	}
	return resolved, nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (c *Commands) Context7LibraryLookup(ctx context.Context, name, query string) (Output, error) {
	if !c.Available(Context7Library) {
		return Output{}, errors.New("Context7 command is unavailable")
	}
	if err := validateBoundedText("library name", name, MaxTargetBytes, true); err != nil {
		return Output{}, err
	}
	if err := validateBoundedText("query", query, MaxQueryBytes, false); err != nil {
		return Output{}, err
	}
	return c.runContext7(ctx, []string{"library", name, query, "--json"})
}

func (c *Commands) Context7Documentation(ctx context.Context, libraryID, query string) (Output, error) {
	if !c.Available(Context7Docs) {
		return Output{}, errors.New("Context7 command is unavailable")
	}
	if len(libraryID) > MaxTargetBytes || !libraryIDPattern.MatchString(libraryID) {
		return Output{}, fmt.Errorf("invalid Context7 library ID %q", libraryID)
	}
	if err := validateBoundedText("query", query, MaxQueryBytes, false); err != nil {
		return Output{}, err
	}
	return c.runContext7(ctx, []string{"docs", libraryID, query, "--json"})
}

func validateBoundedText(name, value string, maxBytes int, rejectOption bool) error {
	if strings.TrimSpace(value) != value || value == "" || len(value) > maxBytes || strings.ContainsRune(value, 0) {
		return fmt.Errorf("invalid %s", name)
	}
	if rejectOption && (strings.HasPrefix(value, "-") || strings.Contains(value, "://")) {
		return fmt.Errorf("invalid %s", name)
	}
	return nil
}

func (c *Commands) runContext7(ctx context.Context, args []string) (Output, error) {
	result, err := c.runner.Run(ctx, Command{Path: c.context7, Args: args, Timeout: c.timeout})
	if err != nil {
		return Output{}, err
	}
	var data any
	decoder := json.NewDecoder(strings.NewReader(result.Stdout))
	decoder.UseNumber()
	if err := decoder.Decode(&data); err != nil {
		return Output{}, fmt.Errorf("Context7 returned malformed JSON: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Output{}, errors.New("Context7 returned multiple JSON values")
	}
	return Output{Data: data, Truncated: result.Truncated}, nil
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, command Command) (CommandOutput, error) {
	timeout := command.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(commandCtx, command.Path, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = command.Env
	cmd.Stdin = nil
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := CommandOutput{
		Stdout: strings.TrimSpace(stdout.String()), Stderr: strings.TrimSpace(stderr.String()),
		Truncated: stdout.truncated || stderr.truncated,
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("documentation command timed out after %s", timeout)
	}
	if err != nil {
		if result.Stderr != "" {
			return result, fmt.Errorf("documentation command failed: %s", result.Stderr)
		}
		return result, fmt.Errorf("documentation command failed: %w", err)
	}
	return result, nil
}

type cappedBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := maxOutputBytes - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(data)
	return original, nil
}

func (b *cappedBuffer) String() string {
	return b.buffer.String()
}
