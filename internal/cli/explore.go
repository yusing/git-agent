package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/agent"
	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/explore"
	"github.com/yusing/git-agent/internal/followup"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/projectidentity"
	searchtask "github.com/yusing/git-agent/internal/tasks/search"
	"github.com/yusing/git-agent/internal/tools"
	"github.com/yusing/git-agent/internal/trace"
)

func (a *App) runExplore(ctx context.Context, args []string) error {
	started := time.Now()
	fs := flag.NewFlagSet("explore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var followUpID string
	var targetValue string
	var fast bool
	var debug bool
	fs.StringVar(&followUpID, "follow-up", "", "fork a completed explore search")
	fs.StringVar(&targetValue, "for", "", "select query target: diagnose, change, behavior, or owner")
	fs.BoolVar(&fast, "fast", false, "use priority service tier")
	fs.BoolVar(&debug, "debug", false, "enable debug output on stderr")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exploreUsageError()
		}
		return errors.Join(err, exploreUsageError())
	}
	targetSpecified := false
	fs.Visit(func(option *flag.Flag) {
		if option.Name == "for" {
			targetSpecified = true
		}
	})
	selectedTarget, err := explore.ParseQueryTarget(targetValue)
	if err != nil {
		return errors.Join(err, exploreUsageError())
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return errors.New("explore requires a question")
	}
	var recorder *trace.Recorder
	var timing *explorePhaseTrace
	if debug {
		var err error
		recorder, err = trace.NewStream("explore", a.stderr)
		if err != nil {
			return err
		}
		timing = newExplorePhaseTrace(started, recorder)
	}

	workspace, err := os.Getwd()
	if err != nil {
		return err
	}
	identity, err := projectidentity.Resolve(workspace)
	if err != nil {
		return err
	}
	repo, repoErr := gitctx.Open(workspace)
	if repoErr != nil && !errors.Is(repoErr, gitctx.ErrNotRepository) {
		return repoErr
	}
	if err := migrateProjectMetadata(identity.Root); err != nil {
		return err
	}
	metadataDir, err := identity.Dir()
	if err != nil {
		return err
	}
	projectID, err := identity.ID()
	if err != nil {
		return err
	}
	store, err := explore.NewStore(metadataDir)
	if err != nil {
		return err
	}
	var parent *explore.Session
	var inheritedTarget explore.QueryTarget
	if followUpID != "" {
		parent, inheritedTarget, err = store.FollowUpParent(followUpID, workspace)
		if err != nil {
			return err
		}
	}
	if followUpID != "" && !targetSpecified {
		selectedTarget = inheritedTarget
	}

	coordinator := explore.NewCoordinator(store, workspace, fast, selectedTarget)
	if debug {
		coordinator.Timing = timing.recordSimple
	}
	if dispositionLog, logErr := explore.NewDispositionLog(projectID); logErr == nil {
		coordinator.DispositionLog = dispositionLog
	}
	lastProgress := ""
	coordinator.Progress = func(status string) {
		if status == lastProgress {
			return
		}
		lastProgress = status
		_, _ = fmt.Fprintf(a.stderr, "explore: %s\n", status)
	}
	var prepare explore.PrepareFunc
	if parent == nil {
		prepare = func(searchCtx context.Context) (explore.Prepared, error) {
			return a.prepareExploreSearch(searchCtx, question, timing)
		}
	}
	timing.recordSimple("setup", time.Since(started))
	output, err := coordinator.Run(ctx, parent, question, prepare, func(
		batchCtx context.Context, batchParent *explore.Session, items []explore.BatchItem,
	) (map[string]explore.BatchResult, error) {
		return a.runExploreBatch(batchCtx, workspace, repo, batchParent, items, fast, debug, recorder, timing)
	})
	if err != nil {
		return errors.Join(err, timing.err())
	}
	outputStarted := time.Now()
	err = writeJSONOutput(a.stdout, output)
	timing.recordSimple("output", time.Since(outputStarted))
	return errors.Join(err, timing.err())
}

func (a *App) exploreSearchOptions(root string) (openai.EmbeddingClient, searchtask.Options, error) {
	embeddingCfg, err := config.ResolveEmbeddings(config.Options{})
	if err != nil {
		return nil, searchtask.Options{}, err
	}
	dimensions, err := config.ResolveEmbeddingDimensions(0)
	if err != nil {
		return nil, searchtask.Options{}, err
	}
	maxInput, err := config.ResolveEmbeddingMaxInput(searchtask.DefaultEmbeddingMaxInputChars)
	if err != nil {
		return nil, searchtask.Options{}, err
	}
	batchInputs, err := config.ResolveEmbeddingBatchInputs(searchtask.DefaultEmbeddingBatchInputs)
	if err != nil {
		return nil, searchtask.Options{}, err
	}
	batchMaxChars, err := config.ResolveEmbeddingBatchMaxChars(searchtask.DefaultEmbeddingBatchMaxChars)
	if err != nil {
		return nil, searchtask.Options{}, err
	}
	concurrency, err := config.ResolveEmbeddingConcurrency(0)
	if err != nil {
		return nil, searchtask.Options{}, err
	}
	fileCfg, err := config.LoadFile()
	if err != nil {
		return nil, searchtask.Options{}, err
	}
	client := a.embeddingClient
	if client == nil {
		client = openai.NewHTTPClient(&http.Client{})
	}
	return client, searchtask.Options{
		Root: root, IndexRemote: fileCfg.Index.Remote, MinScore: searchtask.DefaultMinScore,
		Limit: searchtask.DefaultLimit, CodeOnly: true,
		EmbeddingModel: config.ResolveEmbeddingModel(""), EmbeddingDimensions: dimensions,
		EmbeddingMaxInput: maxInput, EmbeddingBatchInputs: batchInputs,
		EmbeddingBatchMaxChars: batchMaxChars, EmbeddingConcurrency: concurrency,
		APIKey: embeddingCfg.APIKey, BaseURL: embeddingCfg.BaseURL,
	}, nil
}

func (a *App) prepareExploreSearch(ctx context.Context, question string, timing *explorePhaseTrace) (explore.Prepared, error) {
	root, err := os.Getwd()
	if err != nil {
		return explore.Prepared{}, err
	}
	client, opts, err := a.exploreSearchOptions(root)
	if err != nil {
		return explore.Prepared{}, err
	}
	lastStatus := ""
	opts.ProgressLog = func(progress searchtask.Progress) error {
		status := strings.TrimSpace(progress.Status)
		if status == "" || status == lastStatus {
			return nil
		}
		lastStatus = status
		_, writeErr := fmt.Fprintf(a.stderr, "explore search: %s\n", status)
		return writeErr
	}

	warmOpts := opts
	warmOpts.IndexRemote = ""
	warmOpts.RequireWarmIndex = true
	output, err := searchtask.Run(ctx, client, warmOpts, question)
	if errors.Is(err, searchtask.ErrIndexNotWarm) {
		for _, phase := range output.Diagnostics.Timings {
			timing.recordSimple("semantic_search.warm_probe."+phase.Step, phase.Duration)
		}
		return explore.Prepared{}, nil
	}
	for _, phase := range output.Diagnostics.Timings {
		timing.recordSimple("semantic_search."+phase.Step, phase.Duration)
	}
	if err != nil {
		return explore.Prepared{}, err
	}
	deferredFreshness := strings.TrimSpace(opts.IndexRemote) != "" && output.Source.OriginIdentity != ""
	semantic, err := sonic.ConfigStd.MarshalToString(output)
	if err != nil {
		return explore.Prepared{}, fmt.Errorf("encode explore semantic results: %w", err)
	}
	paths := make([]string, 0, len(output.Results))
	for _, result := range output.Results {
		if result.Path != "" && !slices.Contains(paths, result.Path) {
			paths = append(paths, result.Path)
		}
	}
	return explore.Prepared{
		SemanticResults:   semantic,
		GuidancePaths:     paths,
		DeferredFreshness: deferredFreshness,
	}, nil
}

func (a *App) confirmExploreIndexFreshness(ctx context.Context, root string) (searchtask.Output, error) {
	client, opts, err := a.exploreSearchOptions(root)
	if err != nil {
		return searchtask.Output{}, err
	}
	opts.IndexOnly = true
	return searchtask.Run(ctx, client, opts, "")
}

type exploreFreshnessResult struct {
	output searchtask.Output
	err    error
}

func runWithExploreFreshness[T any](
	ctx context.Context,
	required bool,
	confirm func(context.Context) (searchtask.Output, error),
	run func(context.Context) (T, error),
	waiting func(),
) (T, searchtask.Output, error) {
	if !required {
		value, err := run(ctx)
		return value, searchtask.Output{}, err
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	freshness := make(chan exploreFreshnessResult, 1)
	go func() {
		output, err := confirm(workCtx)
		if err != nil {
			cancel()
		}
		freshness <- exploreFreshnessResult{output: output, err: err}
	}()
	value, runErr := run(workCtx)
	if runErr != nil {
		cancel()
	}
	var result exploreFreshnessResult
	select {
	case result = <-freshness:
	default:
		if waiting != nil {
			waiting()
		}
		result = <-freshness
	}
	return value, result.output, errors.Join(runErr, result.err)
}

func (a *App) runExploreBatch(
	ctx context.Context,
	root string,
	repo *gitctx.Repository,
	parent *explore.Session,
	items []explore.BatchItem,
	fast bool,
	debug bool,
	recorder *trace.Recorder,
	timing *explorePhaseTrace,
) (map[string]explore.BatchResult, error) {
	promptSetupStarted := time.Now()
	cfg, err := config.Resolve(config.Options{Fast: fast, Debug: debug})
	if err != nil {
		return nil, err
	}
	selectedTarget := items[0].QueryTarget
	instructionTarget := explore.SystemPromptTarget(parent, selectedTarget)
	promptItems := make([]explore.PromptItem, 0, len(items))
	itemIDs := make([]string, 0, len(items))
	var guidancePaths []string
	for _, item := range items {
		promptItems = append(promptItems, explore.PromptItem{
			ItemID: item.ID, Question: item.Question, SemanticResults: item.SemanticResults,
		})
		itemIDs = append(itemIDs, item.ID)
		for _, path := range item.GuidancePaths {
			if !slices.Contains(guidancePaths, path) {
				guidancePaths = append(guidancePaths, path)
			}
		}
	}
	userPrompt, err := explore.UserPrompt(parent, promptItems)
	if err != nil {
		return nil, err
	}
	renderedGuidance := ""
	if parent == nil {
		renderedGuidance, err = resolveGuidanceForRoot(root, cfg.GuidanceFamily, guidancePaths)
		if err != nil {
			return nil, err
		}
	}
	registry := tools.NewExploreRegistry(root, repo)
	toolSpecs := registry.Definitions(tools.ExploreToolNames())
	allowedTools := toolDefinitionNames(toolSpecs)
	parentSearchID := ""
	if parent != nil {
		parentSearchID = parent.ID
	}
	repoSummary := map[string]any{"root_path": root, "work_path": root}
	if repo != nil {
		repoSummary = repo.Summary()
	}
	if err := recorder.Write("session", map[string]any{
		"command": "explore", "batch_size": len(items), "parent_id": parentSearchID, "repo": repoSummary,
	}); err != nil {
		return nil, err
	}
	client := a.responseClient
	if client == nil {
		client = openai.NewHTTPClient(&http.Client{})
	}
	var parentLineage *followup.Lineage
	if parent != nil {
		lineage := followup.Lineage{
			ID: parent.ID, ParentID: parent.ParentID, Depth: parent.Depth,
			PromptCacheKey: parent.PromptCacheKey,
		}
		parentLineage = &lineage
	}
	lineage := followup.Next(parentLineage, items[0].ID, "explore:"+items[0].ID)
	runner := agent.OpenAIRunner{
		Config: cfg, Client: client, Tools: registry, ToolSpecs: toolSpecs,
		Validator: explore.ValidateAnswers(itemIDs), Trace: recorder, Budget: a.budgetHandler(),
		PromptCacheKey: lineage.PromptCacheKey,
		UsageOutput:    a.stderr,
	}
	if debug {
		runner.Timing = func(event agent.Timing) {
			fields := map[string]any{}
			if event.Step > 0 {
				fields["step"] = event.Step
			}
			if event.Tool != "" {
				fields["tool"] = event.Tool
			}
			timing.record(event.Phase, event.Duration, fields)
		}
	}
	request := agent.Request{
		SystemPrompt: explore.SystemPromptFor(instructionTarget), TextFormat: explore.TextFormat(), AllowedToolNames: allowedTools,
		ParallelToolCalls: true, MaxSteps: cfg.MaxSteps, RepairOnValidator: true,
	}
	if parent == nil {
		request.ToolPolicy = toolPolicy()
		request.Environment = environmentContextForRoot(root, root, "explore", "codebase", cfg.GuidanceFamily, cfg.MaxSteps, cfg.MaxToolCalls)
		request.ProjectGuidance = renderedGuidance
		request.DeveloperInstructions = explore.InitialTargetInstruction(selectedTarget)
		request.UserPrompt = userPrompt
	} else {
		request.Input = explore.FollowUpInput(*parent, userPrompt, selectedTarget)
	}
	timing.recordSimple("prompt_setup", time.Since(promptSetupStarted))

	freshnessRequired := parent == nil && slices.ContainsFunc(items, func(item explore.BatchItem) bool {
		return item.DeferredFreshness
	})
	var freshnessWaitStarted time.Time
	result, freshnessOutput, err := runWithExploreFreshness(
		ctx,
		freshnessRequired,
		func(confirmCtx context.Context) (searchtask.Output, error) {
			return a.confirmExploreIndexFreshness(confirmCtx, root)
		},
		func(runCtx context.Context) (agent.Result, error) {
			agentStarted := time.Now()
			result, err := runner.Run(runCtx, request)
			timing.recordSimple("agent", time.Since(agentStarted))
			return result, err
		},
		func() {
			freshnessWaitStarted = time.Now()
			_, _ = fmt.Fprintln(a.stderr, "explore: confirming_freshness")
		},
	)
	if freshnessRequired {
		for _, phase := range freshnessOutput.Diagnostics.Timings {
			timing.recordSimple("remote_freshness."+phase.Step, phase.Duration)
		}
		timing.recordSimple("remote_freshness", freshnessOutput.Diagnostics.Total)
		if !freshnessWaitStarted.IsZero() {
			timing.recordSimple("freshness_join", time.Since(freshnessWaitStarted))
		}
	}
	if err != nil {
		return nil, err
	}

	answerProcessingStarted := time.Now()
	answers, err := explore.ParseAnswers(result.Text)
	if err != nil {
		timing.recordSimple("answer_processing", time.Since(answerProcessingStarted))
		return nil, err
	}
	history := result.History()
	results := make(map[string]explore.BatchResult, len(answers))
	for id, items := range answers {
		results[id] = explore.BatchResult{Items: items, History: history}
	}
	timing.recordSimple("answer_processing", time.Since(answerProcessingStarted))
	if cfg.Debug {
		a.writeDebugEvent("explore_summary", slog.Int("batch_size", len(items)), slog.Int("tool_calls", result.ToolCalls))
	}
	return results, nil
}

func exploreUsageError() error {
	var usage strings.Builder
	usage.WriteString("Usage: git-agent explore [--debug] [--fast] [--for <diagnose|change|behavior|owner>] [--follow-up <search-id>] <question...>\n\n")
	usage.WriteString("Flags:\n  --debug\n      enable debug output on stderr\n  --fast\n      use priority service tier\n  --for <target>\n      select diagnose, change, behavior, or owner guidance\n  --follow-up <search-id>\n      fork a completed explore search")
	return errors.New(usage.String())
}
