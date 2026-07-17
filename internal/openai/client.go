package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	"github.com/yusing/git-agent/internal/provider"
	"github.com/yusing/git-agent/internal/trace"
)

type Client interface {
	CreateResponse(context.Context, Request) (Response, error)
}

type EmbeddingClient interface {
	CreateEmbeddings(context.Context, EmbeddingRequest) (EmbeddingResponse, error)
}

type Request struct {
	Model              string                      `json:"model"`
	ServiceTier        string                      `json:"service_tier,omitempty"`
	ThinkingMode       string                      `json:"thinking_mode,omitempty"`
	ReasoningSummary   string                      `json:"reasoning_summary,omitempty"`
	BaseURL            string                      `json:"-"`
	APIKey             string                      `json:"-"`
	AuthAccountID      string                      `json:"-"`
	Instructions       string                      `json:"instructions,omitempty"`
	Input              []Item                      `json:"input"`
	Tools              []ToolSpec                  `json:"tools,omitempty"`
	HostedCapabilities []provider.HostedCapability `json:"hosted_capabilities,omitempty"`
	TextFormat         *TextFormat                 `json:"text_format,omitempty"`
	OnStreamEvent      func(StreamEvent) error     `json:"-"`
}

const ReasoningSummaryAuto = "auto"

type StreamEvent struct {
	Kind           string `json:"-"`
	ItemID         string `json:"item_id"`
	OutputIndex    int64  `json:"output_index"`
	SummaryIndex   int64  `json:"summary_index"`
	SequenceNumber int64  `json:"sequence_number"`
	Delta          string `json:"delta,omitempty"`
	Text           string `json:"text,omitempty"`
}

type EmbeddingRequest struct {
	Model      string
	Dimensions int
	BaseURL    string
	APIKey     string
	// Inputs are borrowed for the duration of CreateEmbeddings. Implementations
	// that retain them after returning must clone the strings.
	Inputs []string
}

type EmbeddingResponse struct {
	Model      string
	Vectors    [][]float64
	Dimensions int
}

type TextFormat struct {
	Name        string         `json:"name"`
	Schema      map[string]any `json:"schema"`
	Description string         `json:"description,omitempty"`
	Strict      bool           `json:"strict"`
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
	RawJSON   string `json:"raw_json,omitempty"`
}

type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Schema      map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

type Response struct {
	ID              string           `json:"id,omitempty"`
	Text            string           `json:"text,omitempty"`
	ToolCalls       []ToolCall       `json:"tool_calls,omitempty"`
	Continuation    []Item           `json:"continuation,omitempty"`
	HostedToolCalls []HostedToolCall `json:"hosted_tool_calls,omitempty"`
	FinishKind      string           `json:"finish_kind,omitempty"`
	RawJSON         string           `json:"raw_json,omitempty"`
}

type HostedToolCall struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Status  string   `json:"status"`
	Action  string   `json:"action,omitempty"`
	Queries []string `json:"queries,omitempty"`
	Sources []string `json:"sources,omitempty"`
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

const DefaultDialTimeout = 5 * time.Second

// Source: codex-rs/login/src/auth/default_client.rs:42:352 default_headers
const codexClientIdentity = "codex_cli_rs"

func NewHTTPClient(client *http.Client) *SDKClient {
	return &SDKClient{HTTPClient: withDefaultDialTimeout(client)}
}

func (c *SDKClient) CreateResponse(ctx context.Context, request Request) (Response, error) {
	opts := []option.RequestOption{
		option.WithAPIKey(request.APIKey),
		option.WithBaseURL(request.BaseURL),
	}
	if request.AuthAccountID != "" {
		opts = append(opts,
			option.WithHeader("ChatGPT-Account-ID", request.AuthAccountID),
			option.WithHeader("originator", codexClientIdentity),
			option.WithHeader("User-Agent", codexClientIdentity),
		)
		if request.Model == "gpt-5.6" {
			request.Model = "gpt-5.6-sol"
		}
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
	defer stream.Close()
	var final *responses.Response
	accum := newStreamAccumulator()
	for stream.Next() {
		event := stream.Current()
		accum.apply(event)
		if request.OnStreamEvent != nil {
			streamEvent, ok := reasoningSummaryStreamEvent(event)
			if ok {
				if err := request.OnStreamEvent(streamEvent); err != nil {
					return Response{}, fmt.Errorf("publishing provider stream event: %w", err)
				}
			}
		}
		if event.Type == "response.completed" {
			completed := event.AsResponseCompleted()
			final = &completed.Response
		}
	}
	if err := stream.Err(); err != nil {
		if shouldRetryWithoutStreaming(err) {
			response, fallbackErr := fallbackWithoutStreaming(ctx, client, params, err)
			if fallbackErr != nil {
				return Response{}, responseError(fallbackErr)
			}
			return response, nil
		}
		return Response{}, responseError(err)
	}
	if final == nil && !accum.hasContent() {
		response, err := fallbackWithoutStreaming(ctx, client, params, fmt.Errorf("provider stream ended without response.completed event"))
		if err != nil {
			return Response{}, responseError(err)
		}
		return response, nil
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

func reasoningSummaryStreamEvent(event responses.ResponseStreamEventUnion) (StreamEvent, bool) {
	switch event.Type {
	case "response.reasoning_summary_text.delta":
		delta := event.AsResponseReasoningSummaryTextDelta()
		return StreamEvent{
			Kind:           "reasoning_summary.delta",
			ItemID:         delta.ItemID,
			OutputIndex:    delta.OutputIndex,
			SummaryIndex:   delta.SummaryIndex,
			SequenceNumber: delta.SequenceNumber,
			Delta:          delta.Delta,
		}, true
	case "response.reasoning_summary_text.done":
		done := event.AsResponseReasoningSummaryTextDone()
		return StreamEvent{
			Kind:           "reasoning_summary.done",
			ItemID:         done.ItemID,
			OutputIndex:    done.OutputIndex,
			SummaryIndex:   done.SummaryIndex,
			SequenceNumber: done.SequenceNumber,
			Text:           done.Text,
		}, true
	default:
		return StreamEvent{}, false
	}
}

func (c *SDKClient) CreateEmbeddings(ctx context.Context, request EmbeddingRequest) (EmbeddingResponse, error) {
	if len(request.Inputs) == 0 {
		return EmbeddingResponse{}, errors.New("embedding request requires input")
	}
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

	params := openaisdk.EmbeddingNewParams{
		Model: openaisdk.EmbeddingModel(request.Model),
		Input: openaisdk.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: request.Inputs,
		},
		EncodingFormat: openaisdk.EmbeddingNewParamsEncodingFormatFloat,
	}
	if request.Dimensions > 0 {
		params.Dimensions = openaisdk.Int(int64(request.Dimensions))
	}
	response, err := client.Embeddings.New(ctx, params)
	if err != nil {
		return EmbeddingResponse{}, upstreamError(err)
	}
	if len(response.Data) != len(request.Inputs) {
		return EmbeddingResponse{}, fmt.Errorf("embedding response count = %d, want %d", len(response.Data), len(request.Inputs))
	}

	vectors := make([][]float64, len(response.Data))
	dimensions := 0
	for _, item := range response.Data {
		if item.Index < 0 || int(item.Index) >= len(vectors) {
			return EmbeddingResponse{}, fmt.Errorf("embedding response index %d out of range", item.Index)
		}
		vector := item.Embedding
		if len(vector) == 0 {
			return EmbeddingResponse{}, fmt.Errorf("embedding response index %d is empty", item.Index)
		}
		if dimensions == 0 {
			dimensions = len(vector)
		}
		if len(vector) != dimensions {
			return EmbeddingResponse{}, fmt.Errorf("embedding dimensions mismatch: %d and %d", dimensions, len(vector))
		}
		vectors[item.Index] = vector
	}
	for i, vector := range vectors {
		if vector == nil {
			return EmbeddingResponse{}, fmt.Errorf("embedding response missing index %d", i)
		}
	}
	return EmbeddingResponse{
		Model:      response.Model,
		Vectors:    vectors,
		Dimensions: dimensions,
	}, nil
}

func withDefaultDialTimeout(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	cloned := *client
	if cloned.Transport == nil {
		cloned.Transport = newTransportWithDialTimeout(DefaultDialTimeout)
	}
	return &cloned
}

func newTransportWithDialTimeout(timeout time.Duration) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	return transport
}

func upstreamError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("upstream request failed: %w", err)
}

func responseError(err error) error {
	if failure, ok := hostedCapabilityFailure(err); ok {
		return &provider.UnsupportedCapabilityError{Failure: failure}
	}
	return upstreamError(err)
}

func hostedCapabilityFailure(err error) (provider.CapabilityFailure, bool) {
	apiErr, ok := errors.AsType[*openaisdk.Error](err)
	if !ok || apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusTooManyRequests {
		return provider.CapabilityFailure{}, false
	}
	if apiErr.StatusCode < 400 || apiErr.StatusCode >= 500 {
		return provider.CapabilityFailure{}, false
	}

	param := strings.ToLower(apiErr.Param)
	code := strings.ToLower(apiErr.Code)
	message := strings.ToLower(apiErr.Message)
	switch {
	case strings.Contains(param, "web_search") || strings.Contains(code, "web_search") || strings.Contains(message, "web_search"):
		return provider.CapabilityFailure{Capability: provider.HostedCapabilityWebSearch, Reason: "web_search rejected"}, true
	case strings.HasPrefix(param, "include") && (strings.Contains(message, "source") || strings.Contains(message, "reasoning.encrypted_content")):
		return provider.CapabilityFailure{Capability: provider.HostedCapabilityWebSearch, Reason: "web_search metadata include rejected"}, true
	case param == "max_tool_calls" || strings.Contains(code, "max_tool_calls") || strings.Contains(message, "max_tool_calls"):
		return provider.CapabilityFailure{Capability: provider.HostedCapabilityWebSearch, Reason: "hosted tool call limit rejected"}, true
	default:
		return provider.CapabilityFailure{}, false
	}
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
		callsByID:  map[string]*ToolCall{},
		callsByIdx: map[int64]*ToolCall{},
		textParts:  map[string]string{},
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
		switch item.Type {
		case "reasoning", "web_search_call", "message", "function_call":
			result.Continuation = append(result.Continuation, Item{Type: item.Type, ID: item.ID, RawJSON: item.RawJSON()})
		}
		switch item.Type {
		case "function_call":
			call := item.AsFunctionCall()
			result.ToolCalls = append(result.ToolCalls, ToolCall{
				ID:        call.ID,
				CallID:    call.CallID,
				Name:      call.Name,
				Arguments: call.Arguments,
			})
		case "web_search_call":
			result.HostedToolCalls = append(result.HostedToolCalls, hostedToolCall(item.AsWebSearchCall()))
		}
	}
	return result
}

func hostedToolCall(call responses.ResponseFunctionWebSearch) HostedToolCall {
	result := HostedToolCall{
		ID:     call.ID,
		Type:   "web_search",
		Status: string(call.Status),
		Action: call.Action.Type,
	}
	result.Queries = append(result.Queries, call.Action.Queries...)
	if call.Action.Query != "" {
		result.Queries = append(result.Queries, call.Action.Query)
	}
	if call.Action.Pattern != "" {
		result.Queries = append(result.Queries, call.Action.Pattern)
	}
	for _, source := range call.Action.Sources {
		result.Sources = append(result.Sources, source.URL)
	}
	if call.Action.URL != "" {
		result.Sources = append(result.Sources, call.Action.URL)
	}
	return result
}

func shouldRetryWithoutStreaming(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unexpected end of json input") ||
		strings.Contains(message, "unexpected eof") ||
		strings.Contains(message, "stream error:") &&
			strings.Contains(message, "received from peer") &&
			(strings.Contains(message, "internal_error") || strings.Contains(message, "refused_stream"))
}

func fallbackWithoutStreaming(ctx context.Context, client openaisdk.Client, params responses.ResponseNewParams, streamErr error) (Response, error) {
	fallback, fallbackErr := client.Responses.New(ctx, params)
	if fallbackErr == nil {
		return responseFromCompleted(fallback), nil
	}
	return Response{}, errors.Join(streamErr, fallbackErr)
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
	tools := make([]responses.ToolUnionParam, 0, len(r.Tools)+len(r.HostedCapabilities))
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
	var maxHostedCalls int
	for _, capability := range r.HostedCapabilities {
		switch capability.Kind {
		case provider.HostedCapabilityWebSearch:
			tools = append(tools, responses.ToolParamOfWebSearch(responses.WebSearchToolTypeWebSearch))
			maxHostedCalls = max(maxHostedCalls, capability.MaxCalls)
		default:
			return responses.ResponseNewParams{}, fmt.Errorf("unsupported hosted capability %q", capability.Kind)
		}
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
	if len(r.HostedCapabilities) > 0 {
		params.Include = []responses.ResponseIncludable{
			responses.ResponseIncludableWebSearchCallActionSources,
			responses.ResponseIncludableReasoningEncryptedContent,
		}
	}
	if maxHostedCalls > 0 {
		params.MaxToolCalls = openaisdk.Int(int64(maxHostedCalls))
	}
	if r.TextFormat != nil {
		params.Text = responses.ResponseTextConfigParam{
			Format: responses.ResponseFormatTextConfigParamOfJSONSchema(r.TextFormat.Name, r.TextFormat.Schema),
		}
		if r.TextFormat.Description != "" {
			params.Text.Format.OfJSONSchema.Description = openaisdk.String(r.TextFormat.Description)
		}
		if r.TextFormat.Strict {
			params.Text.Format.OfJSONSchema.Strict = openaisdk.Bool(true)
		}
	}
	if len(tools) > 0 {
		params.ParallelToolCalls = openaisdk.Bool(false)
	}
	if r.ServiceTier != "" {
		params.ServiceTier = responses.ResponseNewParamsServiceTier(r.ServiceTier)
	}
	if r.ThinkingMode != "" || r.ReasoningSummary != "" {
		params.Reasoning = shared.ReasoningParam{
			Effort:  shared.ReasoningEffort(r.ThinkingMode),
			Summary: shared.ReasoningSummary(r.ReasoningSummary),
		}
	}
	return params, nil
}

func (i Item) toSDKParam() (responses.ResponseInputItemUnionParam, error) {
	if i.RawJSON != "" {
		var param responses.ResponseInputItemUnionParam
		if err := sonic.ConfigStd.UnmarshalFromString(i.RawJSON, &param); err != nil {
			return responses.ResponseInputItemUnionParam{}, fmt.Errorf("decode continuation item %q: %w", i.Type, err)
		}
		return param, nil
	}
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

func (r Request) TraceValue() (map[string]any, error) {
	type traceRequest Request
	traceValue := traceRequest(r)
	traceValue.APIKey = ""
	traceValue.BaseURL = r.BaseURL
	normalized := sanitizeTraceRequestValue(trace.Normalize(traceValue))
	root, ok := normalized.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("trace request normalized to %T, want object", normalized)
	}
	return root, nil
}

func (r Request) MarshalTraceJSON() ([]byte, error) {
	traceValue, err := r.TraceValue()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	encoder := sonic.ConfigDefault.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(traceValue); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

func sanitizeTraceRequestValue(value any) any {
	root, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if text, ok := root["instructions"].(string); ok {
		root["instructions"] = sanitizeTracePromptText(text)
	}
	input, ok := root["input"].([]any)
	if !ok {
		return root
	}
	for idx, item := range input {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		delete(message, "raw_json")
		if message["type"] != "message" {
			input[idx] = message
			continue
		}
		text, ok := message["content"].(string)
		if !ok {
			continue
		}
		message["content"] = sanitizeTracePromptText(text)
		input[idx] = message
	}
	root["input"] = input
	return root
}

func sanitizeTracePromptText(text string) string {
	lines := strings.Split(text, "\n")
	for len(lines) > 0 && isTraceWrapperLine(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && isTraceWrapperLine(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}

	sanitized := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.NewReplacer("<", " ", ">", " ").Replace(line)
		line = strings.Join(strings.Fields(line), " ")
		sanitized = append(sanitized, line)
	}
	for len(sanitized) > 0 && sanitized[0] == "" {
		sanitized = sanitized[1:]
	}
	for len(sanitized) > 0 && sanitized[len(sanitized)-1] == "" {
		sanitized = sanitized[:len(sanitized)-1]
	}
	return strings.Join(sanitized, "\n")
}

func isTraceWrapperLine(line string) bool {
	line = strings.TrimSpace(line)
	if len(line) < 3 || line[0] != '<' || line[len(line)-1] != '>' {
		return false
	}
	body := line[1 : len(line)-1]
	if body == "" {
		return false
	}
	if body[0] == '/' {
		body = body[1:]
	}
	if body == "" {
		return false
	}
	for _, r := range body {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != ':' && r != '-' {
			return false
		}
	}
	return true
}
