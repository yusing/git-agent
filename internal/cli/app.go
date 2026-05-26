package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yusing/git-agent/internal/agent"
	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/guidance"
	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/tasks/commitmsg"
	"github.com/yusing/git-agent/internal/tasks/releasenote"
	"github.com/yusing/git-agent/internal/tools"
	"github.com/yusing/git-agent/internal/trace"
)

const (
	releaseNoteMinMaxSteps = 12
	releaseNoteMinTimeout  = 4 * time.Minute
)

type App struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
}

func New() *App {
	return &App{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
}

func (a *App) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("")
	}

	switch args[0] {
	case "commit-msg":
		return a.runCommitMsg(ctx, args[1:])
	case "pr-message":
		return a.runPRMessage(ctx, args[1:])
	case "release-note":
		return a.runReleaseNote(ctx, args[1:])
	case "-h", "--help", "help":
		return usageError("")
	default:
		return usageError(fmt.Sprintf("unknown command %q", args[0]))
	}
}

func (a *App) runCommitMsg(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("commit-msg", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var amend bool
	var opts config.Options
	fs.BoolVar(&amend, "amend", false, "generate an amended commit message")
	registerSharedFlags(fs, &opts)

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("commit-msg does not accept positional arguments")
	}

	mode := commitmsg.ModeNormal
	if amend {
		mode = commitmsg.ModeAmend
	}

	cfg, err := config.Resolve(opts)
	if err != nil {
		return err
	}
	taskCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	repo, err := gitctx.Open(".")
	if err != nil {
		return err
	}
	stagedPaths, err := repo.StagedPaths()
	if err != nil {
		return err
	}
	renderedGuidance, err := resolveGuidanceForPaths(repo, cfg.GuidanceFamily, stagedPaths)
	if err != nil {
		return err
	}
	recorder, err := trace.New(repo.RootPath, "commit-msg")
	if err != nil {
		return err
	}
	if cfg.Debug {
		fmt.Fprintf(a.stderr, "trace_dir=%s\n", recorder.Dir())
	}
	if err := recorder.Write("session", map[string]any{
		"command":      "commit-msg",
		"mode":         mode,
		"repo":         repo.Summary(),
		"staged_paths": stagedPaths,
	}); err != nil {
		return err
	}
	registry := tools.NewRegistry(repo)
	runner := agent.OpenAIRunner{
		Config:    cfg,
		Client:    openai.NewHTTPClient(&http.Client{Timeout: cfg.Timeout}),
		Tools:     registry,
		ToolSpecs: registry.Definitions(tools.CommitMessageToolNames()),
		Validator: func(text string) []string { return commitmsg.Validate(mode, text) },
		Trace:     recorder,
		Budget:    a.budgetHandler(),
	}
	environment := environmentContext(repo, "commit-msg", string(mode), cfg.GuidanceFamily, cfg.MaxSteps, cfg.MaxToolCalls)
	result, err := runner.Run(taskCtx, agent.Request{
		SystemPrompt:      commitmsg.SystemPrompt(mode),
		ToolPolicy:        toolPolicy(),
		Environment:       environment,
		ProjectGuidance:   renderedGuidance,
		UserPrompt:        commitmsg.UserPrompt(mode, cfg.MaxSteps, cfg.MaxToolCalls),
		AllowedToolNames:  tools.CommitMessageToolNames(),
		MaxSteps:          cfg.MaxSteps,
		RepairOnValidator: true,
	})
	if err != nil {
		return err
	}
	result.Text = commitmsg.Shape(result.Text)
	if recent, err := repo.RecentCommits(10); err == nil {
		result.Text = commitmsg.PreserveTaskIDSuffix(result.Text, recent)
	}
	if errs := commitmsg.Validate(mode, result.Text); len(errs) > 0 {
		return fmt.Errorf("validation failed after shaping: %v", errs)
	}
	if err := recorder.Write("final", map[string]any{
		"text":         result.Text,
		"tool_calls":   result.ToolCalls,
		"repair_calls": result.RepairCalls,
	}); err != nil {
		return err
	}
	return a.writeResult(cfg, result)
}

func (a *App) runPRMessage(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pr-message", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var opts config.Options
	registerSharedFlags(fs, &opts)

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("pr-message does not accept positional arguments")
	}

	cfg, err := config.Resolve(opts)
	if err != nil {
		return err
	}
	taskCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	repo, err := gitctx.Open(".")
	if err != nil {
		return err
	}
	prepared, err := commitmsg.PreparePRContext(repo)
	if err != nil {
		return err
	}
	renderedGuidance, err := resolveGuidanceForPaths(repo, cfg.GuidanceFamily, prepared.ChangedPaths)
	if err != nil {
		return err
	}
	recorder, err := trace.New(repo.RootPath, "pr-message")
	if err != nil {
		return err
	}
	if cfg.Debug {
		fmt.Fprintf(a.stderr, "trace_dir=%s\n", recorder.Dir())
	}
	if err := recorder.Write("session", map[string]any{
		"command":       "pr-message",
		"mode":          commitmsg.ModePR,
		"repo":          repo.Summary(),
		"base_ref":      gitctx.PullRequestBaseRef,
		"changed_paths": prepared.ChangedPaths,
		"prepared":      prepared,
	}); err != nil {
		return err
	}
	runner := agent.OpenAIRunner{
		Config:    cfg,
		Client:    openai.NewHTTPClient(&http.Client{Timeout: cfg.Timeout}),
		Validator: func(text string) []string { return commitmsg.Validate(commitmsg.ModePR, text) },
		Trace:     recorder,
		Budget:    a.budgetHandler(),
	}
	environment := environmentContext(repo, "pr-message", gitctx.PullRequestBaseRef+"..HEAD", cfg.GuidanceFamily, cfg.MaxSteps, cfg.MaxToolCalls)
	result, err := runner.Run(taskCtx, agent.Request{
		SystemPrompt:      commitmsg.SystemPrompt(commitmsg.ModePR),
		Environment:       environment,
		ProjectGuidance:   renderedGuidance,
		UserPrompt:        commitmsg.UserPromptWithPreparedPRContext(prepared, cfg.MaxSteps, cfg.MaxToolCalls),
		MaxSteps:          cfg.MaxSteps,
		RepairOnValidator: true,
	})
	if err != nil {
		return err
	}
	result.Text = commitmsg.Shape(result.Text)
	if errs := commitmsg.Validate(commitmsg.ModePR, result.Text); len(errs) > 0 {
		return fmt.Errorf("validation failed after shaping: %v", errs)
	}
	if err := recorder.Write("final", map[string]any{
		"text":         result.Text,
		"tool_calls":   result.ToolCalls,
		"repair_calls": result.RepairCalls,
	}); err != nil {
		return err
	}
	return a.writeResult(cfg, result)
}

func (a *App) runReleaseNote(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("release-note", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var opts config.Options
	registerSharedFlags(fs, &opts)

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("release-note requires <base> <release>")
	}

	cfg, err := config.Resolve(opts)
	if err != nil {
		return err
	}
	if cfg.MaxSteps < releaseNoteMinMaxSteps {
		cfg.MaxSteps = releaseNoteMinMaxSteps
	}
	if cfg.Timeout < releaseNoteMinTimeout {
		cfg.Timeout = releaseNoteMinTimeout
	}
	taskCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	repo, err := gitctx.Open(".")
	if err != nil {
		return err
	}
	renderedGuidance, err := resolveGuidanceForPaths(repo, cfg.GuidanceFamily, nil)
	if err != nil {
		return err
	}
	recorder, err := trace.New(repo.RootPath, "release-note")
	if err != nil {
		return err
	}
	if cfg.Debug {
		fmt.Fprintf(a.stderr, "trace_dir=%s\n", recorder.Dir())
	}
	if err := recorder.Write("session", map[string]any{
		"command": "release-note",
		"range":   fs.Arg(0) + ".." + fs.Arg(1),
		"repo":    repo.Summary(),
	}); err != nil {
		return err
	}
	registry := tools.NewRegistry(repo)
	prepared, err := releasenote.PrepareContext(repo, fs.Arg(0), fs.Arg(1))
	if err != nil {
		return err
	}
	const releaseNoteFallbackTools = "repo_summary"
	runner := agent.OpenAIRunner{
		Config:    cfg,
		Client:    openai.NewHTTPClient(&http.Client{Timeout: cfg.Timeout}),
		Tools:     registry,
		ToolSpecs: registry.Definitions([]string{releaseNoteFallbackTools}),
		Validator: releasenote.Validate,
		Trace:     recorder,
		Budget:    a.budgetHandler(),
	}
	environment := environmentContext(repo, "release-note", fs.Arg(0)+".."+fs.Arg(1), cfg.GuidanceFamily, cfg.MaxSteps, cfg.MaxToolCalls)
	result, err := runner.Run(taskCtx, agent.Request{
		SystemPrompt:      releasenote.SystemPrompt(),
		ToolPolicy:        toolPolicy(),
		Environment:       environment,
		ProjectGuidance:   renderedGuidance,
		UserPrompt:        releasenote.UserPrompt(prepared, cfg.MaxSteps, cfg.MaxToolCalls),
		TextFormat:        releasenote.TextFormat(),
		AllowedToolNames:  []string{releaseNoteFallbackTools},
		MaxSteps:          cfg.MaxSteps,
		RepairOnValidator: true,
	})
	if err != nil {
		return err
	}
	doc, err := releasenote.BuildDocument(result.Text, prepared)
	if err != nil {
		return err
	}
	if errs := releasenote.ValidateDocument(doc, releasenote.ValidationOptions{
		RequireFullChangelog: prepared.RequireFullChangelog,
		RequiredSubmodules:   prepared.RequiredSubmoduleGroups,
	}); len(errs) > 0 {
		return fmt.Errorf("rendered release note validation failed: %v", errs)
	}
	rendered := releasenote.Render(doc)
	result.Text = rendered
	if err := recorder.Write("final", map[string]any{
		"text":         result.Text,
		"tool_calls":   result.ToolCalls,
		"repair_calls": result.RepairCalls,
	}); err != nil {
		return err
	}
	return a.writeResult(cfg, result)
}

func registerSharedFlags(fs *flag.FlagSet, opts *config.Options) {
	fs.StringVar(&opts.Model, "model", "", "override OPENAI_MODEL")
	fs.BoolVar(&opts.Fast, "fast", false, "use priority service tier")
	fs.BoolVar(&opts.Low, "low", false, "use low reasoning effort")
	fs.BoolVar(&opts.Medium, "medium", false, "use medium reasoning effort")
	fs.BoolVar(&opts.High, "high", false, "use high reasoning effort")
	fs.BoolVar(&opts.XHigh, "xhigh", false, "use xhigh reasoning effort")
	fs.StringVar(&opts.BaseURL, "base-url", "", "override OPENAI_BASE_URL")
	fs.StringVar(&opts.Timeout, "timeout", "", "override default request timeout")
	fs.IntVar(&opts.MaxSteps, "max-steps", 0, "override maximum agent steps")
	fs.StringVar(&opts.GuidanceFamily, "guidance-family", "", "force guidance family")
	fs.BoolVar(&opts.Debug, "debug", false, "enable debug output on stderr")
}

func resolveGuidanceForPaths(repo *gitctx.Repository, requestedFamily string, paths []string) (string, error) {
	family, err := guidance.ParseFamily(requestedFamily)
	if err != nil {
		return "", err
	}
	targets := []string{repo.RootPath}
	if len(paths) > 0 {
		targets = make([]string, 0, len(paths))
		for _, path := range paths {
			targets = append(targets, filepath.Join(repo.RootPath, path))
		}
	}
	resolved, err := guidance.ResolveForTargets(repo.RootPath, targets, family)
	if err != nil {
		return "", err
	}
	return resolved.Rendered, nil
}

func toolPolicy() string {
	return `<tool_policy>
Tools are read-only repository inspection functions.
No tool can execute arbitrary shell commands.
No tool can mutate files, the Git index, refs, remotes, network state, or provider state.
Tool outputs use a JSON envelope with ok, tool, data, and truncated fields.
When truncated is true, request narrower data before making broad claims.
</tool_policy>`
}

func environmentContext(repo *gitctx.Repository, command, mode, guidanceFamily string, maxSteps, maxToolCalls int) string {
	return fmt.Sprintf(`<environment_context>
<cwd>%s</cwd>
<repo_root>%s</repo_root>
<command>%s</command>
<mode>%s</mode>
<guidance_family>%s</guidance_family>
<max_model_steps>%d</max_model_steps>
<max_tool_calls>%d</max_tool_calls>
<stdout_contract>final artifact only</stdout_contract>
</environment_context>`, repo.WorkPath, repo.RootPath, command, mode, guidanceFamily, maxSteps, maxToolCalls)
}

func (a *App) budgetHandler() agent.BudgetHandler {
	if !interactiveReader(a.stdin) {
		return nil
	}
	return func(_ context.Context, status agent.BudgetStatus) (agent.BudgetDecision, error) {
		stepBump := max(8, status.MaxSteps/2)
		toolBump := max(8, status.MaxToolCalls/2)
		if toolBump == 8 && status.MaxToolCalls > 0 {
			toolBump = max(4, status.MaxToolCalls/2)
		}
		nextSteps := status.MaxSteps + stepBump
		nextTools := status.MaxToolCalls
		if nextTools > 0 {
			nextTools += toolBump
		}

		prompt := fmt.Sprintf("Budget reached (%s). Extend to %d steps", status.Kind, nextSteps)
		if nextTools > 0 {
			prompt += fmt.Sprintf(" and %d tool calls", nextTools)
		}
		if status.RequestedTool != "" {
			prompt += fmt.Sprintf(" before %q", status.RequestedTool)
		}
		prompt += "? [y/N]: "
		if _, err := fmt.Fprint(a.stderr, prompt); err != nil {
			return agent.BudgetDecision{}, err
		}
		line, err := bufio.NewReader(a.stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return agent.BudgetDecision{}, err
		}
		answer := strings.TrimSpace(strings.ToLower(line))
		if answer != "y" && answer != "yes" {
			return agent.BudgetDecision{}, nil
		}
		return agent.BudgetDecision{
			ExtendSteps:     stepBump,
			ExtendToolCalls: toolBump,
		}, nil
	}
}

func interactiveReader(reader io.Reader) bool {
	file, ok := reader.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func (a *App) writeResult(cfg config.Config, result agent.Result) error {
	if cfg.Debug {
		fmt.Fprintf(a.stderr, "tool_calls=%d repair_calls=%d\n", result.ToolCalls, result.RepairCalls)
	}
	_, err := fmt.Fprintln(a.stdout, strings.TrimSpace(result.Text))
	return err
}

func usageError(prefix string) error {
	var b strings.Builder
	if prefix != "" {
		b.WriteString(prefix)
		b.WriteString("\n\n")
	}
	b.WriteString("usage:\n")
	b.WriteString("  git-agent commit-msg [--amend] [flags]\n")
	b.WriteString("  git-agent pr-message [flags]\n")
	b.WriteString("  git-agent release-note <base> <release> [flags]\n")
	return errors.New(b.String())
}
