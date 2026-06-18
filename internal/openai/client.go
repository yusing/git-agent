package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	"github.com/yusing/git-agent/internal/trace"
)

type Client interface {
	CreateResponse(context.Context, Request) (Response, error)
}

type EmbeddingClient interface {
	CreateEmbeddings(context.Context, EmbeddingRequest) (EmbeddingResponse, error)
}

type Request struct {
	Model         string      `json:"model"`
	ServiceTier   string      `json:"service_tier,omitempty"`
	ThinkingMode  string      `json:"thinking_mode,omitempty"`
	BaseURL       string      `json:"-"`
	APIKey        string      `json:"-"`
	AuthAccountID string      `json:"-"`
	Instructions  string      `json:"instructions,omitempty"`
	Input         []Item      `json:"input"`
	Tools         []ToolSpec  `json:"tools,omitempty"`
	TextFormat    *TextFormat `json:"text_format,omitempty"`
}

type EmbeddingRequest struct {
	Model      string
	Dimensions int
	BaseURL    string
	APIKey     string
	Inputs     []string
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

const DefaultDialTimeout = 5 * time.Second

func NewHTTPClient(client *http.Client) *SDKClient {
	return &SDKClient{HTTPClient: withDefaultDialTimeout(client)}
}

func (c *SDKClient) CreateResponse(ctx context.Context, request Request) (Response, error) {
	opts := []option.RequestOption{
		option.WithAPIKey(request.APIKey),
		option.WithBaseURL(request.BaseURL),
	}
	if request.AuthAccountID != "" {
		opts = append(opts, option.WithHeader("ChatGPT-Account-ID", request.AuthAccountID))
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
		if shouldRetryWithoutStreaming(err) {
			response, fallbackErr := fallbackWithoutStreaming(ctx, client, params, err)
			if fallbackErr != nil {
				return Response{}, upstreamError(fallbackErr)
			}
			return response, nil
		}
		return Response{}, upstreamError(err)
	}
	if final == nil && !accum.hasContent() {
		response, err := fallbackWithoutStreaming(ctx, client, params, fmt.Errorf("provider stream ended without response.completed event"))
		if err != nil {
			return Response{}, upstreamError(err)
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

func shouldRetryWithoutStreaming(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unexpected end of json input") ||
		strings.Contains(message, "unexpected eof")
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
	if r.ThinkingMode != "" {
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(r.ThinkingMode)}
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
	encoder := json.NewEncoder(&buf)
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
		if message["type"] != "message" {
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
