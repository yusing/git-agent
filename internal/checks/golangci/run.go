package golangci

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/bytedance/sonic"
	"github.com/golangci/golangci-lint/v2/pkg/commands"
	"github.com/golangci/golangci-lint/v2/pkg/printers"
	"github.com/golangci/golangci-lint/v2/pkg/result"
	"github.com/yusing/git-agent/internal/checks"
)

const (
	golangCILintVersion  = "v2.12.2"
	maxHelperOutputBytes = 16 << 20
)

func (*Checker) Run(ctx context.Context, executable string, genericPlan checks.Plan) (result checks.Result, returnErr error) {
	plan, ok := genericPlan.(*checkerPlan)
	if !ok || plan == nil || plan.CheckerName() != Name || !plan.Runnable() {
		return checks.Result{}, fmt.Errorf("golangci-lint received an invalid runnable plan")
	}
	if !filepath.IsAbs(executable) {
		return checks.Result{}, fmt.Errorf("checker executable path must be absolute")
	}
	for _, target := range plan.invocations {
		if err := validateInvocation(plan.scope.Root(), target); err != nil {
			return checks.Result{}, fmt.Errorf("validate golangci-lint plan: %w", err)
		}
	}

	tempRoot, err := os.MkdirTemp("", "git-agent-golangci-*")
	if err != nil {
		return checks.Result{}, fmt.Errorf("create private checker directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tempRoot); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove private checker directory: %w", err))
		}
	}()
	if err := validatePrivateDirectory(tempRoot); err != nil {
		return checks.Result{}, err
	}

	var diagnostics []checks.Diagnostic
	for index, target := range plan.invocations {
		if err := ctx.Err(); err != nil {
			return checks.Result{}, err
		}
		invocationRoot := filepath.Join(tempRoot, "invocation-"+strconv.Itoa(index))
		if err := os.Mkdir(invocationRoot, 0o700); err != nil {
			return checks.Result{}, fmt.Errorf("create private checker invocation directory: %w", err)
		}
		cacheRoot := filepath.Join(invocationRoot, "cache")
		if err := os.Mkdir(cacheRoot, 0o700); err != nil {
			return checks.Result{}, fmt.Errorf("create private checker cache: %w", err)
		}
		resultPath := filepath.Join(invocationRoot, "result.json")
		issues, checkErr, terminalErr := runInvocation(
			ctx, executable, tempRoot, plan.scope, cacheRoot, resultPath, target,
		)
		if terminalErr != nil {
			return checks.Result{}, terminalErr
		}
		if checkErr != nil {
			return checks.NewError(Name, checkErr)
		}
		diagnostics = append(diagnostics, issues...)
	}
	return checks.NewResult(Name, diagnostics)
}

func (*Checker) RunHelper(args []string) error {
	fs := flag.NewFlagSet(Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var tempRoot string
	var workspaceRoot string
	var moduleRoot string
	var resultPath string
	fs.StringVar(&tempRoot, "temp-root", "", "")
	fs.StringVar(&workspaceRoot, "workspace-root", "", "")
	fs.StringVar(&moduleRoot, "module-root", "", "")
	fs.StringVar(&resultPath, "result", "", "")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("invalid private checker request: %w", err)
	}
	target := invocation{moduleRoot: moduleRoot, targets: fs.Args()}
	if err := validateHelperRequest(tempRoot, workspaceRoot, resultPath, target); err != nil {
		return err
	}

	argv := append([]string{"golangci-lint", "run"}, target.targets...)
	argv = append(argv,
		"--fix=false",
		"--issues-exit-code=1",
		"--show-stats=false",
		"--output.json.path="+resultPath,
		"--output.text.path="+os.DevNull,
		"--output.tab.path="+os.DevNull,
		"--output.html.path="+os.DevNull,
		"--output.checkstyle.path="+os.DevNull,
		"--output.code-climate.path="+os.DevNull,
		"--output.junit-xml.path="+os.DevNull,
		"--output.teamcity.path="+os.DevNull,
		"--output.sarif.path="+os.DevNull,
	)
	os.Args = argv
	return commands.Execute(commands.BuildInfo{
		GoVersion: runtime.Version(),
		Version:   golangCILintVersion,
	})
}

func runInvocation(
	ctx context.Context,
	executable string,
	tempRoot string,
	scope checks.Scope,
	cacheRoot string,
	resultPath string,
	target invocation,
) ([]checks.Diagnostic, error, error) {
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("create checker stdout pipe: %w", err)
	}
	defer stdoutReader.Close()
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdoutWriter.Close()
		return nil, nil, errors.Join(fmt.Errorf("create checker stderr pipe: %w", err), stdoutReader.Close())
	}
	defer stderrReader.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		_ = stdoutWriter.Close()
		_ = stderrWriter.Close()
		return nil, nil, fmt.Errorf("open null input for checker: %w", err)
	}
	defer null.Close()

	process, err := os.StartProcess(
		executable,
		append([]string{executable}, helperArgs(tempRoot, scope.Root(), resultPath, target)...),
		&os.ProcAttr{
			Dir:   target.moduleRoot,
			Env:   checkerEnvironment(os.Environ(), cacheRoot),
			Files: []*os.File{null, stdoutWriter, stderrWriter},
			Sys:   checkerProcessAttributes(),
		},
	)
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	if err != nil {
		return nil, fmt.Errorf("start bundled golangci-lint helper: %w", err), nil
	}

	capture := &boundedCapture{remaining: maxHelperOutputBytes}
	var drainGroup sync.WaitGroup
	drainGroup.Go(func() {
		_, _ = io.Copy(capture, stdoutReader)
	})
	drainGroup.Go(func() {
		_, _ = io.Copy(capture, stderrReader)
	})

	waitDone := make(chan struct{})
	var state *os.ProcessState
	var waitErr error
	go func() {
		state, waitErr = process.Wait()
		close(waitDone)
	}()
	select {
	case <-ctx.Done():
		killErr := killCheckerProcess(process)
		<-waitDone
		drainGroup.Wait()
		return nil, nil, errors.Join(ctx.Err(), killErr, waitErr)
	case <-waitDone:
		drainGroup.Wait()
	}
	if waitErr != nil {
		return nil, fmt.Errorf("wait for bundled golangci-lint helper: %w", waitErr), nil
	}

	exitCode := state.ExitCode()
	if exitCode != 0 && exitCode != 1 {
		return nil, fmt.Errorf("bundled golangci-lint exited with code %d", exitCode), nil
	}
	jsonResult, err := readJSONResult(resultPath)
	if err != nil {
		return nil, err, nil
	}
	if jsonResult.Report != nil && strings.TrimSpace(jsonResult.Report.Error) != "" {
		return nil, fmt.Errorf("golangci-lint reported an analysis error: %s", jsonResult.Report.Error), nil
	}
	if exitCode == 0 && len(jsonResult.Issues) != 0 {
		return nil, fmt.Errorf("bundled golangci-lint returned issues with a success exit"), nil
	}
	if exitCode == 1 && len(jsonResult.Issues) == 0 {
		return nil, fmt.Errorf("bundled golangci-lint reported findings without diagnostics"), nil
	}

	diagnostics := make([]checks.Diagnostic, 0, len(jsonResult.Issues))
	for _, issue := range jsonResult.Issues {
		diagnostic, ok := normalizeIssue(scope, target, issue)
		if ok {
			diagnostics = append(diagnostics, diagnostic)
		}
	}
	return diagnostics, nil, nil
}

func helperArgs(tempRoot, workspaceRoot, resultPath string, target invocation) []string {
	args := []string{
		checks.PrivateCommand,
		Name,
		"--temp-root", tempRoot,
		"--workspace-root", workspaceRoot,
		"--module-root", target.moduleRoot,
		"--result", resultPath,
		"--",
	}
	return append(args, target.targets...)
}

func validateHelperRequest(tempRoot, workspaceRoot, resultPath string, target invocation) error {
	if err := validatePrivateDirectory(tempRoot); err != nil {
		return err
	}
	if !filepath.IsAbs(workspaceRoot) {
		return fmt.Errorf("private checker workspace must be absolute")
	}
	resolvedRoot, err := filepath.EvalSymlinks(filepath.Clean(workspaceRoot))
	if err != nil || resolvedRoot != filepath.Clean(workspaceRoot) {
		return fmt.Errorf("private checker workspace is unsafe")
	}
	if err := validateInvocation(resolvedRoot, target); err != nil {
		return fmt.Errorf("validate private checker target: %w", err)
	}
	if !filepath.IsAbs(resultPath) {
		return fmt.Errorf("private checker result path must be absolute")
	}
	resultPath = filepath.Clean(resultPath)
	if err := ensureContained(tempRoot, resultPath); err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(resultPath))
	if err != nil || parent != filepath.Dir(resultPath) {
		return fmt.Errorf("private checker result directory is unsafe")
	}
	if err := ensureContained(tempRoot, parent); err != nil {
		return err
	}
	if info, err := os.Lstat(resultPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("private checker result path is unsafe")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect private checker result path: %w", err)
	}
	return nil
}

func validatePrivateDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("private checker directory must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("resolve private checker directory: %w", err)
	}
	if resolved != filepath.Clean(path) {
		return fmt.Errorf("private checker directory must not contain symlinks")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("inspect private checker directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("private checker directory must be owner-only")
	}
	return nil
}

func readJSONResult(path string) (printers.JSONResult, error) {
	file, err := os.Open(path)
	if err != nil {
		return printers.JSONResult{}, fmt.Errorf("bundled golangci-lint did not produce a JSON result")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxHelperOutputBytes+1))
	if err != nil {
		return printers.JSONResult{}, fmt.Errorf("read bundled golangci-lint JSON result: %w", err)
	}
	if len(data) > maxHelperOutputBytes {
		return printers.JSONResult{}, fmt.Errorf("bundled golangci-lint JSON result exceeds %d bytes", maxHelperOutputBytes)
	}
	var parsed printers.JSONResult
	decoder := sonic.ConfigStd.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&parsed); err != nil {
		return printers.JSONResult{}, fmt.Errorf("decode bundled golangci-lint JSON result: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return printers.JSONResult{}, fmt.Errorf("decode bundled golangci-lint JSON result: trailing data")
	}
	if parsed.Issues == nil {
		return printers.JSONResult{}, fmt.Errorf("decode bundled golangci-lint JSON result: issues must be an array")
	}
	return parsed, nil
}

func normalizeIssue(scope checks.Scope, target invocation, issue *result.Issue) (checks.Diagnostic, bool) {
	if issue == nil || issue.Line() < 1 || strings.TrimSpace(issue.Text) == "" {
		return checks.Diagnostic{}, false
	}
	path := strings.TrimSpace(issue.FilePath())
	if path == "" {
		return checks.Diagnostic{}, false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(target.moduleRoot, filepath.FromSlash(path))
	}
	path = filepath.Clean(path)
	if err := ensureContained(target.moduleRoot, path); err != nil {
		return checks.Diagnostic{}, false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return checks.Diagnostic{}, false
	}
	relative, err := filepath.Rel(scope.Root(), resolved)
	if err != nil {
		return checks.Diagnostic{}, false
	}
	repositoryPath := filepath.ToSlash(relative)
	if filepath.Ext(repositoryPath) != ".go" || !scope.Contains(repositoryPath) {
		return checks.Diagnostic{}, false
	}
	return checks.Diagnostic{
		Path:    repositoryPath,
		Line:    issue.Line(),
		Column:  max(issue.Column(), 0),
		Code:    issue.FromLinter,
		Message: issue.Text,
	}, true
}

func checkerEnvironment(environment []string, cacheRoot string) []string {
	const marker = "GOLANGCI_LINT_CACHE="
	filtered := make([]string, 0, len(environment)+1)
	for _, variable := range environment {
		if !strings.HasPrefix(variable, marker) {
			filtered = append(filtered, variable)
		}
	}
	return append(filtered, marker+cacheRoot)
}

type boundedCapture struct {
	mu        sync.Mutex
	remaining int
	data      bytes.Buffer
}

func (c *boundedCapture) Write(value []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	retained := min(len(value), c.remaining)
	if retained > 0 {
		_, _ = c.data.Write(value[:retained])
		c.remaining -= retained
	}
	return len(value), nil
}
