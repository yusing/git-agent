package cli

import (
	"context"
	"errors"
	"time"

	"github.com/yusing/git-agent/internal/trace"
)

const reviewTestCommand = "review-test"

var reviewTestEventInterval = 750 * time.Millisecond

func (a *App) runReviewTest(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: git-agent review-test")
	}
	if !isDetachedChild() {
		return startDetachedTask(reviewTestCommand, nil, a.stdout)
	}
	if detachedTaskID() == "" {
		return errors.New("review-test detached task ID is missing")
	}

	events, err := startDetachedAgentEventServer(a.stderr, reviewTestCommand, detachedTaskID())
	if err != nil {
		return err
	}
	defer events.Close()

	for _, event := range reviewTestEvents(time.Now().UTC()) {
		if err := waitReviewTestEvent(ctx, reviewTestEventInterval); err != nil {
			return err
		}
		if err := events.Publish(event); err != nil {
			return err
		}
	}
	events.Finish()
	return nil
}

func waitReviewTestEvent(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func reviewTestEvents(started time.Time) []trace.Event {
	const summary = "Inspecting deterministic review fixture"
	values := []struct {
		kind  string
		value map[string]any
	}{
		{"reasoning_summary.delta", map[string]any{"delta": summary}},
		{"reasoning_summary.done", map[string]any{"text": summary}},
		{"tool-call", map[string]any{"name": "read_file", "arguments": `{"path":"internal/cli/app.go","line_start":1,"line_end":12}`}},
		{"tool-output", map[string]any{"name": "read_file", "content": "package cli\n"}},
		{"final", map[string]any{"text": map[string]any{
			"summary":        "Deterministic review fixture completed.",
			"recommendation": "APPROVE",
			"findings":       []any{},
		}}},
	}
	events := make([]trace.Event, len(values))
	for index, value := range values {
		events[index] = trace.Event{
			Seq:   index + 1,
			At:    started.Add(time.Duration(index+1) * reviewTestEventInterval),
			Kind:  value.kind,
			Value: value.value,
		}
	}
	return events
}
