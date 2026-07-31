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
	fs := flag.NewFlagSet("explore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var followUpID string
	fs.StringVar(&followUpID, "follow-up", "", "fork a completed explore search")
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

	repo, err := gitctx.Open(".")
	if err != nil {
		return err
	}
	if err := migrateProjectMetadata(repo.RootPath); err != nil {
		return err
	}
	identity := projectidentity.FromRepository(repo)
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
		parent, err = store.FollowUpParent(followUpID)
		if err != nil {
			return err
		}
	}

	workspace, err := os.Getwd()
	if err != nil {
		return err
	}
	coordinator := explore.NewCoordinator(store, workspace)
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
			return a.prepareExploreSearch(searchCtx, question)
		}
	}
	output, err := coordinator.Run(ctx, parent, question, prepare, func(
		batchCtx context.Context, batchParent *explore.Session, items []explore.BatchItem,
	) (map[string]explore.BatchResult, error) {
		return a.runExploreBatch(batchCtx, repo, batchParent, items)
	})
	if err != nil {
		return err
	}
	return writeJSONOutput(a.stdout, output)
}

func (a *App) prepareExploreSearch(ctx context.Context, question string) (explore.Prepared, error) {
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
		client = openai.NewHTTPClient(&http.Client{Timeout: embeddingCfg.Timeout})
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

func (a *App) runExploreBatch(ctx context.Context, repo *gitctx.Repository, parent *explore.Session, items []explore.BatchItem) (map[string]explore.BatchResult, error) {
	cfg, err := config.Resolve(config.Options{})
	if err != nil {
		return nil, err
	}
	taskCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

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
		renderedGuidance, err = resolveGuidanceForPaths(repo, cfg.GuidanceFamily, guidancePaths)
		if err != nil {
			return nil, err
		}
	}
	registry := tools.NewReviewRegistry(repo, nil, tools.ReviewModeCodebase, tools.ReviewScope{}, gitctx.ChangeFingerprint{})
	toolSpecs := registry.Definitions(tools.ExploreToolNames())
	allowedTools := toolDefinitionNames(toolSpecs)
	recorder, err := trace.NewStream("explore", a.stderr)
	if err != nil {
		return nil, err
	}
	parentSearchID := ""
	if parent != nil {
		parentSearchID = parent.ID
	}
	if err := recorder.Write("session", map[string]any{
		"command": "explore", "batch_size": len(items), "parent_id": parentSearchID, "repo": repo.Summary(),
	}); err != nil {
		return nil, err
	}
	client := a.responseClient
	if client == nil {
		client = openai.NewHTTPClient(&http.Client{Timeout: cfg.Timeout})
	}
	runner := agent.OpenAIRunner{
		Config: cfg, Client: client, Tools: registry, ToolSpecs: toolSpecs,
		Validator: explore.ValidateAnswers(itemIDs), Trace: recorder, Budget: a.budgetHandler(),
	}
	request := agent.Request{
		SystemPrompt: explore.SystemPrompt, TextFormat: explore.TextFormat(), AllowedToolNames: allowedTools,
		MaxSteps: cfg.MaxSteps, RepairOnValidator: true,
	}
	if parent == nil {
		request.ToolPolicy = toolPolicy()
		request.Environment = environmentContext(repo, "explore", "codebase", cfg.GuidanceFamily, cfg.MaxSteps, cfg.MaxToolCalls)
		request.ProjectGuidance = renderedGuidance
		request.UserPrompt = userPrompt
	} else {
		request.Input = explore.FollowUpInput(*parent, userPrompt)
	}
	result, err := runner.Run(taskCtx, request)
	if err != nil {
		return nil, err
	}
	answers, err := explore.ParseAnswers(result.Text)
	if err != nil {
		return nil, err
	}
	history := result.History()
	results := make(map[string]explore.BatchResult, len(answers))
	for id, answer := range answers {
		results[id] = explore.BatchResult{Answer: answer, History: history}
	}
	if cfg.Debug {
		a.writeDebugEvent("explore_summary", slog.Int("batch_size", len(items)), slog.Int("tool_calls", result.ToolCalls))
	}
	return results, nil
}

func exploreUsageError() error {
	var usage strings.Builder
	usage.WriteString("Usage: git-agent explore [--follow-up <search-id>] <question...>\n\n")
	usage.WriteString("Flags:\n  --follow-up <search-id>\n      fork a completed explore search")
	return errors.New(usage.String())
}
