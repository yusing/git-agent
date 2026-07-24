package commitmsg

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/bytedance/sonic"
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

type commitMessageStyle int

const (
	commitMessageStyleConventional commitMessageStyle = iota
	commitMessageStyleTitle
)

func FormatSubmoduleOnlyCommit(prepared PreparedCommitContext) (string, bool) {
	if !isSubmoduleOnlyCommit(prepared) {
		return "", false
	}

	submodules := slices.Clone(prepared.StagedSubmodules)
	slices.SortFunc(submodules, func(a, b PreparedSubmodule) int {
		return strings.Compare(a.Path, b.Path)
	})

	paths := make([]string, 0, len(submodules))
	for _, submodule := range submodules {
		paths = append(paths, submodule.Path)
	}

	subject := formatSubmoduleSubject(detectCommitMessageStyle(prepared.RecentCommits), paths)
	body := formatSubmoduleBody(submodules)
	if body == "" {
		return subject, true
	}
	return Shape(subject + "\n\n" + body), true
}

// FormatSubmoduleOnlyCommitForRepo checks the submodule-only path without preparing full commit context.
func FormatSubmoduleOnlyCommitForRepo(repo *gitctx.Repository, stagedPaths []string) (string, bool, error) {
	stagedSubmodules, err := prepareStagedSubmodules(repo)
	if err != nil {
		return "", false, err
	}
	if len(stagedSubmodules) == 0 {
		return "", false, nil
	}
	recentCommits, _ := repo.RecentCommits(10)
	message, ok := FormatSubmoduleOnlyCommit(PreparedCommitContext{
		StagedPaths:      stagedPaths,
		StagedSubmodules: stagedSubmodules,
		RecentCommits:    recentCommits,
	})
	return message, ok, nil
}

func isSubmoduleOnlyCommit(prepared PreparedCommitContext) bool {
	if len(prepared.StagedPaths) == 0 || len(prepared.StagedSubmodules) == 0 {
		return false
	}
	if len(prepared.StagedPaths) != len(prepared.StagedSubmodules) {
		return false
	}

	submodulePaths := map[string]bool{}
	for _, submodule := range prepared.StagedSubmodules {
		if strings.TrimSpace(submodule.Path) == "" {
			return false
		}
		submodulePaths[filepath.ToSlash(submodule.Path)] = true
	}
	for _, path := range prepared.StagedPaths {
		if !submodulePaths[filepath.ToSlash(path)] {
			return false
		}
	}
	return true
}

func detectCommitMessageStyle(recent []gitctx.CommitInfo) commitMessageStyle {
	for _, commit := range recent {
		summary := strings.TrimSpace(commit.Summary)
		switch {
		case isConventionalSummary(summary):
			return commitMessageStyleConventional
		case isTitleCaseSummary(summary):
			return commitMessageStyleTitle
		}
	}
	return commitMessageStyleConventional
}

func isConventionalSummary(summary string) bool {
	prefix, _, ok := strings.Cut(summary, ":")
	return ok && conventionalSubjectPrefixPattern.MatchString(strings.TrimSpace(prefix))
}

func isTitleCaseSummary(summary string) bool {
	if summary == "" || isConventionalSummary(summary) {
		return false
	}
	first := summary[0]
	return first >= 'A' && first <= 'Z'
}

func formatSubmoduleSubject(style commitMessageStyle, paths []string) string {
	target := formatSubmoduleSubjectTarget(paths)
	if style == commitMessageStyleTitle {
		return "Update " + target
	}
	return "chore(deps): update " + target
}

func formatSubmoduleSubjectTarget(paths []string) string {
	switch len(paths) {
	case 0:
		return "submodules"
	case 1:
		return paths[0] + " submodule"
	}
	if len(paths) > 3 {
		return "submodules"
	}
	return humanList(paths) + " submodules"
}

func humanList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", and " + items[len(items)-1]
	}
}

func formatSubmoduleBody(submodules []PreparedSubmodule) string {
	var out []string
	for _, submodule := range submodules {
		if submodule.Path == "" {
			continue
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, submodule.Path)
		entries := formatSubmoduleEntries(submodule)
		for _, entry := range entries {
			out = append(out, "  - "+entry)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func formatSubmoduleEntries(submodule PreparedSubmodule) []string {
	entries := make([]string, 0, len(submodule.Commits))
	for _, commit := range submodule.Commits {
		summary := strings.TrimSpace(commit.Summary)
		if summary == "" {
			continue
		}
		sha := shortSHA(commit.SHA)
		if sha == "" {
			entries = append(entries, summary)
			continue
		}
		entries = append(entries, sha+": "+summary)
	}
	if len(entries) > 0 {
		return entries
	}

	switch {
	case submodule.NewSHA != "":
		return []string{shortSHA(submodule.NewSHA) + ": update submodule pointer"}
	case submodule.OldSHA != "":
		return []string{shortSHA(submodule.OldSHA) + ": remove submodule pointer"}
	default:
		return []string{"update submodule pointer"}
	}
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func SystemPrompt(mode Mode) string {
	if mode == ModeAmend {
		return renderSystemPrompt(systemModeAmendPrompt)
	}
	if mode == ModePR {
		return renderSystemPrompt(systemModePRPrompt)
	}
	return renderSystemPrompt(systemModeNormalPrompt)
}

func UserPrompt(mode Mode, maxSteps, maxToolCalls int) string {
	data := userPromptData{MaxSteps: maxSteps, MaxToolCalls: maxToolCalls}
	if mode == ModeAmend {
		return executeUserPrompt(userAmendPromptTemplate, data)
	}
	if mode == ModePR {
		return executeUserPrompt(userPRPromptTemplate, data)
	}
	return executeUserPrompt(userNormalPromptTemplate, data)
}

type boundedDiffResult struct {
	Text      string
	Truncated bool
}

type focusDiffResult struct {
	Text      string
	Paths     []string
	Truncated bool
}

type collected[T any] struct {
	value T
	err   error
}

func collectWithRepo[T any](wg *sync.WaitGroup, root string, fn func(*gitctx.Repository) (T, error)) *collected[T] {
	result := &collected[T]{}
	wg.Go(func() {
		taskRepo, err := gitctx.Open(root)
		if err != nil {
			result.err = err
			return
		}
		result.value, result.err = fn(taskRepo)
	})
	return result
}

func collectBestEffortWithRepo[T any](wg *sync.WaitGroup, root string, fn func(*gitctx.Repository) (T, error)) *collected[T] {
	return collectWithRepo(wg, root, fn)
}

func PrepareCommitContext(repo *gitctx.Repository) (PreparedCommitContext, error) {
	var wg sync.WaitGroup
	root := repo.RootPath
	stagedPaths := collectWithRepo(&wg, root, (*gitctx.Repository).StagedPaths)
	stagedStatus := collectWithRepo(&wg, root, (*gitctx.Repository).StagedStatus)
	stagedStats := collectWithRepo(&wg, root, (*gitctx.Repository).StagedStat)
	stagedSubmodules := collectWithRepo(&wg, root, prepareStagedSubmodules)
	diff := collectWithRepo(&wg, root, func(taskRepo *gitctx.Repository) (boundedDiffResult, error) {
		text, truncated, err := taskRepo.StagedDiff(48*1024, 1200)
		return boundedDiffResult{Text: text, Truncated: truncated}, err
	})
	recentCommits := collectBestEffortWithRepo(&wg, root, func(taskRepo *gitctx.Repository) ([]gitctx.CommitInfo, error) {
		return taskRepo.RecentCommits(10)
	})
	previousHeadPaths := collectBestEffortWithRepo(&wg, root, (*gitctx.Repository).DiffAgainstParentPaths)
	previousHeadStats := collectBestEffortWithRepo(&wg, root, (*gitctx.Repository).DiffAgainstParentStat)
	previousHeadDiff := collectBestEffortWithRepo(&wg, root, func(taskRepo *gitctx.Repository) (boundedDiffResult, error) {
		text, truncated, err := taskRepo.DiffAgainstParent(24*1024, 700)
		return boundedDiffResult{Text: text, Truncated: truncated}, err
	})
	wg.Wait()

	if stagedPaths.err != nil {
		return PreparedCommitContext{}, stagedPaths.err
	}
	if stagedStatus.err != nil {
		return PreparedCommitContext{}, stagedStatus.err
	}
	if stagedStats.err != nil {
		return PreparedCommitContext{}, stagedStats.err
	}
	if stagedSubmodules.err != nil {
		return PreparedCommitContext{}, stagedSubmodules.err
	}
	if diff.err != nil {
		return PreparedCommitContext{}, diff.err
	}

	contextPack := collectWithRepo(&wg, root, func(taskRepo *gitctx.Repository) (contextpack.ContextPack, error) {
		return prepareContextPack(taskRepo, stagedPaths.value, stagedStatus.value, stagedStats.value), nil
	})
	previousHeadContextPack := collectWithRepo(&wg, root, func(taskRepo *gitctx.Repository) (contextpack.ContextPack, error) {
		return prepareRevisionContextPack(taskRepo, "HEAD", previousHeadPaths.value, previousHeadStats.value), nil
	})
	wg.Wait()
	if contextPack.err != nil {
		return PreparedCommitContext{}, contextPack.err
	}
	if previousHeadContextPack.err != nil {
		return PreparedCommitContext{}, previousHeadContextPack.err
	}

	focusDiff := collectWithRepo(&wg, root, func(taskRepo *gitctx.Repository) (focusDiffResult, error) {
		text, paths, truncated, err := prepareFocusDiff(taskRepo, contextPack.value, diff.value.Text, diff.value.Truncated)
		return focusDiffResult{Text: text, Paths: paths, Truncated: truncated}, err
	})
	outlierDiff := collectWithRepo(&wg, root, func(taskRepo *gitctx.Repository) (boundedDiffResult, error) {
		text, truncated, err := prepareOutlierDiff(taskRepo, contextPack.value)
		return boundedDiffResult{Text: text, Truncated: truncated}, err
	})
	wg.Wait()
	if focusDiff.err != nil {
		return PreparedCommitContext{}, focusDiff.err
	}
	if outlierDiff.err != nil {
		return PreparedCommitContext{}, outlierDiff.err
	}

	return PreparedCommitContext{
		Mode:                      ModeNormal,
		StagedPaths:               stagedPaths.value,
		StagedStatus:              stagedStatus.value,
		StagedStats:               stagedStats.value,
		StagedSubmodules:          stagedSubmodules.value,
		ContextPack:               contextPack.value,
		RecentCommits:             recentCommits.value,
		PreviousHeadPaths:         previousHeadPaths.value,
		PreviousHeadStats:         previousHeadStats.value,
		PreviousHeadContextPack:   previousHeadContextPack.value,
		PreviousHeadDiff:          previousHeadDiff.value.Text,
		PreviousHeadDiffTruncated: previousHeadDiff.value.Truncated,
		FocusDiff:                 focusDiff.value.Text,
		FocusDiffPaths:            focusDiff.value.Paths,
		FocusDiffTruncated:        focusDiff.value.Truncated,
		OutlierDiff:               outlierDiff.value.Text,
		OutlierDiffTruncated:      outlierDiff.value.Truncated,
		Diff:                      diff.value.Text,
		DiffTruncated:             diff.value.Truncated,
	}, nil
}

func PrepareAmendContext(repo *gitctx.Repository) (PreparedAmendContext, error) {
	var wg sync.WaitGroup
	root := repo.RootPath
	originalMessage := collectWithRepo(&wg, root, (*gitctx.Repository).HeadMessage)
	head := collectWithRepo(&wg, root, (*gitctx.Repository).HeadInfo)
	stagedPaths := collectWithRepo(&wg, root, (*gitctx.Repository).StagedPaths)
	stagedStatus := collectWithRepo(&wg, root, (*gitctx.Repository).StagedStatus)
	stagedStats := collectWithRepo(&wg, root, (*gitctx.Repository).StagedStat)
	stagedSubmodules := collectWithRepo(&wg, root, prepareStagedSubmodules)
	finalPaths := collectWithRepo(&wg, root, (*gitctx.Repository).FinalAmendedPaths)
	finalStats := collectWithRepo(&wg, root, (*gitctx.Repository).FinalAmendedStat)
	finalDiff := collectWithRepo(&wg, root, func(taskRepo *gitctx.Repository) (boundedDiffResult, error) {
		text, truncated, err := taskRepo.FinalAmendedDiff(48*1024, 1200)
		return boundedDiffResult{Text: text, Truncated: truncated}, err
	})
	headPaths := collectWithRepo(&wg, root, (*gitctx.Repository).DiffAgainstParentPaths)
	headStats := collectWithRepo(&wg, root, (*gitctx.Repository).DiffAgainstParentStat)
	headDiff := collectWithRepo(&wg, root, func(taskRepo *gitctx.Repository) (boundedDiffResult, error) {
		text, truncated, err := taskRepo.DiffAgainstParent(24*1024, 700)
		return boundedDiffResult{Text: text, Truncated: truncated}, err
	})
	amendDelta := collectWithRepo(&wg, root, func(taskRepo *gitctx.Repository) (boundedDiffResult, error) {
		text, truncated, err := taskRepo.AmendDelta(24*1024, 700)
		return boundedDiffResult{Text: text, Truncated: truncated}, err
	})
	recentCommits := collectBestEffortWithRepo(&wg, root, func(taskRepo *gitctx.Repository) ([]gitctx.CommitInfo, error) {
		return taskRepo.RecentCommits(10)
	})
	wg.Wait()

	if originalMessage.err != nil {
		return PreparedAmendContext{}, originalMessage.err
	}
	if head.err != nil {
		return PreparedAmendContext{}, head.err
	}
	if stagedPaths.err != nil {
		return PreparedAmendContext{}, stagedPaths.err
	}
	if stagedStatus.err != nil {
		return PreparedAmendContext{}, stagedStatus.err
	}
	if stagedStats.err != nil {
		return PreparedAmendContext{}, stagedStats.err
	}
	if stagedSubmodules.err != nil {
		return PreparedAmendContext{}, stagedSubmodules.err
	}
	if finalPaths.err != nil {
		return PreparedAmendContext{}, finalPaths.err
	}
	if finalStats.err != nil {
		return PreparedAmendContext{}, finalStats.err
	}
	if finalDiff.err != nil {
		return PreparedAmendContext{}, finalDiff.err
	}
	if headPaths.err != nil {
		return PreparedAmendContext{}, headPaths.err
	}
	if headStats.err != nil {
		return PreparedAmendContext{}, headStats.err
	}
	if headDiff.err != nil {
		return PreparedAmendContext{}, headDiff.err
	}
	if amendDelta.err != nil {
		return PreparedAmendContext{}, amendDelta.err
	}

	finalContextPack := collectWithRepo(&wg, root, func(taskRepo *gitctx.Repository) (contextpack.ContextPack, error) {
		return prepareIndexContextPack(taskRepo, finalPaths.value, finalStats.value, "final"), nil
	})
	headContextPack := collectWithRepo(&wg, root, func(taskRepo *gitctx.Repository) (contextpack.ContextPack, error) {
		return prepareRevisionContextPack(taskRepo, "HEAD", headPaths.value, headStats.value), nil
	})
	stagedContextPack := collectWithRepo(&wg, root, func(taskRepo *gitctx.Repository) (contextpack.ContextPack, error) {
		return prepareContextPack(taskRepo, stagedPaths.value, stagedStatus.value, stagedStats.value), nil
	})
	wg.Wait()
	if finalContextPack.err != nil {
		return PreparedAmendContext{}, finalContextPack.err
	}
	if headContextPack.err != nil {
		return PreparedAmendContext{}, headContextPack.err
	}
	if stagedContextPack.err != nil {
		return PreparedAmendContext{}, stagedContextPack.err
	}

	return PreparedAmendContext{
		Mode:                ModeAmend,
		OriginalHeadMessage: strings.TrimSpace(originalMessage.value),
		Head:                head.value,
		RecentCommits:       recentCommits.value,
		FinalPaths:          finalPaths.value,
		FinalStats:          finalStats.value,
		FinalContextPack:    finalContextPack.value,
		FinalDiff:           finalDiff.value.Text,
		FinalDiffTruncated:  finalDiff.value.Truncated,
		HeadPaths:           headPaths.value,
		HeadStats:           headStats.value,
		HeadContextPack:     headContextPack.value,
		HeadDiff:            headDiff.value.Text,
		HeadDiffTruncated:   headDiff.value.Truncated,
		StagedPaths:         stagedPaths.value,
		StagedStatus:        stagedStatus.value,
		StagedStats:         stagedStats.value,
		StagedSubmodules:    stagedSubmodules.value,
		StagedContextPack:   stagedContextPack.value,
		AmendDelta:          amendDelta.value.Text,
		AmendDeltaTruncated: amendDelta.value.Truncated,
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
	data, err := sonic.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"mode":%q}`, c.Mode)
	}
	return string(data)
}

func (c PreparedAmendContext) Render() string {
	data, err := sonic.MarshalIndent(c, "", "  ")
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
	data, err := sonic.MarshalIndent(view, "", "  ")
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
	return executeUserPrompt(userPreparedCommitPromptTemplate, userPromptData{
		MaxSteps:        maxSteps,
		MaxToolCalls:    maxToolCalls,
		PreparedContext: prepared.RenderForPrompt(),
	})
}

func UserPromptWithPreparedAmendContext(prepared PreparedAmendContext, maxSteps, maxToolCalls int) string {
	return executeUserPrompt(userPreparedAmendPromptTemplate, userPromptData{
		MaxSteps:        maxSteps,
		MaxToolCalls:    maxToolCalls,
		PreparedContext: prepared.RenderForPrompt(),
	})
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
		commits, err := repo.SubmoduleCommits(change.Path, change.Old, change.New, 50)
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
	data, err := sonic.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"range":%q}`, c.Range)
	}
	return string(data)
}

func UserPromptWithPreparedPRContext(prepared PreparedPRContext, maxSteps, maxToolCalls int) string {
	return executeUserPrompt(userPreparedPRPromptTemplate, userPromptData{
		MaxSteps:        maxSteps,
		MaxToolCalls:    maxToolCalls,
		PreparedContext: prepared.Render(),
	})
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

func ValidateWithPreparedCommitContext(prepared PreparedCommitContext, output string) []string {
	errs := Validate(ModeNormal, output)
	return append(errs, validateStagedSubmoduleSummaries(prepared.StagedSubmodules, output)...)
}

func validateStagedSubmoduleSummaries(submodules []PreparedSubmodule, output string) []string {
	expected := stagedSubmoduleCommitSummaries(submodules)
	if len(expected) == 0 {
		return nil
	}

	normalizedOutput := normalizeSummaryText(output)
	var missing []string
	for _, summary := range expected {
		if !strings.Contains(normalizedOutput, normalizeSummaryText(summary)) {
			missing = append(missing, summary)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("commit message must include staged submodule commit summaries: %s", strings.Join(missing, "; "))}
}

func stagedSubmoduleCommitSummaries(submodules []PreparedSubmodule) []string {
	var summaries []string
	for _, submodule := range submodules {
		for _, commit := range submodule.Commits {
			summary := strings.TrimSpace(commit.Summary)
			if summary != "" {
				summaries = append(summaries, summary)
			}
		}
	}
	return summaries
}

func normalizeSummaryText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
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
