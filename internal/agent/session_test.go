package agent

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/tools"
)

func TestRunnerSDKSessionCompactionAndFollowup(t *testing.T) {
	const task = "Inspect the repository and preserve this task"
	const policy = "Developer policy must survive compaction"
	const instructions = "System task instructions must survive compaction"
	const opaque = "opaque-compacted-session"
	var requests, compactions atomic.Int32
	modelServer := responseServer(t, func(input map[string]jsontext.Value) []any {
		if !bytes.Contains(input["instructions"], []byte(instructions)) {
			t.Error("model request lost task instructions")
		}
		if requests.Add(1) <= 6 {
			output := execOutput(`text(await tools.echo({}));`)
			call := output[0].(map[string]any)
			call["call_id"] = fmt.Sprintf("call_%d", requests.Load())
			call["id"] = fmt.Sprintf("fc_%d", requests.Load())
			return output
		}
		if requests.Load() == 7 {
			return textOutput("done")
		}
		for _, want := range []string{opaque, task, policy} {
			if !bytes.Contains(input["input"], []byte(want)) {
				t.Errorf("post-compaction model input lost %q: %.1500s", want, input["input"])
			}
		}
		if bytes.Contains(input["input"], []byte("uncompressed-evidence")) {
			t.Error("post-compaction model input retained oversized tool output")
		}
		if requests.Load() == 8 && !bytes.Contains(input["input"], []byte("Follow up using compacted history")) {
			t.Error("followup prompt missing")
		}
		return textOutput("done")
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/compact" {
			modelServer.Config.Handler.ServeHTTP(w, r)
			return
		}
		compactions.Add(1)
		var body struct {
			Model string           `json:"model"`
			Input []jsontext.Value `json:"input"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			t.Error(err)
			http.Error(w, "bad compaction request", http.StatusBadRequest)
			return
		}
		serialized, err := json.Marshal(body.Input)
		if err != nil {
			t.Error(err)
		}
		if body.Model != "gpt-test" || !bytes.Contains(serialized, []byte("uncompressed-evidence")) {
			t.Error("compaction did not receive oversized tool evidence for the configured model")
		}
		for _, want := range []string{task, policy} {
			if !bytes.Contains(serialized, []byte(want)) {
				t.Errorf("compaction input lost %q: %.1500s", want, serialized)
			}
		}
		output := make([]jsontext.Value, 0)
		for _, item := range body.Input {
			var message struct {
				Role    string         `json:"role"`
				Content jsontext.Value `json:"content"`
			}
			if err := json.Unmarshal(item, &message); err != nil {
				t.Error(err)
			}
			if message.Role == "user" || message.Role == "developer" || message.Role == "system" {
				content := message.Content
				if content.Kind() == '"' {
					var text string
					if err := json.Unmarshal(content, &text); err != nil {
						t.Error(err)
					}
					content, err = json.Marshal([]any{map[string]any{"type": "input_text", "text": text}})
					if err != nil {
						t.Error(err)
					}
				}
				retained, err := json.Marshal(map[string]any{"type": "message", "role": message.Role, "content": content})
				if err != nil {
					t.Error(err)
				}
				output = append(output, retained)
			}
		}
		output = append(output, jsontext.Value(`{"type":"compaction","id":"cmp_session","encrypted_content":"`+opaque+`"}`))
		w.Header().Set("Content-Type", "application/json")
		if err := json.MarshalWrite(w, map[string]any{
			"id": "resp_compact", "object": "response.compaction", "created_at": 1, "output": output,
			"usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "total_tokens": 110},
		}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	var usage bytes.Buffer
	runner := OpenAIRunner{
		Config: runnerConfig(server), ToolSpecs: echoSpec(), UsageOutput: &usage,
		Tools: testExecutor{execute: func(context.Context, tools.Invocation) (tools.Result, error) {
			return tools.Result{Content: `{"ok":true,"data":"` + strings.Repeat("uncompressed-evidence ", 1000) + `"}`}, nil
		}},
	}
	runner.Config.Model = "gpt-test"
	runner.Config.MaxSteps = 8
	request := Request{SystemPrompt: instructions, DeveloperInstructions: policy, UserPrompt: task}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	result, err := runner.Run(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "done" || result.ToolCalls != 6 || compactions.Load() != 1 || requests.Load() != 7 {
		t.Fatalf("text=%q tools=%d compactions=%d requests=%d", result.Text, result.ToolCalls, compactions.Load(), requests.Load())
	}
	if !strings.Contains(usage.String(), "input_tokens=100") {
		t.Fatal("missing SDK compaction usage")
	}
	history, err := json.Marshal(result.History())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(history, []byte(opaque)) || bytes.Contains(history, []byte("uncompressed-evidence")) {
		t.Fatalf("persisted history compacted=%v old_evidence=%v: %.1500s", bytes.Contains(history, []byte(opaque)), bytes.Contains(history, []byte("uncompressed-evidence")), history)
	}
	followup, err := runner.Run(ctx, Request{
		SystemPrompt: instructions,
		Input:        append(result.History(), openai.NewMessage("user", "Follow up using compacted history")),
	})
	if err != nil || followup.Text != "done" || compactions.Load() != 1 || requests.Load() != 8 {
		t.Fatalf("followup=%+v err=%v compactions=%d requests=%d", followup, err, compactions.Load(), requests.Load())
	}
}
