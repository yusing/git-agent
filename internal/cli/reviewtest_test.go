package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yusing/git-agent/internal/checks"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
)

func TestDryRunEventsEndWithValidReportForEachKind(t *testing.T) {
	for _, kind := range []reviewtask.Kind{reviewtask.KindReview, reviewtask.KindSimplify} {
		var results []checks.Result
		if kind == reviewtask.KindReview {
			result := checks.Result{
				Name: "fixture-checker", Status: checks.StatusSkipped,
				Diagnostics: []checks.Diagnostic{}, Reason: "fixture has no eligible input",
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
