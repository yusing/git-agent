package agent

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/tools"
)

func responseServer(t *testing.T, next func(map[string]jsontext.Value) []any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]jsontext.Value
		if err := json.UnmarshalRead(r.Body, &input); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		output := next(input)
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(value any) {
			data, err := json.Marshal(value)
			if err != nil {
				t.Error(err)
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		for index, item := range output {
			event(map[string]any{"type": "response.output_item.added", "output_index": index, "item": item})
			event(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
		}
		event(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_test", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12, "input_tokens_details": map[string]any{"cached_tokens": 3}}}})
	}))
	t.Cleanup(server.Close)
	return server
}
func textOutput(text string) []any {
	return []any{map[string]any{"type": "message", "id": "msg_test", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}}
}
func execOutput(code string) []any {
	args, _ := json.Marshal(map[string]any{"code": code})
	return []any{map[string]any{"type": "function_call", "id": "fc_test", "call_id": "call_test", "name": "exec", "arguments": string(args)}}
}
func runnerConfig(server *httptest.Server) config.Config {
	return config.Config{APIKey: "synthetic-key", BaseURL: server.URL + "/v1", Model: "test-model", MaxSteps: 5}
}

type testExecutor struct {
	execute  func(context.Context, tools.Invocation) (tools.Result, error)
	snapshot func() error
}

func (e testExecutor) Execute(ctx context.Context, in tools.Invocation) (tools.Result, error) {
	return e.execute(ctx, in)
}
func (e testExecutor) CheckSnapshot() error {
	if e.snapshot != nil {
		return e.snapshot()
	}
	return nil
}
func echoSpec() []tools.Definition {
	return []tools.Definition{{Name: "echo", Description: "Echo arguments", Schema: map[string]any{"type": "object"}}}
}

func TestRunnerSDKJavaScriptParallelToolsAndHistory(t *testing.T) {
	var requests, active, peak atomic.Int32
	server := responseServer(t, func(input map[string]jsontext.Value) []any {
		if requests.Add(1) == 1 {
			if !bytes.Contains(input["tools"], []byte(`"name":"exec"`)) {
				t.Errorf("missing exec tool: %s", input["tools"])
			}
			return execOutput(`text(await Promise.all([tools.echo({n:1}),tools.echo({n:2})]));`)
		}
		if !bytes.Contains(input["input"], []byte(`function_call_output`)) {
			t.Error("missing replayed tool output")
		}
		return textOutput("done")
	})
	var usage bytes.Buffer
	runner := OpenAIRunner{Config: runnerConfig(server), ToolSpecs: echoSpec(), UsageOutput: &usage, Tools: testExecutor{execute: func(ctx context.Context, in tools.Invocation) (tools.Result, error) {
		count := active.Add(1)
		if count > peak.Load() {
			peak.Store(count)
		}
		select {
		case <-time.After(30 * time.Millisecond):
		case <-ctx.Done():
			return tools.Result{}, ctx.Err()
		}
		active.Add(-1)
		return tools.Result{Content: `{"ok":true,"data":` + in.Arguments + `}`}, nil
	}}}
	result, err := runner.Run(t.Context(), Request{UserPrompt: "Inspect", Environment: "test environment"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" || result.ToolCalls != 2 || peak.Load() != 2 {
		t.Fatalf("result=%+v peak=%d", result, peak.Load())
	}
	if !strings.Contains(usage.String(), "cached_input_tokens=3") {
		t.Fatalf("usage=%s", usage.String())
	}
	history := result.History()
	history[0].Type = "changed"
	if result.History()[0].Type == "changed" {
		t.Fatal("history aliases result")
	}
	followup, err := runner.Run(t.Context(), Request{Input: append(result.History(), openai.NewMessage("user", "Recall"))})
	if err != nil || followup.Text != "done" {
		t.Fatalf("followup=%+v err=%v", followup, err)
	}
}
func TestRunnerRepairsAndNormalizesOnce(t *testing.T) {
	var requests atomic.Int32
	server := responseServer(t, func(input map[string]jsontext.Value) []any {
		if requests.Add(1) == 1 {
			return textOutput("bad")
		}
		if string(input["tools"]) != "[]" {
			t.Errorf("repair exposed tools: %s", input["tools"])
		}
		return textOutput(" good ")
	})
	runner := OpenAIRunner{Config: runnerConfig(server), Normalize: strings.TrimSpace, Validator: func(text string) []string {
		if text != "good" {
			return []string{"expected good"}
		}
		return nil
	}}
	result, err := runner.Run(t.Context(), Request{UserPrompt: "Generate", RepairOnValidator: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "good" || result.RepairCalls != 1 || requests.Load() != 2 {
		t.Fatalf("result=%+v requests=%d", result, requests.Load())
	}
}
func TestRunnerToolFailureAndSnapshotDrift(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint(stale), func(t *testing.T) {
			var requests atomic.Int32
			server := responseServer(t, func(input map[string]jsontext.Value) []any {
				if requests.Add(1) == 1 {
					return execOutput(`text(await tools.echo({}));`)
				}
				if !bytes.Contains(input["input"], []byte("missing evidence")) {
					t.Errorf("missing error envelope: %s", input["input"])
				}
				return textOutput("recovered")
			})
			runner := OpenAIRunner{Config: runnerConfig(server), ToolSpecs: echoSpec(), Tools: testExecutor{execute: func(context.Context, tools.Invocation) (tools.Result, error) {
				return tools.Result{}, errors.New("missing evidence")
			}, snapshot: func() error {
				if stale {
					return gitctx.ErrChangeSnapshotStale
				}
				return nil
			}}}
			result, err := runner.Run(t.Context(), Request{UserPrompt: "Inspect"})
			if stale {
				if !errors.Is(err, gitctx.ErrChangeSnapshotStale) {
					t.Fatalf("err=%v", err)
				}
				if requests.Load() != 1 {
					t.Fatal("continued after snapshot drift")
				}
			} else if err != nil || result.Text != "recovered" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}
func TestRunnerSandboxErrorDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, code, diagnostic string }{
		{"output", `text('x'.repeat(65536));`, "JavaScript output limit exceeded; emit a smaller result"},
		{"redaction", `throw new Error('synthetic-key synthetic-account');`, "[REDACTED] [REDACTED]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := responseServer(t, func(input map[string]jsontext.Value) []any {
				if requests.Add(1) == 1 {
					return execOutput(tc.code)
				}
				var items []struct {
					Type   string `json:"type"`
					Output string `json:"output"`
				}
				if err := json.Unmarshal(input["input"], &items); err != nil {
					t.Error(err)
					return textOutput("invalid input")
				}
				var output string
				for _, item := range items {
					if item.Type == "function_call_output" {
						output = item.Output
					}
				}
				if !strings.Contains(output, tc.diagnostic) || strings.Contains(output, "synthetic-") {
					t.Errorf("tool diagnostic = %q, want %q with credentials redacted", output, tc.diagnostic)
				}
				return textOutput("recovered")
			})
			cfg := runnerConfig(server)
			cfg.AuthAccountID = "synthetic-account"
			runner := OpenAIRunner{Config: cfg, ToolSpecs: echoSpec()}
			result, err := runner.Run(t.Context(), Request{UserPrompt: "Inspect"})
			if err != nil || result.Text != "recovered" || requests.Load() != 2 {
				t.Fatalf("result=%+v requests=%d err=%v", result, requests.Load(), err)
			}
		})
	}
}

func TestRunnerCancels(t *testing.T) {
	var requests atomic.Int32
	server := responseServer(t, func(map[string]jsontext.Value) []any { requests.Add(1); return textOutput("done") })
	runner := OpenAIRunner{Config: runnerConfig(server)}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := runner.Run(ctx, Request{UserPrompt: "cancel"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestRunnerStepBudgetFinalizationAndExtension(t *testing.T) {
	for _, extend := range []bool{false, true} {
		t.Run(fmt.Sprint(extend), func(t *testing.T) {
			var requests atomic.Int32
			server := responseServer(t, func(input map[string]jsontext.Value) []any {
				if requests.Add(1) == 1 {
					return execOutput(`text(await tools.echo({}));`)
				}
				if !bytes.Contains(input["input"], []byte("budget-evidence")) {
					t.Error("budget transition lost tool evidence")
				}
				if !extend && string(input["tools"]) != "[]" {
					t.Errorf("finalization has tools: %s", input["tools"])
				}
				return textOutput("done")
			})
			var usage bytes.Buffer
			budgets := 0
			runner := OpenAIRunner{Config: runnerConfig(server), ToolSpecs: echoSpec(), UsageOutput: &usage,
				Tools: testExecutor{execute: func(context.Context, tools.Invocation) (tools.Result, error) {
					return tools.Result{Content: `{"ok":true,"data":"budget-evidence"}`}, nil
				}}, Budget: func(context.Context, BudgetStatus) (BudgetDecision, error) {
					budgets++
					if extend {
						return BudgetDecision{ExtendSteps: 2}, nil
					}
					return BudgetDecision{}, nil
				}}
			result, err := runner.Run(t.Context(), Request{UserPrompt: "Inspect", MaxSteps: 1})
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != "done" || budgets != 1 || requests.Load() != 2 || strings.Count(usage.String(), "llm.usage") != 2 {
				t.Fatalf("result=%+v budgets=%d requests=%d usage=%s", result, budgets, requests.Load(), usage.String())
			}
			history, _ := json.Marshal(result.History())
			if !bytes.Contains(history, []byte("budget-evidence")) {
				t.Fatal("history lost tool evidence")
			}
		})
	}
}

func TestRunnerReplaysLegacyResponsesHistory(t *testing.T) {
	server := responseServer(t, func(input map[string]jsontext.Value) []any {
		for _, want := range []string{"developer", "legacy policy", "encrypted_content", "legacy answer", "legacy-tool"} {
			if !bytes.Contains(input["input"], []byte(want)) {
				t.Errorf("missing %s: %s", want, input["input"])
			}
		}
		return textOutput("continued")
	})
	runner := OpenAIRunner{Config: runnerConfig(server)}
	_, err := runner.Run(t.Context(), Request{Input: []openai.Item{
		openai.NewMessage("developer", "legacy policy"),
		{Type: "reasoning", RawJSON: `{"type":"reasoning","id":"r","encrypted_content":"opaque","summary":[]}`},
		{Type: "function_call", CallID: "legacy", Name: "read_file", Arguments: `{}`},
		{Type: "function_call_output", CallID: "legacy", Output: "legacy-tool"},
		openai.NewMessage("assistant", "legacy answer"),
		openai.NewMessage("user", "continue"),
	}})
	if err != nil {
		t.Fatal(err)
	}
}
func TestRequestInstructionsExposeSchemasAndPathPolicy(t *testing.T) {
	text := requestInstructions("task", []tools.Definition{{Name: "read_file", Description: "Read a file", Schema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}})
	for _, want := range []string{"tools.read_file", "Promise.all", "text(value)", `"path"`, "copied verbatim"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
}
