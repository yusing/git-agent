package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

type fishCompletionCase struct {
	name string
	line string
	want []string
}

type fishCompletionOption struct {
	name            string
	takesValue      bool
	value           string
	valueCandidates []string
	disabled        bool
}

type fishCompletionCommand struct {
	name    string
	options []fishCompletionOption
}

func TestFishCompletionSyntax(t *testing.T) {
	fish := requireFish(t)
	cmd := exec.CommandContext(t.Context(), fish, "--no-config", "--no-execute", fishCompletionPath(t))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fish completion syntax: %v\n%s", err, output)
	}
}

func TestFishCompletionReloadReplacesExistingCandidates(t *testing.T) {
	fish := requireFish(t)
	workDir := t.TempDir()
	got := runFishCompletionScript(
		t,
		fish,
		fishCompletionReloadScript,
		fishCompletionPath(t),
		workDir,
		completionTestEnvironment(t.TempDir(), t.TempDir()),
		"git-agent index migrate ",
	)
	want := []string{"--dry-run", "--to"}
	if !slices.Equal(got, want) {
		t.Fatalf("reloaded candidates = %q, want %q", got, want)
	}
}

func TestFishCompletionCandidates(t *testing.T) {
	fish := requireFish(t)
	workDir := t.TempDir()
	for _, name := range []string{"artifact.json", "notes.md"} {
		if err := os.WriteFile(filepath.Join(workDir, name), nil, 0o644); err != nil {
			t.Fatalf("write completion fixture %s: %v", name, err)
		}
	}
	if err := os.Mkdir(filepath.Join(workDir, "scope-dir"), 0o755); err != nil {
		t.Fatalf("create completion fixture directory: %v", err)
	}

	fakeBin := t.TempDir()
	writeCompletionExecutable(t, fakeBin, "git", `#!/bin/sh
echo "unexpected git completion diagnostic" >&2
case "$1" in
for-each-ref)
	printf '%s\n' main origin/main v1.2.3
	;;
rev-parse)
	printf '%s\n' deadbee
	;;
*)
	exit 64
	;;
esac
`)
	writeCompletionExecutable(t, fakeBin, "git-agent", `#!/bin/sh
echo "unexpected git-agent completion diagnostic" >&2
if [ "$1" = search ] && [ "$2" = --ls-remotes ] && [ "$3" = --format ] && [ "$4" = completion ]; then
	printf '%s\n' https://example.test/acme/repo.git ssh://git@example.test/acme/other.git
	exit 0
fi
exit 64
`)

	refs := []string{"deadbee", "main", "origin/main", "v1.2.3"}
	remotes := []string{"https://example.test/acme/repo.git", "ssh://git@example.test/acme/other.git"}
	paths := []string{"artifact.json", "notes.md", "scope-dir/"}
	cases := fishCompletionCases(refs, remotes, paths)
	env := completionTestEnvironment(fakeBin, t.TempDir())
	completionPath := fishCompletionPath(t)
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := runFishCompletion(t, fish, completionPath, workDir, env, test.line)
			want := sortedCompletionCandidates(test.want)
			if !slices.Equal(got, want) {
				t.Fatalf("complete -C %q = %q, want %q", test.line, got, want)
			}
		})
	}
}

const fishCompletionScript = `
set -g fish_complete_path
complete -e -c git-agent
source "$argv[1]"; or exit 1
for item in (complete -C "$argv[2]")
    set -l fields (string split -m 1 \t -- "$item")
    string unescape -- "$fields[1]"
end
`

const fishCompletionReloadScript = `
set -g fish_complete_path
complete -e -c git-agent
complete -c git-agent -n 'test (count (commandline -opc)) -gt 1; and test (commandline -opc)[2] = index' -a sync
complete -c git-agent -n 'test (count (commandline -opc)) -gt 1; and test (commandline -opc)[2] = index' -a migrate
source "$argv[1]"; or exit 1
for item in (complete -C "$argv[2]")
    set -l fields (string split -m 1 \t -- "$item")
    string unescape -- "$fields[1]"
end
`

func runFishCompletion(t *testing.T, fish, completionPath, workDir string, env []string, line string) []string {
	t.Helper()
	return runFishCompletionScript(t, fish, fishCompletionScript, completionPath, workDir, env, line)
}

func runFishCompletionScript(t *testing.T, fish, script, completionPath, workDir string, env []string, line string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, fish, "--no-config", "-c", script, completionPath, line)
	cmd.Dir = workDir
	cmd.Env = env
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run fish completion: %v\nstderr:\n%s", err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("fish completion wrote diagnostics:\n%s", stderr.String())
	}
	return completionSegmentCandidates(stdout.String())
}

func fishCompletionCases(refs, remotes, paths []string) []fishCompletionCase {
	var cases []fishCompletionCase
	add := func(name, line string, want []string) {
		cases = append(cases, fishCompletionCase{name: name, line: line, want: slices.Clone(want)})
	}

	root := []string{"commit", "commit-msg", "config", "help", "index", "pr-message", "release-note", "review", "search", "simplify"}
	add("root commands", "git-agent ", root)
	for _, candidate := range root {
		add("root partial "+candidate, "git-agent "+candidate, candidatesWithPrefix(root, candidate))
	}
	add("root option prefix", "git-agent --", nil)
	add("unknown root command", "git-agent future ", nil)
	add("help has no children", "git-agent help ", nil)

	add("config key", "git-agent config ", []string{"index.remote"})
	add("config partial key", "git-agent config i", []string{"index.remote"})
	add("config option", "git-agent config --", []string{"--unset"})
	add("config unset key", "git-agent config --unset ", []string{"index.remote"})
	add("config key accepts free URL", "git-agent config index.remote ", nil)
	add("config complete value", "git-agent config index.remote ssh://example.test/repo.git ", nil)
	add("config option cannot follow key", "git-agent config index.remote --", nil)
	add("config unset complete", "git-agent config --unset index.remote ", nil)
	add("config duplicate unset", "git-agent config --unset --unset --", nil)

	add("index subcommands", "git-agent index ", []string{"migrate", "sync"})
	add("index partial migrate", "git-agent index m", []string{"migrate"})
	add("index sync complete", "git-agent index sync ", nil)
	add("index sync has no options", "git-agent index sync --", nil)
	add("index unknown subcommand", "git-agent index future --", nil)
	add("index migrate next options", "git-agent index migrate ", []string{"--dry-run", "--to"})
	add("index migrate options", "git-agent index migrate --", []string{"--dry-run", "--to"})
	add("index migrate partial option", "git-agent index migrate --t", []string{"--to"})
	add("index migrate to value", "git-agent index migrate --to ", []string{"v2"})
	add("index migrate partial value", "git-agent index migrate --to v", []string{"v2"})
	add("index migrate next option after target", "git-agent index migrate --to v2 ", []string{"--dry-run"})
	add("index migrate after target", "git-agent index migrate --to v2 --", []string{"--dry-run"})
	add("index migrate next option after dry run", "git-agent index migrate --dry-run ", []string{"--to"})
	add("index migrate after dry run", "git-agent index migrate --dry-run --", []string{"--to"})
	add("index migrate reverse order value", "git-agent index migrate --dry-run --to ", []string{"v2"})
	add("index migrate complete", "git-agent index migrate --dry-run --to v2 --", nil)
	add("index migrate complete reverse order", "git-agent index migrate --to v2 --dry-run --", nil)
	add("index migrate duplicate target", "git-agent index migrate --to v2 --to v2 --", nil)
	add("index migrate duplicate dry run", "git-agent index migrate --dry-run --dry-run --", nil)
	add("index migrate malformed target", "git-agent index migrate --to v3 --", nil)
	add("index migrate malformed positional", "git-agent index migrate garbage --", nil)
	add("index migrate unrelated collision", "git-agent config migrate --t", nil)

	positionals := append(slices.Clone(refs), "major", "minor", "patch")
	add("release note first positional", "git-agent release-note ", positionals)
	add("release note partial positional", "git-agent release-note m", candidatesWithPrefix(positionals, "m"))
	add("release note second ref", "git-agent release-note main ", refs)
	add("release note bump complete", "git-agent release-note patch ", nil)
	add("release note range complete", "git-agent release-note main v1.2.3 ", nil)
	add("release note options before positional", "git-agent release-note --debug ", positionals)
	add("release note path option before positional", "git-agent release-note --out notes.md ", positionals)
	add("release note option terminator", "git-agent release-note -- ", positionals)
	add("release note option after positional", "git-agent release-note main --", nil)
	add("release note unknown option", "git-agent release-note --future value ", nil)
	add("release note conflicting efforts", "git-agent release-note --low --high ", nil)

	commands := fishCompletionCommands(refs, remotes, paths)
	for _, command := range commands {
		base := completionOptionNames(command.options)
		add(command.name+" option surface", "git-agent "+command.name+" --", base)
		if command.name != "release-note" {
			add(command.name+" blank positional", "git-agent "+command.name+" ", nil)
		}

		for _, option := range command.options {
			prefix := "--" + option.name
			add(command.name+" partial "+option.name, "git-agent "+command.name+" "+prefix, candidatesWithPrefix(base, prefix))
			if option.takesValue {
				add(command.name+" "+option.name+" values", "git-agent "+command.name+" "+prefix+" ", option.valueCandidates)
			}

			used := []fishCompletionOption{option}
			add(command.name+" after "+option.name, completionLine(command.name, used), expectedOptionCandidates(command, used))
			add(command.name+" duplicate "+option.name, completionLine(command.name, []fishCompletionOption{option, option}), nil)
			if option.takesValue {
				add(command.name+" single dash "+option.name, completionLineWithOption(command.name, "-"+option.name+" "+option.value), expectedOptionCandidates(command, used))
				add(command.name+" equals "+option.name, completionLineWithOption(command.name, "--"+option.name+"="+option.value), expectedOptionCandidates(command, used))
			} else {
				add(command.name+" single dash "+option.name, completionLineWithOption(command.name, "-"+option.name), expectedOptionCandidates(command, used))
				add(command.name+" true "+option.name, completionLineWithOption(command.name, "--"+option.name+"=true"), expectedOptionCandidates(command, used))
				disabled := option
				disabled.disabled = true
				add(command.name+" false "+option.name, completionLineWithOption(command.name, "--"+option.name+"=false"), expectedOptionCandidates(command, []fishCompletionOption{disabled}))
			}
		}

		for first := range command.options {
			for second := first + 1; second < len(command.options); second++ {
				ordered := []fishCompletionOption{command.options[first], command.options[second]}
				name := command.name + " pair " + command.options[first].name + "+" + command.options[second].name
				add(name, completionLine(command.name, ordered), expectedOptionCandidates(command, ordered))

				slices.Reverse(ordered)
				name = command.name + " pair " + command.options[second].name + "+" + command.options[first].name
				add(name, completionLine(command.name, ordered), expectedOptionCandidates(command, ordered))
			}
		}
	}

	add("review prompt stops flags", "git-agent review inspect-this --", nil)
	add("simplify prompt stops flags", "git-agent simplify simplify-this --", nil)
	add("commit rejects positional flag continuation", "git-agent commit unexpected --", nil)
	add("pr message rejects positional flag continuation", "git-agent pr-message unexpected --", nil)
	add("invalid guidance value", "git-agent commit --guidance-family future --", nil)
	add("unknown command option", "git-agent commit --future value --", nil)

	searchCommand := completionCommandNamed(commands, "search")
	customModel := completionOptionNamed(searchCommand.options, "embedding-model")
	customModel.value = "provider-model"
	add("custom embedding model", "git-agent search --embedding-model provider-model --", expectedOptionCandidates(searchCommand, []fishCompletionOption{customModel}))
	customDimensions := completionOptionNamed(searchCommand.options, "embedding-dimensions")
	customDimensions.value = "2048"
	add("custom embedding dimensions", "git-agent search --embedding-dimensions 2048 --", expectedOptionCandidates(searchCommand, []fishCompletionOption{customDimensions}))
	add("zero embedding dimensions", "git-agent search --embedding-dimensions 0 --", nil)
	add("negative embedding dimensions", "git-agent search --embedding-dimensions -1 --", nil)
	add("non-integer embedding dimensions", "git-agent search --embedding-dimensions many --", nil)

	add("search query stops flags", "git-agent search query --", nil)
	add("search default formats", "git-agent search --format ", []string{"brief", "json"})
	add("search list formats", "git-agent search --ls --format ", []string{"json", "text"})
	add("search remote-list formats", "git-agent search --ls-remotes --format ", []string{"completion", "json", "text"})
	add("search file-list formats", "git-agent search --ls-files --format ", []string{"json", "tree"})
	add("search list options", "git-agent search --ls --", []string{"--format", "--remote"})
	add("search remote-list options", "git-agent search --ls-remotes --", []string{"--format"})
	add("search file-list options", "git-agent search --ls-files --", []string{"--format", "--no-tests", "--remote", "--rev", "--scope"})
	add("search text format can select list mode", "git-agent search --format text --", []string{"--ls", "--ls-remotes"})
	add("search tree format can select file-list mode", "git-agent search --format tree --", []string{"--ls-files"})
	add("search completion format can select remote-list mode", "git-agent search --format completion --", []string{"--ls-remotes"})
	add("search incompatible list format", "git-agent search --format brief --ls --", nil)
	add("search incompatible file-list format", "git-agent search --format text --ls-files --", nil)
	add("search invalid format", "git-agent search --format future --", nil)
	add("search list rejects normal flag", "git-agent search --ls --code --", nil)
	add("search remote-list rejects remote", "git-agent search --ls-remotes --remote ", nil)
	add("search file-list valid combination", "git-agent search --ls-files --remote https://example.test/acme/repo.git --rev main --format tree --", []string{"--no-tests", "--scope"})
	add("search list modes conflict", "git-agent search --ls --ls-files --", nil)

	add("unrelated command collision", "git-agent help search --", nil)
	add("unknown nested collision", "git-agent future review --", nil)

	return cases
}

func fishCompletionCommands(refs, remotes, paths []string) []fishCompletionCommand {
	shared := []fishCompletionOption{
		{name: "model", takesValue: true, value: "gpt-test"},
		{name: "fast"},
		{name: "low"},
		{name: "medium"},
		{name: "high"},
		{name: "xhigh"},
		{name: "base-url", takesValue: true, value: "https://api.example.test"},
		{name: "timeout", takesValue: true, value: "30s"},
		{name: "max-steps", takesValue: true, value: "20"},
		{name: "guidance-family", takesValue: true, value: "auto", valueCandidates: []string{"agents", "auto", "claude", "codex", "none"}},
		{name: "append-prompt", takesValue: true, value: "hint"},
		{name: "debug"},
		{name: "pprof", takesValue: true, value: "127.0.0.1:6060"},
	}
	withShared := func(specific ...fishCompletionOption) []fishCompletionOption {
		return append(slices.Clone(specific), shared...)
	}

	review := withShared(
		fishCompletionOption{name: "codebase"},
		fishCompletionOption{name: "uncommitted"},
		fishCompletionOption{name: "staged"},
		fishCompletionOption{name: "wait", takesValue: true, value: "task-123"},
		fishCompletionOption{name: "max-web-searches", takesValue: true, value: "4"},
		fishCompletionOption{name: "dry-run"},
		fishCompletionOption{name: "orchestration-artifact", takesValue: true, value: "artifact.json", valueCandidates: slices.Clone(paths)},
	)
	search := []fishCompletionOption{
		{name: "rev", takesValue: true, value: "main", valueCandidates: slices.Clone(refs)},
		{name: "remote", takesValue: true, value: "https://example.test/acme/repo.git", valueCandidates: slices.Clone(remotes)},
		{name: "scope", takesValue: true, value: "scope-dir", valueCandidates: slices.Clone(paths)},
		{name: "min-score", takesValue: true, value: "0.4"},
		{name: "limit", takesValue: true, value: "10"},
		{name: "format", takesValue: true, value: "json", valueCandidates: []string{"brief", "json"}},
		{name: "index"},
		{name: "reindex"},
		{name: "code"},
		{name: "no-tests"},
		{name: "agent"},
		{name: "ls"},
		{name: "ls-remotes"},
		{name: "ls-files"},
		{name: "embedding-model", takesValue: true, value: "text-embedding-3-small", valueCandidates: []string{"text-embedding-3-large", "text-embedding-3-small"}},
		{name: "embedding-dimensions", takesValue: true, value: "1536", valueCandidates: []string{"1024", "1536", "3072", "512", "768"}},
		{name: "base-url", takesValue: true, value: "https://api.example.test"},
		{name: "timeout", takesValue: true, value: "30s"},
		{name: "debug"},
		{name: "pprof", takesValue: true, value: "127.0.0.1:6060"},
	}

	return []fishCompletionCommand{
		{name: "commit", options: withShared(fishCompletionOption{name: "amend"})},
		{name: "commit-msg", options: withShared(fishCompletionOption{name: "amend"})},
		{name: "pr-message", options: slices.Clone(shared)},
		{name: "release-note", options: withShared(fishCompletionOption{name: "out", takesValue: true, value: "notes.md", valueCandidates: slices.Clone(paths)})},
		{name: "review", options: slices.Clone(review)},
		{name: "simplify", options: slices.Clone(review)},
		{name: "search", options: search},
	}
}

func expectedOptionCandidates(command fishCompletionCommand, used []fishCompletionOption) []string {
	seen := make(map[string]fishCompletionOption, len(used))
	for _, option := range used {
		if _, duplicate := seen[option.name]; duplicate {
			return nil
		}
		seen[option.name] = option
	}

	if countEnabledOptions(seen, "low", "medium", "high", "xhigh") > 1 {
		return nil
	}
	if command.name == "review" || command.name == "simplify" {
		if countEnabledOptions(seen, "codebase", "uncommitted", "staged") > 1 {
			return nil
		}
		if _, wait := seen["wait"]; wait && len(seen) != 1 {
			return nil
		}
	}
	if command.name == "search" && !validSearchCompletionState(seen) {
		return nil
	}

	var candidates []string
	for _, option := range command.options {
		if _, alreadyUsed := seen[option.name]; alreadyUsed {
			continue
		}
		if !completionOptionCompatible(command.name, option.name, seen) {
			continue
		}
		candidates = append(candidates, "--"+option.name)
	}
	return candidates
}

func completionOptionCompatible(command, candidate string, seen map[string]fishCompletionOption) bool {
	if slices.Contains([]string{"low", "medium", "high", "xhigh"}, candidate) && countEnabledOptions(seen, "low", "medium", "high", "xhigh") > 0 {
		return false
	}
	if command == "review" || command == "simplify" {
		if _, wait := seen["wait"]; wait {
			return false
		}
		if candidate == "wait" {
			return len(seen) == 0
		}
		if slices.Contains([]string{"codebase", "uncommitted", "staged"}, candidate) {
			return countEnabledOptions(seen, "codebase", "uncommitted", "staged") == 0
		}
	}
	if command == "search" {
		return searchCompletionOptionCompatible(candidate, seen)
	}
	return true
}

func validSearchCompletionState(seen map[string]fishCompletionOption) bool {
	modes := seenOptionNames(seen, "ls", "ls-remotes", "ls-files")
	if len(modes) > 1 {
		return false
	}
	if len(modes) == 0 {
		return true
	}
	allowed, formats := searchModeContract(modes[0])
	for option := range seen {
		if !allowed[option] {
			return false
		}
	}
	if format, ok := seen["format"]; ok && !slices.Contains(formats, format.value) {
		return false
	}
	return true
}

func searchCompletionOptionCompatible(candidate string, seen map[string]fishCompletionOption) bool {
	modes := seenOptionNames(seen, "ls", "ls-remotes", "ls-files")
	if len(modes) == 1 {
		allowed, _ := searchModeContract(modes[0])
		return allowed[candidate]
	}

	if slices.Contains([]string{"ls", "ls-remotes", "ls-files"}, candidate) {
		allowed, formats := searchModeContract(candidate)
		for option := range seen {
			if !allowed[option] {
				return false
			}
		}
		if format, ok := seen["format"]; ok && !slices.Contains(formats, format.value) {
			return false
		}
		return true
	}
	if format, ok := seen["format"]; ok {
		return format.value == "json" || format.value == "brief"
	}
	return true
}

func searchModeContract(mode string) (map[string]bool, []string) {
	switch mode {
	case "ls":
		return map[string]bool{"ls": true, "remote": true, "format": true}, []string{"text", "json"}
	case "ls-remotes":
		return map[string]bool{"ls-remotes": true, "format": true}, []string{"text", "json", "completion"}
	case "ls-files":
		return map[string]bool{"ls-files": true, "format": true, "remote": true, "rev": true, "scope": true, "no-tests": true}, []string{"tree", "json"}
	default:
		return nil, nil
	}
}

func completionLine(command string, options []fishCompletionOption) string {
	var line strings.Builder
	fmt.Fprintf(&line, "git-agent %s", command)
	for _, option := range options {
		fmt.Fprintf(&line, " --%s", option.name)
		if option.takesValue {
			line.WriteByte(' ')
			line.WriteString(option.value)
		}
	}
	line.WriteString(" --")
	return line.String()
}

func completionLineWithOption(command, option string) string {
	return fmt.Sprintf("git-agent %s %s --", command, option)
}

func completionOptionNames(options []fishCompletionOption) []string {
	result := make([]string, 0, len(options))
	for _, option := range options {
		result = append(result, "--"+option.name)
	}
	return result
}

func candidatesWithPrefix(candidates []string, prefix string) []string {
	var result []string
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, prefix) {
			result = append(result, candidate)
		}
	}
	return result
}

func countEnabledOptions(seen map[string]fishCompletionOption, names ...string) int {
	return len(seenOptionNames(seen, names...))
}

func seenOptionNames(seen map[string]fishCompletionOption, names ...string) []string {
	var result []string
	for _, name := range names {
		if option, ok := seen[name]; ok && !option.disabled {
			result = append(result, name)
		}
	}
	return result
}

func completionCommandNamed(commands []fishCompletionCommand, name string) fishCompletionCommand {
	for _, command := range commands {
		if command.name == name {
			return command
		}
	}
	panic("missing completion command " + name)
}

func completionOptionNamed(options []fishCompletionOption, name string) fishCompletionOption {
	for _, option := range options {
		if option.name == name {
			return option
		}
	}
	panic("missing completion option " + name)
}

func completionSegmentCandidates(segment string) []string {
	segment = strings.TrimSuffix(segment, "\n")
	if segment == "" {
		return nil
	}
	return sortedCompletionCandidates(strings.Split(segment, "\n"))
}

func sortedCompletionCandidates(candidates []string) []string {
	result := slices.Clone(candidates)
	slices.Sort(result)
	return result
}

func requireFish(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("fish")
	if err != nil {
		t.Skip("fish is required to test completions")
	}
	return path
}

func fishCompletionPath(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate completion test source")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..", "completions", "git-agent.fish")
}

func writeCompletionExecutable(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

func completionTestEnvironment(fakeBin, configHome string) []string {
	values := map[string]string{
		"PATH":            fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"XDG_CONFIG_HOME": configHome,
		"LC_ALL":          "C",
	}
	env := os.Environ()
	result := make([]string, 0, len(env)+len(values))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok {
			if _, replaced := values[name]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}
