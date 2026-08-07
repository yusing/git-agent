package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/provider"
	"github.com/yusing/git-agent/internal/tools"
	"github.com/yusing/git-agent/internal/trace"
)

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
	AllowedToolNames      []string
	ParallelToolCalls     bool
	MaxSteps              int
	RepairOnValidator     bool
	Input                 []openai.Item
	ControlTool           *tools.Definition
}

type Result struct {
	Text            string
	ToolCalls       int
	RepairCalls     int
	ToolCallsByName map[string]int
	UsedSkills      []string
	messages        []openai.Item
}

// History returns a replayable snapshot of the completed conversation.
func (r Result) History() []openai.Item {
	return slices.Clone(r.messages)
}

type NodeResult struct {
	Final  *Result
	Branch *BranchRequest
}

type BranchRequest struct {
	CallID          string
	Arguments       string
	ToolCalls       int
	RepairCalls     int
	ToolCallsByName map[string]int
	UsedSkills      []string
	messages        []openai.Item
}

func (b BranchRequest) ForkInput(output string, sameModel bool) []openai.Item {
	messages := slices.Clone(b.messages)
	if !sameModel {
		messages = openai.PortableItems(messages)
	}
	messages = append(messages, openai.NewFunctionCallOutput(b.CallID, output))
	return messages
}

type Validator func(string) []string

type TextNormalizer func(string) string

type BudgetKind string

const (
	BudgetKindModelSteps BudgetKind = "model_steps"
	BudgetKindToolCalls  BudgetKind = "tool_calls"
	BudgetKindNoProgress BudgetKind = "no_progress"
	BudgetKindContext    BudgetKind = "context"
)

type BudgetStatus struct {
	Kind          BudgetKind
	Limit         int
	Used          int
	Step          int
	MaxSteps      int
	MaxToolCalls  int
	RequestedTool string
}

type BudgetDecision struct {
	ExtendSteps     int
	ExtendToolCalls int
}

type Timing struct {
	Phase    string
	Duration time.Duration
	Step     int
	Tool     string
}

type BudgetHandler func(context.Context, BudgetStatus) (BudgetDecision, error)

type OpenAIRunner struct {
	Config             config.Config
	Client             openai.Client
	Tools              *tools.Registry
	ToolSpecs          []tools.Definition
	HostedCapabilities []provider.HostedCapability
	Validator          Validator
	Normalize          TextNormalizer
	Trace              *trace.Recorder
	Budget             BudgetHandler
	ReasoningSummary   string
	PromptCacheKey     string
	ObserveUsage       func(openai.Usage)
	UsageOutput        io.Writer
	Timing             func(Timing)
}

type runState struct {
	hostedCapabilities []provider.HostedCapability
}

func (r *OpenAIRunner) Run(ctx context.Context, request Request) (Result, error) {
	outcome, err := r.RunNode(ctx, request)
	if err != nil {
		return Result{}, err
	}
	if outcome.Branch != nil {
		return Result{}, errors.New("branch outcome requires a tree coordinator")
	}
	if outcome.Final == nil {
		return Result{}, errors.New("agent returned no node outcome")
	}
	return *outcome.Final, nil
}

func (r *OpenAIRunner) RunNode(ctx context.Context, request Request) (NodeResult, error) {
	if r.Client == nil {
		return NodeResult{}, errors.New("openai client is required")
	}
	if request.MaxSteps <= 0 {
		request.MaxSteps = r.Config.MaxSteps
	}

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

	toolSpecs := make([]openai.ToolSpec, 0, len(r.ToolSpecs)+1)
	for _, def := range r.ToolSpecs {
		if len(request.AllowedToolNames) > 0 && !slices.Contains(request.AllowedToolNames, def.Name) {
			continue
		}
		toolSpecs = append(toolSpecs, openai.ToolSpec{Name: def.Name, Description: def.Description, Schema: def.Schema, Strict: def.Strict})
	}
	controlToolName := ""
	if request.ControlTool != nil {
		def := request.ControlTool
		controlToolName = def.Name
		toolSpecs = append(toolSpecs, openai.ToolSpec{Name: def.Name, Description: def.Description, Schema: def.Schema, Strict: def.Strict})
	}

	state := &runState{hostedCapabilities: slices.Clone(r.HostedCapabilities)}
	parallelToolCalls := request.ParallelToolCalls
	stableInstructions := requestInstructions(request.SystemPrompt, toolSpecs, state.hostedCapabilities)
	runResult, err := r.runUntilOutcome(ctx, stableInstructions, messages, toolSpecs, request.TextFormat, request.MaxSteps, state, controlToolName, parallelToolCalls)
	if err != nil {
		return NodeResult{}, err
	}
	if runResult.Branch != nil {
		return runResult, nil
	}
	if runResult.Final == nil {
		return NodeResult{}, errors.New("agent returned no node outcome")
	}
	result := *runResult.Final
	r.normalizeResult(&result)

	if r.Validator != nil {
		var validationStarted time.Time
		if r.Timing != nil {
			validationStarted = time.Now()
		}
		errs := r.Validator(result.Text)
		if r.Timing != nil {
			r.Timing(Timing{Phase: "validation", Duration: time.Since(validationStarted)})
		}
		if len(errs) > 0 {
			if !request.RepairOnValidator {
				return NodeResult{}, fmt.Errorf("validation failed: %v", errs)
			}
			repairMessages := slices.Clone(result.messages)
			if len(repairMessages) == 0 {
				repairMessages = append(slices.Clone(messages), openai.NewMessage("assistant", result.Text))
			}
			repairMessages = append(repairMessages, openai.NewMessage("user", renderRepairPrompt(errs)))
			var repairStarted time.Time
			if r.Timing != nil {
				repairStarted = time.Now()
			}
			repairedOutcome, err := r.runUntilOutcome(ctx, stableInstructions, repairMessages, nil, request.TextFormat, 1, state, "", false)
			if r.Timing != nil {
				r.Timing(Timing{Phase: "repair", Duration: time.Since(repairStarted)})
			}
			if err != nil {
				return NodeResult{}, err
			}
			if repairedOutcome.Branch != nil {
				return NodeResult{}, errors.New("provider branched during schema repair")
			}
			if repairedOutcome.Final == nil {
				return NodeResult{}, errors.New("schema repair returned no outcome")
			}
			repaired := *repairedOutcome.Final
			result.RepairCalls++
			result.Text = repaired.Text
			result.messages = slices.Clone(repaired.messages)
			r.normalizeResult(&result)
			if r.Timing != nil {
				validationStarted = time.Now()
			}
			errs = r.Validator(result.Text)
			if r.Timing != nil {
				r.Timing(Timing{Phase: "validation", Duration: time.Since(validationStarted)})
			}
			if len(errs) > 0 {
				return NodeResult{}, fmt.Errorf("validation failed after repair: %v", errs)
			}
		}
	}
	return NodeResult{Final: &result}, nil
}

func (r *OpenAIRunner) normalizeResult(result *Result) {
	if r.Normalize != nil {
		result.Text = r.Normalize(result.Text)
	}
}

func (r *OpenAIRunner) runUntilOutcome(ctx context.Context, stableInstructions string, messages []openai.Item, toolSpecs []openai.ToolSpec, textFormat *openai.TextFormat, maxSteps int, state *runState, controlToolName string, parallelToolCalls bool) (NodeResult, error) {
	var result Result
	maxToolCalls := r.Config.MaxToolCalls
	started := time.Now()
	seenCalls := map[string]struct{}{}
	seenCallIDs := map[string]struct{}{}
	for index, item := range messages {
		if item.Type != "function_call" {
			continue
		}
		callID, name, arguments, err := functionCallIdentity(item)
		if err != nil {
			return NodeResult{}, fmt.Errorf("input item %d: %w", index, err)
		}
		if _, duplicate := seenCallIDs[callID]; duplicate {
			return NodeResult{}, fmt.Errorf("input item %d: duplicate function call ID %q", index, callID)
		}
		seenCallIDs[callID] = struct{}{}
		seenCalls[toolCallSignature(openai.ToolCall{Name: name, Arguments: arguments})] = struct{}{}
	}
	for step := 0; step < maxSteps; step++ {
		requestMessages := requestInputWithBudget(
			messages, step+1, maxSteps, result.ToolCalls, maxToolCalls, r.PromptCacheKey != "",
		)
		req := r.providerRequest(
			stableInstructions,
			requestMessages,
			toolSpecs,
			state.hostedCapabilities,
			textFormat,
			parallelToolCalls,
		)
		estimatedTokens := estimateRequestTokens(req)
		r.attachRetryStatus(&req, step+1, maxSteps, result.ToolCalls, maxToolCalls, estimatedTokens, started)
		if err := r.writeRuntimeStatus("requesting", step+1, maxSteps, result.ToolCalls, maxToolCalls, estimatedTokens, 0, started); err != nil {
			return NodeResult{}, err
		}
		if step == 0 && r.Config.ContextTokens > 0 && estimatedTokens >= r.Config.ContextTokens {
			if err := r.Trace.Write("budget", map[string]any{
				"kind": BudgetKindContext, "decision": "reject", "reason": "initial_context_budget_exhausted",
				"step": step + 1, "used": estimatedTokens, "limit": r.Config.ContextTokens,
			}); err != nil {
				return NodeResult{}, err
			}
			return NodeResult{}, fmt.Errorf("initial request estimated at %d tokens meets or exceeds context budget %d", estimatedTokens, r.Config.ContextTokens)
		}
		// Local function calls stay under the runner's budget. Any outbound
		// max_tool_calls value belongs only to provider-hosted capabilities.
		if err := writeTraceRequest(r.Trace, req); err != nil {
			return NodeResult{}, err
		}
		timing := r.Timing
		var providerStarted time.Time
		if timing != nil {
			providerStarted = time.Now()
		}
		response, err := r.Client.CreateResponse(ctx, req)
		if timing != nil {
			timing(Timing{Phase: "provider_request", Duration: time.Since(providerStarted), Step: step + 1})
		}
		if err != nil {
			failure, ok := unsupportedEnabledCapability(err, state.hostedCapabilities)
			if ok {
				if err := r.traceCapabilityFailure(failure); err != nil {
					return NodeResult{}, err
				}
				state.hostedCapabilities = removeHostedCapability(state.hostedCapabilities, failure.Capability)
				messages = append(requestMessages, openai.NewMessage("developer", strings.TrimSpace(hostedCapabilityFailurePrompt)))
				step--
				continue
			}
			return NodeResult{}, err
		}
		messages = requestMessages
		usage := response.Usage
		r.observeUsage(usage)
		r.writeUsageMetrics(step+1, usage)
		if err := writeTraceResponse(r.Trace, response); err != nil {
			return NodeResult{}, err
		}
		if err := r.traceHostedToolCalls(response.HostedToolCalls); err != nil {
			return NodeResult{}, err
		}
		inputTokens := int(usage.InputTokens)
		if err := r.writeRuntimeStatus("response_received", step+1, maxSteps, result.ToolCalls, maxToolCalls, estimatedTokens, inputTokens, started); err != nil {
			return NodeResult{}, err
		}
		if r.Config.ContextTokens > 0 && inputTokens >= r.Config.ContextTokens && len(response.ToolCalls) > 0 {
			final, err := r.finalizeForGuard(ctx, stableInstructions, messages, result, textFormat, BudgetStatus{
				Kind: BudgetKindContext, Used: inputTokens, Step: step + 1,
				Limit: r.Config.ContextTokens, MaxSteps: maxSteps, MaxToolCalls: maxToolCalls,
			}, "context_budget_exhausted", started)
			return NodeResult{Final: &final}, err
		}
		if len(response.ToolCalls) == 0 {
			if response.Text == "" {
				return NodeResult{}, errors.New("provider returned no text and no tool calls")
			}
			result.Text = response.Text
			result.messages = appendResponseMessages(messages, response)
			return NodeResult{Final: &result}, nil
		}
		batch, err := validateToolCallBatch(response.ToolCalls, toolSpecs, controlToolName, seenCalls, seenCallIDs)
		if err != nil {
			if r.Tools == nil && !slices.ContainsFunc(response.ToolCalls, func(call openai.ToolCall) bool {
				return controlToolName != "" && call.Name == controlToolName
			}) {
				return NodeResult{}, errors.New("provider requested tools but no registry is configured")
			}
			return NodeResult{}, err
		}
		if r.Tools == nil && (!batch.control || len(batch.calls) > 1) {
			return NodeResult{}, errors.New("provider requested tools but no registry is configured")
		}
		batchMessages := appendToolCallBatch(messages, response, batch.calls)
		if batch.duplicateTool != "" {
			final, err := r.finalizeForGuard(ctx, stableInstructions, batchMessages, result, textFormat, BudgetStatus{
				Kind: BudgetKindNoProgress, Used: result.ToolCalls, Step: step + 1,
				MaxSteps: maxSteps, MaxToolCalls: maxToolCalls, RequestedTool: batch.duplicateTool,
			}, "repeated_tool_call", started)
			return NodeResult{Final: &final}, err
		}
		for maxToolCalls > 0 && result.ToolCalls+len(batch.calls) > maxToolCalls {
			remaining := max(0, maxToolCalls-result.ToolCalls)
			requestedTool := batch.calls[min(remaining, len(batch.calls)-1)].Name
			recovered, updatedSteps, updatedTools, err := r.resolveBudgetExhaustion(ctx, stableInstructions, batchMessages, result, textFormat, BudgetStatus{
				Kind:          BudgetKindToolCalls,
				Limit:         maxToolCalls,
				Used:          result.ToolCalls,
				Step:          step + 1,
				MaxSteps:      maxSteps,
				MaxToolCalls:  maxToolCalls,
				RequestedTool: requestedTool,
			}, started)
			if err != nil {
				return NodeResult{}, err
			}
			if recovered.Text != "" {
				return NodeResult{Final: &recovered}, nil
			}
			maxSteps = updatedSteps
			maxToolCalls = updatedTools
		}
		messages = batchMessages
		for signature := range batch.signatures {
			seenCalls[signature] = struct{}{}
		}
		for _, call := range batch.calls {
			seenCallIDs[call.CallID] = struct{}{}
		}
		localCalls := make([]openai.ToolCall, 0, len(batch.calls))
		var controlCall openai.ToolCall
		for _, call := range batch.calls {
			if controlToolName != "" && call.Name == controlToolName {
				controlCall = call
				continue
			}
			localCalls = append(localCalls, call)
		}
		for _, call := range localCalls {
			if err := r.Trace.Write("tool-call", call); err != nil {
				return NodeResult{}, err
			}
		}
		timing = r.Timing
		var toolBatchStarted time.Time
		if timing != nil && len(localCalls) > 0 {
			toolBatchStarted = time.Now()
		}
		executions := r.executeToolCalls(ctx, localCalls, timing != nil)
		if timing != nil && len(localCalls) > 0 {
			for index, call := range localCalls {
				timing(Timing{Phase: "tool", Duration: executions[index].duration, Step: step + 1, Tool: call.Name})
			}
			timing(Timing{Phase: "tool_batch", Duration: time.Since(toolBatchStarted), Step: step + 1})
		}
		if len(localCalls) > 0 {
			if err := r.Tools.CheckReviewSnapshot(); err != nil {
				return NodeResult{}, err
			}
		}
		for index, call := range localCalls {
			toolResult := executions[index].result
			err := executions[index].err
			toolSucceeded := err == nil
			if ctxErr := ctx.Err(); ctxErr != nil {
				return NodeResult{}, fmt.Errorf("tool %s canceled: %w", call.Name, ctxErr)
			}
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, gitctx.ErrChangeSnapshotStale) {
					return NodeResult{}, fmt.Errorf("tool %s failed: %w", call.Name, err)
				}
				toolResult, err = tools.ErrorResult(call.Name, err)
				if err != nil {
					return NodeResult{}, fmt.Errorf("encode tool %s error: %w", call.Name, err)
				}
			}
			if err := r.Trace.Write("tool-output", map[string]any{
				"name":      call.Name,
				"call_id":   call.CallID,
				"content":   toolResult.Content,
				"truncated": toolResult.Truncated,
			}); err != nil {
				return NodeResult{}, err
			}
			result.ToolCalls++
			result.ToolCallsByName = addToolCall(result.ToolCallsByName, call.Name)
			if skill, ok := tools.UsedSkill(tools.Invocation{Name: call.Name, Arguments: call.Arguments}); toolSucceeded && ok && !slices.Contains(result.UsedSkills, skill) {
				result.UsedSkills = append(result.UsedSkills, skill)
			}
			messages = append(messages, openai.NewFunctionCallOutput(call.CallID, toolResult.Content))
		}
		if batch.control {
			result.ToolCalls++
			result.ToolCallsByName = addToolCall(result.ToolCallsByName, controlCall.Name)
			return NodeResult{Branch: &BranchRequest{
				CallID: controlCall.CallID, Arguments: controlCall.Arguments,
				ToolCalls: result.ToolCalls, RepairCalls: result.RepairCalls,
				ToolCallsByName: result.ToolCallsByName,
				UsedSkills:      slices.Clone(result.UsedSkills), messages: messages,
			}}, nil
		}
		nextRequest := r.providerRequest(stableInstructions, requestInputWithBudget(messages, step+2, maxSteps, result.ToolCalls, maxToolCalls, r.PromptCacheKey != ""), toolSpecs, state.hostedCapabilities, textFormat, parallelToolCalls)
		nextTokens := estimateRequestTokens(nextRequest)
		if r.Config.ContextTokens > 0 && nextTokens >= r.Config.ContextTokens {
			final, err := r.finalizeForGuard(ctx, stableInstructions, messages, result, textFormat, BudgetStatus{
				Kind: BudgetKindContext, Used: nextTokens, Step: step + 1,
				Limit: r.Config.ContextTokens, MaxSteps: maxSteps, MaxToolCalls: maxToolCalls,
			}, "context_budget_exhausted", started)
			return NodeResult{Final: &final}, err
		}
		if step == maxSteps-1 {
			recovered, updatedSteps, updatedTools, err := r.resolveBudgetExhaustion(ctx, stableInstructions, messages, result, textFormat, BudgetStatus{
				Kind:         BudgetKindModelSteps,
				Limit:        maxSteps,
				Used:         step + 1,
				Step:         step + 1,
				MaxSteps:     maxSteps,
				MaxToolCalls: maxToolCalls,
			}, started)
			if err != nil {
				return NodeResult{}, err
			}
			if recovered.Text != "" {
				return NodeResult{Final: &recovered}, nil
			}
			maxSteps = updatedSteps
			maxToolCalls = updatedTools
		}
	}
	return NodeResult{}, fmt.Errorf("agent exceeded maximum model steps (%d)", maxSteps)
}

func (r *OpenAIRunner) finalizeForGuard(ctx context.Context, instructions string, messages []openai.Item, current Result, textFormat *openai.TextFormat, status BudgetStatus, reason string, started time.Time) (Result, error) {
	finalized, err := r.finalizeWithoutTools(ctx, instructions, messages, textFormat, status, current.ToolCalls, started)
	if err != nil {
		return Result{}, err
	}
	finalized.copyActivity(current)
	if err := r.Trace.Write("budget", map[string]any{
		"kind": status.Kind, "decision": "finalize", "reason": reason,
		"step": status.Step, "used": status.Used, "limit": status.Limit,
		"tool": status.RequestedTool,
	}); err != nil {
		return Result{}, err
	}
	return finalized, nil
}

func (r *OpenAIRunner) writeRuntimeStatus(phase string, step, maxSteps, toolCalls, maxToolCalls, estimatedTokens, inputTokens int, started time.Time) error {
	if r.Trace == nil {
		return nil
	}
	return r.Trace.Write("runtime.status", r.runtimeStatusValue(phase, step, maxSteps, toolCalls, maxToolCalls, estimatedTokens, inputTokens, started))
}

func (r *OpenAIRunner) attachRetryStatus(request *openai.Request, step, maxSteps, toolCalls, maxToolCalls, estimatedTokens int, started time.Time) {
	if r.Trace == nil {
		return
	}
	request.OnRetry = func(event openai.RetryEvent) error {
		value := r.runtimeStatusValue("retrying_provider", step, maxSteps, toolCalls, maxToolCalls, estimatedTokens, 0, started)
		value["retry_attempt"] = event.Attempt
		value["max_retry_attempts"] = event.MaxAttempts
		value["retry_reason"] = event.Reason
		value["abandoned_provider_attempt"] = event.Attempt
		value["provider_attempt"] = event.Attempt + 1
		return r.Trace.Write("runtime.status", value)
	}
}

func (r *OpenAIRunner) runtimeStatusValue(phase string, step, maxSteps, toolCalls, maxToolCalls, estimatedTokens, inputTokens int, started time.Time) map[string]any {
	return map[string]any{
		"phase": phase, "step": step, "max_steps": maxSteps,
		"tool_calls": toolCalls, "max_tool_calls": maxToolCalls,
		"elapsed_ms":               time.Since(started).Milliseconds(),
		"estimated_context_tokens": estimatedTokens, "input_tokens": inputTokens,
		"context_budget_tokens": r.Config.ContextTokens,
	}
}

func estimateRequestTokens(request openai.Request) int {
	data, _ := sonic.ConfigStd.Marshal(struct {
		Instructions       string                      `json:"instructions"`
		Input              []openai.Item               `json:"input"`
		Tools              []openai.ToolSpec           `json:"tools"`
		HostedCapabilities []provider.HostedCapability `json:"hosted_capabilities"`
		TextFormat         *openai.TextFormat          `json:"text_format"`
		ParallelToolCalls  bool                        `json:"parallel_tool_calls"`
	}{request.Instructions, request.Input, request.Tools, request.HostedCapabilities, request.TextFormat, request.ParallelToolCalls})
	return (len(data) + 3) / 4
}

func (r *OpenAIRunner) observeUsage(usage openai.Usage) {
	if r.ObserveUsage != nil {
		r.ObserveUsage(usage)
	}
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

func toolCallSignature(call openai.ToolCall) string {
	arguments := strings.TrimSpace(call.Arguments)
	var value any
	if sonic.ConfigStd.UnmarshalFromString(arguments, &value) == nil {
		if canonical, err := sonic.ConfigStd.Marshal(value); err == nil {
			arguments = string(canonical)
		}
	}
	return call.Name + "\x00" + arguments
}

type toolCallBatch struct {
	calls         []openai.ToolCall
	signatures    map[string]struct{}
	control       bool
	duplicateTool string
}

type toolExecution struct {
	result   tools.Result
	err      error
	duration time.Duration
}

func (r *OpenAIRunner) executeToolCalls(ctx context.Context, calls []openai.ToolCall, measureDuration bool) []toolExecution {
	executions := make([]toolExecution, len(calls))
	var wait sync.WaitGroup
	for index, call := range calls {
		wait.Go(func() {
			var started time.Time
			if measureDuration {
				started = time.Now()
			}
			executions[index].result, executions[index].err = r.Tools.Execute(ctx, tools.Invocation{
				Name: call.Name, Arguments: call.Arguments,
			})
			if measureDuration {
				executions[index].duration = time.Since(started)
			}
		})
	}
	wait.Wait()
	return executions
}

func validateToolCallBatch(
	calls []openai.ToolCall,
	toolSpecs []openai.ToolSpec,
	controlToolName string,
	seenCalls map[string]struct{},
	seenCallIDs map[string]struct{},
) (toolCallBatch, error) {
	controlCalls := 0
	for _, call := range calls {
		if controlToolName != "" && call.Name == controlToolName {
			controlCalls++
		}
	}
	if controlCalls > 1 {
		return toolCallBatch{}, fmt.Errorf("%s may be called at most once in a provider response", controlToolName)
	}

	batch := toolCallBatch{
		calls:      make([]openai.ToolCall, 0, len(calls)),
		signatures: make(map[string]struct{}, len(calls)),
		control:    controlCalls == 1,
	}
	batchCallIDs := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		callID := call.CallID
		if callID == "" {
			callID = call.ID
		}
		if callID == "" {
			return toolCallBatch{}, errors.New("function call ID is required")
		}
		if _, duplicate := seenCallIDs[callID]; duplicate {
			return toolCallBatch{}, fmt.Errorf("duplicate function call ID %q", callID)
		}
		if _, duplicate := batchCallIDs[callID]; duplicate {
			return toolCallBatch{}, fmt.Errorf("duplicate function call ID %q within provider response", callID)
		}
		if call.Name == "" {
			return toolCallBatch{}, fmt.Errorf("function call %q name is required", callID)
		}
		if !toolAllowed(call.Name, toolSpecs) {
			return toolCallBatch{}, fmt.Errorf("tool %s is not allowed for this request", call.Name)
		}

		call.CallID = callID
		signature := toolCallSignature(call)
		if batch.duplicateTool == "" {
			_, seenBefore := seenCalls[signature]
			_, repeatedInBatch := batch.signatures[signature]
			if seenBefore || repeatedInBatch {
				batch.duplicateTool = call.Name
			}
		}
		batchCallIDs[callID] = struct{}{}
		batch.signatures[signature] = struct{}{}
		batch.calls = append(batch.calls, call)
	}
	return batch, nil
}

func appendToolCallBatch(messages []openai.Item, response openai.Response, calls []openai.ToolCall) []openai.Item {
	result := append(slices.Clone(messages), response.Continuation...)
	if len(response.Continuation) == 0 {
		for _, call := range calls {
			result = append(result, openai.NewFunctionCall(call))
		}
	}
	return result
}

func (r *OpenAIRunner) resolveBudgetExhaustion(ctx context.Context, instructions string, messages []openai.Item, current Result, textFormat *openai.TextFormat, status BudgetStatus, started time.Time) (Result, int, int, error) {
	if r.Budget != nil {
		decision, err := r.Budget(ctx, status)
		if err != nil {
			return Result{}, 0, 0, err
		}
		extensionApplies := decision.ExtendSteps > 0 || decision.ExtendToolCalls > 0
		switch status.Kind {
		case BudgetKindModelSteps:
			extensionApplies = decision.ExtendSteps > 0
		case BudgetKindToolCalls:
			extensionApplies = decision.ExtendToolCalls > 0
		}
		if extensionApplies {
			nextSteps := status.MaxSteps + max(0, decision.ExtendSteps)
			nextTools := status.MaxToolCalls
			if nextTools > 0 || decision.ExtendToolCalls > 0 {
				nextTools += max(0, decision.ExtendToolCalls)
			}
			if err := r.Trace.Write("budget", map[string]any{
				"kind":                   status.Kind,
				"decision":               "extend",
				"previous_max_steps":     status.MaxSteps,
				"previous_max_toolcalls": status.MaxToolCalls,
				"next_max_steps":         nextSteps,
				"next_max_toolcalls":     nextTools,
			}); err != nil {
				return Result{}, 0, 0, err
			}
			return Result{}, nextSteps, nextTools, nil
		}
	}

	finalized, err := r.finalizeWithoutTools(ctx, instructions, messages, textFormat, status, current.ToolCalls, started)
	if err != nil {
		return Result{}, 0, 0, err
	}
	finalized.copyActivity(current)
	if err := r.Trace.Write("budget", map[string]any{
		"kind":          status.Kind,
		"decision":      "finalize",
		"max_steps":     status.MaxSteps,
		"max_toolcalls": status.MaxToolCalls,
		"used":          status.Used,
	}); err != nil {
		return Result{}, 0, 0, err
	}
	return finalized, status.MaxSteps, status.MaxToolCalls, nil
}

func addToolCall(counts map[string]int, name string) map[string]int {
	if counts == nil {
		counts = map[string]int{}
	}
	counts[name]++
	return counts
}

func (r *Result) copyActivity(source Result) {
	r.ToolCalls = source.ToolCalls
	r.RepairCalls = source.RepairCalls
	r.ToolCallsByName = source.ToolCallsByName
	r.UsedSkills = source.UsedSkills
}

type conversationFunctionCall struct {
	name     string
	answered bool
}

func completePendingFunctionCalls(messages []openai.Item) ([]openai.Item, error) {
	calls := make(map[string]*conversationFunctionCall)
	order := make([]string, 0)
	for index, item := range messages {
		switch item.Type {
		case "function_call":
			callID, name, _, err := functionCallIdentity(item)
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", index, err)
			}
			if _, exists := calls[callID]; exists {
				return nil, fmt.Errorf("input item %d: duplicate function call ID %q", index, callID)
			}
			calls[callID] = &conversationFunctionCall{name: name}
			order = append(order, callID)
		case "function_call_output":
			callID, err := functionCallOutputID(item)
			if err != nil {
				return nil, fmt.Errorf("input item %d: %w", index, err)
			}
			call, exists := calls[callID]
			if !exists {
				return nil, fmt.Errorf("input item %d: function call output %q has no preceding call", index, callID)
			}
			if call.answered {
				return nil, fmt.Errorf("input item %d: duplicate function call output %q", index, callID)
			}
			call.answered = true
		}
	}

	completed := slices.Clone(messages)
	for _, callID := range order {
		call := calls[callID]
		if call.answered {
			continue
		}
		result, err := tools.ErrorResult(call.name, errors.New("tool call was not executed because the agent is finalizing without tools"))
		if err != nil {
			return nil, fmt.Errorf("encode skipped tool output %q: %w", callID, err)
		}
		completed = append(completed, openai.NewFunctionCallOutput(callID, result.Content))
	}
	return completed, nil
}

func functionCallIdentity(item openai.Item) (string, string, string, error) {
	id, callID, name, arguments, err := functionItemFields(item, "function_call")
	if err != nil {
		return "", "", "", err
	}
	if callID == "" {
		callID = id
	}
	if callID == "" {
		return "", "", "", errors.New("function call ID is required")
	}
	if name == "" {
		return "", "", "", fmt.Errorf("function call %q name is required", callID)
	}
	return callID, name, arguments, nil
}

func functionCallOutputID(item openai.Item) (string, error) {
	_, callID, _, _, err := functionItemFields(item, "function_call_output")
	if err != nil {
		return "", err
	}
	if callID == "" {
		return "", errors.New("function call output ID is required")
	}
	return callID, nil
}

func functionItemFields(item openai.Item, expectedType string) (string, string, string, string, error) {
	if item.RawJSON == "" {
		return item.ID, item.CallID, item.Name, item.Arguments, nil
	}
	var raw struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	if err := sonic.ConfigStd.UnmarshalFromString(item.RawJSON, &raw); err != nil {
		return "", "", "", "", fmt.Errorf("decode %s: %w", strings.ReplaceAll(expectedType, "_", " "), err)
	}
	if raw.Type != expectedType {
		return "", "", "", "", fmt.Errorf("raw item type %q does not match %s", raw.Type, expectedType)
	}
	return raw.ID, raw.CallID, raw.Name, raw.Arguments, nil
}

func (r *OpenAIRunner) finalizeWithoutTools(ctx context.Context, instructions string, messages []openai.Item, textFormat *openai.TextFormat, status BudgetStatus, toolCalls int, started time.Time) (Result, error) {
	completedMessages, err := completePendingFunctionCalls(messages)
	if err != nil {
		return Result{}, fmt.Errorf("prepare forced finalization input: %w", err)
	}
	finalMessage := openai.NewMessage("developer", finalizationNotice(status)+"\n\n"+strings.TrimSpace(forcedFinalizationPrompt))
	finalMessage.PromptCacheBreakpoint = r.PromptCacheKey != ""
	finalMessages := append(completedMessages, finalMessage)
	req := r.providerRequest(instructions, finalMessages, nil, nil, textFormat, false)
	r.attachRetryStatus(&req, status.Step, status.MaxSteps, toolCalls, status.MaxToolCalls, estimateRequestTokens(req), started)
	if err := writeTraceRequest(r.Trace, req); err != nil {
		return Result{}, err
	}
	timing := r.Timing
	var providerStarted time.Time
	if timing != nil {
		providerStarted = time.Now()
	}
	response, err := r.Client.CreateResponse(ctx, req)
	if timing != nil {
		timing(Timing{Phase: "provider_request", Duration: time.Since(providerStarted), Step: status.Step})
	}
	if err != nil {
		return Result{}, err
	}
	usage := response.Usage
	r.observeUsage(usage)
	r.writeUsageMetrics(status.Step, usage)
	if err := writeTraceResponse(r.Trace, response); err != nil {
		return Result{}, err
	}
	if len(response.ToolCalls) > 0 {
		return Result{}, fmt.Errorf("provider requested tools during forced finalization")
	}
	if response.Text == "" {
		return Result{}, errors.New("provider returned no text during forced finalization")
	}
	resultMessages := appendResponseMessages(finalMessages, response)
	return Result{Text: response.Text, messages: resultMessages}, nil
}

func appendResponseMessages(messages []openai.Item, response openai.Response) []openai.Item {
	result := append(slices.Clone(messages), response.Continuation...)
	if len(response.Continuation) == 0 {
		result = append(result, openai.NewMessage("assistant", response.Text))
	}
	return result
}

func (r *OpenAIRunner) providerRequest(instructions string, input []openai.Item, toolSpecs []openai.ToolSpec, hostedCapabilities []provider.HostedCapability, textFormat *openai.TextFormat, parallelToolCalls bool) openai.Request {
	request := openai.Request{
		Model:              r.Config.Model,
		ServiceTier:        r.Config.ServiceTier,
		ThinkingMode:       r.Config.ThinkingEffort,
		ReasoningSummary:   r.ReasoningSummary,
		BaseURL:            r.Config.BaseURL,
		APIKey:             r.Config.APIKey,
		AuthAccountID:      r.Config.AuthAccountID,
		Instructions:       instructions,
		PromptCacheKey:     r.PromptCacheKey,
		ParallelToolCalls:  parallelToolCalls,
		Input:              input,
		Tools:              toolSpecs,
		HostedCapabilities: hostedCapabilities,
		TextFormat:         textFormat,
	}
	if r.Trace != nil {
		request.OnStreamEvent = func(event openai.StreamEvent) error {
			return r.Trace.WriteExact(event.Kind, event)
		}
	}
	return request
}

func finalizationNotice(status BudgetStatus) string {
	reason := fmt.Sprintf("model-step budget reached at %d/%d", status.Used, status.Limit)
	switch status.Kind {
	case BudgetKindToolCalls:
		reason = fmt.Sprintf("tool-call budget reached at %d/%d before %q", status.Used, status.Limit, status.RequestedTool)
	case BudgetKindContext:
		reason = fmt.Sprintf("context budget reached at %d/%d tokens", status.Used, status.Limit)
	case BudgetKindNoProgress:
		reason = fmt.Sprintf("no semantic progress before repeated %q tool call", status.RequestedTool)
	}
	return renderBudgetExhaustedPrompt(budgetExhaustedPromptData{
		Reason:       reason,
		MaxSteps:     status.MaxSteps,
		MaxToolCalls: status.MaxToolCalls,
	})
}

func toolAllowed(name string, toolSpecs []openai.ToolSpec) bool {
	return slices.ContainsFunc(toolSpecs, func(spec openai.ToolSpec) bool {
		return spec.Name == name
	})
}

func unsupportedEnabledCapability(err error, capabilities []provider.HostedCapability) (provider.CapabilityFailure, bool) {
	unsupported, ok := errors.AsType[*provider.UnsupportedCapabilityError](err)
	if !ok || !slices.ContainsFunc(capabilities, func(capability provider.HostedCapability) bool {
		return capability.Kind == unsupported.Failure.Capability
	}) {
		return provider.CapabilityFailure{}, false
	}
	return unsupported.Failure, true
}

func removeHostedCapability(capabilities []provider.HostedCapability, kind provider.HostedCapabilityKind) []provider.HostedCapability {
	return slices.DeleteFunc(capabilities, func(capability provider.HostedCapability) bool {
		return capability.Kind == kind
	})
}

func (r *OpenAIRunner) traceCapabilityFailure(failure provider.CapabilityFailure) error {
	if r.Trace == nil {
		return nil
	}
	return r.Trace.Write("hosted-capability", map[string]any{
		"kind":   failure.Capability,
		"status": "disabled",
		"reason": "provider_rejected_capability",
	})
}

func (r *OpenAIRunner) traceHostedToolCalls(calls []openai.HostedToolCall) error {
	if r.Trace == nil {
		return nil
	}
	for _, call := range calls {
		if err := r.Trace.Write("hosted-tool-call", map[string]any{
			"id":      boundedTraceText(call.ID, 256),
			"type":    boundedTraceText(call.Type, 64),
			"status":  boundedTraceText(call.Status, 64),
			"action":  boundedTraceText(call.Action, 64),
			"queries": boundedTraceList(call.Queries, 8, 512),
			"sources": boundedTraceList(call.Sources, 20, 2048),
		}); err != nil {
			return err
		}
	}
	return nil
}

func boundedTraceList(values []string, maxItems, maxBytes int) []string {
	values = values[:min(len(values), maxItems)]
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = boundedTraceText(value, maxBytes)
	}
	return result
}

func boundedTraceText(value string, maxBytes int) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func writeTraceRequest(recorder *trace.Recorder, request openai.Request) error {
	if recorder == nil {
		return nil
	}
	value, err := request.TraceValue()
	if err != nil {
		return recorder.WriteStructured("request", map[string]any{"error": err.Error()})
	}
	return recorder.WriteStructured("request", value)
}

func writeTraceResponse(recorder *trace.Recorder, response openai.Response) error {
	if recorder == nil {
		return nil
	}
	continuationTypes := make([]string, 0, len(response.Continuation))
	for _, item := range response.Continuation {
		continuationTypes = append(continuationTypes, item.Type)
	}
	return recorder.Write("response", map[string]any{
		"id":                 response.ID,
		"text":               response.Text,
		"tool_calls":         response.ToolCalls,
		"hosted_tool_calls":  len(response.HostedToolCalls),
		"continuation_types": continuationTypes,
		"finish_kind":        response.FinishKind,
	})
}

func requestInstructions(taskInstructions string, toolSpecs []openai.ToolSpec, hostedCapabilities []provider.HostedCapability) string {
	prefix := taskInstructions
	if prefix != "" {
		prefix += "\n\n"
	}
	if len(toolSpecs) == 0 && len(hostedCapabilities) == 0 {
		return prefix + strings.TrimSpace(noToolsPrompt)
	}
	promptTools := make([]requestPromptTool, 0, len(hostedCapabilities)+len(toolSpecs))
	for _, capability := range hostedCapabilities {
		if capability.Kind == provider.HostedCapabilityWebSearch {
			promptTools = append(promptTools, requestPromptTool{Hosted: true})
		}
	}
	for _, spec := range toolSpecs {
		promptTools = append(promptTools, requestPromptTool{Name: spec.Name, Description: spec.Description})
	}
	return prefix + renderRequestPrompt(requestPromptData{
		Tools: promptTools,
		ReadFileAvailable: slices.ContainsFunc(toolSpecs, func(spec openai.ToolSpec) bool {
			return spec.Name == "read_file"
		}),
	})
}

func requestInputWithBudget(messages []openai.Item, step, maxSteps, usedTools, maxTools int, cacheBreakpoint bool) []openai.Item {
	budget := openai.NewMessage("developer", renderBudgetStatusPrompt(budgetStatusPromptData{
		Step: step, MaxSteps: maxSteps, RemainingTools: max(0, maxTools-usedTools), MaxTools: maxTools,
	}))
	budget.PromptCacheBreakpoint = cacheBreakpoint
	return append(slices.Clone(messages), budget)
}
