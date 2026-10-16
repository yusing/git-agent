package agent

import (
	"bytes"
	"context"
	_ "embed"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/tools"
	"github.com/yusing/git-agent/internal/trace"
)

//go:embed dist/runtime.mjs
var runtimeBundle []byte

type Runner interface {
	Run(context.Context, Request) (Result, error)
}

type Request struct {
	SystemPrompt          string
	ToolPolicy            string
	Environment           string
	SkillInstructions     string
	ProjectGuidance       string
	DeveloperInstructions string
	UserPrompt            string
	TextFormat            *openai.TextFormat
	MaxSteps              int
	RepairOnValidator     bool
	Input                 []openai.Item
}

type Result struct {
	Text        string
	ToolCalls   int
	RepairCalls int
	messages    []openai.Item
}

// History returns a replayable snapshot of the completed conversation.
func (r Result) History() []openai.Item {
	return slices.Clone(r.messages)
}

type Validator func(string) []string

type TextNormalizer func(string) string

type BudgetStatus struct{ MaxSteps int }
type BudgetDecision struct{ ExtendSteps int }

type Timing struct {
	Phase    string
	Duration time.Duration
	Step     int
	Tool     string
}

// ToolExecutor runs admitted tools and checks whether their backing evidence remains current.
type ToolExecutor interface {
	Execute(context.Context, tools.Invocation) (tools.Result, error)
	CheckSnapshot() error
}

type BudgetHandler func(context.Context, BudgetStatus) (BudgetDecision, error)

type OpenAIRunner struct {
	Config         config.Config
	Tools          ToolExecutor
	ToolSpecs      []tools.Definition
	Validator      Validator
	Normalize      TextNormalizer
	Trace          *trace.Recorder
	Budget         BudgetHandler
	PromptCacheKey string
	UsageOutput    io.Writer
	Timing         func(Timing)
}

type runtimeEvent struct {
	Type       string         `json:"type"`
	ID         int            `json:"id"`
	Name       string         `json:"name"`
	Args       jsontext.Value `json:"args"`
	Text       string         `json:"text"`
	Message    string         `json:"message"`
	Phase      string         `json:"phase"`
	Kind       string         `json:"kind"`
	Value      jsontext.Value `json:"value"`
	Step       int            `json:"step"`
	DurationMS float64        `json:"duration_ms"`
	Usage      struct {
		Input   int64 `json:"inputTokens"`
		Output  int64 `json:"outputTokens"`
		Details struct {
			Cached  int64 `json:"cached_tokens"`
			Written int64 `json:"cache_write_tokens"`
		} `json:"inputTokensDetails"`
	} `json:"usage"`
	ToolCalls   int           `json:"toolCalls"`
	RepairCalls int           `json:"repairCalls"`
	History     []openai.Item `json:"history"`
	Budget      BudgetStatus  `json:"budget"`
}

// Run delegates model execution and history to the embedded Agents SDK runtime.
// Only fixed runtime code is launched; model-generated code stays in QuickJS.
func (r *OpenAIRunner) Run(ctx context.Context, request Request) (Result, error) {
	if request.MaxSteps <= 0 {
		request.MaxSteps = r.Config.MaxSteps
	}
	messages, specs, instructions := r.requestContext(request)
	dir, err := os.MkdirTemp("", "git-agent-runtime-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(dir)
	runtimePath := filepath.Join(dir, "runtime.mjs")
	if err := os.WriteFile(runtimePath, runtimeBundle, 0o600); err != nil {
		return Result{}, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(runCtx, "node", runtimePath)
	command.WaitDelay = 2 * time.Second
	stdin, err := command.StdinPipe()
	if err != nil {
		return Result{}, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return Result{}, fmt.Errorf("start Agents SDK runtime (Node.js 22.12+ required): %w", err)
	}
	var writeMu sync.Mutex
	send := func(value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return json.MarshalEncode(jsontext.NewEncoder(stdin), value, jsonv1.FormatDurationAsNano(true), json.Deterministic(true))
	}
	err = send(map[string]any{"config": r.Config, "instructions": instructions, "input": messages, "tools": specs, "cacheKey": r.PromptCacheKey, "maxSteps": request.MaxSteps, "repair": request.RepairOnValidator, "textFormat": request.TextFormat, "finalPrompt": strings.TrimSpace(forcedFinalizationPrompt)})
	var mu sync.Mutex
	var failure error
	fail := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		if failure == nil {
			failure = err
		}
		mu.Unlock()
		cancel()
	}
	if err != nil {
		fail(err)
	}
	var outputMu sync.Mutex
	timing := func(event Timing) {
		outputMu.Lock()
		defer outputMu.Unlock()
		if r.Timing != nil {
			r.Timing(event)
		}
	}
	var providerStarted time.Time
	var workers sync.WaitGroup
	cancellations := make(map[int]context.CancelFunc)
	var result Result
	finished := false
	decoder := jsontext.NewDecoder(stdout)
	for {
		var event runtimeEvent
		if err := json.UnmarshalDecode(decoder, &event); err != nil {
			if !errors.Is(err, io.EOF) {
				fail(fmt.Errorf("decode SDK runtime event: %w", err))
			}
			break
		}
		switch event.Type {
		case "tool":
			if r.Tools == nil || !slices.ContainsFunc(specs, func(spec tools.Definition) bool { return spec.Name == event.Name }) {
				fail(fmt.Errorf("runtime requested unregistered tool %q", event.Name))
				break
			}
			toolCtx, stop := context.WithCancel(runCtx)
			mu.Lock()
			cancellations[event.ID] = stop
			mu.Unlock()
			workers.Go(func() {
				defer stop()
				defer func() { mu.Lock(); delete(cancellations, event.ID); mu.Unlock() }()
				started := time.Now()
				if err := r.Trace.Write("tool-call", map[string]any{"name": event.Name, "arguments": string(event.Args)}); err != nil {
					fail(err)
					return
				}
				output, err := r.Tools.Execute(toolCtx, tools.Invocation{Name: event.Name, Arguments: string(event.Args)})
				if snapshotErr := r.Tools.CheckSnapshot(); snapshotErr != nil {
					fail(snapshotErr)
					return
				}
				if runCtx.Err() != nil {
					return
				}
				if toolCtx.Err() != nil {
					return
				}
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, gitctx.ErrChangeSnapshotStale) {
						fail(err)
						return
					}
					output, err = tools.ErrorResult(event.Name, err)
				}
				if err != nil {
					fail(err)
					return
				}
				timing(Timing{Phase: "tool", Duration: time.Since(started), Step: event.Step, Tool: event.Name})
				if err := r.Trace.Write("tool-output", map[string]any{"name": event.Name, "content": output.Content, "truncated": output.Truncated}); err != nil {
					fail(err)
					return
				}
				fail(send(map[string]any{"id": event.ID, "result": jsontext.Value(output.Content)}))
			})
		case "cancel-tool":
			mu.Lock()
			stop := cancellations[event.ID]
			mu.Unlock()
			if stop != nil {
				stop()
			}
		case "snapshot":
			if r.Tools != nil {
				if err := r.Tools.CheckSnapshot(); err != nil {
					fail(err)
					break
				}
			}
			fail(send(map[string]any{"id": event.ID, "result": true}))
		case "validate":
			started := time.Now()
			text := event.Text
			if r.Normalize != nil {
				text = r.Normalize(text)
			}
			var validationErrors []string
			if r.Validator != nil {
				validationErrors = r.Validator(text)
			}
			timing(Timing{Phase: "validation", Duration: time.Since(started)})
			fail(send(map[string]any{"id": event.ID, "result": map[string]any{"text": text, "errors": validationErrors, "repairPrompt": renderRepairPrompt(validationErrors)}}))
		case "budget":
			var decision BudgetDecision
			var err error
			if r.Budget != nil {
				decision, err = r.Budget(runCtx, event.Budget)
			}
			if err != nil {
				fail(err)
				break
			}
			fail(send(map[string]any{"id": event.ID, "result": decision}))
		case "usage":
			if !providerStarted.IsZero() {
				timing(Timing{Phase: "provider_request", Duration: time.Since(providerStarted), Step: event.Step})
			}
			outputMu.Lock()
			r.writeUsageMetrics(event.Step, openai.Usage{InputTokens: event.Usage.Input, OutputTokens: event.Usage.Output, CachedInputTokens: event.Usage.Details.Cached, CacheWriteInputTokens: event.Usage.Details.Written})
			outputMu.Unlock()
		case "progress":
			if event.Phase == "requesting" || event.Phase == "compacting" {
				providerStarted = time.Now()
			}
			fail(r.Trace.Write("runtime-status", map[string]any{"phase": event.Phase, "model": r.Config.Model}))
		case "timing":
			timing(Timing{Phase: event.Phase, Duration: time.Duration(event.DurationMS * float64(time.Millisecond)), Step: event.Step})
		case "trace":
			fail(r.Trace.Write(event.Kind, event.Value))
		case "error":
			fail(errors.New(event.Message))
		case "result":
			result = Result{Text: event.Text, ToolCalls: event.ToolCalls, RepairCalls: event.RepairCalls, messages: event.History}
			finished = true
		default:
			fail(fmt.Errorf("unknown SDK runtime event %q", event.Type))
		}
	}
	_ = stdin.Close()
	waitErr := command.Wait()
	cancel()
	workers.Wait()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	mu.Lock()
	err = failure
	mu.Unlock()
	if err == nil && waitErr != nil {
		err = fmt.Errorf("SDK runtime: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	if err == nil && !finished {
		err = errors.New("SDK runtime ended without a result")
	}
	if err != nil {
		message := err.Error()
		for _, secret := range []string{r.Config.APIKey, r.Config.AuthAccountID} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[REDACTED]")
			}
		}
		if message == err.Error() {
			return Result{}, err
		}
		return Result{}, errors.New(message)
	}
	return result, nil
}

func (r *OpenAIRunner) requestContext(request Request) ([]openai.Item, []tools.Definition, string) {
	messages := slices.Clone(request.Input)
	if request.Input == nil {
		if request.ToolPolicy != "" {
			messages = append(messages, openai.NewMessage("developer", request.ToolPolicy))
		}
		if request.Environment != "" {
			messages = append(messages, openai.NewMessage("developer", request.Environment))
		}
		if request.SkillInstructions != "" {
			messages = append(messages, openai.NewMessage("developer", request.SkillInstructions))
		}
		if request.ProjectGuidance != "" {
			messages = append(messages, openai.NewMessage("developer", request.ProjectGuidance))
		}
		if request.DeveloperInstructions != "" {
			messages = append(messages, openai.NewMessage("developer", request.DeveloperInstructions))
		}
		messages = append(messages, openai.NewMessage("user", request.UserPrompt))
	}

	return messages, r.ToolSpecs, requestInstructions(request.SystemPrompt, r.ToolSpecs)
}

func (r *OpenAIRunner) writeUsageMetrics(step int, usage openai.Usage) {
	if r.UsageOutput == nil || usage == (openai.Usage{}) {
		return
	}
	_ = trace.WriteConsoleDiagnostic(r.UsageOutput, "llm.usage",
		slog.Int("step", step),
		slog.Int64("input_tokens", usage.InputTokens),
		slog.Int64("cached_input_tokens", usage.CachedInputTokens),
		slog.Int64("cache_write_input_tokens", usage.CacheWriteInputTokens),
		slog.Int64("output_tokens", usage.OutputTokens),
	)
}

func requestInstructions(taskInstructions string, toolSpecs []tools.Definition) string {
	prefix := taskInstructions
	if prefix != "" {
		prefix += "\n\n"
	}
	if len(toolSpecs) == 0 {
		return prefix + strings.TrimSpace(noToolsPrompt)
	}
	promptTools := make([]requestPromptTool, 0, len(toolSpecs))
	for _, spec := range toolSpecs {
		parameters, _ := json.Marshal(spec.Schema, json.Deterministic(true))
		promptTools = append(promptTools, requestPromptTool{Name: spec.Name, Description: spec.Description, Parameters: string(parameters)})
	}
	return prefix + renderRequestPrompt(requestPromptData{
		Tools: promptTools,
		ReadFileAvailable: slices.ContainsFunc(toolSpecs, func(spec tools.Definition) bool {
			return spec.Name == "read_file"
		}),
	})
}
