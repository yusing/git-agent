package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yusing/git-agent/internal/checks"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
)

func TestReviewTestStreamsDeterministicDetachedEvents(t *testing.T) {
	const taskID = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	t.Setenv(detachedChildEnv, "1")
	t.Setenv(detachedTaskIDEnv, taskID)
	originalInterval := reviewTestEventInterval
	reviewTestEventInterval = 10 * time.Millisecond
	t.Cleanup(func() { reviewTestEventInterval = originalInterval })

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	app := &App{stdout: io.Discard, stderr: writer}
	done := make(chan error, 1)
	go func() { done <- app.Run(t.Context(), []string{reviewTestCommand}) }()

	launch, err := readDetachedLaunch(reader)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Command != reviewTestCommand || launch.ID != taskID || launch.PID != os.Getpid() {
		t.Fatalf("launch = %#v", launch)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, launch.Endpoint.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := localHTTPTestClient(t, launch.Endpoint).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr, <-done); err != nil {
		t.Fatal(err)
	}
	stream := string(data)
	for _, want := range []string{
		`event: reasoning_summary.delta`,
		`event: reasoning_summary.done`,
		`event: tool-call`,
		`event: tool-output`,
		`event: final`,
		`Deterministic review fixture completed.`,
	} {
		if !strings.Contains(stream, want) {
			t.Fatalf("event stream missing %q:\n%s", want, stream)
		}
	}
}

func TestReviewTestRejectsArguments(t *testing.T) {
	err := (&App{stdout: io.Discard, stderr: io.Discard}).Run(t.Context(), []string{reviewTestCommand, "extra"})
	if err == nil || err.Error() != "usage: git-agent review-test" {
		t.Fatalf("error = %v", err)
	}
}

func TestDryRunEventsEndWithValidReportForEachKind(t *testing.T) {
	for _, kind := range []reviewtask.Kind{reviewtask.KindReview, reviewtask.KindSimplify} {
		var results []checks.Result
		if kind == reviewtask.KindReview {
			result, err := checks.NewSkipped("fixture-checker", "fixture has no eligible input")
			if err != nil {
				t.Fatal(err)
			}
			results = []checks.Result{result}
		}
		events, err := dryRunEvents(kind, nil, results)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 15 {
			t.Fatalf("%s dry-run event count = %d", kind, len(events))
		}
		seen := map[any]bool{}
		for _, event := range events {
			if event.Kind == "tool-call" {
				seen[event.Value["name"]] = true
			}
		}
		if len(seen) != 5 {
			t.Fatalf("%s dry-run unique tools = %d", kind, len(seen))
		}
		data, err := json.Marshal(events[len(events)-1].Value["text"])
		if err != nil {
			t.Fatal(err)
		}
		if kind == reviewtask.KindReview {
			var report reviewtask.FinalReviewReport
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatal(err)
			}
			if err := reviewtask.ValidateFinalReviewReport(report); err != nil {
				t.Fatalf("%s dry-run report invalid: %v", kind, err)
			}
		} else if problems := reviewtask.Validate(kind, string(data)); len(problems) != 0 {
			t.Fatalf("%s dry-run report invalid: %v", kind, problems)
		}
	}
}

func TestDryRunEventDelayIsProviderLike(t *testing.T) {
	for range 100 {
		if delay := dryRunEventDelay(); delay < 500*time.Millisecond || delay > time.Second {
			t.Fatalf("dry-run delay = %s", delay)
		}
	}
}
