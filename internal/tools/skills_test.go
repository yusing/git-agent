package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/yusing/git-agent/internal/skillcmd"
)

type skillRunner struct {
	commands []skillcmd.Command
}

func (r *skillRunner) Run(_ context.Context, command skillcmd.Command) (skillcmd.Output, error) {
	r.commands = append(r.commands, command)
	return skillcmd.Output{Stdout: "delegated output", Stderr: "delegated progress"}, nil
}

func TestSkillToolsDelegateRead(t *testing.T) {
	runner := &skillRunner{}
	manager := skillcmd.DiscoverWithOptions(skillcmd.Options{
		Dir:    t.TempDir(),
		Lookup: func(string) (string, error) { return "/tools/skills-mgr", nil },
		Runner: runner,
	})
	registry := &Registry{tools: map[string]Tool{}}
	register(registry, skillTools(manager))

	definitions := registry.Definitions(SkillToolNames())
	if len(definitions) != 1 {
		t.Fatalf("definitions = %#v", definitions)
	}
	for _, definition := range definitions {
		if !definition.Strict {
			t.Fatalf("%s is not strict", definition.Name)
		}
	}

	invocation := Invocation{Name: SkillsReadToolName, Arguments: `{"locator":"go/references/style.md","range":"2:5"}`}
	result, err := registry.Execute(t.Context(), invocation)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		OK   bool   `json:"ok"`
		Tool string `json:"tool"`
		Data struct {
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Tool != invocation.Name ||
		envelope.Data.Stdout != "delegated output" || envelope.Data.Stderr != "delegated progress" {
		t.Fatalf("envelope = %#v", envelope)
	}

	want := []string{"get", "go/references/style.md", "2:5"}
	if len(runner.commands) != 1 {
		t.Fatalf("commands = %#v", runner.commands)
	}
	if !slices.Equal(runner.commands[0].Args, want) {
		t.Fatalf("command = %#v, want %#v", runner.commands[0].Args, want)
	}
	if runner.commands[0].Path != filepath.Clean("/tools/skills-mgr") {
		t.Fatalf("path = %q", runner.commands[0].Path)
	}
}

func TestSkillToolsAreOmittedWhenSkillsManagerIsUnavailable(t *testing.T) {
	manager := skillcmd.DiscoverWithOptions(skillcmd.Options{
		Lookup: func(string) (string, error) { return "", filepath.ErrBadPattern },
	})
	if tools := skillTools(manager); len(tools) != 0 {
		t.Fatalf("tools = %#v", tools)
	}
}
