package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/tools"
	"github.com/yusing/git-agent/internal/trace"
)

type Runner interface {
	Run(context.Context, Request) (Result, error)
}

type Request struct {
	SystemPrompt      string
	ToolPolicy        string
	Environment       string
	SkillInstructions string
	ProjectGuidance   string
	UserPrompt        string
	TextFormat        *openai.TextFormat
	AllowedToolNames  []string
	MaxSteps          int
	RepairOnValidator bool
}

type Result struct {
	Text        string
	ToolCalls   int
	RepairCalls int
	messages    []openai.Item
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

type BudgetHandler func(context.Context, BudgetStatus) (BudgetDecision, error)

type OpenAIRunner struct {
	Config           config.Config
	Client           openai.Client
	Tools            *tools.Registry
	ToolSpecs        []tools.Definition
	Validator        Validator
	Normalize        TextNormalizer
	Trace            *trace.Recorder
	Budget           BudgetHandler
	ReasoningSummary string
}

func (r *OpenAIRunner) Run(ctx context.Context, request Request) (Result, error) {
	if r.Client == nil {
		return Result{}, errors.New("openai client is required")
	}
	if request.MaxSteps <= 0 {
		request.MaxSteps = r.Config.MaxSteps
	}

	messages := []openai.Item{}
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
	messages = append(messages, openai.NewMessage("user", request.UserPrompt))

	toolSpecs := make([]openai.ToolSpec, 0, len(r.ToolSpecs))
	for _, def := range r.ToolSpecs {
		if len(request.AllowedToolNames) > 0 && !slices.Contains(request.AllowedToolNames, def.Name) {
			continue
		}
		toolSpecs = append(toolSpecs, openai.ToolSpec{Name: def.Name, Description: def.Description, Schema: def.Schema, Strict: def.Strict})
	}

	result, err := r.runUntilText(ctx, request.SystemPrompt, messages, toolSpecs, request.AllowedToolNames, request.TextFormat, request.MaxSteps)
	if err != nil {
		return Result{}, err
	}
	r.normalizeResult(&result)

	if r.Validator != nil {
		if errs := r.Validator(result.Text); len(errs) > 0 {
			if !request.RepairOnValidator {
				return Result{}, fmt.Errorf("validation failed: %v", errs)
			}
			repairMessages := slices.Clone(result.messages)
			if len(repairMessages) == 0 {
				repairMessages = append(slices.Clone(messages), openai.NewMessage("assistant", result.Text))
			}
			repairMessages = append(repairMessages, openai.NewMessage("user", fmt.Sprintf("Repair the output to satisfy these validation errors: %v\nReturn only the corrected final artifact.", errs)))
			repaired, err := r.runUntilText(ctx, request.SystemPrompt, repairMessages, nil, nil, request.TextFormat, 1)
			if err != nil {
				return Result{}, err
			}
			result.RepairCalls++
			result.Text = repaired.Text
			r.normalizeResult(&result)
			if errs := r.Validator(result.Text); len(errs) > 0 {
				return Result{}, fmt.Errorf("validation failed after repair: %v", errs)
			}
		}
	}
	return result, nil
}

func (r *OpenAIRunner) normalizeResult(result *Result) {
	if r.Normalize != nil {
		result.Text = r.Normalize(result.Text)
	}
}

func (r *OpenAIRunner) runUntilText(ctx context.Context, instructions string, messages []openai.Item, toolSpecs []openai.ToolSpec, allowedToolNames []string, textFormat *openai.TextFormat, maxSteps int) (Result, error) {
	var result Result
	maxToolCalls := r.Config.MaxToolCalls
	started := time.Now()
	seenCalls := map[string]struct{}{}
	for step := 0; step < maxSteps; step++ {
		req := r.providerRequest(
			requestInstructions(instructions, toolSpecs, step+1, maxSteps, result.ToolCalls, maxToolCalls),
			messages,
			toolSpecs,
			textFormat,
		)
		estimatedTokens := estimateRequestTokens(req)
		if err := r.writeRuntimeStatus("requesting", step+1, maxSteps, result.ToolCalls, maxToolCalls, estimatedTokens, 0, started); err != nil {
			return Result{}, err
		}
		if step == 0 && r.Config.ContextTokens > 0 && estimatedTokens >= r.Config.ContextTokens {
			if err := r.Trace.Write("budget", map[string]any{
				"kind": BudgetKindContext, "decision": "reject", "reason": "initial_context_budget_exhausted",
				"step": step + 1, "used": estimatedTokens, "limit": r.Config.ContextTokens,
			}); err != nil {
				return Result{}, err
			}
			return Result{}, fmt.Errorf("initial request estimated at %d tokens meets or exceeds context budget %d", estimatedTokens, r.Config.ContextTokens)
		}
		// Do not ever add outbound max_tool_calls again.
		// OpenAI-compatible providers in our target path reject it on /responses.
		// The local runner already enforces the tool-call ceiling.
		if err := writeTraceRequest(r.Trace, req); err != nil {
			return Result{}, err
		}
		response, err := r.Client.CreateResponse(ctx, req)
		if err != nil {
			return Result{}, err
		}
		if err := r.Trace.Write("response", response); err != nil {
			return Result{}, err
		}
		inputTokens := responseInputTokens(response)
		if err := r.writeRuntimeStatus("response_received", step+1, maxSteps, result.ToolCalls, maxToolCalls, estimatedTokens, inputTokens, started); err != nil {
			return Result{}, err
		}
		if r.Config.ContextTokens > 0 && inputTokens >= r.Config.ContextTokens && len(response.ToolCalls) > 0 {
			return r.finalizeForGuard(ctx, instructions, messages, result, textFormat, BudgetStatus{
				Kind: BudgetKindContext, Used: inputTokens, Step: step + 1,
				Limit: r.Config.ContextTokens, MaxSteps: maxSteps, MaxToolCalls: maxToolCalls,
			}, "context_budget_exhausted")
		}
		if len(response.ToolCalls) == 0 {
			if response.Text == "" {
				return Result{}, errors.New("provider returned no text and no tool calls")
			}
			result.Text = response.Text
			result.messages = append(slices.Clone(messages), openai.NewMessage("assistant", response.Text))
			return result, nil
		}
		if r.Tools == nil {
			return Result{}, errors.New("provider requested tools but no registry is configured")
		}
		for _, call := range response.ToolCalls {
			callSignature := toolCallSignature(call)
			if _, duplicate := seenCalls[callSignature]; duplicate {
				return r.finalizeForGuard(ctx, instructions, messages, result, textFormat, BudgetStatus{
					Kind: BudgetKindNoProgress, Used: result.ToolCalls, Step: step + 1,
					MaxSteps: maxSteps, MaxToolCalls: maxToolCalls, RequestedTool: call.Name,
				}, "repeated_tool_call")
			}
			seenCalls[callSignature] = struct{}{}
			if maxToolCalls > 0 && result.ToolCalls >= maxToolCalls {
				recovered, updatedSteps, updatedTools, err := r.resolveBudgetExhaustion(ctx, instructions, messages, result, textFormat, BudgetStatus{
					Kind:          BudgetKindToolCalls,
					Limit:         maxToolCalls,
					Used:          result.ToolCalls,
					Step:          step + 1,
					MaxSteps:      maxSteps,
					MaxToolCalls:  maxToolCalls,
					RequestedTool: call.Name,
				})
				if err != nil {
					return Result{}, err
				}
				if recovered.Text != "" {
					return recovered, nil
				}
				maxSteps = updatedSteps
				maxToolCalls = updatedTools
			}
			if !toolAllowed(call.Name, toolSpecs, allowedToolNames) {
				return Result{}, fmt.Errorf("tool %s is not allowed for this request", call.Name)
			}
			if err := r.Trace.Write("tool-call", call); err != nil {
				return Result{}, err
			}
			toolResult, err := r.Tools.Execute(ctx, tools.Invocation{Name: call.Name, Arguments: call.Arguments})
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Result{}, fmt.Errorf("tool %s canceled: %w", call.Name, ctxErr)
			}
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return Result{}, fmt.Errorf("tool %s failed: %w", call.Name, err)
				}
				toolResult, err = tools.ErrorResult(call.Name, err)
				if err != nil {
					return Result{}, fmt.Errorf("encode tool %s error: %w", call.Name, err)
				}
			}
			if err := r.Trace.Write("tool-output", map[string]any{
				"name":      call.Name,
				"call_id":   call.CallID,
				"content":   toolResult.Content,
				"truncated": toolResult.Truncated,
			}); err != nil {
				return Result{}, err
			}
			result.ToolCalls++
			callID := call.CallID
			if callID == "" {
				callID = call.ID
			}
			call.CallID = callID
			messages = append(messages, openai.NewFunctionCall(call))
			messages = append(messages, openai.NewFunctionCallOutput(callID, toolResult.Content))
			nextRequest := r.providerRequest(instructions, messages, toolSpecs, textFormat)
			nextTokens := estimateRequestTokens(nextRequest)
			if r.Config.ContextTokens > 0 && nextTokens >= r.Config.ContextTokens {
				return r.finalizeForGuard(ctx, instructions, messages, result, textFormat, BudgetStatus{
					Kind: BudgetKindContext, Used: nextTokens, Step: step + 1,
					Limit: r.Config.ContextTokens, MaxSteps: maxSteps, MaxToolCalls: maxToolCalls,
				}, "context_budget_exhausted")
			}
		}
		if step == maxSteps-1 {
			recovered, updatedSteps, updatedTools, err := r.resolveBudgetExhaustion(ctx, instructions, messages, result, textFormat, BudgetStatus{
				Kind:         BudgetKindModelSteps,
				Limit:        maxSteps,
				Used:         step + 1,
				Step:         step + 1,
				MaxSteps:     maxSteps,
				MaxToolCalls: maxToolCalls,
			})
			if err != nil {
				return Result{}, err
			}
			if recovered.Text != "" {
				return recovered, nil
			}
			maxSteps = updatedSteps
			maxToolCalls = updatedTools
		}
	}
	return Result{}, fmt.Errorf("agent exceeded maximum model steps (%d)", maxSteps)
}

func (r *OpenAIRunner) finalizeForGuard(ctx context.Context, instructions string, messages []openai.Item, current Result, textFormat *openai.TextFormat, status BudgetStatus, reason string) (Result, error) {
	finalized, err := r.finalizeWithoutTools(ctx, instructions, messages, textFormat, status)
	if err != nil {
		return Result{}, err
	}
	finalized.ToolCalls = current.ToolCalls
	finalized.RepairCalls = current.RepairCalls
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
	return r.Trace.Write("runtime.status", map[string]any{
		"phase": phase, "step": step, "max_steps": maxSteps,
		"tool_calls": toolCalls, "max_tool_calls": maxToolCalls,
		"elapsed_ms":               time.Since(started).Milliseconds(),
		"estimated_context_tokens": estimatedTokens, "input_tokens": inputTokens,
		"context_budget_tokens": r.Config.ContextTokens,
	})
}

func estimateRequestTokens(request openai.Request) int {
	data, _ := json.Marshal(struct {
		Instructions string             `json:"instructions"`
		Input        []openai.Item      `json:"input"`
		Tools        []openai.ToolSpec  `json:"tools"`
		TextFormat   *openai.TextFormat `json:"text_format"`
	}{request.Instructions, request.Input, request.Tools, request.TextFormat})
	return (len(data) + 3) / 4
}

func responseInputTokens(response openai.Response) int {
	if response.RawJSON == "" {
		return 0
	}
	var payload struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(response.RawJSON), &payload) != nil {
		return 0
	}
	return payload.Usage.InputTokens
}

func toolCallSignature(call openai.ToolCall) string {
	arguments := strings.TrimSpace(call.Arguments)
	var value any
	if json.Unmarshal([]byte(arguments), &value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			arguments = string(canonical)
		}
	}
	return call.Name + "\x00" + arguments
}

func (r *OpenAIRunner) resolveBudgetExhaustion(ctx context.Context, instructions string, messages []openai.Item, current Result, textFormat *openai.TextFormat, status BudgetStatus) (Result, int, int, error) {
	if r.Budget != nil {
		decision, err := r.Budget(ctx, status)
		if err != nil {
			return Result{}, 0, 0, err
		}
		if decision.ExtendSteps > 0 || decision.ExtendToolCalls > 0 {
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

	finalized, err := r.finalizeWithoutTools(ctx, instructions, messages, textFormat, status)
	if err != nil {
		return Result{}, 0, 0, err
	}
	finalized.ToolCalls = current.ToolCalls
	finalized.RepairCalls = current.RepairCalls
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

func (r *OpenAIRunner) finalizeWithoutTools(ctx context.Context, instructions string, messages []openai.Item, textFormat *openai.TextFormat, status BudgetStatus) (Result, error) {
	finalMessages := append(slices.Clone(messages), openai.NewMessage("developer", finalizationNotice(status)))
	req := r.providerRequest(finalArtifactInstructions(instructions), finalMessages, nil, textFormat)
	if err := writeTraceRequest(r.Trace, req); err != nil {
		return Result{}, err
	}
	response, err := r.Client.CreateResponse(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if err := r.Trace.Write("response", response); err != nil {
		return Result{}, err
	}
	if len(response.ToolCalls) > 0 {
		return Result{}, fmt.Errorf("provider requested tools during forced finalization")
	}
	if response.Text == "" {
		return Result{}, errors.New("provider returned no text during forced finalization")
	}
	return Result{
		Text:     response.Text,
		messages: append(slices.Clone(finalMessages), openai.NewMessage("assistant", response.Text)),
	}, nil
}

func (r *OpenAIRunner) providerRequest(instructions string, input []openai.Item, toolSpecs []openai.ToolSpec, textFormat *openai.TextFormat) openai.Request {
	request := openai.Request{
		Model:            r.Config.Model,
		ServiceTier:      r.Config.ServiceTier,
		ThinkingMode:     r.Config.ThinkingEffort,
		ReasoningSummary: r.ReasoningSummary,
		BaseURL:          r.Config.BaseURL,
		APIKey:           r.Config.APIKey,
		AuthAccountID:    r.Config.AuthAccountID,
		Instructions:     instructions,
		Input:            input,
		Tools:            toolSpecs,
		TextFormat:       textFormat,
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
	return fmt.Sprintf(`<budget_exhausted>
reason: %s
current_step_limit: %d
current_tool_call_limit: %d
Do not call tools.
Do not ask for more evidence.
Use only evidence already gathered in the conversation.
Produce the best final artifact immediately.
If evidence is partial, stay conservative and avoid unsupported claims.
</budget_exhausted>`, reason, status.MaxSteps, status.MaxToolCalls)
}

func toolAllowed(name string, toolSpecs []openai.ToolSpec, allowedToolNames []string) bool {
	if len(allowedToolNames) > 0 {
		return slices.Contains(allowedToolNames, name)
	}
	if len(toolSpecs) == 0 {
		return false
	}
	return slices.ContainsFunc(toolSpecs, func(spec openai.ToolSpec) bool {
		return spec.Name == name
	})
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

func requestInstructions(taskInstructions string, toolSpecs []openai.ToolSpec, step, maxSteps, usedTools, maxTools int) string {
	prefix := taskInstructions
	if prefix != "" {
		prefix += "\n\n"
	}
	if len(toolSpecs) == 0 {
		return prefix + "No tools are available. Return only the final artifact."
	}
	var toolsList strings.Builder
	toolsList.WriteString("# Available tools\n")
	for _, spec := range toolSpecs {
		fmt.Fprintf(&toolsList, "- %s: %s\n", spec.Name, spec.Description)
	}
	remainingTools := max(0, maxTools-usedTools)
	return prefix + toolsList.String() + fmt.Sprintf(`
# Agent loop
You are in a bounded agent loop.
This is model step %d of %d. You have %d of %d tool calls remaining.
Use only listed read-only tools.
Call tools only when they reduce material uncertainty; do not call tools just to repeat provided context.
When a tool output has ok=false, correct the invocation or use different evidence and continue.
%sPrefer narrow tool calls that target the missing evidence.
Conclude before the remaining budget reaches zero.
Do not ask the user for more evidence.
Return only the final artifact when enough evidence has been gathered.`, step, maxSteps, remainingTools, maxTools, skillToolInstruction(toolSpecs))
}

func skillToolInstruction(toolSpecs []openai.ToolSpec) string {
	if !slices.ContainsFunc(toolSpecs, func(spec openai.ToolSpec) bool { return spec.Name == tools.SkillReadToolName }) {
		return ""
	}
	return "Use " + tools.SkillReadToolName + " only after a listed skill is relevant, and only to read that skill's SKILL.md or text files under its references/ directory.\n"
}

func finalArtifactInstructions(taskInstructions string) string {
	prefix := taskInstructions
	if prefix != "" {
		prefix += "\n\n"
	}
	return prefix + `# Forced finalization
Do not call tools.
Use only evidence already gathered in the conversation.
If evidence is partial, stay conservative and avoid unsupported claims.
Return only the best final artifact possible.`
}
