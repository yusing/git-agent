package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestConvertsToSDKStructuredInputAndTools(t *testing.T) {
	t.Parallel()

	params, err := Request{
		Model:        "test-model",
		Instructions: "follow spec",
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

func TestMarshalTraceJSONRedactsAPIKey(t *testing.T) {
	t.Parallel()

	data, err := Request{Model: "m", APIKey: "secret", BaseURL: "http://example"}.MarshalTraceJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatalf("trace leaked API key: %s", data)
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

func marshalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}
