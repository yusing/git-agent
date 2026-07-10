package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/skills"
	"github.com/yusing/git-agent/internal/textutil"
)

type Definition struct {
	Name        string
	Description string
	Schema      map[string]any
	Strict      bool
}

type Invocation struct {
	Name      string
	Arguments string
}

type Result struct {
	Content   string
	Truncated bool
}

type Tool interface {
	Definition() Definition
	Execute(context.Context, Invocation) (Result, error)
}

type Registry struct {
	tools map[string]Tool
}

const SkillReadToolName = "skills_read"

func NewRegistryWithSkills(repo *gitctx.Repository, skillStore *skills.Store) *Registry {
	registry := &Registry{tools: map[string]Tool{}}
	for _, tool := range []Tool{
		repoSummaryTool{repo: repo},
		listFilesTool{repo: repo},
		readFileTool{repo: repo},
		searchFilesTool{repo: repo},
		gitStagedPathsTool{repo: repo},
		gitStagedStatusTool{repo: repo},
		gitStagedStatTool{repo: repo},
		gitStagedDiffTool{repo: repo},
		gitStagedDiffForPathsTool{repo: repo},
		gitRecentCommitsTool{repo: repo},
		gitHeadShowTool{repo: repo},
		gitDiffAgainstParentTool{repo: repo},
		gitFinalAmendedDiffTool{repo: repo},
		gitAmendDeltaTool{repo: repo},
		gitShowFileAtRevTool{repo: repo},
		gitPRBaseTool{repo: repo},
		gitPRPathsTool{repo: repo},
		gitPRStatTool{repo: repo},
		gitPRDiffTool{repo: repo},
		gitPRCommitsTool{repo: repo},
		resolveRefTool{repo: repo},
		gitLogRangeTool{repo: repo},
		gitmodulesTableTool{repo: repo},
		submoduleGitlinkRangeTool{repo: repo},
		submoduleLogRangeTool{repo: repo},
		repoKindTool{repo: repo},
	} {
		registry.tools[tool.Definition().Name] = tool
	}
	if skillStore.Len() == 0 {
		return registry
	}
	tool := skillsReadTool{store: skillStore}
	registry.tools[tool.Definition().Name] = tool
	return registry
}

func (r *Registry) Definitions(names []string) []Definition {
	defs := make([]Definition, 0, len(names))
	for _, name := range names {
		if tool, ok := r.tools[name]; ok {
			defs = append(defs, tool.Definition())
		}
	}
	return defs
}

func (r *Registry) Execute(ctx context.Context, invocation Invocation) (Result, error) {
	tool, ok := r.tools[invocation.Name]
	if !ok {
		return Result{}, fmt.Errorf("tool %q is not registered", invocation.Name)
	}
	return tool.Execute(ctx, invocation)
}

func CommitMessageToolNames() []string {
	return []string{
		"repo_summary",
		"list_files",
		"read_file",
		"search_files",
		"git_staged_paths",
		"git_staged_status",
		"git_staged_stat",
		"git_staged_diff",
		"git_staged_diff_for_paths",
		"git_recent_commits",
		"git_head_show",
		"git_diff_against_parent",
		"git_final_amended_diff",
		"git_amend_delta",
		"git_show_file_at_rev",
	}
}

func SkillToolNames() []string {
	return []string{SkillReadToolName}
}

const (
	defaultMaxBytes = 32 * 1024
	defaultMaxLines = 800

	deprecatedReleaseNoteToolPrefix = "Deprecated: release-note generation now precomputes this evidence in Go and no longer exposes this tool to the model. "
)

var skippedDirs = map[string]bool{
	".git":       true,
	".git-agent": true,
	".omx":       true,
}

type repoTool struct {
	repo *gitctx.Repository
}

func schema(properties map[string]any, required ...string) map[string]any {
	requiredSet := make(map[string]struct{}, len(properties))
	for _, name := range required {
		requiredSet[name] = struct{}{}
	}
	for name := range properties {
		requiredSet[name] = struct{}{}
	}
	requiredList := make([]string, 0, len(requiredSet))
	for name := range requiredSet {
		requiredList = append(requiredList, name)
	}
	sort.Strings(requiredList)

	result := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
		"required":             requiredList,
	}
	return result
}

func jsonResult(tool string, value any, truncated bool) (Result, error) {
	envelope := map[string]any{
		"ok":        true,
		"tool":      tool,
		"data":      value,
		"truncated": truncated,
	}
	data, err := sonic.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return Result{}, err
	}
	return Result{Content: string(data), Truncated: truncated}, nil
}

func parseArgs[T any](raw string) (T, error) {
	var value T
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "\uFEFF")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return value, nil
	}
	err := sonic.UnmarshalString(raw, &value)
	if err != nil {
		return value, fmt.Errorf("invalid tool arguments %q: %w", raw, err)
	}
	return value, nil
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func stringArrayProp(description string) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": description,
		"items":       map[string]any{"type": "string"},
	}
}

func intProp(description string, min, max int) map[string]any {
	prop := map[string]any{"type": "integer", "description": description}
	if min > 0 {
		prop["minimum"] = min
	}
	if max > 0 {
		prop["maximum"] = max
	}
	return prop
}

func emptySchema() map[string]any {
	return schema(map[string]any{})
}

func cappedSchema() map[string]any {
	return schema(map[string]any{
		"max_bytes": intProp("Maximum bytes to return.", 1, 65536),
		"max_lines": intProp("Maximum lines to return.", 1, 2000),
	})
}

type repoSummaryTool repoTool

func (t repoSummaryTool) Definition() Definition {
	return Definition{Name: "repo_summary", Description: "Return current repository metadata.", Schema: emptySchema(), Strict: true}
}

func (t repoSummaryTool) Execute(context.Context, Invocation) (Result, error) {
	return jsonResult("repo_summary", t.repo.Summary(), false)
}

type listFilesTool repoTool

func (t listFilesTool) Definition() Definition {
	return Definition{Name: "list_files", Description: "List repository files under an optional directory prefix.", Schema: schema(map[string]any{
		"path":        stringProp("Repository-relative directory prefix."),
		"max_entries": intProp("Maximum file entries to return.", 1, 1000),
	}), Strict: true}
}

func (t listFilesTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Path       string `json:"path"`
		MaxEntries int    `json:"max_entries"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	maxEntries := args.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 200
	}
	root, err := safePath(t.repo.RootPath, args.Path)
	if err != nil {
		return Result{}, err
	}
	var files []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && skippedDirs[d.Name()] {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(t.repo.RootPath, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	slices.Sort(files)
	truncated := false
	if len(files) > maxEntries {
		files = files[:maxEntries]
		truncated = true
	}
	return jsonResult("list_files", map[string]any{"files": files}, truncated)
}

type readFileTool repoTool

func (t readFileTool) Definition() Definition {
	return Definition{Name: "read_file", Description: "Read a UTF-8 repository file with byte and line caps.", Schema: schema(map[string]any{
		"path":      stringProp("Repository-relative file path."),
		"max_bytes": intProp("Maximum bytes to return.", 1, 65536),
		"max_lines": intProp("Maximum lines to return.", 1, 2000),
	}, "path"), Strict: true}
}

func (t readFileTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Path     string `json:"path"`
		MaxBytes int    `json:"max_bytes"`
		MaxLines int    `json:"max_lines"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	if args.Path == "" {
		return Result{}, fmt.Errorf("path is required")
	}
	path, err := safePath(t.repo.RootPath, args.Path)
	if err != nil {
		return Result{}, err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	maxBytes, maxLines := normalizeCaps(args.MaxBytes, args.MaxLines)
	limited, truncated := textutil.Limit(string(content), maxBytes, maxLines)
	return jsonResult("read_file", map[string]any{"path": args.Path, "content": limited}, truncated)
}

type skillsReadTool struct {
	store *skills.Store
}

func (t skillsReadTool) Definition() Definition {
	locators := make([]string, 0, t.store.Len())
	for _, skill := range t.store.Skills() {
		locators = append(locators, skill.Locator)
	}
	return Definition{Name: SkillReadToolName, Description: "Read SKILL.md or a text file under references/ for a discovered skill root.", Schema: skillsReadSchema(locators), Strict: true}
}

func (t skillsReadTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		SourceLocator string `json:"source_locator"`
		Path          string `json:"path"`
		MaxBytes      int    `json:"max_bytes"`
		MaxLines      int    `json:"max_lines"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	skill, ok := t.store.Lookup(args.SourceLocator)
	if !ok {
		return Result{}, fmt.Errorf("unknown skill source locator: %s", args.SourceLocator)
	}
	rel, ok := cleanSkillReadPath(args.Path)
	if !ok {
		return Result{}, fmt.Errorf("skill path is not readable by this tool: %s", args.Path)
	}
	file, err := openSkillReadFile(skill, rel)
	if err != nil {
		return Result{}, err
	}
	defer file.Close()
	maxBytes, maxLines := normalizeCaps(args.MaxBytes, args.MaxLines)
	content, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return Result{}, err
	}
	if bytes.Contains(content, []byte{0}) {
		return Result{}, fmt.Errorf("skill path is not a text file: %s", rel)
	}
	limited, truncated := textutil.Limit(string(content), maxBytes, maxLines)
	if len(content) > maxBytes {
		truncated = true
	}
	return jsonResult(SkillReadToolName, map[string]any{"source_locator": args.SourceLocator, "path": rel, "content": limited}, truncated)
}

func openSkillReadFile(skill skills.Skill, rel string) (*os.File, error) {
	rootPath := skill.Root
	openPath := rel
	if strings.HasPrefix(rel, "references/") {
		rootPath = filepath.Join(skill.Root, "references")
		openPath = strings.TrimPrefix(rel, "references/")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	file, err := root.Open(openPath)
	if err != nil {
		root.Close()
		return nil, err
	}
	if err := root.Close(); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func skillsReadSchema(locators []string) map[string]any {
	return schema(map[string]any{
		"source_locator": map[string]any{
			"type":        "string",
			"description": "Exact source locator from the initial Skills section.",
			"enum":        locators,
		},
		"path":      stringProp(`Relative path under the selected skill root. Use "SKILL.md" for the skill body; only files under references/ are readable otherwise.`),
		"max_bytes": intProp("Maximum bytes to return.", 1, 65536),
		"max_lines": intProp("Maximum lines to return.", 1, 2000),
	}, "source_locator", "path", "max_bytes", "max_lines")
}

func cleanSkillReadPath(rel string) (string, bool) {
	if strings.TrimSpace(rel) == "" {
		return "SKILL.md", true
	}
	for part := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		if part == ".." {
			return "", false
		}
	}
	cleaned := filepath.ToSlash(filepath.Clean(rel))
	if cleaned == "SKILL.md" || strings.HasPrefix(cleaned, "references/") {
		return cleaned, true
	}
	return "", false
}

type searchFilesTool repoTool

func (t searchFilesTool) Definition() Definition {
	return Definition{Name: "search_files", Description: "Search repository files for a literal pattern.", Schema: schema(map[string]any{
		"pattern":     stringProp("Literal search string."),
		"path":        stringProp("Repository-relative directory prefix."),
		"max_matches": intProp("Maximum matches to return.", 1, 1000),
	}, "pattern"), Strict: true}
}

func (t searchFilesTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		MaxMatches int    `json:"max_matches"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	if args.Pattern == "" {
		return Result{}, fmt.Errorf("pattern is required")
	}
	maxMatches := args.MaxMatches
	if maxMatches <= 0 {
		maxMatches = 100
	}
	root, err := safePath(t.repo.RootPath, args.Path)
	if err != nil {
		return Result{}, err
	}
	var matches []map[string]any
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && skippedDirs[d.Name()] {
			return filepath.SkipDir
		}
		if d.IsDir() || len(matches) >= maxMatches {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(t.repo.RootPath, path)
		lineNo := 0
		for line := range strings.SplitSeq(string(content), "\n") {
			lineNo++
			if strings.Contains(line, args.Pattern) {
				matches = append(matches, map[string]any{"path": filepath.ToSlash(rel), "line": lineNo, "text": line})
				if len(matches) >= maxMatches {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	truncated := len(matches) >= maxMatches
	return jsonResult("search_files", map[string]any{"matches": matches}, truncated)
}

type gitStagedPathsTool repoTool

func (t gitStagedPathsTool) Definition() Definition {
	return Definition{Name: "git_staged_paths", Description: "List paths with staged changes.", Schema: emptySchema(), Strict: true}
}

func (t gitStagedPathsTool) Execute(context.Context, Invocation) (Result, error) {
	paths, err := t.repo.StagedPaths()
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_staged_paths", map[string]any{"paths": paths}, false)
}

type gitStagedStatusTool repoTool

func (t gitStagedStatusTool) Definition() Definition {
	return Definition{Name: "git_staged_status", Description: "Return staged and worktree status for changed paths.", Schema: emptySchema(), Strict: true}
}

func (t gitStagedStatusTool) Execute(context.Context, Invocation) (Result, error) {
	status, err := t.repo.StagedStatus()
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_staged_status", status, false)
}

type gitStagedStatTool repoTool

func (t gitStagedStatTool) Definition() Definition {
	return Definition{Name: "git_staged_stat", Description: "Return staged diff line stats by file.", Schema: emptySchema(), Strict: true}
}

func (t gitStagedStatTool) Execute(context.Context, Invocation) (Result, error) {
	stats, err := t.repo.StagedStat()
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_staged_stat", stats, false)
}

type gitStagedDiffTool repoTool

func (t gitStagedDiffTool) Definition() Definition {
	return Definition{Name: "git_staged_diff", Description: "Return staged diff versus HEAD with caps.", Schema: cappedSchema(), Strict: true}
}

func (t gitStagedDiffTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[capArgs](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	text, truncated, err := t.repo.StagedDiff(normalizeCaps(args.MaxBytes, args.MaxLines))
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_staged_diff", map[string]any{"diff": text}, truncated)
}

type gitStagedDiffForPathsTool repoTool

func (t gitStagedDiffForPathsTool) Definition() Definition {
	return Definition{Name: "git_staged_diff_for_paths", Description: "Return staged diff versus HEAD for selected repository-relative paths with caps.", Schema: schema(map[string]any{
		"paths":     stringArrayProp("Repository-relative staged paths to include."),
		"max_bytes": intProp("Maximum bytes to return.", 1, 65536),
		"max_lines": intProp("Maximum lines to return.", 1, 2000),
	}, "paths"), Strict: true}
}

func (t gitStagedDiffForPathsTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Paths    []string `json:"paths"`
		MaxBytes int      `json:"max_bytes"`
		MaxLines int      `json:"max_lines"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	if len(args.Paths) == 0 {
		return Result{}, fmt.Errorf("paths is required")
	}
	maxBytes, maxLines := normalizeCaps(args.MaxBytes, args.MaxLines)
	text, truncated, err := t.repo.StagedDiffForPaths(args.Paths, maxBytes, maxLines)
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_staged_diff_for_paths", map[string]any{"paths": args.Paths, "diff": text}, truncated)
}

type gitRecentCommitsTool repoTool

func (t gitRecentCommitsTool) Definition() Definition {
	return Definition{Name: "git_recent_commits", Description: "Return recent commits for style reference.", Schema: schema(map[string]any{
		"limit": intProp("Maximum commits to return.", 1, 100),
	}), Strict: true}
}

func (t gitRecentCommitsTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Limit int `json:"limit"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	if args.Limit <= 0 {
		args.Limit = 10
	}
	commits, err := t.repo.RecentCommits(args.Limit)
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_recent_commits", commits, false)
}

type gitHeadShowTool repoTool

func (t gitHeadShowTool) Definition() Definition {
	return Definition{Name: "git_head_show", Description: "Return HEAD commit metadata and patch.", Schema: cappedSchema(), Strict: true}
}

func (t gitHeadShowTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[capArgs](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	text, truncated, err := t.repo.HeadShow(normalizeCaps(args.MaxBytes, args.MaxLines))
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_head_show", map[string]any{"show": text}, truncated)
}

type gitDiffAgainstParentTool repoTool

func (t gitDiffAgainstParentTool) Definition() Definition {
	return Definition{Name: "git_diff_against_parent", Description: "Return HEAD diff versus first parent.", Schema: cappedSchema(), Strict: true}
}

func (t gitDiffAgainstParentTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[capArgs](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	text, truncated, err := t.repo.DiffAgainstParent(normalizeCaps(args.MaxBytes, args.MaxLines))
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_diff_against_parent", map[string]any{"diff": text}, truncated)
}

type gitFinalAmendedDiffTool repoTool

func (t gitFinalAmendedDiffTool) Definition() Definition {
	return Definition{Name: "git_final_amended_diff", Description: "Return the final amended commit diff versus its first parent, overlaying staged changes on HEAD.", Schema: cappedSchema(), Strict: true}
}

func (t gitFinalAmendedDiffTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[capArgs](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	text, truncated, err := t.repo.FinalAmendedDiff(normalizeCaps(args.MaxBytes, args.MaxLines))
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_final_amended_diff", map[string]any{"diff": text}, truncated)
}

type gitAmendDeltaTool repoTool

func (t gitAmendDeltaTool) Definition() Definition {
	return Definition{Name: "git_amend_delta", Description: "Return staged-vs-HEAD diagnostic diff for amend mode.", Schema: cappedSchema(), Strict: true}
}

func (t gitAmendDeltaTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[capArgs](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	text, truncated, err := t.repo.AmendDelta(normalizeCaps(args.MaxBytes, args.MaxLines))
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_amend_delta", map[string]any{"diff": text}, truncated)
}

type gitShowFileAtRevTool repoTool

func (t gitShowFileAtRevTool) Definition() Definition {
	return Definition{Name: "git_show_file_at_rev", Description: "Read a file from a commit-ish.", Schema: schema(map[string]any{
		"rev":       stringProp("Commit-ish revision to read from."),
		"path":      stringProp("Repository-relative file path."),
		"max_bytes": intProp("Maximum bytes to return.", 1, 65536),
		"max_lines": intProp("Maximum lines to return.", 1, 2000),
	}, "rev", "path"), Strict: true}
}

func (t gitShowFileAtRevTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Rev      string `json:"rev"`
		Path     string `json:"path"`
		MaxBytes int    `json:"max_bytes"`
		MaxLines int    `json:"max_lines"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	maxBytes, maxLines := normalizeCaps(args.MaxBytes, args.MaxLines)
	text, truncated, err := t.repo.ShowFileAtRev(args.Rev, args.Path, maxBytes, maxLines)
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_show_file_at_rev", map[string]any{"content": text}, truncated)
}

type gitPRBaseTool repoTool

func (t gitPRBaseTool) Definition() Definition {
	return Definition{Name: "git_pr_base", Description: "Return origin/HEAD base and current HEAD metadata for pr-message generation.", Schema: emptySchema(), Strict: true}
}

func (t gitPRBaseTool) Execute(context.Context, Invocation) (Result, error) {
	base, err := t.repo.PullRequestBase()
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_pr_base", map[string]any{
		"base_ref": gitctx.PullRequestBaseRef,
		"base":     base,
		"head_sha": t.repo.HeadSHA,
		"branch":   t.repo.Branch,
	}, false)
}

type gitPRPathsTool repoTool

func (t gitPRPathsTool) Definition() Definition {
	return Definition{Name: "git_pr_paths", Description: "List paths changed between origin/HEAD and current HEAD.", Schema: emptySchema(), Strict: true}
}

func (t gitPRPathsTool) Execute(context.Context, Invocation) (Result, error) {
	paths, err := t.repo.PullRequestPaths()
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_pr_paths", map[string]any{"paths": paths}, false)
}

type gitPRStatTool repoTool

func (t gitPRStatTool) Definition() Definition {
	return Definition{Name: "git_pr_stat", Description: "Return diff line stats for current HEAD versus origin/HEAD.", Schema: emptySchema(), Strict: true}
}

func (t gitPRStatTool) Execute(context.Context, Invocation) (Result, error) {
	stats, err := t.repo.PullRequestStat()
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_pr_stat", stats, false)
}

type gitPRDiffTool repoTool

func (t gitPRDiffTool) Definition() Definition {
	return Definition{Name: "git_pr_diff", Description: "Return current HEAD diff versus origin/HEAD with caps.", Schema: cappedSchema(), Strict: true}
}

func (t gitPRDiffTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[capArgs](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	text, truncated, err := t.repo.PullRequestDiff(normalizeCaps(args.MaxBytes, args.MaxLines))
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_pr_diff", map[string]any{"diff": text}, truncated)
}

type gitPRCommitsTool repoTool

func (t gitPRCommitsTool) Definition() Definition {
	return Definition{Name: "git_pr_commits", Description: "Return commits reachable from current HEAD, stopping before origin/HEAD.", Schema: schema(map[string]any{
		"limit": intProp("Maximum commits to return.", 1, 100),
	}), Strict: true}
}

func (t gitPRCommitsTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Limit int `json:"limit"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	if args.Limit <= 0 {
		args.Limit = 20
	}
	commits, err := t.repo.PullRequestCommits(args.Limit)
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_pr_commits", commits, len(commits) >= args.Limit)
}

type resolveRefTool repoTool

func (t resolveRefTool) Definition() Definition {
	return Definition{Name: "resolve_ref", Description: deprecatedReleaseNoteToolPrefix + "Resolve a ref to a commit hash.", Schema: schema(map[string]any{
		"ref": stringProp("Git revision/ref/tag name."),
	}, "ref"), Strict: true}
}

func (t resolveRefTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Ref string `json:"ref"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	hash, err := t.repo.ResolveRef(args.Ref)
	if err != nil {
		return Result{}, err
	}
	return jsonResult("resolve_ref", map[string]string{"ref": args.Ref, "sha": hash}, false)
}

type gitLogRangeTool repoTool

func (t gitLogRangeTool) Definition() Definition {
	return Definition{Name: "git_log_range", Description: deprecatedReleaseNoteToolPrefix + "Return commits reachable from release and stopping before base.", Schema: schema(map[string]any{
		"base":    stringProp("Base ref excluded from the range."),
		"release": stringProp("Release ref included as range tip."),
		"limit":   intProp("Maximum commits to return.", 1, 500),
	}, "base", "release"), Strict: true}
}

func (t gitLogRangeTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Base    string `json:"base"`
		Release string `json:"release"`
		Limit   int    `json:"limit"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	if args.Limit <= 0 {
		args.Limit = 200
	}
	commits, err := t.repo.LogFrom(args.Base, args.Release, args.Limit)
	if err != nil {
		return Result{}, err
	}
	return jsonResult("git_log_range", commits, len(commits) >= args.Limit)
}

type gitmodulesTableTool repoTool

func (t gitmodulesTableTool) Definition() Definition {
	return Definition{Name: "gitmodules_table", Description: deprecatedReleaseNoteToolPrefix + "Read .gitmodules if present.", Schema: emptySchema(), Strict: true}
}

func (t gitmodulesTableTool) Execute(context.Context, Invocation) (Result, error) {
	content, err := os.ReadFile(filepath.Join(t.repo.RootPath, ".gitmodules"))
	if os.IsNotExist(err) {
		return jsonResult("gitmodules_table", map[string]any{"present": false}, false)
	}
	if err != nil {
		return Result{}, err
	}
	limited, truncated := textutil.Limit(string(content), defaultMaxBytes, defaultMaxLines)
	return jsonResult("gitmodules_table", map[string]any{"present": true, "content": limited}, truncated)
}

type submoduleGitlinkRangeTool repoTool

func (t submoduleGitlinkRangeTool) Definition() Definition {
	return Definition{Name: "submodule_gitlink_range", Description: deprecatedReleaseNoteToolPrefix + "Return changed submodule gitlinks between two refs.", Schema: schema(map[string]any{
		"base":    stringProp("Base ref excluded from the range."),
		"release": stringProp("Release ref included as range tip."),
	}, "base", "release"), Strict: true}
}

func (t submoduleGitlinkRangeTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Base    string `json:"base"`
		Release string `json:"release"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	changes, err := t.repo.SubmoduleGitlinkRange(args.Base, args.Release)
	if err != nil {
		return Result{}, err
	}
	return jsonResult("submodule_gitlink_range", changes, false)
}

type submoduleLogRangeTool repoTool

func (t submoduleLogRangeTool) Definition() Definition {
	return Definition{Name: "submodule_log_range", Description: deprecatedReleaseNoteToolPrefix + "Return submodule commits when local checkout is available.", Schema: schema(map[string]any{
		"path":    stringProp("Repository-relative submodule path."),
		"base":    stringProp("Base submodule commit/ref excluded from range."),
		"release": stringProp("Release submodule commit/ref included as range tip."),
		"limit":   intProp("Maximum commits to return.", 1, 500),
	}, "path", "base", "release"), Strict: true}
}

func (t submoduleLogRangeTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Path    string `json:"path"`
		Base    string `json:"base"`
		Release string `json:"release"`
		Limit   int    `json:"limit"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	path, err := safePath(t.repo.RootPath, args.Path)
	if err != nil {
		return Result{}, err
	}
	sub, err := gitctx.Open(path)
	if err != nil {
		return jsonResult("submodule_log_range", map[string]any{"available": false, "error": err.Error()}, false)
	}
	if args.Limit <= 0 {
		args.Limit = 100
	}
	commits, err := sub.LogFrom(args.Base, args.Release, args.Limit)
	if err != nil {
		return Result{}, err
	}
	return jsonResult("submodule_log_range", map[string]any{"available": true, "commits": commits}, len(commits) >= args.Limit)
}

type repoKindTool repoTool

func (t repoKindTool) Definition() Definition {
	return Definition{Name: "repo_kind", Description: deprecatedReleaseNoteToolPrefix + "Return coarse repository kind.", Schema: emptySchema(), Strict: true}
}

func (t repoKindTool) Execute(context.Context, Invocation) (Result, error) {
	return jsonResult("repo_kind", map[string]string{"kind": t.repo.RepoKind()}, false)
}

type capArgs struct {
	MaxBytes int `json:"max_bytes"`
	MaxLines int `json:"max_lines"`
}

func normalizeCaps(maxBytes, maxLines int) (int, int) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if maxLines <= 0 {
		maxLines = defaultMaxLines
	}
	return maxBytes, maxLines
}

func safePath(root, rel string) (string, error) {
	if rel == "" {
		return root, nil
	}
	cleaned := filepath.Clean(rel)
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("absolute paths are not allowed: %s", rel)
	}
	path := filepath.Join(root, cleaned)
	resolvedRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolvedPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if !pathInsideRoot(resolvedRoot, resolvedPath) {
		return "", fmt.Errorf("path escapes repository: %s", rel)
	}
	return resolvedPath, nil
}

func pathInsideRoot(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}
