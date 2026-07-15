package cli

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
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
