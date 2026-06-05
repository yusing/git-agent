package commitmsg

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/yusing/git-agent/internal/contextpack"
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

var (
	taskIDSuffixPattern              = regexp.MustCompile(`(?:\s+\(T\d+\))+$`)
	conventionalSubjectPrefixPattern = regexp.MustCompile(`^[a-z]+(?:\([^)]+\))?!?$`)
)

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
	Mode                      Mode                    `json:"mode"`
	StagedPaths               []string                `json:"staged_paths"`
	StagedStatus              []gitctx.PathChange     `json:"staged_status"`
	StagedStats               []gitctx.FileStat       `json:"staged_stats"`
	StagedSubmodules          []PreparedSubmodule     `json:"staged_submodules,omitempty"`
	ContextPack               contextpack.ContextPack `json:"context_pack"`
	RecentCommits             []gitctx.CommitInfo     `json:"recent_commits"`
	PreviousHeadPaths         []string                `json:"previous_head_paths,omitempty"`
	PreviousHeadStats         []gitctx.FileStat       `json:"previous_head_stats,omitempty"`
	PreviousHeadContextPack   contextpack.ContextPack `json:"previous_head_context_pack"`
	PreviousHeadDiff          string                  `json:"previous_head_diff,omitempty"`
	PreviousHeadDiffTruncated bool                    `json:"previous_head_diff_truncated,omitempty"`
	FocusDiff                 string                  `json:"focus_diff,omitempty"`
	FocusDiffPaths            []string                `json:"focus_diff_paths,omitempty"`
	FocusDiffTruncated        bool                    `json:"focus_diff_truncated,omitempty"`
	OutlierDiff               string                  `json:"outlier_diff,omitempty"`
	OutlierDiffTruncated      bool                    `json:"outlier_diff_truncated,omitempty"`
	Diff                      string                  `json:"diff"`
	DiffTruncated             bool                    `json:"diff_truncated"`
}

type PreparedAmendContext struct {
	Mode                Mode                    `json:"mode"`
	OriginalHeadMessage string                  `json:"original_head_message"`
	Head                gitctx.CommitInfo       `json:"head"`
	RecentCommits       []gitctx.CommitInfo     `json:"recent_commits"`
	FinalPaths          []string                `json:"final_paths"`
	FinalStats          []gitctx.FileStat       `json:"final_stats"`
	FinalContextPack    contextpack.ContextPack `json:"final_context_pack"`
	FinalDiff           string                  `json:"final_diff"`
	FinalDiffTruncated  bool                    `json:"final_diff_truncated"`
	HeadPaths           []string                `json:"head_paths"`
	HeadStats           []gitctx.FileStat       `json:"head_stats"`
	HeadContextPack     contextpack.ContextPack `json:"head_context_pack"`
	HeadDiff            string                  `json:"head_diff"`
	HeadDiffTruncated   bool                    `json:"head_diff_truncated"`
	StagedPaths         []string                `json:"staged_paths"`
	StagedStatus        []gitctx.PathChange     `json:"staged_status"`
	StagedStats         []gitctx.FileStat       `json:"staged_stats"`
	StagedSubmodules    []PreparedSubmodule     `json:"staged_submodules,omitempty"`
	StagedContextPack   contextpack.ContextPack `json:"staged_context_pack"`
	AmendDelta          string                  `json:"amend_delta"`
	AmendDeltaTruncated bool                    `json:"amend_delta_truncated"`
}

type PreparedSubmodule struct {
	Path                  string              `json:"path"`
	OldSHA                string              `json:"old_sha,omitempty"`
	NewSHA                string              `json:"new_sha,omitempty"`
	LocalHistoryAvailable bool                `json:"local_history_available"`
	AvailabilityError     string              `json:"availability_error,omitempty"`
	Commits               []gitctx.CommitInfo `json:"commits,omitempty"`
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
Choose 'refactor' when the staged diff mainly moves, extracts, centralizes, or reorganizes existing behavior, even if helper packages/files/tests are added.
Choose 'feat' only when the staged diff introduces a user-visible capability, API, command, config option, or behavior that did not exist before.
For extraction-heavy changes, prefer verbs such as "extract", "move", "centralize", "consolidate", or "reuse" over defaulting to "add" because files are new.
Body optional. When present, keep it concise, naturally wrapped, and within three short paragraphs.
When useful, the body may include compact nested detail blocks for submodule updates or grouped follow-up facts, but only when the diff clearly supports them.
When submodule commit summaries are provided, describe their actual changes instead of only saying the submodule ref moved.
`
	if mode == ModeAmend {
		return textutil.NormalizePrompt(common + `
Amend mode:
Describe the final amended commit as one commit versus its parent.
Treat the final amended diff as authoritative for what changed.
Treat the original HEAD commit message as the anchor, not disposable context.
Preserve the original subject and high-level story; revise body details only when the final amended diff proves them false.
Small staged cleanups, tests, docs, or formatter changes must not replace a broad original commit message with a narrow delta message.
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
- Previous HEAD message is the anchor for subject, tone, scope, and task IDs when still supported.
- HEAD vs staged views are diagnostic only; do not dual-narrate them.
Use git_final_amended_diff as authoritative.
Use git_head_show, git_diff_against_parent, and git_amend_delta only as diagnostics.
When prepared_amend_context is provided, read it before making tool calls; it already contains the latest HEAD commit being amended plus the final amended diff.
If staged content already matches the final amended story, preserve the original message or polish wording only.
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
- if the staged diff is large or truncated, use path-filtered staged diffs for omitted or high-churn clusters before finalizing broad claims
- classify extraction/move-only work as refactor, not feat, even when new helper files appear
Structured context to gather:
- current directory
- current branch
- staged paths
- recent commits
- git status
- git stats
- full staged diff
Useful follow-up for large diffs: git_staged_diff_for_paths on specific staged paths or clusters.
Prefer the same style family as recent history: concise conventional subject, then focused rationale/details only when they add signal.
Start with git_staged_paths, git_staged_status, git_staged_stat, git_staged_diff, and git_recent_commits.
Return only the commit message.
`)
}

func UserPromptWithOriginalAmendMessage(originalMessage string, maxSteps, maxToolCalls int) string {
	return textutil.NormalizePrompt(fmt.Sprintf(`
Current limits: %d total model steps, %d total tool calls. Spend budget carefully and finish within it.

Generate a commit message for the final post-amend commit result.
Start from original_head_message. It is the default answer and the message to preserve.
Rules:
- original_head_message is the anchor for subject, type/scope, task IDs, and high-level intent
- keep the original subject
- when staged changes are cleanup/refinement around the existing commit, preserve the original message wording instead of rewriting the commit around the staged delta
- use final amended evidence only to correct unsupported details or polish the existing message
- if evidence is incomplete or ambiguous, return original_head_message unchanged
- never replace a broad original commit message with a narrow message about only staged cleanup, tests, docs, or formatting

Evidence order:
1. original_head_message: anchor and default
2. final amended commit vs parent: authoritative support check
3. staged-vs-HEAD diagnostics: useful only to identify what changed during the amend

Use git_final_amended_diff as the authoritative support check.
Use git_head_show, git_diff_against_parent, and git_amend_delta only as diagnostics.
Return only the commit message.

<original_head_message>
%s
</original_head_message>
`, maxSteps, maxToolCalls, strings.TrimSpace(originalMessage)))
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
	stagedSubmodules, err := prepareStagedSubmodules(repo)
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
	contextPack := prepareContextPack(repo, stagedPaths, stagedStatus, stagedStats)
	previousHeadContextPack := prepareRevisionContextPack(repo, "HEAD", previousHeadPaths, previousHeadStats)
	focusDiff, focusDiffPaths, focusDiffTruncated, err := prepareFocusDiff(repo, contextPack, diff, diffTruncated)
	if err != nil {
		return PreparedCommitContext{}, err
	}
	outlierDiff, outlierDiffTruncated, err := prepareOutlierDiff(repo, contextPack)
	if err != nil {
		return PreparedCommitContext{}, err
	}

	return PreparedCommitContext{
		Mode:                      ModeNormal,
		StagedPaths:               stagedPaths,
		StagedStatus:              stagedStatus,
		StagedStats:               stagedStats,
		StagedSubmodules:          stagedSubmodules,
		ContextPack:               contextPack,
		RecentCommits:             recentCommits,
		PreviousHeadPaths:         previousHeadPaths,
		PreviousHeadStats:         previousHeadStats,
		PreviousHeadContextPack:   previousHeadContextPack,
		PreviousHeadDiff:          previousHeadDiff,
		PreviousHeadDiffTruncated: previousHeadDiffTruncated,
		FocusDiff:                 focusDiff,
		FocusDiffPaths:            focusDiffPaths,
		FocusDiffTruncated:        focusDiffTruncated,
		OutlierDiff:               outlierDiff,
		OutlierDiffTruncated:      outlierDiffTruncated,
		Diff:                      diff,
		DiffTruncated:             diffTruncated,
	}, nil
}

func PrepareAmendContext(repo *gitctx.Repository) (PreparedAmendContext, error) {
	originalMessage, err := repo.HeadMessage()
	if err != nil {
		return PreparedAmendContext{}, err
	}
	head, err := repo.HeadInfo()
	if err != nil {
		return PreparedAmendContext{}, err
	}
	stagedPaths, err := repo.StagedPaths()
	if err != nil {
		return PreparedAmendContext{}, err
	}
	stagedStatus, err := repo.StagedStatus()
	if err != nil {
		return PreparedAmendContext{}, err
	}
	stagedStats, err := repo.StagedStat()
	if err != nil {
		return PreparedAmendContext{}, err
	}
	stagedSubmodules, err := prepareStagedSubmodules(repo)
	if err != nil {
		return PreparedAmendContext{}, err
	}
	finalPaths, err := repo.FinalAmendedPaths()
	if err != nil {
		return PreparedAmendContext{}, err
	}
	finalStats, err := repo.FinalAmendedStat()
	if err != nil {
		return PreparedAmendContext{}, err
	}
	finalDiff, finalDiffTruncated, err := repo.FinalAmendedDiff(48*1024, 1200)
	if err != nil {
		return PreparedAmendContext{}, err
	}
	headPaths, err := repo.DiffAgainstParentPaths()
	if err != nil {
		return PreparedAmendContext{}, err
	}
	headStats, err := repo.DiffAgainstParentStat()
	if err != nil {
		return PreparedAmendContext{}, err
	}
	headDiff, headDiffTruncated, err := repo.DiffAgainstParent(24*1024, 700)
	if err != nil {
		return PreparedAmendContext{}, err
	}
	amendDelta, amendDeltaTruncated, err := repo.AmendDelta(24*1024, 700)
	if err != nil {
		return PreparedAmendContext{}, err
	}
	recentCommits, _ := repo.RecentCommits(10)

	return PreparedAmendContext{
		Mode:                ModeAmend,
		OriginalHeadMessage: strings.TrimSpace(originalMessage),
		Head:                head,
		RecentCommits:       recentCommits,
		FinalPaths:          finalPaths,
		FinalStats:          finalStats,
		FinalContextPack:    prepareIndexContextPack(repo, finalPaths, finalStats, "final"),
		FinalDiff:           finalDiff,
		FinalDiffTruncated:  finalDiffTruncated,
		HeadPaths:           headPaths,
		HeadStats:           headStats,
		HeadContextPack:     prepareRevisionContextPack(repo, "HEAD", headPaths, headStats),
		HeadDiff:            headDiff,
		HeadDiffTruncated:   headDiffTruncated,
		StagedPaths:         stagedPaths,
		StagedStatus:        stagedStatus,
		StagedStats:         stagedStats,
		StagedSubmodules:    stagedSubmodules,
		StagedContextPack:   prepareContextPack(repo, stagedPaths, stagedStatus, stagedStats),
		AmendDelta:          amendDelta,
		AmendDeltaTruncated: amendDeltaTruncated,
	}, nil
}

func prepareOutlierDiff(repo *gitctx.Repository, pack contextpack.ContextPack) (string, bool, error) {
	if !contextpack.IsLargeGeneratedHeavy(pack) || len(pack.Outliers) == 0 {
		return "", false, nil
	}
	paths := make([]string, 0, len(pack.Outliers))
	for _, outlier := range pack.Outliers {
		paths = append(paths, outlier.Path)
	}
	return repo.StagedDiffForPaths(paths, 48*1024, 1200)
}

func prepareFocusDiff(repo *gitctx.Repository, pack contextpack.ContextPack, diff string, diffTruncated bool) (string, []string, bool, error) {
	if !diffTruncated || contextpack.IsLargeGeneratedHeavy(pack) {
		return "", nil, false, nil
	}
	paths := focusDiffPaths(pack, diff, 5)
	if len(paths) == 0 {
		return "", nil, false, nil
	}
	focusDiff, truncated, err := repo.StagedDiffForPaths(paths, 64*1024, 1600)
	if err != nil {
		return "", nil, false, err
	}
	return focusDiff, paths, truncated, nil
}

func focusDiffPaths(pack contextpack.ContextPack, diff string, limit int) []string {
	candidates := focusDiffCandidates(pack)
	if len(candidates) == 0 || limit <= 0 {
		return nil
	}
	var paths []string
	for _, candidate := range candidates {
		if diffMentionsPath(diff, candidate.Path) {
			continue
		}
		paths = append(paths, candidate.Path)
		if len(paths) >= limit {
			return paths
		}
	}
	if len(paths) == 0 {
		paths = append(paths, candidates[0].Path)
	}
	return paths
}

func focusDiffCandidates(pack contextpack.ContextPack) []contextpack.FileSummary {
	seen := map[string]bool{}
	var candidates []contextpack.FileSummary
	add := func(file contextpack.FileSummary) {
		if file.Path == "" || seen[file.Path] {
			return
		}
		seen[file.Path] = true
		candidates = append(candidates, file)
	}
	for _, group := range pack.Groups {
		for _, file := range group.TopChurn {
			add(file)
		}
		for _, file := range group.Samples {
			add(file)
		}
	}
	for _, file := range pack.Outliers {
		add(file)
	}
	slices.SortFunc(candidates, func(a, b contextpack.FileSummary) int {
		left := a.Adds + a.Deletes
		right := b.Adds + b.Deletes
		if left != right {
			return right - left
		}
		return strings.Compare(a.Path, b.Path)
	})
	return candidates
}

func diffMentionsPath(diff, path string) bool {
	path = filepath.ToSlash(path)
	return strings.Contains(diff, " b/"+path) ||
		strings.Contains(diff, " a/"+path) ||
		strings.Contains(diff, "+++ b/"+path) ||
		strings.Contains(diff, "--- a/"+path)
}

func (c PreparedCommitContext) Render() string {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"mode":%q}`, c.Mode)
	}
	return string(data)
}

func (c PreparedAmendContext) Render() string {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"mode":%q}`, c.Mode)
	}
	return string(data)
}

func (c PreparedAmendContext) RenderForPrompt() string {
	return c.Render()
}

func (c PreparedAmendContext) TraceValue() any {
	return c
}

func (c PreparedCommitContext) RenderForPrompt() string {
	if !c.useCompactPrompt() {
		return c.Render()
	}
	view := map[string]any{
		"mode":              c.Mode,
		"recent_commits":    c.RecentCommits,
		"staged_submodules": c.StagedSubmodules,
	}
	if c.compactCurrentForPrompt() {
		view["context_pack"] = c.ContextPack
		view["diff_ref"] = "prepared_commit_context.diff"
		view["diff_truncated"] = c.DiffTruncated
		if c.OutlierDiff != "" {
			view["outlier_diff"] = c.OutlierDiff
			view["outlier_diff_truncated"] = c.OutlierDiffTruncated
		}
	} else {
		view["staged_paths"] = c.StagedPaths
		view["staged_status"] = c.StagedStatus
		view["staged_stats"] = c.StagedStats
		view["context_pack"] = c.ContextPack
		view["diff"] = c.Diff
		view["diff_truncated"] = c.DiffTruncated
	}
	if c.compactPreviousForPrompt() {
		view["previous_head_ref"] = "prepared_commit_context.previous_head_diff"
		view["previous_head_context_pack"] = c.PreviousHeadContextPack
		view["previous_head_summary"] = summarizePreviousHead(c.PreviousHeadPaths, c.PreviousHeadStats, c.PreviousHeadDiffTruncated)
	} else {
		view["previous_head_paths"] = c.PreviousHeadPaths
		view["previous_head_stats"] = c.PreviousHeadStats
		view["previous_head_context_pack"] = c.PreviousHeadContextPack
		view["previous_head_diff"] = c.PreviousHeadDiff
		view["previous_head_diff_truncated"] = c.PreviousHeadDiffTruncated
	}
	data, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"mode":%q}`, c.Mode)
	}
	return string(data)
}

func (c PreparedCommitContext) TraceValue() any {
	if len(c.PreviousHeadPaths) <= 100 && len(c.StagedPaths) <= 100 {
		return c
	}
	view := map[string]any{
		"mode":                         c.Mode,
		"staged_submodules":            c.StagedSubmodules,
		"context_pack":                 c.ContextPack,
		"recent_commits":               c.RecentCommits,
		"previous_head_summary":        summarizePreviousHead(c.PreviousHeadPaths, c.PreviousHeadStats, c.PreviousHeadDiffTruncated),
		"previous_head_context_pack":   c.PreviousHeadContextPack,
		"previous_head_diff":           c.PreviousHeadDiff,
		"previous_head_diff_truncated": c.PreviousHeadDiffTruncated,
		"diff":                         c.Diff,
		"diff_truncated":               c.DiffTruncated,
	}
	if c.OutlierDiff != "" {
		view["outlier_diff"] = c.OutlierDiff
		view["outlier_diff_truncated"] = c.OutlierDiffTruncated
	}
	if c.FocusDiff != "" {
		view["focus_diff_paths"] = c.FocusDiffPaths
		view["focus_diff"] = c.FocusDiff
		view["focus_diff_truncated"] = c.FocusDiffTruncated
	}
	if len(c.StagedPaths) > 100 {
		view["staged_summary"] = summarizeStaged(c.StagedPaths, c.StagedStats, c.DiffTruncated)
	} else {
		view["staged_paths"] = c.StagedPaths
		view["staged_status"] = c.StagedStatus
		view["staged_stats"] = c.StagedStats
	}
	return view
}

func (c PreparedCommitContext) useCompactPrompt() bool {
	return c.compactCurrentForPrompt() || c.compactPreviousForPrompt()
}

func (c PreparedCommitContext) compactCurrentForPrompt() bool {
	return contextpack.IsLargeGeneratedHeavy(c.ContextPack)
}

func (c PreparedCommitContext) compactPreviousForPrompt() bool {
	return len(c.PreviousHeadPaths) > 100 ||
		contextpack.IsLargeGeneratedHeavy(c.PreviousHeadContextPack)
}

func UserPromptWithPreparedCommitContext(prepared PreparedCommitContext, maxSteps, maxToolCalls int) string {
	return textutil.NormalizePrompt(fmt.Sprintf(`
Current limits: %d total model steps, %d total tool calls. Spend budget carefully and finish within it.

Generate a commit message from the staged diff.
Mission: describe only the staged changes represented by prepared_commit_context.
Rules:
- prepared_commit_context is authoritative
- context_pack summarizes the authoritative staged scope when present
- staged_paths, staged_status, and staged_stats summarize the authoritative staged scope when present
- diff defines the output scope when present; otherwise use context_pack plus refs and stay conservative
- focus_diff contains extra authoritative staged hunks for high-churn paths omitted or cut off by a truncated bounded diff
- outlier_diff contains authoritative raw staged hunks for small outlier files when dominant generated hunks are compacted
- recent_commits are style reference only
- previous_head_paths, previous_head_stats, previous_head_diff, previous_head_summary, and previous_head_context_pack are contrast only; use them to understand what was already done in HEAD, then describe only the new staged delta
- if previous_head_diff_truncated is true, rely on previous_head_paths/stats for contrast shape instead of assuming omitted hunks
- cover every distinct staged-diff change cluster; account for small outlier files when they carry behavior changes
- if diff_truncated is true or broad clusters are represented only by paths/stats/context_pack, use git_staged_diff_for_paths for omitted or high-churn clusters before finalizing broad claims
- if focus_diff is present, use it together with diff and context_pack; focus_diff_paths explains which omitted or cut-off paths it covers
- choose refactor when staged evidence shows extraction, relocation, deduplication, or internal reorganization of existing behavior; choose feat only for genuinely new user-visible capability/API/command/config behavior
- do not default to "add" phrasing because files are new; for extraction-heavy changes prefer "extract", "move", "centralize", "consolidate", or "reuse"
- if staged_submodules contains commits, use those submodule commit summaries as staged evidence; do not collapse them to a generic "newer submodule refs" message
- do not copy phrasing from recent commits or previous_head_diff as if it were current staged work
- if diff_truncated is true, stay conservative and describe only visible evidence
- if outlier_diff_truncated is true, stay conservative for outlier details beyond visible hunks
Return only the commit message.

<prepared_commit_context>
%s
</prepared_commit_context>
	`, maxSteps, maxToolCalls, prepared.RenderForPrompt()))
}

func UserPromptWithPreparedAmendContext(prepared PreparedAmendContext, maxSteps, maxToolCalls int) string {
	return textutil.NormalizePrompt(fmt.Sprintf(`
Current limits: %d total model steps, %d total tool calls. Spend budget carefully and finish within it.

Generate a commit message for the final post-amend commit result represented by prepared_amend_context.
Mission: describe the final amended commit as one commit, not the staged delta.
Rules:
- prepared_amend_context is authoritative initial evidence; it includes the latest HEAD commit being amended before any tool calls
- original_head_message is the default answer and anchor for subject, type/scope, task IDs, and high-level intent
- keep the original subject
- final_paths, final_stats, final_context_pack, and final_diff describe the final amended commit vs its first parent and are the authoritative support check
- head, head_paths, head_stats, head_context_pack, and head_diff describe the current HEAD/latest commit being amended; use them to preserve the original high-level story
- staged_paths, staged_status, staged_stats, staged_context_pack, staged_submodules, and amend_delta are diagnostics only; never base the subject or narrative on staged changes alone
- when staged changes are cleanup/refinement around the existing commit, preserve the original message wording instead of rewriting the commit around the staged delta
- use final amended evidence only to correct unsupported details or polish the existing message
- if final_diff_truncated, head_diff_truncated, or amend_delta_truncated is true, stay conservative and request narrower tool context before changing broad claims
- if evidence remains incomplete or ambiguous, return original_head_message unchanged
- never replace a broad original commit message with a narrow message about only staged cleanup, tests, docs, or formatting
- no delta/process phrasing such as "also", "this amend", or "in addition"
Tools remain available for narrow follow-up inspection when prepared evidence is truncated or ambiguous:
- git_final_amended_diff is the authoritative extra diff tool
- git_head_show and git_diff_against_parent inspect HEAD diagnostics
- git_amend_delta inspects staged-vs-HEAD diagnostics
Return only the commit message.

<prepared_amend_context>
%s
</prepared_amend_context>
`, maxSteps, maxToolCalls, prepared.RenderForPrompt()))
}

func prepareContextPack(repo *gitctx.Repository, paths []string, status []gitctx.PathChange, stats []gitctx.FileStat) contextpack.ContextPack {
	statusByPath := map[string]string{}
	for _, change := range status {
		statusByPath[change.Path] = change.Staging
	}
	statsByPath := map[string]gitctx.FileStat{}
	for _, stat := range stats {
		statsByPath[stat.Path] = stat
	}

	files := make([]contextpack.FileFact, 0, len(paths))
	for _, path := range paths {
		stat := statsByPath[path]
		header := ""
		if filepath.Ext(path) == ".go" {
			header, _, _ = repo.StagedFilePrefix(path, 8*1024)
		}
		files = append(files, contextpack.FileFact{
			Path:     path,
			Status:   statusByPath[path],
			Adds:     stat.Adds,
			Deletes:  stat.Deletes,
			IsBinary: stat.IsBinary,
			Header:   header,
		})
	}
	return contextpack.Build(files, contextpack.Options{})
}

func prepareIndexContextPack(repo *gitctx.Repository, paths []string, stats []gitctx.FileStat, status string) contextpack.ContextPack {
	statsByPath := map[string]gitctx.FileStat{}
	for _, stat := range stats {
		statsByPath[stat.Path] = stat
	}

	files := make([]contextpack.FileFact, 0, len(paths))
	for _, path := range paths {
		stat := statsByPath[path]
		header := ""
		if filepath.Ext(path) == ".go" {
			header, _, _ = repo.StagedFilePrefix(path, 8*1024)
		}
		files = append(files, contextpack.FileFact{
			Path:     path,
			Status:   status,
			Adds:     stat.Adds,
			Deletes:  stat.Deletes,
			IsBinary: stat.IsBinary,
			Header:   header,
		})
	}
	return contextpack.Build(files, contextpack.Options{})
}

func prepareRevisionContextPack(repo *gitctx.Repository, rev string, paths []string, stats []gitctx.FileStat) contextpack.ContextPack {
	statsByPath := map[string]gitctx.FileStat{}
	for _, stat := range stats {
		statsByPath[stat.Path] = stat
	}

	files := make([]contextpack.FileFact, 0, len(paths))
	for _, path := range paths {
		stat := statsByPath[path]
		header := ""
		if filepath.Ext(path) == ".go" {
			header, _, _ = repo.ShowFileAtRev(rev, path, 8*1024, 0)
		}
		files = append(files, contextpack.FileFact{
			Path:     path,
			Status:   "changed",
			Adds:     stat.Adds,
			Deletes:  stat.Deletes,
			IsBinary: stat.IsBinary,
			Header:   header,
		})
	}
	return contextpack.Build(files, contextpack.Options{})
}

func summarizePreviousHead(paths []string, stats []gitctx.FileStat, truncated bool) map[string]any {
	summary := summarizeFileSet(paths, stats)
	summary["diff_truncated"] = truncated
	return summary
}

func summarizeStaged(paths []string, stats []gitctx.FileStat, truncated bool) map[string]any {
	summary := summarizeFileSet(paths, stats)
	summary["diff_truncated"] = truncated
	return summary
}

func summarizeFileSet(paths []string, stats []gitctx.FileStat) map[string]any {
	summary := map[string]any{"paths": len(paths)}
	var adds, deletes int
	for _, stat := range stats {
		adds += stat.Adds
		deletes += stat.Deletes
	}
	summary["adds"] = adds
	summary["deletes"] = deletes
	if len(paths) > 0 {
		limit := min(5, len(paths))
		summary["sample_paths"] = paths[:limit]
	}
	return summary
}

func prepareStagedSubmodules(repo *gitctx.Repository) ([]PreparedSubmodule, error) {
	changes, err := repo.StagedSubmoduleChanges()
	if err != nil {
		return nil, err
	}
	submodules := make([]PreparedSubmodule, 0, len(changes))
	for _, change := range changes {
		submodule := PreparedSubmodule{
			Path:   change.Path,
			OldSHA: change.Old,
			NewSHA: change.New,
		}
		if change.Old == "" || change.New == "" {
			submodules = append(submodules, submodule)
			continue
		}
		subRepo, err := gitctx.Open(filepath.Join(repo.RootPath, change.Path))
		if err != nil {
			submodule.AvailabilityError = err.Error()
			submodules = append(submodules, submodule)
			continue
		}
		commits, err := subRepo.LogFrom(change.Old, change.New, 50)
		if err != nil {
			submodule.AvailabilityError = err.Error()
			submodules = append(submodules, submodule)
			continue
		}
		submodule.LocalHistoryAvailable = true
		submodule.Commits = commits
		submodules = append(submodules, submodule)
	}
	return submodules, nil
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

func ValidateAmendAgainstOriginal(originalMessage, output string) []string {
	errs := Validate(ModeAmend, output)
	originalSubject := firstSubjectLine(originalMessage)
	if originalSubject == "" {
		return errs
	}
	subject := firstSubjectLine(output)
	if subject != originalSubject {
		errs = append(errs, fmt.Sprintf("amend output must preserve original HEAD subject %q, got %q", originalSubject, subject))
	}
	return errs
}

func firstSubjectLine(message string) string {
	for line := range strings.SplitSeq(strings.TrimSpace(message), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func Shape(output string) string {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return ""
	}

	subject, body := splitSubjectAndBody(trimmed)
	if body == "" {
		return subject
	}
	return subject + "\n\n" + textutil.WrapBody(body, 72)
}

func splitSubjectAndBody(text string) (string, string) {
	parts := strings.SplitN(text, "\n\n", 2)
	firstParagraph := strings.Split(parts[0], "\n")
	if shouldUnwrapSubjectContinuation(firstParagraph) {
		subject := joinTrimmedLines(firstParagraph)
		if len(parts) == 1 {
			return subject, ""
		}
		return subject, parts[1]
	}

	subjectParts := strings.SplitN(text, "\n", 2)
	subject := strings.TrimSpace(subjectParts[0])
	if len(subjectParts) == 1 {
		return subject, ""
	}
	return subject, strings.TrimSpace(subjectParts[1])
}

func shouldUnwrapSubjectContinuation(lines []string) bool {
	if len(lines) < 2 {
		return false
	}

	first := strings.TrimSpace(lines[0])
	second := strings.TrimSpace(lines[1])
	return endsWithSubjectConnector(first) ||
		(len(first) >= 50 && looksLikeConventionalSubject(first) && startsLowercase(second))
}

func endsWithSubjectConnector(text string) bool {
	for _, suffix := range []string{" and", " or", " to", " for", " with", " across", ","} {
		if strings.HasSuffix(text, suffix) {
			return true
		}
	}
	return false
}

func looksLikeConventionalSubject(text string) bool {
	prefix, summary, ok := strings.Cut(text, ":")
	return ok && summary != "" && conventionalSubjectPrefixPattern.MatchString(prefix)
}

func startsLowercase(text string) bool {
	for _, r := range text {
		return r >= 'a' && r <= 'z'
	}
	return false
}

func joinTrimmedLines(lines []string) string {
	trimmed := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			trimmed = append(trimmed, line)
		}
	}
	return strings.Join(trimmed, " ")
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
