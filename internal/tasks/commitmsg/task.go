package commitmsg

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/textutil"
)

type Mode string

const (
	ModeNormal Mode = "normal"
	ModeAmend  Mode = "amend"
	ModePR     Mode = "pr"
)

type Request struct {
	Mode Mode
}

var taskIDSuffixPattern = regexp.MustCompile(`(?:\s+\(T\d+\))+$`)

type PreparedPRContext struct {
	Range         string              `json:"range"`
	BaseRef       string              `json:"base_ref"`
	Base          gitctx.CommitInfo   `json:"base"`
	HeadSHA       string              `json:"head_sha"`
	Branch        string              `json:"branch,omitempty"`
	ChangedPaths  []string            `json:"changed_paths"`
	Stats         []gitctx.FileStat   `json:"stats"`
	BranchCommits []gitctx.CommitInfo `json:"branch_commits"`
	RecentCommits []gitctx.CommitInfo `json:"recent_commits"`
	Diff          string              `json:"diff"`
	DiffTruncated bool                `json:"diff_truncated"`
}

type PreparedCommitContext struct {
	Mode                      Mode                `json:"mode"`
	StagedPaths               []string            `json:"staged_paths"`
	StagedStatus              []gitctx.PathChange `json:"staged_status"`
	StagedStats               []gitctx.FileStat   `json:"staged_stats"`
	RecentCommits             []gitctx.CommitInfo `json:"recent_commits"`
	PreviousHeadPaths         []string            `json:"previous_head_paths,omitempty"`
	PreviousHeadStats         []gitctx.FileStat   `json:"previous_head_stats,omitempty"`
	PreviousHeadDiff          string              `json:"previous_head_diff,omitempty"`
	PreviousHeadDiffTruncated bool                `json:"previous_head_diff_truncated,omitempty"`
	Diff                      string              `json:"diff"`
	DiffTruncated             bool                `json:"diff_truncated"`
}

func SystemPrompt(mode Mode) string {
	common := `
You draft high-signal Git commit messages that match repository history and the actual diff evidence.
Use provided context and, when tools are available, tools to inspect repository state before writing.
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
	if mode == ModePR {
		return textutil.NormalizePrompt(common + `
PR message mode:
Draft a squash merge commit message for the current branch versus origin/HEAD.
Describe the branch result as one coherent commit, not as a list of individual commits.
Treat the current-branch diff against origin/HEAD as authoritative for what changed.
Use branch commits as supporting evidence for intent and grouping only.
Do not write pull-request prose, review instructions, or release notes.
Avoid process phrasing such as "this PR" unless it is part of an existing task ID or literal code.
`)
	}
	return textutil.NormalizePrompt(common + `
Normal mode:
Inspect staged diff only.
Treat staged paths as authoritative scope.
Ignore unstaged and untracked work.
Match recent repo commit style when possible, including existing task IDs when still supported.
Cover each distinct high-signal staged change cluster that appears in the diff.
Use previous HEAD diff only as contrast to avoid restating previous work as current staged work.
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
	if mode == ModePR {
		return textutil.NormalizePrompt(budget + `
Generate a squash merge commit message for the current branch versus origin/HEAD.
Mission: describe the final branch change as one commit.
Rules:
- origin/HEAD is the base branch ref
- HEAD is the current branch tip
- changed paths between origin/HEAD and HEAD are authoritative scope
- branch commits explain intent and grouping, but do not emit a commit-by-commit changelog
- ignore staged/unstaged work unless it is already part of HEAD
- preserve task IDs when branch commits and diff support them
Structured context to gather:
- current directory
- current branch
- origin/HEAD base SHA and HEAD SHA
- changed paths
- branch commits
- diff stats
- full current-branch diff against origin/HEAD
Prefer the same style family as recent history: concise conventional subject, then focused rationale/details only when they add signal.
Start with git_pr_base, git_pr_paths, git_pr_commits, git_pr_stat, git_pr_diff, and git_recent_commits.
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
- cover every distinct staged-diff change cluster; do not drop a secondary file/behavior just because another cluster dominates the diff
- use recent commits and previous HEAD diff only as style/contrast; do not restate previous work as current staged work
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

func PrepareCommitContext(repo *gitctx.Repository) (PreparedCommitContext, error) {
	stagedPaths, err := repo.StagedPaths()
	if err != nil {
		return PreparedCommitContext{}, err
	}
	stagedStatus, err := repo.StagedStatus()
	if err != nil {
		return PreparedCommitContext{}, err
	}
	stagedStats, err := repo.StagedStat()
	if err != nil {
		return PreparedCommitContext{}, err
	}
	recentCommits, _ := repo.RecentCommits(10)
	previousHeadPaths, _ := repo.DiffAgainstParentPaths()
	previousHeadStats, _ := repo.DiffAgainstParentStat()
	previousHeadDiff, previousHeadDiffTruncated, _ := repo.DiffAgainstParent(24*1024, 700)
	diff, diffTruncated, err := repo.StagedDiff(48*1024, 1200)
	if err != nil {
		return PreparedCommitContext{}, err
	}

	return PreparedCommitContext{
		Mode:                      ModeNormal,
		StagedPaths:               stagedPaths,
		StagedStatus:              stagedStatus,
		StagedStats:               stagedStats,
		RecentCommits:             recentCommits,
		PreviousHeadPaths:         previousHeadPaths,
		PreviousHeadStats:         previousHeadStats,
		PreviousHeadDiff:          previousHeadDiff,
		PreviousHeadDiffTruncated: previousHeadDiffTruncated,
		Diff:                      diff,
		DiffTruncated:             diffTruncated,
	}, nil
}

func (c PreparedCommitContext) Render() string {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"mode":%q}`, c.Mode)
	}
	return string(data)
}

func UserPromptWithPreparedCommitContext(prepared PreparedCommitContext, maxSteps, maxToolCalls int) string {
	return textutil.NormalizePrompt(fmt.Sprintf(`
Current limits: %d total model steps, %d total tool calls. Spend budget carefully and finish within it.

Generate a commit message from the staged diff.
Mission: describe only prepared_commit_context.diff.
Rules:
- prepared_commit_context is authoritative
- staged_paths, staged_status, and staged_stats summarize the authoritative staged scope
- diff defines the output scope; ignore unstaged and untracked work
- recent_commits are style reference only
- previous_head_paths, previous_head_stats, and previous_head_diff are contrast only; use them to understand what was already done in HEAD, then describe only the new staged delta
- if previous_head_diff_truncated is true, rely on previous_head_paths/stats for contrast shape instead of assuming omitted hunks
- cover every distinct staged-diff change cluster; account for small outlier files when they carry behavior changes
- do not copy phrasing from recent commits or previous_head_diff as if it were current staged work
- if diff_truncated is true, stay conservative and describe only visible evidence
Return only the commit message.

<prepared_commit_context>
%s
</prepared_commit_context>
`, maxSteps, maxToolCalls, prepared.Render()))
}

func PreparePRContext(repo *gitctx.Repository) (PreparedPRContext, error) {
	base, err := repo.PullRequestBase()
	if err != nil {
		return PreparedPRContext{}, err
	}
	paths, err := repo.PullRequestPaths()
	if err != nil {
		return PreparedPRContext{}, err
	}
	stats, err := repo.PullRequestStat()
	if err != nil {
		return PreparedPRContext{}, err
	}
	branchCommits, err := repo.PullRequestCommits(50)
	if err != nil {
		return PreparedPRContext{}, err
	}
	recentCommits, err := repo.RecentCommits(10)
	if err != nil {
		return PreparedPRContext{}, err
	}
	diff, diffTruncated, err := repo.PullRequestDiff(48*1024, 1200)
	if err != nil {
		return PreparedPRContext{}, err
	}
	return PreparedPRContext{
		Range:         gitctx.PullRequestBaseRef + "..HEAD",
		BaseRef:       gitctx.PullRequestBaseRef,
		Base:          base,
		HeadSHA:       repo.HeadSHA,
		Branch:        repo.Branch,
		ChangedPaths:  paths,
		Stats:         stats,
		BranchCommits: branchCommits,
		RecentCommits: recentCommits,
		Diff:          diff,
		DiffTruncated: diffTruncated,
	}, nil
}

func (c PreparedPRContext) Render() string {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"range":%q}`, c.Range)
	}
	return string(data)
}

func UserPromptWithPreparedPRContext(prepared PreparedPRContext, maxSteps, maxToolCalls int) string {
	return textutil.NormalizePrompt(fmt.Sprintf(`
Current limits: %d total model steps, %d total tool calls. Spend budget carefully and finish within it.

Generate a squash merge commit message for the current branch versus origin/HEAD.
Mission: describe the final branch change as one coherent commit.
Rules:
- prepared_pr_context is authoritative
- changed_paths and diff define the output scope
- branch_commits explain intent and grouping, but do not emit a commit-by-commit changelog
- ignore staged/unstaged work unless it is already part of HEAD
- preserve task IDs when branch commits and diff support them
- if diff_truncated is true, stay conservative and describe only visible evidence
Return only the commit message.

<prepared_pr_context>
%s
</prepared_pr_context>
`, maxSteps, maxToolCalls, prepared.Render()))
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

func PreserveTaskIDSuffix(output string, references []gitctx.CommitInfo) string {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	subject := strings.TrimSpace(lines[0])
	if subject == "" || taskIDSuffixPattern.MatchString(subject) {
		return trimmed
	}
	for _, reference := range references {
		referenceSubject := strings.TrimSpace(reference.Summary)
		suffixLocation := taskIDSuffixPattern.FindStringIndex(referenceSubject)
		if suffixLocation == nil {
			continue
		}
		baseSubject := strings.TrimSpace(referenceSubject[:suffixLocation[0]])
		if subject != baseSubject {
			continue
		}
		lines[0] = subject + referenceSubject[suffixLocation[0]:]
		return strings.Join(lines, "\n")
	}
	if suffix := dominantRecentTaskIDSuffix(subject, references); suffix != "" {
		lines[0] = subject + suffix
		return strings.Join(lines, "\n")
	}
	if suffix := latestRecentTaskIDSuffix(references); suffix != "" {
		lines[0] = subject + suffix
		return strings.Join(lines, "\n")
	}
	return trimmed
}

func latestRecentTaskIDSuffix(references []gitctx.CommitInfo) string {
	if len(references) == 0 {
		return ""
	}
	referenceSubject := strings.TrimSpace(references[0].Summary)
	suffixLocation := taskIDSuffixPattern.FindStringIndex(referenceSubject)
	if suffixLocation == nil {
		return ""
	}
	return referenceSubject[suffixLocation[0]:]
}

func dominantRecentTaskIDSuffix(subject string, references []gitctx.CommitInfo) string {
	var suffix string
	var sameSuffixRun []string
	for _, reference := range references {
		referenceSubject := strings.TrimSpace(reference.Summary)
		suffixLocation := taskIDSuffixPattern.FindStringIndex(referenceSubject)
		if suffixLocation == nil {
			break
		}
		referenceSuffix := referenceSubject[suffixLocation[0]:]
		if suffix == "" {
			suffix = referenceSuffix
		}
		if referenceSuffix != suffix {
			break
		}
		sameSuffixRun = append(sameSuffixRun, referenceSubject)
	}
	if len(sameSuffixRun) < 2 {
		return ""
	}

	subjectScope := conventionalScope(subject)
	if subjectScope == "" {
		return suffix
	}
	for _, referenceSubject := range sameSuffixRun {
		if conventionalScope(referenceSubject) == subjectScope {
			return suffix
		}
	}
	return ""
}

func conventionalScope(subject string) string {
	prefix, _, ok := strings.Cut(subject, ":")
	if !ok {
		return ""
	}
	open := strings.Index(prefix, "(")
	if open < 0 || !strings.HasSuffix(prefix, ")") {
		return ""
	}
	return prefix[open+1 : len(prefix)-1]
}
