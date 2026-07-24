package skillcmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type recordingRunner struct {
	commands []Command
	output   Output
}

func (r *recordingRunner) Run(_ context.Context, command Command) (Output, error) {
	r.commands = append(r.commands, command)
	return r.output, nil
}

func TestManagerDelegatesListAndRead(t *testing.T) {
	runner := &recordingRunner{output: Output{Stdout: "delegated", Stderr: "progress", Truncated: true}}
	manager := DiscoverWithOptions(Options{
		Dir:     t.TempDir(),
		Timeout: time.Second,
		Lookup:  func(string) (string, error) { return "/tools/skills-mgr", nil },
		Runner:  runner,
	})

	list, err := manager.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	read, err := manager.Read(t.Context(), "go/references/style.md", "3:8")
	if err != nil {
		t.Fatal(err)
	}
	if list.Stdout != "delegated" || read.Stdout != "delegated" ||
		list.Stderr != "progress" || read.Stderr != "progress" ||
		!list.Truncated || !read.Truncated {
		t.Fatalf("outputs = %#v %#v", list, read)
	}
	want := [][]string{
		{"list"},
		{"get", "go/references/style.md", "3:8"},
	}
	if len(runner.commands) != len(want) {
		t.Fatalf("commands = %#v", runner.commands)
	}
	for i, args := range want {
		if !slices.Equal(runner.commands[i].Args, args) {
			t.Fatalf("command %d args = %#v, want %#v", i, runner.commands[i].Args, args)
		}
	}
	if runner.commands[0].Timeout != time.Second || runner.commands[1].Timeout != time.Second {
		t.Fatalf("timeouts = %s, %s", runner.commands[0].Timeout, runner.commands[1].Timeout)
	}
	if runner.commands[0].Dir != filepath.Clean(manager.dir) {
		t.Fatalf("dir = %q, want %q", runner.commands[0].Dir, manager.dir)
	}
}

func TestManagerReadSupportsCompleteFile(t *testing.T) {
	runner := &recordingRunner{}
	manager := DiscoverWithOptions(Options{
		Lookup: func(string) (string, error) { return "/tools/skills-mgr", nil },
		Runner: runner,
	})
	if _, err := manager.Read(t.Context(), "go", ""); err != nil {
		t.Fatal(err)
	}
	if got := runner.commands[0].Args; !slices.Equal(got, []string{"get", "go"}) {
		t.Fatalf("args = %#v", got)
	}
}

func TestManagerIsUnavailableWhenLookupFails(t *testing.T) {
	manager := DiscoverWithOptions(Options{
		Lookup: func(string) (string, error) { return "", errors.New("missing") },
	})
	if manager.Available() {
		t.Fatal("manager unexpectedly available")
	}
}

func TestManagerInvokesResolvedSkillsManagerDirectly(t *testing.T) {
	dir := t.TempDir()
	bin := t.TempDir()
	path := filepath.Join(bin, "skills-mgr")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s|%s' \"$PWD\" \"$*\"\nprintf 'progress' >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	output, err := Discover(dir).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if output.Stdout != filepath.Clean(dir)+"|list" || output.Stderr != "progress" {
		t.Fatalf("output = %#v", output)
	}
}
