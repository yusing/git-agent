package cli

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yusing/git-agent/internal/trace"
)

func TestAgentEventServerRequiresToken(t *testing.T) {
	server, err := startAgentEventServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	eventURL, err := url.Parse(server.URL())
	if err != nil {
		t.Fatal(err)
	}
	if eventURL.Query().Get("token") == "" {
		t.Fatalf("event URL has no token: %s", eventURL)
	}
	eventURL.RawQuery = ""
	response, err := http.Get(eventURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}

func TestAgentEventServerReplaysEventsAndHonorsLastEventID(t *testing.T) {
	server, err := startAgentEventServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	for _, event := range []trace.Event{
		{Seq: 1, At: time.Unix(1, 0).UTC(), Kind: "session.started", Value: map[string]any{"command": "review"}},
		{Seq: 2, At: time.Unix(2, 0).UTC(), Kind: "final", Value: map[string]any{"text": map[string]any{"findings": []any{}}}},
	} {
		if err := server.Publish(event); err != nil {
			t.Fatal(err)
		}
	}
	server.Finish()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Last-Event-ID", "1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	stream := string(data)
	if !strings.Contains(stream, "event: final\n") || !strings.Contains(stream, "id: 2\n") {
		t.Fatalf("stream missing final event:\n%s", stream)
	}
	if strings.Contains(stream, "session.started") || strings.Contains(stream, "id: 1\n") {
		t.Fatalf("stream replayed acknowledged event:\n%s", stream)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("content type = %q", contentType)
	}
}

func TestEventRecorderPublishesEveryTraceEvent(t *testing.T) {
	server, err := startAgentEventServer()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	recorder, err := trace.NewEventStream("review", server.Publish)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("session", map[string]any{"mode": "staged"}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("reasoning_summary.delta", map[string]any{"item_id": "rs_1", "summary_index": 0, "delta": "Inspecting "}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("reasoning_summary.done", map[string]any{"item_id": "rs_1", "summary_index": 0, "text": "Inspecting changed files"}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Write("final", map[string]any{"text": map[string]any{"findings": []any{}}}); err != nil {
		t.Fatal(err)
	}
	server.Finish()

	response, err := http.Get(server.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	stream := string(data)
	for _, kind := range []string{"session.started", "session", "reasoning_summary.delta", "reasoning_summary.done", "final"} {
		if !strings.Contains(stream, "event: "+kind+"\n") {
			t.Fatalf("stream missing %s:\n%s", kind, stream)
		}
	}
	if !strings.Contains(stream, `"delta":"Inspecting "`) || !strings.Contains(stream, `"text":"Inspecting changed files"`) {
		t.Fatalf("stream missing reasoning summary payloads:\n%s", stream)
	}
}
