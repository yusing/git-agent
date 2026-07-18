package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	backgroundtask "github.com/yusing/git-agent/internal/background"
	"github.com/yusing/git-agent/internal/projectidentity"
	"github.com/yusing/git-agent/internal/trace"
)

const cliWaitTaskID = "CDEFGHIJKLMNOPQRSTUVWXYZAB"

func TestReviewAndSimplifyWaitPrintOnlyRepeatableFinalJSON(t *testing.T) {
	for _, command := range []string{"review", "simplify"} {
		t.Run(command, func(t *testing.T) {
			repoDir := initRepo(t)
			t.Chdir(repoDir)
			t.Setenv("HOME", t.TempDir())
			store := backgroundStoreForCurrentProject(t)
			completeWaitTask(t, store, command, trace.Event{
				Kind:  "final",
				Value: map[string]any{"text": map[string]any{"summary": "complete", "items": []any{}}},
			})

			for range 2 {
				var stdout bytes.Buffer
				var stderr bytes.Buffer
				app := &App{stdout: &stdout, stderr: &stderr}
				if err := app.Run(t.Context(), []string{command, "--wait", cliWaitTaskID}); err != nil {
					t.Fatal(err)
				}
				if stderr.Len() != 0 {
					t.Fatalf("stderr = %q", stderr.String())
				}
				var report map[string]any
				if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
					t.Fatalf("stdout is not strict JSON: %v\n%s", err, stdout.String())
				}
				if report["summary"] != "complete" || strings.Count(stdout.String(), "\n") != 1 {
					t.Fatalf("stdout = %q", stdout.String())
				}
			}
		})
	}
}

func TestWaitFindsTaskFromAnotherRepository(t *testing.T) {
	sourceRepo := initRepo(t)
	home := os.Getenv("HOME")
	t.Chdir(sourceRepo)
	store := backgroundStoreForCurrentProject(t)
	completeWaitTask(t, store, "review", trace.Event{
		Kind:  "final",
		Value: map[string]any{"text": map[string]any{"summary": "complete", "items": []any{}}},
	})

	otherDir := t.TempDir()
	legacyPath := filepath.Join(otherDir, ".git-agent")
	if err := os.WriteFile(legacyPath, []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Chdir(otherDir)
	var stdout bytes.Buffer
	app := &App{stdout: &stdout, stderr: &bytes.Buffer{}}
	if err := app.Run(t.Context(), []string{"review", "--wait", cliWaitTaskID}); err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report["summary"] != "complete" {
		t.Fatalf("report = %#v", report)
	}
	if legacy, err := os.ReadFile(legacyPath); err != nil || string(legacy) != "untouched\n" {
		t.Fatalf("legacy metadata = %q, %v", legacy, err)
	}
}

func TestWaitFailuresKeepStdoutEmpty(t *testing.T) {
	repoDir := initRepo(t)
	t.Chdir(repoDir)
	t.Setenv("HOME", t.TempDir())
	store := backgroundStoreForCurrentProject(t)
	completeWaitTask(t, store, "review", trace.Event{Kind: "error", Value: map[string]any{"message": "stored failure"}})

	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "stored error", args: []string{"review", "--wait", cliWaitTaskID}, want: "stored failure"},
		{name: "wrong kind", args: []string{"simplify", "--wait", cliWaitTaskID}, want: "belongs to review"},
		{name: "invalid ID", args: []string{"review", "--wait", "../escape"}, want: "invalid background task ID"},
		{name: "unknown ID", args: []string{"review", "--wait", "DEFGHIJKLMNOPQRSTUVWXYZABC"}, want: "unknown background task"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			app := &App{stdout: &stdout, stderr: &bytes.Buffer{}}
			err := app.Run(t.Context(), test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func completeWaitTask(t *testing.T, store *backgroundtask.Store, command string, terminal trace.Event) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.Create(cliWaitTaskID, command, os.Getpid(), now); err != nil {
		t.Fatal(err)
	}
	terminal.Seq = 4
	terminal.At = now.Add(time.Second)
	if err := store.Complete(cliWaitTaskID, terminal, nil, terminal.At); err != nil {
		t.Fatal(err)
	}
}

func TestWaitRejectsEveryOtherReviewInput(t *testing.T) {
	for _, suffix := range [][]string{
		{"--codebase"},
		{"--uncommitted"},
		{"--staged"},
		{"--timeout", "1s"},
		{"--model", "test-model"},
		{"--max-steps", "1"},
		{"--depth", "fast"},
		{"--fast"},
		{"--low"},
		{"--medium"},
		{"--high"},
		{"--xhigh"},
		{"--base-url", "https://example.test"},
		{"--guidance-family", "none"},
		{"--append-prompt", "hint"},
		{"--debug"},
		{"--pprof", "127.0.0.1:0"},
		{"prompt"},
	} {
		args := append([]string{"review", "--wait", cliWaitTaskID}, suffix...)
		var stdout bytes.Buffer
		app := &App{stdout: &stdout, stderr: &bytes.Buffer{}}
		err := app.Run(t.Context(), args)
		if err == nil || !strings.Contains(err.Error(), "--wait cannot be combined") {
			t.Fatalf("args %q error = %v", args, err)
		}
		if stdout.Len() != 0 {
			t.Fatalf("args %q stdout = %q", args, stdout.String())
		}
	}
}

func backgroundStoreForCurrentProject(t *testing.T) *backgroundtask.Store {
	t.Helper()
	identity, err := projectidentity.Resolve(".")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := identity.Dir()
	if err != nil {
		t.Fatal(err)
	}
	store, err := backgroundtask.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
