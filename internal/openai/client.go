package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

type Client interface {
	CreateResponse(context.Context, Request) (Response, error)
}

type Request struct {
	Model        string     `json:"model"`
	BaseURL      string     `json:"-"`
	APIKey       string     `json:"-"`
	Instructions string     `json:"instructions,omitempty"`
	Input        []Item     `json:"input"`
	Tools        []ToolSpec `json:"tools,omitempty"`
}

type Item struct {
	Type      string `json:"type"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

type Response struct {
	ID         string     `json:"id,omitempty"`
	Text       string     `json:"text,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	FinishKind string     `json:"finish_kind,omitempty"`
	RawJSON    string     `json:"raw_json,omitempty"`
}

type ToolCall struct {
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type SDKClient struct {
	HTTPClient *http.Client
}

func NewHTTPClient(client *http.Client) *SDKClient {
	return &SDKClient{HTTPClient: client}
}

func (c *SDKClient) CreateResponse(ctx context.Context, request Request) (Response, error) {
	opts := []option.RequestOption{
		option.WithAPIKey(request.APIKey),
		option.WithBaseURL(request.BaseURL),
	}
	if c.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(c.HTTPClient))
	} else {
		opts = append(opts, option.WithHTTPClient(http.DefaultClient))
	}
	client := openaisdk.NewClient(opts...)

	params, err := request.toSDKParams()
	if err != nil {
		return Response{}, err
	}
	stream := client.Responses.NewStreaming(ctx, params)
	var final *responses.Response
	accum := newStreamAccumulator()
	for stream.Next() {
		event := stream.Current()
		accum.apply(event)
		if event.Type == "response.completed" {
			completed := event.AsResponseCompleted()
			final = &completed.Response
		}
	}
	if err := stream.Err(); err != nil {
		return Response{}, err
	}
	if final == nil && !accum.hasContent() {
		return Response{}, fmt.Errorf("provider stream ended without response.completed event")
	}

	result := accum.response()
	if final != nil {
		result = responseFromCompleted(final)
		if result.Text == "" {
			result.Text = accum.text()
		}
		result.ToolCalls = mergeToolCalls(result.ToolCalls, accum.toolCalls())
	}
	return result, nil
}

type streamAccumulator struct {
	responseID string
	finishKind string
	callsByID  map[string]*ToolCall
	callsByIdx map[int64]*ToolCall
	callOrder  []string
	textParts  map[string]string
	textOrder  []string
}

func newStreamAccumulator() *streamAccumulator {
	return &streamAccumulator{
		callsByID: map[string]*ToolCall{},
		callsByIdx: map[int64]*ToolCall{},
		textParts: map[string]string{},
	}
}

func (a *streamAccumulator) apply(event responses.ResponseStreamEventUnion) {
	switch event.Type {
	case "response.output_item.added":
		added := event.AsResponseOutputItemAdded()
		a.applyOutputItem(added.OutputIndex, added.Item)
	case "response.output_item.done":
		done := event.AsResponseOutputItemDone()
		a.applyOutputItem(done.OutputIndex, done.Item)
	case "response.function_call_arguments.delta":
		delta := event.AsResponseFunctionCallArgumentsDelta()
		call := a.ensureCall(delta.ItemID, delta.OutputIndex)
		call.Arguments += delta.Delta
	case "response.function_call_arguments.done":
		done := event.AsResponseFunctionCallArgumentsDone()
		call := a.ensureCall(done.ItemID, done.OutputIndex)
		call.Name = done.Name
		call.Arguments = done.Arguments
	case "response.output_text.delta":
		delta := event.AsResponseOutputTextDelta()
		if delta.Delta != "" {
			key := textPartKey(delta.ItemID, delta.ContentIndex)
			a.appendTextPart(key, delta.Delta)
		}
	case "response.output_text.done":
		done := event.AsResponseOutputTextDone()
		if done.Text != "" {
			key := textPartKey(done.ItemID, done.ContentIndex)
			a.setTextPart(key, done.Text)
		}
	case "response.completed":
		completed := event.AsResponseCompleted()
		a.responseID = completed.Response.ID
		a.finishKind = string(completed.Response.Status)
	}
}

func (a *streamAccumulator) applyOutputItem(outputIndex int64, item responses.ResponseOutputItemUnion) {
	switch item.Type {
	case "function_call":
		call := item.AsFunctionCall()
		entry := a.ensureCall(call.ID, outputIndex)
		entry.ID = call.ID
		entry.CallID = call.CallID
		entry.Name = call.Name
		entry.Arguments = call.Arguments
	case "message":
		message := item.AsMessage()
		for idx, content := range message.Content {
			if content.Type != "output_text" || content.Text == "" {
				continue
			}
			a.setTextPart(textPartKey(message.ID, int64(idx)), content.Text)
		}
	}
}

func (a *streamAccumulator) ensureCall(id string, outputIndex int64) *ToolCall {
	if id != "" {
		if call, ok := a.callsByID[id]; ok {
			if outputIndex >= 0 {
				a.callsByIdx[outputIndex] = call
			}
			return call
		}
	}
	if outputIndex >= 0 {
		if call, ok := a.callsByIdx[outputIndex]; ok {
			if id != "" && call.ID == "" {
				call.ID = id
				a.callsByID[id] = call
			}
			return call
		}
	}
	if id == "" {
		id = fmt.Sprintf("stream-call-%d", len(a.callOrder)+1)
	}
	if call, ok := a.callsByID[id]; ok {
		return call
	}
	call := &ToolCall{ID: id}
	a.callsByID[id] = call
	if outputIndex >= 0 {
		a.callsByIdx[outputIndex] = call
	}
	a.callOrder = append(a.callOrder, id)
	return call
}

func (a *streamAccumulator) toolCalls() []ToolCall {
	if len(a.callOrder) == 0 {
		return nil
	}
	calls := make([]ToolCall, 0, len(a.callOrder))
	for _, id := range a.callOrder {
		call := *a.callsByID[id]
		if call.CallID == "" {
			call.CallID = call.ID
		}
		call.Arguments = strings.TrimSpace(call.Arguments)
		calls = append(calls, call)
	}
	return calls
}

func (a *streamAccumulator) hasContent() bool {
	return strings.TrimSpace(a.text()) != "" || len(a.callOrder) > 0
}

func (a *streamAccumulator) response() Response {
	return Response{
		ID:         a.responseID,
		Text:       a.text(),
		ToolCalls:  a.toolCalls(),
		FinishKind: a.finishKind,
	}
}

func (a *streamAccumulator) appendTextPart(key, delta string) {
	if _, ok := a.textParts[key]; !ok {
		a.textOrder = append(a.textOrder, key)
	}
	a.textParts[key] += delta
}

func (a *streamAccumulator) setTextPart(key, text string) {
	if _, ok := a.textParts[key]; !ok {
		a.textOrder = append(a.textOrder, key)
	}
	a.textParts[key] = text
}

func (a *streamAccumulator) text() string {
	if len(a.textOrder) == 0 {
		return ""
	}
	var b strings.Builder
	for _, key := range a.textOrder {
		b.WriteString(a.textParts[key])
	}
	return b.String()
}

func textPartKey(itemID string, contentIndex int64) string {
	return fmt.Sprintf("%s:%d", itemID, contentIndex)
}

func responseFromCompleted(final *responses.Response) Response {
	result := Response{
		ID:         final.ID,
		Text:       final.OutputText(),
		FinishKind: string(final.Status),
		RawJSON:    final.RawJSON(),
	}
	for _, item := range final.Output {
		if item.Type != "function_call" {
			continue
		}
		call := item.AsFunctionCall()
		result.ToolCalls = append(result.ToolCalls, ToolCall{
			ID:        call.ID,
			CallID:    call.CallID,
			Name:      call.Name,
			Arguments: call.Arguments,
		})
	}
	return result
}

func mergeToolCalls(primary, streamed []ToolCall) []ToolCall {
	if len(primary) == 0 {
		return streamed
	}
	streamedByID := map[string]ToolCall{}
	streamedByCallID := map[string]ToolCall{}
	for _, call := range streamed {
		if call.ID != "" {
			streamedByID[call.ID] = call
		}
		if call.CallID != "" {
			streamedByCallID[call.CallID] = call
		}
	}

	merged := make([]ToolCall, 0, max(len(primary), len(streamed)))
	for i, call := range primary {
		patch, ok := streamedByID[call.ID]
		if !ok && call.CallID != "" {
			patch, ok = streamedByCallID[call.CallID]
		}
		if !ok && i < len(streamed) {
			patch, ok = streamed[i], true
		}
		if ok {
			if call.ID == "" {
				call.ID = patch.ID
			}
			if call.CallID == "" {
				call.CallID = patch.CallID
			}
			if call.Name == "" {
				call.Name = patch.Name
			}
			if strings.TrimSpace(call.Arguments) == "" {
				call.Arguments = patch.Arguments
			}
		}
		if call.CallID == "" {
			call.CallID = call.ID
		}
		call.Arguments = strings.TrimSpace(call.Arguments)
		if call.Name == "" {
			continue
		}
		merged = append(merged, call)
	}
	if len(merged) > 0 {
		return merged
	}
	return streamed
}

func (r Request) toSDKParams() (responses.ResponseNewParams, error) {
	tools := make([]responses.ToolUnionParam, 0, len(r.Tools))
	for _, spec := range r.Tools {
		tools = append(tools, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name:        spec.Name,
				Description: openaisdk.String(spec.Description),
				Parameters:  spec.Schema,
				Strict:      openaisdk.Bool(spec.Strict),
			},
		})
	}

	input := make(responses.ResponseInputParam, 0, len(r.Input))
	for _, item := range r.Input {
		param, err := item.toSDKParam()
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
		input = append(input, param)
	}

	params := responses.ResponseNewParams{
		Model:        r.Model,
		Input:        responses.ResponseNewParamsInputUnion{OfInputItemList: input},
		Instructions: openaisdk.String(r.Instructions),
		Tools:        tools,
		Store:        openaisdk.Bool(false),
	}
	if len(tools) > 0 {
		params.ParallelToolCalls = openaisdk.Bool(false)
	}
	return params, nil
}

func (i Item) toSDKParam() (responses.ResponseInputItemUnionParam, error) {
	switch i.Type {
	case "message":
		if i.Role == "assistant" {
			return responses.ResponseInputItemUnionParam{
				OfOutputMessage: &responses.ResponseOutputMessageParam{
					ID:     i.ID,
					Status: responses.ResponseOutputMessageStatusCompleted,
					Content: []responses.ResponseOutputMessageContentUnionParam{{
						OfOutputText: &responses.ResponseOutputTextParam{Text: i.Content},
					}},
				},
			}, nil
		}
		return responses.ResponseInputItemUnionParam{
			OfInputMessage: &responses.ResponseInputItemMessageParam{
				Role: i.Role,
				Content: responses.ResponseInputMessageContentListParam{{
					OfInputText: &responses.ResponseInputTextParam{Text: i.Content},
				}},
			},
		}, nil
	case "function_call":
		return responses.ResponseInputItemUnionParam{
			OfFunctionCall: &responses.ResponseFunctionToolCallParam{
				ID:        openaisdk.String(i.ID),
				CallID:    i.CallID,
				Name:      i.Name,
				Arguments: i.Arguments,
			},
		}, nil
	case "function_call_output":
		return responses.ResponseInputItemUnionParam{
			OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
				CallID: i.CallID,
				Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
					OfString: openaisdk.String(i.Output),
				},
			},
		}, nil
	default:
		return responses.ResponseInputItemUnionParam{}, fmt.Errorf("unsupported input item type %q", i.Type)
	}
}

func NewMessage(role, text string) Item {
	return Item{Type: "message", Role: role, Content: text}
}

func NewFunctionCall(call ToolCall) Item {
	return Item{
		Type:      "function_call",
		ID:        call.ID,
		CallID:    call.CallID,
		Name:      call.Name,
		Arguments: call.Arguments,
	}
}

func NewFunctionCallOutput(callID, output string) Item {
	return Item{Type: "function_call_output", CallID: callID, Output: output}
}

func (r Request) MarshalTraceJSON() ([]byte, error) {
	type traceRequest Request
	trace := traceRequest(r)
	trace.APIKey = ""
	trace.BaseURL = r.BaseURL
	return json.MarshalIndent(trace, "", "  ")
}
