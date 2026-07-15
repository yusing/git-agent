package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	tests := []struct {
		name string
		err  *openaisdk.Error
		want bool
	}{
		{"web search", &openaisdk.Error{StatusCode: 400, Param: "tools[0].type", Message: "web_search is not supported"}, true},
		{"source include", &openaisdk.Error{StatusCode: 422, Param: "include", Message: "web search sources unsupported"}, true},
		{"hosted limit", &openaisdk.Error{StatusCode: 400, Param: "max_tool_calls", Message: "unknown parameter"}, true},
		{"unrelated bad request", &openaisdk.Error{StatusCode: 400, Param: "text.format", Message: "invalid schema"}, false},
		{"auth", &openaisdk.Error{StatusCode: 401, Param: "tools", Message: "web_search unauthorized"}, false},
		{"rate limit", &openaisdk.Error{StatusCode: 429, Param: "max_tool_calls", Message: "rate limit"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, got := hostedCapabilityFailure(test.err)
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
	if events[0].Kind != "reasoning_summary.delta" || events[0].Delta != "Inspecting " {
		t.Fatalf("delta event = %#v", events[0])
	}
	if events[1].Kind != "reasoning_summary.done" || events[1].Text != "Inspecting changed files" {
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

func TestCreateResponseFallsBackToNonStreamingOnMalformedStreamJSON(t *testing.T) {
	t.Parallel()

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, marshalJSON(map[string]any{
			"id":         "resp_fallback",
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
					"text":        "fallback text",
					"annotations": []any{},
				}},
			}},
		}))
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
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if resp.Text != "fallback text" {
		t.Fatalf("text = %q", resp.Text)
	}
}

func TestRetryWithoutStreamingForTransientHTTP2StreamErrors(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		message string
		want    bool
	}{
		{name: "internal", message: "stream error: stream ID 55; INTERNAL_ERROR; received from peer", want: true},
		{name: "refused stream", message: "stream error: stream ID 55; REFUSED_STREAM; received from peer", want: true},
		{name: "cancel", message: "stream error: stream ID 55; CANCEL; received from peer", want: false},
		{name: "local internal", message: "stream error: stream ID 55; INTERNAL_ERROR", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := fmt.Errorf("provider stream: %w", errors.New(test.message))
			if got := shouldRetryWithoutStreaming(err); got != test.want {
				t.Fatalf("shouldRetryWithoutStreaming() = %v, want %v", got, test.want)
			}
		})
	}
}

func marshalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}
