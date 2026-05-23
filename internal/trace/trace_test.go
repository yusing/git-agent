package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	if err := recorder.Write("tool-output", map[string]any{
		"output": `{"ok":true,"data":{"paths":["README.md"]}}`,
	}); err != nil {
		t.Fatal(err)
	}

	files, err := filepath.Glob(filepath.Join(recorder.Dir(), "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %#v", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	output, ok := got["output"].(map[string]any)
	if !ok {
		t.Fatalf("output type = %T", got["output"])
	}
	if output["ok"] != true {
		t.Fatalf("ok = %#v", output["ok"])
	}
}
