package commitmsg

import (
	"fmt"
	"strings"

	"github.com/yusing/git-agent/internal/textutil"
)

type Mode string

const (
	ModeNormal Mode = "normal"
	ModeAmend  Mode = "amend"
)

type Request struct {
	Mode Mode
}

func SystemPrompt(mode Mode) string {
	common := `
You draft high-signal Git commit messages that match repository history and the actual diff evidence.
Use tools to inspect repository state before writing.
Return only the final commit message.
No Markdown fences. No explanations.
Subject line first. Blank line before body only when body exists.
Wrap body lines around 72 columns.
Recent commits are style reference only.
Describe only supported changes. Do not invent motivations unsupported by the diff.
Infer accurate type, scope, and impact from the evidence.
Body optional. When present, keep it concise, naturally wrapped, and within three short paragraphs.
When useful, the body may include compact nested detail blocks for submodule updates or grouped follow-up facts, but only when the diff clearly supports them.
`
	if mode == ModeAmend {
		return textutil.NormalizePrompt(common + `
Amend mode:
Describe the final amended commit as one commit versus its parent.
Treat the final amended diff as authoritative for what changed.
Do not narrate a delta or process.
Do not sound like an addendum, follow-up, or layered update.
Never tell the story as previous HEAD plus extra staged changes.
Avoid "also", "this amend", "in addition", and similar process phrasing.
Preserve task IDs or scope markers only when supported by the final diff.
`)
	}
	return textutil.NormalizePrompt(common + `
Normal mode:
Inspect staged diff only.
Treat staged paths as authoritative scope.
Ignore unstaged and untracked work.
Match recent repo commit style when possible, including existing task IDs when still supported.
Use related file reads only when the staged diff is ambiguous.
`)
}

func UserPrompt(mode Mode, maxSteps, maxToolCalls int) string {
	budget := fmt.Sprintf("Current limits: %d total model steps, %d total tool calls. Spend budget carefully and finish within it.\n\n", maxSteps, maxToolCalls)
	if mode == ModeAmend {
		return textutil.NormalizePrompt(budget + `
Generate a commit message for the final post-amend commit result.
How to read the evidence:
- Full amended commit vs parent comes first and is authoritative for what to describe.
- Previous HEAD message is reference only for tone / scope / task IDs when still supported.
- HEAD vs staged views are diagnostic only; do not dual-narrate them.
Use git_final_amended_diff as authoritative.
Use git_head_show, git_diff_against_parent, and git_amend_delta only as diagnostics.
If staged content already matches the final amended story, polish wording only.
Return only the commit message.
`)
	}
	return textutil.NormalizePrompt(budget + `
Generate a commit message from the staged diff.
Mission: describe only staged changes.
Rules:
- staged paths are authoritative scope
- ignore unstaged and untracked work
- preserve task IDs when recent history and staged diff support them
- inspect related files only if the staged diff is ambiguous
Structured context to gather:
- current directory
- current branch
- staged paths
- recent commits
- git status
- git stats
- full staged diff
Prefer the same style family as recent history: concise conventional subject, then focused rationale/details only when they add signal.
Start with git_staged_paths, git_staged_status, git_staged_stat, git_staged_diff, and git_recent_commits.
Return only the commit message.
`)
}

func Validate(mode Mode, output string) []string {
	var errs []string
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		errs = append(errs, "output is empty")
		return errs
	}
	if strings.Contains(trimmed, "```") {
		errs = append(errs, "output contains code fences")
	}
	lines := strings.Split(trimmed, "\n")
	subject := strings.TrimSpace(lines[0])
	if subject == "" {
		errs = append(errs, "subject is missing")
	}
	forbidden := []string{"here is", "commit message:", "explanation:", "i would"}
	lower := strings.ToLower(trimmed)
	for _, phrase := range forbidden {
		if strings.Contains(lower, phrase) {
			errs = append(errs, fmt.Sprintf("stray commentary phrase %q", phrase))
		}
	}
	if mode == ModeAmend {
		for _, phrase := range []string{"this amend", "in addition", "also"} {
			if strings.Contains(lower, phrase) {
				errs = append(errs, fmt.Sprintf("amend output uses process/delta phrase %q", phrase))
			}
		}
	}
	for i, line := range lines[1:] {
		if len(line) > 90 {
			errs = append(errs, fmt.Sprintf("body line %d is too long", i+2))
		}
	}
	return errs
}

func Shape(output string) string {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return ""
	}
	parts := strings.SplitN(trimmed, "\n", 2)
	subject := strings.TrimSpace(parts[0])
	if len(parts) == 1 || strings.TrimSpace(parts[1]) == "" {
		return subject
	}
	body := textutil.WrapBody(parts[1], 72)
	return subject + "\n\n" + body
}
