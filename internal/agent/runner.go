package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"

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
}

type Validator func(string) []string

type BudgetKind string

const (
	BudgetKindModelSteps BudgetKind = "model_steps"
	BudgetKindToolCalls  BudgetKind = "tool_calls"
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
	Config    config.Config
	Client    openai.Client
	Tools     *tools.Registry
	ToolSpecs []tools.Definition
	Validator Validator
	Trace     *trace.Recorder
	Budget    BudgetHandler
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

	if r.Validator != nil {
		if errs := r.Validator(result.Text); len(errs) > 0 {
			if !request.RepairOnValidator {
				return Result{}, fmt.Errorf("validation failed: %v", errs)
			}
			repairMessages := append(messages, openai.NewMessage("assistant", result.Text))
			repairMessages = append(repairMessages, openai.NewMessage("user", fmt.Sprintf("Repair the output to satisfy these validation errors: %v\nReturn only the corrected final artifact.", errs)))
			repaired, err := r.runUntilText(ctx, request.SystemPrompt, repairMessages, nil, nil, request.TextFormat, 1)
			if err != nil {
				return Result{}, err
			}
			result.RepairCalls++
			result.Text = repaired.Text
			if errs := r.Validator(result.Text); len(errs) > 0 {
				return Result{}, fmt.Errorf("validation failed after repair: %v", errs)
			}
		}
	}
	return result, nil
}

func (r *OpenAIRunner) runUntilText(ctx context.Context, instructions string, messages []openai.Item, toolSpecs []openai.ToolSpec, allowedToolNames []string, textFormat *openai.TextFormat, maxSteps int) (Result, error) {
	var result Result
	maxToolCalls := r.Config.MaxToolCalls
	for step := 0; step < maxSteps; step++ {
		req := openai.Request{
			Model:         r.Config.Model,
			ServiceTier:   r.Config.ServiceTier,
			ThinkingMode:  r.Config.ThinkingEffort,
			BaseURL:       r.Config.BaseURL,
			APIKey:        r.Config.APIKey,
			AuthAccountID: r.Config.AuthAccountID,
			Instructions:  requestInstructions(instructions, toolSpecs),
			Input:         messages,
			Tools:         toolSpecs,
			TextFormat:    textFormat,
			// Do not ever add outbound max_tool_calls again.
			// OpenAI-compatible providers in our target path reject it on /responses.
			// The local runner already enforces the tool-call ceiling.
		}
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
		if len(response.ToolCalls) == 0 {
			if response.Text == "" {
				return Result{}, errors.New("provider returned no text and no tool calls")
			}
			result.Text = response.Text
			return result, nil
		}
		if r.Tools == nil {
			return Result{}, errors.New("provider requested tools but no registry is configured")
		}
		for _, call := range response.ToolCalls {
			if maxToolCalls > 0 && result.ToolCalls >= maxToolCalls {
				recovered, updatedSteps, updatedTools, err := r.resolveBudgetExhaustion(ctx, instructions, messages, result, BudgetStatus{
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
			if err != nil {
				return Result{}, fmt.Errorf("tool %s failed: %w", call.Name, err)
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
		}
		if step == maxSteps-1 {
			recovered, updatedSteps, updatedTools, err := r.resolveBudgetExhaustion(ctx, instructions, messages, result, BudgetStatus{
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

func (r *OpenAIRunner) resolveBudgetExhaustion(ctx context.Context, instructions string, messages []openai.Item, current Result, status BudgetStatus) (Result, int, int, error) {
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

	finalized, err := r.finalizeWithoutTools(ctx, instructions, messages, status)
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

func (r *OpenAIRunner) finalizeWithoutTools(ctx context.Context, instructions string, messages []openai.Item, status BudgetStatus) (Result, error) {
	finalMessages := append(slices.Clone(messages), openai.NewMessage("developer", finalizationNotice(status)))
	req := openai.Request{
		Model:         r.Config.Model,
		ServiceTier:   r.Config.ServiceTier,
		ThinkingMode:  r.Config.ThinkingEffort,
		BaseURL:       r.Config.BaseURL,
		APIKey:        r.Config.APIKey,
		AuthAccountID: r.Config.AuthAccountID,
		Instructions:  finalArtifactInstructions(instructions),
		Input:         finalMessages,
	}
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
	return Result{Text: response.Text}, nil
}

func finalizationNotice(status BudgetStatus) string {
	reason := fmt.Sprintf("model-step budget reached at %d/%d", status.Used, status.Limit)
	if status.Kind == BudgetKindToolCalls {
		reason = fmt.Sprintf("tool-call budget reached at %d/%d before %q", status.Used, status.Limit, status.RequestedTool)
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
		return true
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

func requestInstructions(taskInstructions string, toolSpecs []openai.ToolSpec) string {
	prefix := taskInstructions
	if prefix != "" {
		prefix += "\n\n"
	}
	if len(toolSpecs) == 0 {
		return prefix + "Return only the final artifact. Do not call tools."
	}
	return prefix + "Use only listed read-only tools. Return only the final artifact when enough evidence has been gathered."
}

func finalArtifactInstructions(taskInstructions string) string {
	prefix := taskInstructions
	if prefix != "" {
		prefix += "\n\n"
	}
	return prefix + "Do not call tools. Return only the best final artifact possible from the evidence already gathered."
}
