package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeExpandsEmbeddedJSONStrings(t *testing.T) {
	t.Parallel()

	normalized := Normalize(map[string]any{
		"call_id": "call_123",
		"output": `{
  "data": {
    "paths": [
      "README.md",
      "internal/agent/runner.go"
    ]
  },
  "ok": true,
  "tool": "git_staged_paths",
  "truncated": false
}`,
		"message": "plain text",
	})

	root, ok := normalized.(map[string]any)
	if !ok {
		t.Fatalf("normalized type = %T", normalized)
	}
	output, ok := root["output"].(map[string]any)
	if !ok {
		t.Fatalf("output type = %T", root["output"])
	}
	if root["message"] != "plain text" {
		t.Fatalf("message = %#v", root["message"])
	}
	if output["tool"] != "git_staged_paths" {
		t.Fatalf("tool = %#v", output["tool"])
	}
	if output["ok"] != true {
		t.Fatalf("ok = %#v", output["ok"])
	}
	data, ok := output["data"].(map[string]any)
	if !ok {
		t.Fatalf("data type = %T", output["data"])
	}
	paths, ok := data["paths"].([]any)
	if !ok {
		t.Fatalf("paths type = %T", data["paths"])
	}
	if len(paths) != 2 || paths[0] != "README.md" || paths[1] != "internal/agent/runner.go" {
		t.Fatalf("paths = %#v", paths)
	}
}

func TestRecorderWriteExpandsEmbeddedJSONStrings(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	recorder, err := New(root, "commit-msg")
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("request", map[string]any{
		"model":        "test-model",
		"instructions": "draft commit",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "hello"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("tool-call", map[string]any{
		"id":        "fc_1",
		"call_id":   "call_1",
		"name":      "git_staged_paths",
		"arguments": map[string]any{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("tool-output", map[string]any{
		"call_id": "call_1",
		"content": `{"ok":true,"data":{"paths":["README.md"]}}`,
	}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(recorder.Dir(), "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 3 {
		t.Fatalf("items = %#v", got.Items)
	}
	output, ok := got.Items[2]["output"].(map[string]any)
	if !ok {
		t.Fatalf("output type = %T", got.Items[2]["output"])
	}
	if output["ok"] != true {
		t.Fatalf("ok = %#v", output["ok"])
	}
}

func TestRecorderWritesEventLogAndCompactSession(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	recorder, err := New(root, "commit-msg")
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("session", map[string]any{
		"command": "commit-msg",
		"mode":    "normal",
	}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("request", map[string]any{
		"model":        "test-model",
		"instructions": "draft commit",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "hello"},
		},
		"tools": []map[string]any{
			{"name": "git_staged_paths"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("response", map[string]any{
		"id":          "resp_1",
		"finish_kind": "completed",
		"text":        "feat(trace): compact trace layout",
	}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("final", map[string]any{
		"text": "feat(trace): compact trace layout",
	}); err != nil {
		t.Fatal(err)
	}

	sessionData, err := os.ReadFile(filepath.Join(recorder.Dir(), "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		Version int              `json:"version"`
		Session map[string]any   `json:"session"`
		Static  map[string]any   `json:"static"`
		Items   []map[string]any `json:"items"`
		Steps   []map[string]any `json:"steps"`
		Final   map[string]any   `json:"final"`
	}
	if err := json.Unmarshal(sessionData, &session); err != nil {
		t.Fatal(err)
	}
	if session.Version != 2 {
		t.Fatalf("version = %d", session.Version)
	}
	if session.Session["command"] != "commit-msg" {
		t.Fatalf("session.command = %#v", session.Session["command"])
	}
	if session.Static["model"] != "test-model" {
		t.Fatalf("static.model = %#v", session.Static["model"])
	}
	if len(session.Items) != 1 {
		t.Fatalf("items = %#v", session.Items)
	}
	if len(session.Steps) != 1 {
		t.Fatalf("steps = %#v", session.Steps)
	}
	if session.Final["text"] != "feat(trace): compact trace layout" {
		t.Fatalf("final = %#v", session.Final)
	}

	eventsFile, err := os.Open(filepath.Join(recorder.Dir(), "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	defer eventsFile.Close()

	scanner := bufio.NewScanner(eventsFile)
	var count int
	for scanner.Scan() {
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("event count = %d", count)
	}
}

func TestRecorderStoresLargeSnapshotStringsAsArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	recorder, err := New(root, "commit-msg")
	if err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("diff --git a/file.go b/file.go\n+line\n", 700)
	if err := recorder.Write("session", map[string]any{
		"prepared_commit_context": map[string]any{
			"diff": large,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("request", map[string]any{
		"model":        "test-model",
		"instructions": "draft commit",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": large},
		},
	}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(recorder.Dir(), "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "diff --git") > 100 {
		t.Fatalf("large diff was copied into session snapshot:\n%s", data[:min(len(data), 2000)])
	}
	events, err := os.ReadFile(filepath.Join(recorder.Dir(), "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(events), "diff --git") > 100 {
		t.Fatalf("large diff was copied into event log:\n%s", events[:min(len(events), 2000)])
	}

	var session map[string]any
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	sessionRoot := session["session"].(map[string]any)
	prepared := sessionRoot["prepared_commit_context"].(map[string]any)
	diffRef := prepared["diff"].(map[string]any)
	artifactPath := filepath.Join(recorder.Dir(), diffRef["path"].(string))
	artifactData, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(artifactData) != large {
		t.Fatalf("artifact mismatch")
	}
}
