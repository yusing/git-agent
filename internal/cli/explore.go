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
	var fast bool
	var debug bool
	fs.StringVar(&followUpID, "follow-up", "", "fork a completed explore search")
	fs.BoolVar(&fast, "fast", false, "use priority service tier")
	fs.BoolVar(&debug, "debug", false, "enable debug output on stderr")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exploreUsageError()
		}
		return err
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
	if followUpID != "" {
		parent, err = store.FollowUpParent(followUpID, workspace)
		if err != nil {
			return err
		}
	}

	coordinator := explore.NewCoordinator(store, workspace, fast)
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

func (a *App) prepareExploreSearch(ctx context.Context, question string, timing *explorePhaseTrace) (explore.Prepared, error) {
	root, err := os.Getwd()
	if err != nil {
		return explore.Prepared{}, err
	}
	embeddingCfg, err := config.ResolveEmbeddings(config.Options{})
	if err != nil {
		return explore.Prepared{}, err
	}
	dimensions, err := config.ResolveEmbeddingDimensions(0)
	if err != nil {
		return explore.Prepared{}, err
	}
	maxInput, err := config.ResolveEmbeddingMaxInput(searchtask.DefaultEmbeddingMaxInputChars)
	if err != nil {
		return explore.Prepared{}, err
	}
	batchInputs, err := config.ResolveEmbeddingBatchInputs(searchtask.DefaultEmbeddingBatchInputs)
	if err != nil {
		return explore.Prepared{}, err
	}
	batchMaxChars, err := config.ResolveEmbeddingBatchMaxChars(searchtask.DefaultEmbeddingBatchMaxChars)
	if err != nil {
		return explore.Prepared{}, err
	}
	concurrency, err := config.ResolveEmbeddingConcurrency(0)
	if err != nil {
		return explore.Prepared{}, err
	}
	fileCfg, err := config.LoadFile()
	if err != nil {
		return explore.Prepared{}, err
	}
	client := a.embeddingClient
	if client == nil {
		client = openai.NewHTTPClient(&http.Client{})
	}
	lastStatus := ""
	output, err := searchtask.Run(ctx, client, searchtask.Options{
		Root: root, IndexRemote: fileCfg.Index.Remote, MinScore: searchtask.DefaultMinScore,
		Limit: searchtask.DefaultLimit, CodeOnly: true,
		EmbeddingModel: config.ResolveEmbeddingModel(""), EmbeddingDimensions: dimensions,
		EmbeddingMaxInput: maxInput, EmbeddingBatchInputs: batchInputs,
		EmbeddingBatchMaxChars: batchMaxChars, EmbeddingConcurrency: concurrency,
		APIKey: embeddingCfg.APIKey, BaseURL: embeddingCfg.BaseURL,
		ProgressLog: func(progress searchtask.Progress) error {
			status := strings.TrimSpace(progress.Status)
			if status == "" || status == lastStatus {
				return nil
			}
			lastStatus = status
			_, writeErr := fmt.Fprintf(a.stderr, "explore search: %s\n", status)
			return writeErr
		},
	}, question)
	for _, phase := range output.Diagnostics.Timings {
		timing.recordSimple("semantic_search."+phase.Step, phase.Duration)
	}
	if err != nil {
		return explore.Prepared{}, err
	}
	semantic, err := sonic.ConfigStd.Marshal(output)
	if err != nil {
		return explore.Prepared{}, fmt.Errorf("encode explore semantic results: %w", err)
	}
	paths := make([]string, 0, len(output.Results))
	for _, result := range output.Results {
		if result.Path != "" && !slices.Contains(paths, result.Path) {
			paths = append(paths, result.Path)
		}
	}
	return explore.Prepared{SemanticResults: string(semantic), GuidancePaths: paths}, nil
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
	runner := agent.OpenAIRunner{
		Config: cfg, Client: client, Tools: registry, ToolSpecs: toolSpecs,
		Validator: explore.ValidateAnswers(itemIDs), Trace: recorder, Budget: a.budgetHandler(),
		PromptCacheKey: explore.PromptCacheKey(parent, items),
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
		SystemPrompt: explore.SystemPrompt, TextFormat: explore.TextFormat(), AllowedToolNames: allowedTools,
		ParallelToolCalls: true, MaxSteps: cfg.MaxSteps, RepairOnValidator: true,
	}
	if parent == nil {
		request.ToolPolicy = toolPolicy()
		request.Environment = environmentContextForRoot(root, root, "explore", "codebase", cfg.GuidanceFamily, cfg.MaxSteps, cfg.MaxToolCalls)
		request.ProjectGuidance = renderedGuidance
		request.UserPrompt = userPrompt
	} else {
		request.Input = explore.FollowUpInput(*parent, userPrompt)
	}
	timing.recordSimple("prompt_setup", time.Since(promptSetupStarted))

	agentStarted := time.Now()
	result, err := runner.Run(ctx, request)
	timing.recordSimple("agent", time.Since(agentStarted))
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
	for id, answer := range answers {
		results[id] = explore.BatchResult{Answer: answer, History: history}
	}
	timing.recordSimple("answer_processing", time.Since(answerProcessingStarted))
	if cfg.Debug {
		a.writeDebugEvent("explore_summary", slog.Int("batch_size", len(items)), slog.Int("tool_calls", result.ToolCalls))
	}
	return results, nil
}

func exploreUsageError() error {
	var usage strings.Builder
	usage.WriteString("Usage: git-agent explore [--debug] [--fast] [--follow-up <search-id>] <question...>\n\n")
	usage.WriteString("Flags:\n  --debug\n      enable debug output on stderr\n  --fast\n      use priority service tier\n  --follow-up <search-id>\n      fork a completed explore search")
	return errors.New(usage.String())
}
