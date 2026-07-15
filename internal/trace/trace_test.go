package trace

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestNewStreamWritesHumanConsoleTrace(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	recorder, err := NewStream("commit", &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("final", map[string]any{"text": "Add parser"}); err != nil {
		t.Fatal(err)
	}

	lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("event lines = %d:\n%s", len(lines), out.Bytes())
	}
	for idx, line := range lines {
		text := string(line)
		for _, forbidden := range []string{"seq=", "time=", "level=", "msg=", "at=", "kind=", "value."} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("line %d contains machine trace field %q: %s", idx+1, forbidden, text)
			}
		}
		if !strings.Contains(text, " INF ") {
			t.Fatalf("line %d missing human level: %s", idx+1, text)
		}
	}
	if got := string(lines[1]); !strings.Contains(got, " INF final ") || !strings.Contains(got, "text=\"Add parser\"") {
		t.Fatalf("final event line = %s", got)
	}
}

func TestNewStreamCompactsMultilineStringsForConsole(t *testing.T) {
	t.Parallel()

	multiline := strings.Repeat("diff --git a/file.go b/file.go\n+line\n", 20)
	var out bytes.Buffer
	recorder, err := NewStream("commit", &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("final", map[string]any{"text": multiline}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `\n+line\n`) {
		t.Fatalf("stream line contains escaped newlines:\n%s", out.String())
	}
	for _, want := range []string{" INF final\n", "\n  text:\n", "diff --git a/file.go b/file.go\n", "\n    +line\n", "… truncated"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stream line missing %q:\n%s", want, out.String())
		}
	}
	if strings.Count(out.String(), "diff --git") > 10 {
		t.Fatalf("multiline stream value was not compacted:\n%s", out.String())
	}
}

func TestNewStreamCompactsLargeStringsInline(t *testing.T) {
	t.Parallel()

	large := strings.Repeat("x", largeStringPreviewThreshold)
	var out bytes.Buffer
	recorder, err := NewStream("commit", &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("final", map[string]any{"text": large}); err != nil {
		t.Fatal(err)
	}
	line := out.String()
	for _, want := range []string{" INF final\n", "\n  text:\n", "… truncated"} {
		if !strings.Contains(line, want) {
			t.Fatalf("stream line missing %q:\n%s", want, line)
		}
	}
	if strings.Contains(line, strings.Repeat("x", consoleStringPreviewBytes+1)) {
		t.Fatalf("large stream value was not compacted:\n%s", line)
	}
	if strings.Contains(line, "text.path=") {
		t.Fatalf("stream preview should not reference artifact path: %s", line)
	}
}

func TestWriteExactPreservesMachineConsumedStrings(t *testing.T) {
	t.Parallel()

	var events []Event
	recorder, err := NewEventStream("review", func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("x", largeStringPreviewThreshold+1)
	if err := recorder.WriteExact("reasoning_summary.done", map[string]any{
		"delta": "{}",
		"text":  large,
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %#v", events)
	}
	if events[1].Value["delta"] != "{}" || events[1].Value["text"] != large {
		t.Fatalf("exact event changed strings: %#v", events[1])
	}
}

func TestNewStreamRequestOmitsInstructions(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	recorder, err := NewStream("commit", &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("request", map[string]any{
		"model":        "test-model",
		"instructions": "draft commit message",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": "hello"},
		},
		"tools": []map[string]any{
			{"name": "git_staged_paths"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	for _, want := range []string{" INF request ", "model=test-model", "input_items=1", "tools=1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream request missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"instructions", "draft commit message"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("stream request kept %q:\n%s", forbidden, got)
		}
	}
}

func TestConsoleTraceColorsFieldKeys(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	at := time.Date(2026, 6, 5, 15, 4, 5, 0, time.Local)
	if err := writeConsoleEvent(&out, at, "final", map[string]any{"text": "Add parser"}, true); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, consoleColorKey+"text"+consoleColorReset+"=\"Add parser\"") {
		t.Fatalf("field key was not colored:\n%q", got)
	}
}

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
