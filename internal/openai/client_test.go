package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/yusing/git-agent/internal/provider"
)

func TestRequestConvertsToSDKStructuredInputAndTools(t *testing.T) {
	t.Parallel()

	params, err := Request{
		Model:            "test-model",
		ServiceTier:      "priority",
		ThinkingMode:     "xhigh",
		ReasoningSummary: ReasoningSummaryAuto,
		Instructions:     "follow spec",
		Input: []Item{
			NewMessage("developer", "guidance"),
			NewMessage("user", "task"),
			NewMessage("assistant", "draft reply"),
			NewFunctionCall(ToolCall{ID: "fc_1", CallID: "call_1", Name: "repo_summary", Arguments: "{}"}),
			NewFunctionCallOutput("call_1", `{"ok":true}`),
		},
		Tools: []ToolSpec{{
			Name:        "repo_summary",
			Description: "summary",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
			Strict:      true,
		}},
	}.toSDKParams()
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		`"instructions":"follow spec"`,
		`"role":"developer"`,
		`"service_tier":"priority"`,
		`"effort":"xhigh"`,
		`"summary":"auto"`,
		`"type":"input_text"`,
		`"type":"output_text"`,
		`"type":"function_call"`,
		`"type":"function_call_output"`,
		`"name":"repo_summary"`,
		`"strict":true`,
		`"parallel_tool_calls":false`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("SDK payload missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, `"max_tool_calls":`) {
		t.Fatalf("SDK payload should omit max_tool_calls for provider compatibility: %s", got)
	}
}

func TestRequestConvertsHostedWebSearchCapability(t *testing.T) {
	t.Parallel()

	params, err := Request{
		Model: "test-model",
		Input: []Item{NewMessage("user", "task")},
		HostedCapabilities: []provider.HostedCapability{{
			Kind: provider.HostedCapabilityWebSearch, MaxCalls: 4,
		}},
	}.toSDKParams()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		`"tools":[{"type":"web_search"}]`,
		`"include":["web_search_call.action.sources","reasoning.encrypted_content"]`,
		`"max_tool_calls":4`,
		`"store":false`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("hosted request missing %s: %s", want, got)
		}
	}
}

func TestRequestOmitsHostedCallLimitWhenUncapped(t *testing.T) {
	t.Parallel()

	params, err := Request{
		Model: "test-model", Input: []Item{NewMessage("user", "task")},
		HostedCapabilities: []provider.HostedCapability{{Kind: provider.HostedCapabilityWebSearch}},
	}.toSDKParams()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); strings.Contains(got, `"max_tool_calls"`) {
		t.Fatalf("uncapped request contains max_tool_calls: %s", got)
	}
}

func TestRequestReplaysRawContinuationItems(t *testing.T) {
	t.Parallel()

	params, err := Request{
		Model: "test-model",
		Input: []Item{
			{Type: "reasoning", RawJSON: `{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"cipher","status":"completed"}`},
			{Type: "web_search_call", RawJSON: `{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","queries":["Go API"]}}`},
			{Type: "function_call", RawJSON: `{"id":"fc_1","type":"function_call","call_id":"call_1","name":"repo_summary","arguments":"{}","status":"completed"}`},
			NewFunctionCallOutput("call_1", `{"ok":true}`),
		},
	}.toSDKParams()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{`"encrypted_content":"cipher"`, `"type":"web_search_call"`, `"type":"function_call"`, `"type":"function_call_output"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("continuation payload missing %s: %s", want, got)
		}
	}
}

func TestResponsePreservesContinuationAndHostedMetadata(t *testing.T) {
	t.Parallel()

	var completed responses.Response
	err := json.Unmarshal([]byte(`{
		"id":"resp_1","status":"completed",
		"output":[
			{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"cipher","status":"completed"},
			{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","queries":["Go 1.26 API"],"sources":[{"type":"url","url":"https://go.dev/doc/"}]}},
			{"id":"fc_1","type":"function_call","call_id":"call_1","name":"repo_summary","arguments":"{}","status":"completed"}
		]
	}`), &completed)
	if err != nil {
		t.Fatal(err)
	}
	result := responseFromCompleted(&completed)
	if len(result.Continuation) != 3 {
		t.Fatalf("continuation = %#v", result.Continuation)
	}
	if got := []string{result.Continuation[0].Type, result.Continuation[1].Type, result.Continuation[2].Type}; !slices.Equal(got, []string{"reasoning", "web_search_call", "function_call"}) {
		t.Fatalf("continuation order = %v", got)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].CallID != "call_1" {
		t.Fatalf("tool calls = %#v", result.ToolCalls)
	}
	if len(result.HostedToolCalls) != 1 || !slices.Equal(result.HostedToolCalls[0].Queries, []string{"Go 1.26 API"}) || !slices.Equal(result.HostedToolCalls[0].Sources, []string{"https://go.dev/doc/"}) {
		t.Fatalf("hosted calls = %#v", result.HostedToolCalls)
	}
}

func TestHostedCapabilityFailureClassificationIsNarrow(t *testing.T) {
	t.Parallel()

	var unknownBody openaisdk.Error
	if err := json.Unmarshal([]byte(`{"future_error":{"detail":"unknown"}}`), &unknownBody); err != nil {
		t.Fatal(err)
	}
	unknownBody.StatusCode = http.StatusBadRequest
	enabledRequest := Request{HostedCapabilities: []provider.HostedCapability{{Kind: provider.HostedCapabilityWebSearch}}}

	tests := []struct {
		name    string
		err     *openaisdk.Error
		request Request
		want    bool
	}{
		{name: "web search", err: &openaisdk.Error{StatusCode: 400, Param: "tools[0].type", Message: "web_search is not supported"}, request: enabledRequest, want: true},
		{name: "source include", err: &openaisdk.Error{StatusCode: 422, Param: "include", Message: "web search sources unsupported"}, request: enabledRequest, want: true},
		{name: "hosted limit", err: &openaisdk.Error{StatusCode: 400, Param: "max_tool_calls", Message: "unknown parameter"}, request: enabledRequest, want: true},
		{name: "unrelated bad request", err: &openaisdk.Error{StatusCode: 400, Param: "text.format", Message: "invalid schema"}, request: enabledRequest},
		{name: "unrelated web search collision", err: &openaisdk.Error{StatusCode: 400, Param: "text.format", Message: "invalid schema while web_search is enabled"}, request: enabledRequest},
		{name: "unrelated hosted limit collision", err: &openaisdk.Error{StatusCode: 400, Param: "text.format", Message: "invalid max_tool_calls schema example"}, request: enabledRequest},
		{name: "unknown future tool error", err: &openaisdk.Error{StatusCode: 400, Param: "tools[0].future", Message: "unsupported future field"}, request: enabledRequest},
		{name: "auth", err: &openaisdk.Error{StatusCode: 401, Param: "tools", Message: "web_search unauthorized"}, request: enabledRequest},
		{name: "rate limit", err: &openaisdk.Error{StatusCode: 429, Param: "max_tool_calls", Message: "rate limit"}, request: enabledRequest},
		{name: "disabled capability", err: &openaisdk.Error{StatusCode: 400, Param: "tools[0].type", Message: "web_search is not supported"}},
		{
			name: "empty ChatGPT capped rejection",
			err:  &openaisdk.Error{StatusCode: 400},
			request: Request{
				AuthAccountID: "workspace",
				HostedCapabilities: []provider.HostedCapability{{
					Kind: provider.HostedCapabilityWebSearch, MaxCalls: 1,
				}},
			},
			want: true,
		},
		{
			name: "empty API key capped rejection",
			err:  &openaisdk.Error{StatusCode: 400},
			request: Request{HostedCapabilities: []provider.HostedCapability{{
				Kind: provider.HostedCapabilityWebSearch, MaxCalls: 1,
			}}},
		},
		{
			name: "empty ChatGPT uncapped rejection",
			err:  &openaisdk.Error{StatusCode: 400},
			request: Request{
				AuthAccountID:      "workspace",
				HostedCapabilities: []provider.HostedCapability{{Kind: provider.HostedCapabilityWebSearch}},
			},
		},
		{
			name: "unknown ChatGPT error body",
			err:  &unknownBody,
			request: Request{
				AuthAccountID: "workspace",
				HostedCapabilities: []provider.HostedCapability{{
					Kind: provider.HostedCapabilityWebSearch, MaxCalls: 1,
				}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, got := hostedCapabilityFailure(test.err, test.request)
			if got != test.want {
				t.Fatalf("classified = %t, want %t", got, test.want)
			}
		})
	}
}

func TestRequestOmitsServiceTierAndThinkingModeByDefault(t *testing.T) {
	t.Parallel()

	params, err := Request{
		Model: "test-model",
		Input: []Item{
			NewMessage("user", "task"),
		},
	}.toSDKParams()
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, unwanted := range []string{
		`"service_tier":`,
		`"reasoning":`,
	} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("SDK payload should omit %s: %s", unwanted, got)
		}
	}
}

func TestRequestSupportsLowThinkingMode(t *testing.T) {
	t.Parallel()

	params, err := Request{
		Model:        "test-model",
		ThinkingMode: "low",
		Input: []Item{
			NewMessage("user", "task"),
		},
	}.toSDKParams()
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, `"reasoning":{"effort":"low"}`) {
		t.Fatalf("SDK payload missing low reasoning effort: %s", got)
	}
}

func TestCreateResponseUsesChatGPTRequestContract(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "workspace-123" {
			t.Fatalf("ChatGPT-Account-ID = %q", got)
		}
		if got := r.Header.Get("originator"); got != codexClientIdentity {
			t.Fatalf("originator = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != codexClientIdentity {
			t.Fatalf("User-Agent = %q", got)
		}
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "gpt-5.6-sol" {
			t.Fatalf("model = %q", payload.Model)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ")
		fmt.Fprint(w, `{"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","object":"response","created_at":0,"status":"completed","model":"test-model","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}]}}`)
		fmt.Fprint(w, "\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	resp, err := NewHTTPClient(server.Client()).CreateResponse(context.Background(), Request{
		Model:         "gpt-5.6",
		BaseURL:       server.URL,
		APIKey:        "access-token",
		AuthAccountID: "workspace-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hello" {
		t.Fatalf("text = %q", resp.Text)
	}
}

func TestCreateResponseClassifiesEmptyChatGPTCappedBadRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	_, err := NewHTTPClient(server.Client()).CreateResponse(t.Context(), Request{
		Model: "test-model", BaseURL: server.URL, APIKey: "access-token", AuthAccountID: "workspace-123",
		HostedCapabilities: []provider.HostedCapability{{Kind: provider.HostedCapabilityWebSearch, MaxCalls: 1}},
	})
	unsupported, ok := errors.AsType[*provider.UnsupportedCapabilityError](err)
	if !ok || unsupported.Failure.Capability != provider.HostedCapabilityWebSearch {
		t.Fatalf("error = %v, want hosted web-search capability rejection", err)
	}
}

func TestCreateEmbeddingsSendsDimensions(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q", got)
		}
		var payload struct {
			Input      []string `json:"input"`
			Model      string   `json:"model"`
			Dimensions int      `json:"dimensions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "text-embedding-3-small" {
			t.Fatalf("model = %q", payload.Model)
		}
		if payload.Dimensions != 1024 {
			t.Fatalf("dimensions = %d", payload.Dimensions)
		}
		if len(payload.Input) != 2 || payload.Input[0] != "alpha" || payload.Input[1] != "beta" {
			t.Fatalf("input = %#v", payload.Input)
		}
		data := make([]map[string]any, len(payload.Input))
		for i := range payload.Input {
			data[i] = map[string]any{
				"object":    "embedding",
				"index":     i,
				"embedding": testEmbeddingVector(payload.Dimensions),
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"model":  payload.Model,
			"data":   data,
			"usage":  map[string]any{"prompt_tokens": 1, "total_tokens": 1},
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	resp, err := NewHTTPClient(server.Client()).CreateEmbeddings(t.Context(), EmbeddingRequest{
		Model:      "text-embedding-3-small",
		Dimensions: 1024,
		BaseURL:    server.URL,
		APIKey:     "test-key",
		Inputs:     []string{"alpha", "beta"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Dimensions != 1024 {
		t.Fatalf("response dimensions = %d", resp.Dimensions)
	}
	if len(resp.Vectors) != 2 || len(resp.Vectors[0]) != 1024 || len(resp.Vectors[1]) != 1024 {
		t.Fatalf("vectors = %#v", resp.Vectors)
	}
}

func TestNewHTTPClientInstallsBoundedDialTransport(t *testing.T) {
	t.Parallel()

	raw := &http.Client{Timeout: time.Minute}
	client := NewHTTPClient(raw).HTTPClient

	if client == raw {
		t.Fatal("NewHTTPClient should clone the supplied client before adding transport defaults")
	}
	if raw.Transport != nil {
		t.Fatal("NewHTTPClient mutated the supplied client")
	}
	if client.Timeout != time.Minute {
		t.Fatalf("Timeout = %v, want %v", client.Timeout, time.Minute)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("DialContext is nil")
	}
}

func TestNewHTTPClientPreservesCustomTransport(t *testing.T) {
	t.Parallel()

	custom := &errorRoundTripper{err: errors.New("boom")}
	client := NewHTTPClient(&http.Client{Transport: custom}).HTTPClient

	if client.Transport != custom {
		t.Fatalf("Transport = %#v, want custom transport", client.Transport)
	}
}

func TestCreateResponseReportsUpstreamFailure(t *testing.T) {
	t.Parallel()

	upstreamErr := errors.New("dial tcp: i/o timeout")
	client := NewHTTPClient(&http.Client{Transport: &errorRoundTripper{err: upstreamErr}})

	_, err := client.CreateResponse(context.Background(), Request{
		Model:   "test-model",
		BaseURL: "http://upstream.invalid",
		APIKey:  "test-key",
		Input: []Item{
			NewMessage("user", "task"),
		},
	})
	if err == nil {
		t.Fatal("expected upstream failure")
	}
	if !strings.Contains(err.Error(), "upstream request failed") {
		t.Fatalf("error = %q, want upstream failure context", err)
	}
	if !errors.Is(err, upstreamErr) {
		t.Fatalf("error does not wrap upstream cause: %v", err)
	}
}

func TestRequestSupportsStructuredJSONTextFormat(t *testing.T) {
	t.Parallel()

	params, err := Request{
		Model: "test-model",
		Input: []Item{
			NewMessage("user", "task"),
		},
		TextFormat: &TextFormat{
			Name:        "release_note",
			Description: "Structured release note payload.",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title": map[string]any{"type": "string"},
				},
				"required":             []string{"title"},
				"additionalProperties": false,
			},
			Strict: true,
		},
	}.toSDKParams()
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	text, ok := got["text"].(map[string]any)
	if !ok {
		t.Fatalf("text = %#v", got["text"])
	}
	format, ok := text["format"].(map[string]any)
	if !ok {
		t.Fatalf("format = %#v", text["format"])
	}
	if format["type"] != "json_schema" {
		t.Fatalf("format.type = %#v", format["type"])
	}
	if format["name"] != "release_note" {
		t.Fatalf("format.name = %#v", format["name"])
	}
	if format["description"] != "Structured release note payload." {
		t.Fatalf("format.description = %#v", format["description"])
	}
	if format["strict"] != true {
		t.Fatalf("format.strict = %#v", format["strict"])
	}
	schema, ok := format["schema"].(map[string]any)
	if !ok {
		t.Fatalf("schema = %#v", format["schema"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %#v", schema["properties"])
	}
	title, ok := properties["title"].(map[string]any)
	if !ok {
		t.Fatalf("title = %#v", properties["title"])
	}
	if title["type"] != "string" {
		t.Fatalf("title.type = %#v", title["type"])
	}
}

type errorRoundTripper struct {
	err error
}

func (r *errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, r.err
}

func testEmbeddingVector(dimensions int) []float64 {
	vector := make([]float64, dimensions)
	if dimensions > 0 {
		vector[0] = 1
	}
	return vector
}

func TestMarshalTraceJSONRedactsAPIKey(t *testing.T) {
	t.Parallel()

	data, err := Request{
		Model:   "m",
		APIKey:  "secret",
		BaseURL: "http://example",
		Instructions: strings.Join([]string{
			"<environment_context>",
			"<cwd>/repo</cwd>",
			"<mode>normal</mode>",
			"</environment_context>",
		}, "\n"),
		Input: []Item{
			NewMessage("developer", strings.Join([]string{
				"<tool_policy>",
				"Use read-only tools.",
				"</tool_policy>",
			}, "\n")),
			NewFunctionCallOutput("call_1", `{"ok":true,"data":{"paths":["README.md"]}}`),
		},
	}.MarshalTraceJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatalf("trace leaked API key: %s", data)
	}
	if strings.Contains(string(data), `\u003c`) {
		t.Fatalf("trace kept escaped angle brackets: %s", data)
	}
	for _, forbidden := range []string{"<tool_policy>", "</tool_policy>", "<environment_context>", "</environment_context>"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("trace kept wrapper tag %q: %s", forbidden, data)
		}
	}
	if strings.Contains(string(data), `\"ok\":true`) {
		t.Fatalf("trace kept nested json as escaped text: %s", data)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	input, ok := got["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("input = %#v", got["input"])
	}
	if got["instructions"] != "cwd /repo /cwd\nmode normal /mode" {
		t.Fatalf("instructions = %#v", got["instructions"])
	}
	message, ok := input[0].(map[string]any)
	if !ok {
		t.Fatalf("message = %#v", input[0])
	}
	if message["content"] != "Use read-only tools." {
		t.Fatalf("message content = %#v", message["content"])
	}
	item, ok := input[1].(map[string]any)
	if !ok {
		t.Fatalf("item = %#v", input[1])
	}
	output, ok := item["output"].(map[string]any)
	if !ok {
		t.Fatalf("output type = %T", item["output"])
	}
	if output["ok"] != true {
		t.Fatalf("output = %#v", output)
	}
}

func TestTraceValueReturnsStructuredSanitizedRequest(t *testing.T) {
	t.Parallel()

	value, err := Request{
		Model:   "m",
		APIKey:  "secret",
		BaseURL: "http://example",
		Instructions: strings.Join([]string{
			"<environment_context>",
			"<cwd>/repo</cwd>",
			"</environment_context>",
		}, "\n"),
		Input: []Item{
			NewMessage("developer", strings.Join([]string{
				"<tool_policy>",
				"Use read-only tools.",
				"</tool_policy>",
			}, "\n")),
			NewFunctionCallOutput("call_1", `{"ok":true,"data":{"paths":["README.md"]}}`),
		},
	}.TraceValue()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := value["api_key"]; ok {
		t.Fatalf("trace leaked api_key field: %#v", value)
	}
	if value["instructions"] != "cwd /repo /cwd" {
		t.Fatalf("instructions = %#v", value["instructions"])
	}
	input, ok := value["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("input = %#v", value["input"])
	}
	message, ok := input[0].(map[string]any)
	if !ok || message["content"] != "Use read-only tools." {
		t.Fatalf("message = %#v", input[0])
	}
	item, ok := input[1].(map[string]any)
	if !ok {
		t.Fatalf("item = %#v", input[1])
	}
	output, ok := item["output"].(map[string]any)
	if !ok || output["ok"] != true {
		t.Fatalf("output = %#v", item["output"])
	}
}

func TestCreateResponseCollectsStreamedToolCallsWithoutCompletedPayload(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{
			{
				"type":            "response.created",
				"sequence_number": 1,
				"response": map[string]any{
					"id":     "resp_1",
					"status": "in_progress",
				},
			},
			{
				"type":            "response.output_item.added",
				"sequence_number": 2,
				"output_index":    0,
				"item": map[string]any{
					"id":        "fc_1",
					"type":      "function_call",
					"status":    "in_progress",
					"call_id":   "call_1",
					"name":      "list_files",
					"arguments": "",
				},
			},
			{
				"type":            "response.function_call_arguments.delta",
				"sequence_number": 3,
				"output_index":    0,
				"item_id":         "fc_1",
				"delta":           `{"path":"docs",`,
			},
			{
				"type":            "response.function_call_arguments.delta",
				"sequence_number": 4,
				"output_index":    0,
				"item_id":         "fc_1",
				"delta":           `"max_entries":5}`,
			},
			{
				"type":            "response.function_call_arguments.done",
				"sequence_number": 5,
				"output_index":    0,
				"item_id":         "fc_1",
				"name":            "list_files",
				"arguments":       `{"path":"docs","max_entries":5}`,
			},
			{
				"type":            "response.completed",
				"sequence_number": 6,
				"response": map[string]any{
					"id":         "resp_1",
					"object":     "response",
					"created_at": 0,
					"status":     "completed",
					"model":      "test-model",
					"output":     []any{},
				},
			},
		} {
			fmt.Fprintf(w, "data: %s\n\n", marshalJSON(event))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := NewHTTPClient(server.Client())
	resp, err := client.CreateResponse(context.Background(), Request{
		Model:   "test-model",
		BaseURL: server.URL,
		APIKey:  "test-key",
		Tools: []ToolSpec{{
			Name:        "list_files",
			Description: "list files",
			Schema:      map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
			Strict:      true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if resp.ID != "resp_1" {
		t.Fatalf("response id = %q", resp.ID)
	}
	if resp.FinishKind != "completed" {
		t.Fatalf("finish_kind = %q", resp.FinishKind)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", resp.ToolCalls)
	}
	call := resp.ToolCalls[0]
	if call.ID != "fc_1" || call.CallID != "call_1" || call.Name != "list_files" || call.Arguments != `{"path":"docs","max_entries":5}` {
		t.Fatalf("tool call = %#v", call)
	}
}

func TestCreateResponseStreamsReasoningSummaries(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{
			{
				"type":            "response.reasoning_summary_text.delta",
				"sequence_number": 1,
				"output_index":    0,
				"item_id":         "rs_1",
				"summary_index":   0,
				"delta":           "Inspecting ",
			},
			{
				"type":            "response.reasoning_summary_text.done",
				"sequence_number": 2,
				"output_index":    0,
				"item_id":         "rs_1",
				"summary_index":   0,
				"text":            "Inspecting changed files",
			},
			{
				"type":            "response.completed",
				"sequence_number": 3,
				"response": map[string]any{
					"id":         "resp_1",
					"object":     "response",
					"created_at": 0,
					"status":     "completed",
					"model":      "test-model",
					"output": []map[string]any{{
						"id":     "msg_1",
						"type":   "message",
						"status": "completed",
						"role":   "assistant",
						"content": []map[string]any{{
							"type":        "output_text",
							"text":        "done",
							"annotations": []any{},
						}},
					}},
				},
			},
		} {
			fmt.Fprintf(w, "data: %s\n\n", marshalJSON(event))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	var events []StreamEvent
	response, err := NewHTTPClient(server.Client()).CreateResponse(t.Context(), Request{
		Model:            "test-model",
		BaseURL:          server.URL,
		APIKey:           "test-key",
		ReasoningSummary: ReasoningSummaryAuto,
		OnStreamEvent: func(event StreamEvent) error {
			events = append(events, event)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Text != "done" {
		t.Fatalf("response text = %q", response.Text)
	}
	if len(events) != 2 {
		t.Fatalf("stream events = %#v", events)
	}
	if events[0].Kind != "reasoning_summary.delta" || events[0].ProviderAttempt != 1 || events[0].Delta != "Inspecting " {
		t.Fatalf("delta event = %#v", events[0])
	}
	if events[1].Kind != "reasoning_summary.done" || events[1].ProviderAttempt != 1 || events[1].Text != "Inspecting changed files" {
		t.Fatalf("done event = %#v", events[1])
	}
}

func TestCreateResponsePrefersCompletedPayloadTextAndFallsBackToStreamText(t *testing.T) {
	t.Parallel()

	t.Run("completed_payload", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", marshalJSON(map[string]any{
				"type":            "response.completed",
				"sequence_number": 1,
				"response": map[string]any{
					"id":         "resp_text",
					"object":     "response",
					"created_at": 0,
					"status":     "completed",
					"model":      "test-model",
					"output": []map[string]any{{
						"id":     "msg_1",
						"type":   "message",
						"status": "completed",
						"role":   "assistant",
						"content": []map[string]any{{
							"type":        "output_text",
							"text":        "hello from completed payload",
							"annotations": []any{},
						}},
					}},
				},
			}))
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		defer server.Close()

		resp, err := NewHTTPClient(server.Client()).CreateResponse(context.Background(), Request{
			Model:   "test-model",
			BaseURL: server.URL,
			APIKey:  "test-key",
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Text != "hello from completed payload" {
			t.Fatalf("text = %q", resp.Text)
		}
	})

	t.Run("stream_text_fallback", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []map[string]any{
				{
					"type":            "response.output_item.added",
					"sequence_number": 1,
					"output_index":    0,
					"item": map[string]any{
						"id":     "msg_1",
						"type":   "message",
						"status": "in_progress",
						"role":   "assistant",
						"content": []map[string]any{{
							"type":        "output_text",
							"text":        "",
							"annotations": []any{},
						}},
					},
				},
				{
					"type":            "response.output_text.delta",
					"sequence_number": 2,
					"output_index":    0,
					"item_id":         "msg_1",
					"content_index":   0,
					"delta":           "hello ",
					"logprobs":        []any{},
				},
				{
					"type":            "response.output_text.delta",
					"sequence_number": 3,
					"output_index":    0,
					"item_id":         "msg_1",
					"content_index":   0,
					"delta":           "world",
					"logprobs":        []any{},
				},
				{
					"type":            "response.completed",
					"sequence_number": 4,
					"response": map[string]any{
						"id":         "resp_stream_text",
						"object":     "response",
						"created_at": 0,
						"status":     "completed",
						"model":      "test-model",
						"output":     []any{},
					},
				},
			} {
				fmt.Fprintf(w, "data: %s\n\n", marshalJSON(event))
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		}))
		defer server.Close()

		resp, err := NewHTTPClient(server.Client()).CreateResponse(context.Background(), Request{
			Model:   "test-model",
			BaseURL: server.URL,
			APIKey:  "test-key",
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Text != "hello world" {
			t.Fatalf("text = %q", resp.Text)
		}
	})
}

func TestCreateResponseRepairsIncompleteCompletedToolCallsFromStream(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{
			{
				"type":            "response.output_item.added",
				"sequence_number": 1,
				"output_index":    0,
				"item": map[string]any{
					"id":        "fc_1",
					"type":      "function_call",
					"status":    "in_progress",
					"call_id":   "call_1",
					"name":      "",
					"arguments": "",
				},
			},
			{
				"type":            "response.function_call_arguments.done",
				"sequence_number": 2,
				"output_index":    0,
				"item_id":         "fc_1",
				"name":            "git_staged_paths",
				"arguments":       `{}`,
			},
			{
				"type":            "response.completed",
				"sequence_number": 3,
				"response": map[string]any{
					"id":         "resp_incomplete_tool",
					"object":     "response",
					"created_at": 0,
					"status":     "completed",
					"model":      "test-model",
					"output": []map[string]any{{
						"id":        "fc_1",
						"type":      "function_call",
						"status":    "completed",
						"call_id":   "",
						"name":      "",
						"arguments": "",
					}},
				},
			},
		} {
			fmt.Fprintf(w, "data: %s\n\n", marshalJSON(event))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	resp, err := NewHTTPClient(server.Client()).CreateResponse(context.Background(), Request{
		Model:   "test-model",
		BaseURL: server.URL,
		APIKey:  "test-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", resp.ToolCalls)
	}
	call := resp.ToolCalls[0]
	if call.Name != "git_staged_paths" || call.Arguments != `{}` || call.CallID != "call_1" {
		t.Fatalf("repaired call = %#v", call)
	}
}

func TestCreateResponseRetriesStreamingOnMalformedStreamJSON(t *testing.T) {
	t.Parallel()

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assertStreamingRequest(t, r)
		if requests == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {")
			return
		}
		writeCompletedSSE(t, w, "resp_retry", "retry text")
	}))
	defer server.Close()

	var retryEvents []RetryEvent
	resp, err := NewHTTPClient(server.Client()).CreateResponse(context.Background(), Request{
		Model:   "test-model",
		BaseURL: server.URL,
		APIKey:  "test-key",
		OnRetry: func(event RetryEvent) error {
			retryEvents = append(retryEvents, event)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if resp.Text != "retry text" {
		t.Fatalf("text = %q", resp.Text)
	}
	if len(retryEvents) != 1 || retryEvents[0].Attempt != 1 || retryEvents[0].MaxAttempts != 1 || retryEvents[0].Reason != RetryReasonMalformedStream {
		t.Fatalf("retry events = %#v", retryEvents)
	}
}

func TestCreateResponseRetriesStreamingAfterPeerHTTP2Reset(t *testing.T) {
	t.Parallel()

	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		assertStreamingRequest(t, request)
		body := io.ReadCloser(&readErrorCloser{err: errors.New("stream error: stream ID 15; INTERNAL_ERROR; received from peer")})
		if requests == 2 {
			body = io.NopCloser(strings.NewReader(completedSSE("resp_retry", "recovered")))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       body,
			Request:    request,
		}, nil
	})
	var retryEvents []RetryEvent
	response, err := NewHTTPClient(&http.Client{Transport: transport}).CreateResponse(t.Context(), Request{
		Model:   "test-model",
		BaseURL: "http://provider.test",
		APIKey:  "test-key",
		OnRetry: func(event RetryEvent) error {
			retryEvents = append(retryEvents, event)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || response.Text != "recovered" {
		t.Fatalf("requests = %d, response = %#v", requests, response)
	}
	if len(retryEvents) != 1 || retryEvents[0].Reason != RetryReasonPeerStreamReset {
		t.Fatalf("retry events = %#v", retryEvents)
	}
}

func TestCreateResponseRetriesStreamWithoutCompletedEvent(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assertStreamingRequest(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			fmt.Fprintf(w, "data: %s\n\n", marshalJSON(map[string]any{
				"type": "response.reasoning_summary_text.delta", "sequence_number": 1,
				"output_index": 0, "item_id": "rs_abandoned", "summary_index": 0,
				"delta": "abandoned reasoning",
			}))
			fmt.Fprintf(w, "data: %s\n\n", marshalJSON(map[string]any{
				"type": "response.output_text.delta", "sequence_number": 2,
				"output_index": 0, "item_id": "msg_partial", "content_index": 0,
				"delta": "discarded partial text", "logprobs": []any{},
			}))
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", marshalJSON(map[string]any{
			"type": "response.reasoning_summary_text.delta", "sequence_number": 1,
			"output_index": 0, "item_id": "rs_retry", "summary_index": 0,
			"delta": "retry reasoning",
		}))
		fmt.Fprint(w, completedSSE("resp_retry", "complete retry"))
	}))
	defer server.Close()

	var timeline []string
	response, err := NewHTTPClient(server.Client()).CreateResponse(t.Context(), Request{
		Model: "test-model", BaseURL: server.URL, APIKey: "test-key",
		OnStreamEvent: func(event StreamEvent) error {
			timeline = append(timeline, fmt.Sprintf("stream:%d:%s", event.ProviderAttempt, event.Delta))
			return nil
		},
		OnRetry: func(event RetryEvent) error {
			timeline = append(timeline, fmt.Sprintf("retry:%d", event.Attempt))
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || response.Text != "complete retry" {
		t.Fatalf("requests = %d, response = %#v", requests, response)
	}
	wantTimeline := []string{"stream:1:abandoned reasoning", "retry:1", "stream:2:retry reasoning"}
	if !slices.Equal(timeline, wantTimeline) {
		t.Fatalf("timeline = %#v, want %#v", timeline, wantTimeline)
	}
}

func TestCreateResponseStopsAfterOneStreamingRetry(t *testing.T) {
	t.Parallel()

	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		assertStreamingRequest(t, request)
		attempt := "retry attempt"
		if requests == 1 {
			attempt = "first attempt"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &readErrorCloser{err: fmt.Errorf("unexpected EOF during %s", attempt)},
			Request:    request,
		}, nil
	})

	_, err := NewHTTPClient(&http.Client{Transport: transport}).CreateResponse(t.Context(), Request{
		Model: "test-model", BaseURL: "http://provider.test", APIKey: "test-key",
	})
	if err == nil || !strings.Contains(err.Error(), "first attempt") || !strings.Contains(err.Error(), "retry attempt") {
		t.Fatalf("error = %v, want both attempt failures", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestCreateResponseDoesNotRetryLocalStreamEventFailure(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assertStreamingRequest(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", marshalJSON(map[string]any{
			"type": "response.reasoning_summary_text.delta", "sequence_number": 1,
			"output_index": 0, "item_id": "rs_1", "summary_index": 0, "delta": "working",
		}))
	}))
	defer server.Close()

	publishErr := errors.New("unexpected EOF in local trace sink")
	_, err := NewHTTPClient(server.Client()).CreateResponse(t.Context(), Request{
		Model: "test-model", BaseURL: server.URL, APIKey: "test-key",
		OnStreamEvent: func(StreamEvent) error {
			return publishErr
		},
	})
	if !errors.Is(err, publishErr) {
		t.Fatalf("error = %v, want local trace failure", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestCreateResponsePreservesInitialFailureWhenRetryStreamEventFails(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assertStreamingRequest(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		if requests == 1 {
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", marshalJSON(map[string]any{
			"type": "response.reasoning_summary_text.delta", "sequence_number": 1,
			"output_index": 0, "item_id": "rs_retry", "summary_index": 0, "delta": "working",
		}))
	}))
	defer server.Close()

	publishErr := errors.New("retry trace sink failed")
	_, err := NewHTTPClient(server.Client()).CreateResponse(t.Context(), Request{
		Model: "test-model", BaseURL: server.URL, APIKey: "test-key",
		OnStreamEvent: func(StreamEvent) error {
			return publishErr
		},
	})
	if !errors.Is(err, errIncompleteProviderStream) || !errors.Is(err, publishErr) {
		t.Fatalf("error = %v, want initial stream and retry publication failures", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestCreateResponseDoesNotRetryWhenRetryProgressFails(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assertStreamingRequest(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {")
	}))
	defer server.Close()

	publishErr := errors.New("retry progress sink failed")
	_, err := NewHTTPClient(server.Client()).CreateResponse(t.Context(), Request{
		Model: "test-model", BaseURL: server.URL, APIKey: "test-key",
		OnRetry: func(RetryEvent) error {
			return publishErr
		},
	})
	if !errors.Is(err, publishErr) {
		t.Fatalf("error = %v, want retry progress failure", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestCreateResponseCancellationPreventsStreamingRetry(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assertStreamingRequest(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {")
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(t.Context())
	_, err := NewHTTPClient(server.Client()).CreateResponse(ctx, Request{
		Model: "test-model", BaseURL: server.URL, APIKey: "test-key",
		OnRetry: func(RetryEvent) error {
			cancel()
			return nil
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestStreamRetryClassificationIsNarrow(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		err        error
		wantReason RetryReason
		want       bool
	}{
		{name: "unexpected EOF identity", err: io.ErrUnexpectedEOF, wantReason: RetryReasonMalformedStream, want: true},
		{name: "malformed JSON", err: errors.New("unexpected end of JSON input"), wantReason: RetryReasonMalformedStream, want: true},
		{name: "wrapped unexpected EOF", err: fmt.Errorf("decode stream: %w", errors.New("unexpected EOF")), wantReason: RetryReasonMalformedStream, want: true},
		{name: "peer internal", err: errors.New("stream error: stream ID 55; INTERNAL_ERROR; received from peer"), wantReason: RetryReasonPeerStreamReset, want: true},
		{name: "wrapped peer refused", err: fmt.Errorf("provider stream: %w", errors.New("stream error: stream ID 57; REFUSED_STREAM; received from peer")), wantReason: RetryReasonPeerStreamReset, want: true},
		{name: "cancel", err: errors.New("stream error: stream ID 55; CANCEL; received from peer")},
		{name: "local internal", err: errors.New("stream error: stream ID 55; INTERNAL_ERROR")},
		{name: "unrelated collision", err: errors.New("not a stream error: stream ID 55; INTERNAL_ERROR; received from peer")},
		{name: "malformed stream id collision", err: errors.New("stream error: stream ID backup; INTERNAL_ERROR; received from peer")},
		{name: "unknown future code", err: errors.New("stream error: stream ID 59; ENHANCE_YOUR_CALM; received from peer")},
		{name: "provider error text collision", err: &openaisdk.Error{StatusCode: 400, Message: "unexpected EOF"}},
		{name: "nil"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reason, got := streamRetryReason(test.err)
			if got != test.want || reason != test.wantReason {
				t.Fatalf("streamRetryReason() = (%q, %v), want (%q, %v)", reason, got, test.wantReason, test.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type readErrorCloser struct {
	err error
}

func (r *readErrorCloser) Read([]byte) (int, error) { return 0, r.err }
func (r *readErrorCloser) Close() error             { return nil }

func assertStreamingRequest(t *testing.T, request *http.Request) {
	t.Helper()
	var payload struct {
		Stream bool `json:"stream"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Stream {
		t.Fatal("Responses retry omitted stream=true")
	}
}

func writeCompletedSSE(t *testing.T, writer http.ResponseWriter, responseID, text string) {
	t.Helper()
	writer.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(writer, completedSSE(responseID, text))
}

func completedSSE(responseID, text string) string {
	event := map[string]any{
		"type": "response.completed", "sequence_number": 1,
		"response": map[string]any{
			"id": responseID, "object": "response", "created_at": 0,
			"status": "completed", "model": "test-model",
			"output": []map[string]any{{
				"id": "msg_1", "type": "message", "status": "completed", "role": "assistant",
				"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
			}},
		},
	}
	return fmt.Sprintf("data: %s\n\ndata: [DONE]\n\n", marshalJSON(event))
}

func marshalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}
