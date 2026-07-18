package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/bytedance/sonic"
	"github.com/go-git/go-git/v6/plumbing/format/index"
	"github.com/yusing/git-agent/internal/doccmd"
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
	tools       map[string]Tool
	reviewGuard *reviewStateGuard
}

type reviewStateGuard struct {
	repo        *gitctx.Repository
	mode        ReviewMode
	fingerprint gitctx.ChangeFingerprint
}

const SkillReadToolName = "skills_read"

type ReviewMode string

const (
	ReviewModeCodebase    ReviewMode = "codebase"
	ReviewModeUncommitted ReviewMode = "uncommitted"
	ReviewModeStaged      ReviewMode = "staged"
)

type ReviewChange struct {
	Path     string `json:"path"`
	Staging  string `json:"staging"`
	Worktree string `json:"worktree,omitempty"`
	Adds     int    `json:"adds"`
	Deletes  int    `json:"deletes"`
	IsBinary bool   `json:"is_binary,omitempty"`
}

type ReviewScope struct {
	Changes []ReviewChange
}

func NewReviewScope(paths []string, status []gitctx.PathChange, stats []gitctx.FileStat) ReviewScope {
	statusByPath := make(map[string]gitctx.PathChange, len(status))
	for _, change := range status {
		statusByPath[change.Path] = change
	}
	statsByPath := make(map[string]gitctx.FileStat, len(stats))
	for _, stat := range stats {
		statsByPath[stat.Path] = stat
	}
	changes := make([]ReviewChange, 0, len(paths))
	for _, path := range paths {
		status := statusByPath[path]
		stat := statsByPath[path]
		changes = append(changes, ReviewChange{
			Path: path, Staging: status.Staging, Worktree: status.Worktree,
			Adds: stat.Adds, Deletes: stat.Deletes, IsBinary: stat.IsBinary,
		})
	}
	return ReviewScope{Changes: changes}
}

func NewRegistryWithSkills(repo *gitctx.Repository, skillStore *skills.Store) *Registry {
	return newRegistry(repo, skillStore)
}

func NewReviewRegistryWithSkills(repo *gitctx.Repository, skillStore *skills.Store, mode ReviewMode, scope ReviewScope, fingerprint gitctx.ChangeFingerprint, manifests ...*OrchestrationManifest) *Registry {
	registry := &Registry{tools: map[string]Tool{}}
	if repo != nil && mode != ReviewModeCodebase {
		registry.reviewGuard = &reviewStateGuard{repo: repo, mode: mode, fingerprint: fingerprint}
	}
	register(registry, []Tool{
		repoSummaryTool{repo: repo},
		listFilesTool{repo: repo, mode: mode},
		readFileTool{repo: repo, mode: mode},
		inspectFileTool{repo: repo, mode: mode},
		jqTool{repo: repo, mode: mode},
		grepTool{repo: repo, mode: mode},
		findTool{repo: repo, mode: mode},
	})
	if mode != ReviewModeCodebase {
		register(registry, []Tool{
			reviewChangesTool{mode: mode, scope: scope},
			reviewDiffTool{repo: repo, mode: mode},
			reviewDiffForPathsTool{repo: repo, mode: mode},
		})
	}
	registerSkillsRead(registry, skillStore)
	if len(manifests) == 1 && manifests[0] != nil {
		register(registry, []Tool{orchestrationArtifactTool{manifest: manifests[0]}})
	}
	root := "."
	if repo != nil {
		root = repo.RootPath
	}
	registerDocumentation(registry, doccmd.Discover(root))
	return registry
}

func newRegistry(repo *gitctx.Repository, skillStore *skills.Store) *Registry {
	registry := &Registry{tools: map[string]Tool{}}
	register(registry, []Tool{
		repoSummaryTool{repo: repo},
		listFilesTool{repo: repo},
		readFileTool{repo: repo},
		inspectFileTool{repo: repo},
		grepTool{repo: repo},
		findTool{repo: repo},
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
	})
	registerSkillsRead(registry, skillStore)
	return registry
}

func register(registry *Registry, tools []Tool) {
	for _, tool := range tools {
		registry.tools[tool.Definition().Name] = tool
	}
}

func registerSkillsRead(registry *Registry, skillStore *skills.Store) {
	if skillStore.Len() == 0 {
		return
	}
	tool := skillsReadTool{store: skillStore}
	registry.tools[tool.Definition().Name] = tool
}

func registerDocumentation(registry *Registry, commands *doccmd.Commands) {
	for _, tool := range documentationTools(commands) {
		registry.tools[tool.Definition().Name] = tool
	}
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
	if r.reviewGuard != nil && invocation.Name != SkillReadToolName {
		if err := r.reviewGuard.check(); err != nil {
			return Result{}, err
		}
	}
	return tool.Execute(ctx, invocation)
}

func (g *reviewStateGuard) check() error {
	switch g.mode {
	case ReviewModeStaged:
		return g.repo.CheckStagedFingerprint(g.fingerprint)
	case ReviewModeUncommitted:
		return g.repo.CheckUncommittedFingerprint(g.fingerprint)
	default:
		return nil
	}
}

func CommitMessageToolNames() []string {
	return []string{
		"repo_summary",
		"list_files",
		"read_file",
		"inspect_file",
		"grep",
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

func ReviewToolCandidates(mode ReviewMode) []string {
	names := []string{
		"repo_summary", "list_files", "read_file", "inspect_file", jqToolName, "grep", "find", OrchestrationArtifactToolName,
		string(doccmd.GoDoc), string(doccmd.RustDoc), string(doccmd.Context7Library), string(doccmd.Context7Docs),
	}
	if mode != ReviewModeCodebase {
		names = append(names, "review_changes", "review_diff", "review_diff_for_paths")
	}
	return names
}

func SkillToolNames() []string {
	return []string{SkillReadToolName}
}

const (
	defaultMaxBytes = 32 * 1024
	defaultMaxLines = 800
	maxErrorBytes   = 4 * 1024
	maxErrorLines   = 40

	deprecatedReleaseNoteToolPrefix = "Deprecated: release-note generation now precomputes this evidence in Go and no longer exposes this tool to the model. "
)

var skippedDirs = map[string]bool{
	".git":       true,
	".git-agent": true,
	".omx":       true,
}

func walkRepository(repo *gitctx.Repository, requested string, visit func(*os.Root, string, string, fs.DirEntry) error) error {
	walkRoot, err := cleanRepoPath(requested)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(repo.RootPath)
	if err != nil {
		return err
	}
	trackedFiles, trackedDirs, err := trackedRepositoryPaths(repo)
	if err != nil {
		_ = root.Close()
		return err
	}
	walkErr := fs.WalkDir(root.FS(), walkRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		path = filepath.ToSlash(path)
		protected := hasSkippedDir(path)
		if entry.IsDir() && protected && !trackedDirs[path] {
			return filepath.SkipDir
		}
		if !entry.IsDir() && protected && !trackedFiles[path] {
			return nil
		}
		return visit(root, walkRoot, path, entry)
	})
	return errors.Join(walkErr, root.Close())
}

func trackedRepositoryPaths(repo *gitctx.Repository) (map[string]bool, map[string]bool, error) {
	idx, err := repo.Repo.Storer.Index()
	if err != nil {
		return nil, nil, err
	}
	files := make(map[string]bool, len(idx.Entries))
	dirs := map[string]bool{".": true}
	for _, entry := range idx.Entries {
		path := filepath.ToSlash(entry.Name)
		files[path] = true
		for dir := pathpkg.Dir(path); dir != "."; dir = pathpkg.Dir(dir) {
			dirs[dir] = true
		}
	}
	return files, dirs, nil
}

func hasSkippedDir(path string) bool {
	for part := range strings.SplitSeq(filepath.ToSlash(path), "/") {
		if skippedDirs[part] {
			return true
		}
	}
	return false
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
	return marshalResultEnvelope(map[string]any{
		"ok":        true,
		"tool":      tool,
		"data":      value,
		"truncated": truncated,
	}, truncated)
}

// ErrorResult returns the stable tool-output envelope used when the model can
// correct a failed invocation and continue the agent loop.
func ErrorResult(tool string, err error) (Result, error) {
	message, truncated := textutil.Limit(err.Error(), maxErrorBytes, maxErrorLines)
	return marshalResultEnvelope(map[string]any{
		"ok":        false,
		"tool":      tool,
		"error":     message,
		"truncated": truncated,
	}, truncated)
}

func marshalResultEnvelope(envelope map[string]any, truncated bool) (Result, error) {
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

type listFilesTool struct {
	repo *gitctx.Repository
	mode ReviewMode
}

var errListFilesComplete = errors.New("list_files entry cap reached")

func stagedIndexEntries(repo *gitctx.Repository, requested string) ([]*index.Entry, error) {
	root, err := cleanRepoPath(requested)
	if err != nil {
		return nil, err
	}
	idx, err := repo.Repo.Storer.Index()
	if err != nil {
		return nil, err
	}
	prefix := ""
	if root != "." {
		prefix = strings.TrimSuffix(root, "/")
	}
	byPath := make(map[string]*index.Entry, len(idx.Entries))
	for _, entry := range idx.Entries {
		path := filepath.ToSlash(entry.Name)
		if prefix != "" && path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		byPath[path] = entry
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	entries := make([]*index.Entry, 0, len(paths))
	for _, path := range paths {
		entries = append(entries, byPath[path])
	}
	return entries, nil
}

func stagedFilePaths(repo *gitctx.Repository, requested string, maxEntries int) ([]string, bool, error) {
	entries, err := stagedIndexEntries(repo, requested)
	if err != nil {
		return nil, false, err
	}
	truncated := len(entries) > maxEntries
	if truncated {
		entries = entries[:maxEntries]
	}
	paths := make([]string, len(entries))
	for i, entry := range entries {
		paths[i] = filepath.ToSlash(entry.Name)
	}
	return paths, truncated, nil
}

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
	if t.mode == ReviewModeStaged {
		files, truncated, err := stagedFilePaths(t.repo, args.Path, maxEntries)
		if err != nil {
			return Result{}, err
		}
		return jsonResult("list_files", map[string]any{"files": files}, truncated)
	}
	var files []string
	err = walkRepository(t.repo, args.Path, func(_ *os.Root, _, path string, entry fs.DirEntry) error {
		if entry.IsDir() {
			return nil
		}
		files = append(files, path)
		if len(files) > maxEntries {
			return errListFilesComplete
		}
		return nil
	})
	if err != nil && !errors.Is(err, errListFilesComplete) {
		return Result{}, err
	}
	truncated := len(files) > maxEntries
	if truncated {
		files = files[:maxEntries]
	}
	return jsonResult("list_files", map[string]any{"files": files}, truncated)
}

type readFileTool struct {
	repo *gitctx.Repository
	mode ReviewMode
}

func (t readFileTool) Definition() Definition {
	return Definition{Name: "read_file", Description: "Read a UTF-8 repository file with byte and line caps.", Schema: schema(map[string]any{
		"path":             stringProp("Repository-relative file path."),
		"source":           fileSourceProp(t.mode),
		"line_start":       intProp("Optional inclusive first line. Zero starts at line 1.", 0, 10000000),
		"line_end":         intProp("Optional inclusive last line. Zero reads through EOF.", 0, 10000000),
		"with_line_number": map[string]any{"type": "boolean", "description": "Prepend original line numbers like nl -ba."},
		"max_bytes":        intProp("Maximum bytes to return.", 1, 65536),
		"max_lines":        intProp("Maximum lines to return.", 1, 2000),
	}, "path"), Strict: true}
}

func fileSourceProp(mode ReviewMode) map[string]any {
	description := "File source. Empty means worktree."
	if mode == ReviewModeStaged {
		description = "File source. Empty means index; worktree is unavailable in staged mode."
	}
	return enumStringProp(description, "", "worktree", "index", "head")
}

func (t readFileTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Path           string `json:"path"`
		Source         string `json:"source"`
		LineStart      int    `json:"line_start"`
		LineEnd        int    `json:"line_end"`
		WithLineNumber bool   `json:"with_line_number"`
		MaxBytes       int    `json:"max_bytes"`
		MaxLines       int    `json:"max_lines"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	if args.Path == "" {
		return Result{}, fmt.Errorf("path is required")
	}
	reader, source, err := openInspectedFile(t.repo, t.mode, args.Path, args.Source)
	if err != nil {
		return Result{}, err
	}
	defer reader.Close()
	maxBytes, maxLines := normalizeCaps(args.MaxBytes, args.MaxLines)
	content, first, last, truncated, err := readLineRange(reader, args.LineStart, args.LineEnd, maxBytes, maxLines, args.WithLineNumber)
	if err != nil {
		return Result{}, err
	}
	return jsonResult("read_file", map[string]any{
		"path":       args.Path,
		"source":     source,
		"line_start": first,
		"line_end":   last,
		"content":    content,
	}, truncated)
}

func openInspectedFile(repo *gitctx.Repository, mode ReviewMode, rawPath, source string) (io.ReadCloser, string, error) {
	path, err := cleanRepoPath(rawPath)
	if err != nil {
		return nil, "", err
	}
	if mode == ReviewModeStaged {
		if source == "worktree" {
			return nil, "", fmt.Errorf("source worktree is unavailable in staged mode; use index or head")
		}
		if source == "" {
			source = "index"
		}
	} else if source == "" {
		source = "worktree"
	}
	if source != "worktree" && source != "index" && source != "head" {
		return nil, "", fmt.Errorf("source must be worktree, index, or head")
	}
	var reader io.ReadCloser
	if mode == ReviewModeUncommitted {
		reader, err = repo.OpenUncommittedReviewFile(gitctx.FileSource(source), path)
	} else {
		reader, err = openRepositoryFile(repo, path, source)
	}
	return reader, source, err
}

func enumStringProp(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

func openRepositoryFile(repo *gitctx.Repository, path, source string) (io.ReadCloser, error) {
	return repo.OpenFile(gitctx.FileSource(source), path)
}

func readLineRange(reader io.Reader, start, end, maxBytes, maxLines int, withLineNumber bool) (string, int, int, bool, error) {
	if start < 0 || end < 0 || (end > 0 && start > end) {
		return "", 0, 0, false, fmt.Errorf("invalid line range %d:%d", start, end)
	}
	if start == 0 {
		start = 1
	}
	var output bytes.Buffer
	currentLine := 1
	fileLines := 0
	selectedLines := 0
	lastSelected := 0
	lineHasByte := false
	buffer := make([]byte, 32*1024)
	for {
		n, readErr := reader.Read(buffer)
		for _, value := range buffer[:n] {
			lineHasByte = true
			selected := currentLine >= start && (end == 0 || currentLine <= end)
			if selected {
				if lastSelected != currentLine {
					if selectedLines >= maxLines {
						return validUTF8Prefix(output.String()), start, lastSelected, true, nil
					}
					selectedLines++
					lastSelected = currentLine
					if withLineNumber {
						prefix := fmt.Sprintf("%6d\t", currentLine)
						remaining := maxBytes - output.Len()
						if len(prefix) > remaining {
							output.WriteString(prefix[:remaining])
							return output.String(), start, lastSelected, true, nil
						}
						output.WriteString(prefix)
					}
				}
				if output.Len() >= maxBytes {
					return validUTF8Prefix(output.String()), start, lastSelected, true, nil
				}
				if value == 0 {
					return "", 0, 0, false, errors.New("file is not UTF-8 text")
				}
				output.WriteByte(value)
			}
			if value == '\n' {
				fileLines = currentLine
				if end > 0 && currentLine >= end {
					if !utf8.ValidString(output.String()) {
						return "", 0, 0, false, errors.New("file is not UTF-8 text")
					}
					return output.String(), start, lastSelected, false, nil
				}
				currentLine++
				lineHasByte = false
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return "", 0, 0, false, readErr
			}
			if lineHasByte {
				fileLines = currentLine
			}
			if start > fileLines && (start != 1 || fileLines != 0) {
				return "", 0, 0, false, fmt.Errorf("line_start %d exceeds file length %d", start, fileLines)
			}
			if !utf8.ValidString(output.String()) {
				return "", 0, 0, false, errors.New("file is not UTF-8 text")
			}
			return output.String(), start, lastSelected, false, nil
		}
	}
}

func validUTF8Prefix(value string) string {
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
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
		_ = root.Close()
		return nil, err
	}
	if err := root.Close(); err != nil {
		_ = file.Close()
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

type grepTool struct {
	repo *gitctx.Repository
	mode ReviewMode
}

func (t grepTool) Definition() Definition {
	return Definition{Name: "grep", Description: "Search repository text files with a safe RE2 expression.", Schema: schema(map[string]any{
		"pattern":     stringProp("RE2 regular expression."),
		"path":        stringProp("Repository-relative directory prefix."),
		"glob":        stringProp("Optional repository-relative or basename glob, such as *.go."),
		"max_matches": intProp("Maximum matches to return.", 1, 1000),
	}, "pattern"), Strict: true}
}

func (t grepTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		MaxMatches int    `json:"max_matches"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	if args.Pattern == "" {
		return Result{}, fmt.Errorf("pattern is required")
	}
	pattern, err := regexp.Compile(args.Pattern)
	if err != nil {
		return Result{}, fmt.Errorf("invalid RE2 pattern: %w", err)
	}
	if args.Glob != "" {
		if _, err := pathpkg.Match(args.Glob, "probe"); err != nil {
			return Result{}, fmt.Errorf("invalid glob: %w", err)
		}
	}
	maxMatches := args.MaxMatches
	if maxMatches <= 0 {
		maxMatches = 100
	}
	if t.mode == ReviewModeStaged {
		return t.executeStaged(pattern, args.Path, args.Glob, maxMatches)
	}
	var matches []map[string]any
	truncated := false
	err = walkRepository(t.repo, args.Path, func(root *os.Root, _, path string, entry fs.DirEntry) error {
		if entry.IsDir() {
			return nil
		}
		if len(matches) >= maxMatches {
			truncated = true
			return fs.SkipAll
		}
		if args.Glob != "" && !globMatches(args.Glob, path) {
			return nil
		}
		file, err := root.Open(path)
		if err != nil {
			return nil
		}
		if info, err := entry.Info(); err == nil && info.Size() > 2*1024*1024 {
			truncated = true
		}
		scanner := bufio.NewScanner(io.LimitReader(file, 2*1024*1024+1))
		scanner.Buffer(make([]byte, 64*1024), 256*1024)
		lineNo := 0
		binary := false
		hitLimit := false
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			if strings.IndexByte(line, 0) >= 0 {
				binary = true
				break
			}
			if pattern.MatchString(line) {
				matches = append(matches, map[string]any{"path": path, "line": lineNo, "text": limitMatchText(line)})
				if len(matches) >= maxMatches {
					truncated = true
					hitLimit = true
					break
				}
			}
		}
		scanErr := scanner.Err()
		_ = file.Close()
		if scanErr != nil {
			truncated = true
		}
		if binary {
			return nil
		}
		if hitLimit {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return jsonResult("grep", map[string]any{"matches": matches}, truncated)
}

func (t grepTool) executeStaged(pattern *regexp.Regexp, requested, glob string, maxMatches int) (Result, error) {
	entries, err := stagedIndexEntries(t.repo, requested)
	if err != nil {
		return Result{}, err
	}
	var matches []map[string]any
	truncated := false
	for _, entry := range entries {
		path := filepath.ToSlash(entry.Name)
		if glob != "" && !globMatches(glob, path) {
			continue
		}
		blob, err := t.repo.Repo.BlobObject(entry.Hash)
		if err != nil {
			continue
		}
		reader, err := blob.Reader()
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(io.LimitReader(reader, 2*1024*1024+1))
		scanner.Buffer(make([]byte, 64*1024), 256*1024)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			if strings.IndexByte(line, 0) >= 0 {
				break
			}
			if pattern.MatchString(line) {
				matches = append(matches, map[string]any{"path": path, "line": lineNo, "text": limitMatchText(line)})
				if len(matches) >= maxMatches {
					truncated = true
					break
				}
			}
		}
		if scanner.Err() != nil || blob.Size > 2*1024*1024 {
			truncated = true
		}
		_ = reader.Close()
		if len(matches) >= maxMatches {
			break
		}
	}
	return jsonResult("grep", map[string]any{"matches": matches}, truncated)
}

func globMatches(pattern, path string) bool {
	matched, _ := pathpkg.Match(pattern, path)
	if matched {
		return true
	}
	matched, _ = pathpkg.Match(pattern, pathpkg.Base(path))
	return matched
}

func limitMatchText(text string) string {
	const maxMatchBytes = 1000
	if len(text) <= maxMatchBytes {
		return text
	}
	text = text[:maxMatchBytes]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text + "…"
}

type findTool struct {
	repo *gitctx.Repository
	mode ReviewMode
}

func (t findTool) Definition() Definition {
	return Definition{Name: "find", Description: "Find repository files or directories by safe glob.", Schema: schema(map[string]any{
		"path":        stringProp("Repository-relative directory prefix."),
		"name":        stringProp("Optional basename or repository-relative glob."),
		"type":        enumStringProp("Entry type. Empty means any.", "", "any", "file", "directory"),
		"max_entries": intProp("Maximum entries to return.", 1, 1000),
	}), Strict: true}
}

func (t findTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Path       string `json:"path"`
		Name       string `json:"name"`
		Type       string `json:"type"`
		MaxEntries int    `json:"max_entries"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	if args.Name != "" {
		if _, err := pathpkg.Match(args.Name, "probe"); err != nil {
			return Result{}, fmt.Errorf("invalid name glob: %w", err)
		}
	}
	maxEntries := args.MaxEntries
	if maxEntries <= 0 {
		maxEntries = 200
	}
	if t.mode == ReviewModeStaged {
		return t.executeStaged(args.Path, args.Name, args.Type, maxEntries)
	}
	var entries []map[string]any
	truncated := false
	err = walkRepository(t.repo, args.Path, func(_ *os.Root, walkRoot, path string, entry fs.DirEntry) error {
		if path == walkRoot {
			return nil
		}
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		}
		if args.Type != "" && args.Type != "any" && args.Type != kind {
			return nil
		}
		if args.Name != "" && !globMatches(args.Name, path) {
			return nil
		}
		if len(entries) >= maxEntries {
			truncated = true
			return fs.SkipAll
		}
		entries = append(entries, map[string]any{"path": path, "type": kind})
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return jsonResult("find", map[string]any{"entries": entries}, truncated)
}

func (t findTool) executeStaged(requested, name, entryType string, maxEntries int) (Result, error) {
	files, _, err := stagedFilePaths(t.repo, requested, int(^uint(0)>>1))
	if err != nil {
		return Result{}, err
	}
	root, err := cleanRepoPath(requested)
	if err != nil {
		return Result{}, err
	}
	kinds := map[string]string{}
	for _, file := range files {
		kinds[file] = "file"
		for parent := pathpkg.Dir(file); parent != "."; parent = pathpkg.Dir(parent) {
			if root != "." && parent != root && !strings.HasPrefix(parent, root+"/") {
				break
			}
			kinds[parent] = "directory"
			if parent == root {
				break
			}
		}
	}
	paths := make([]string, 0, len(kinds))
	for path := range kinds {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	entries := make([]map[string]any, 0, min(len(paths), maxEntries))
	truncated := false
	for _, path := range paths {
		kind := kinds[path]
		if entryType != "" && entryType != "any" && entryType != kind {
			continue
		}
		if name != "" && !globMatches(name, path) {
			continue
		}
		if len(entries) >= maxEntries {
			truncated = true
			break
		}
		entries = append(entries, map[string]any{"path": path, "type": kind})
	}
	return jsonResult("find", map[string]any{"entries": entries}, truncated)
}

type reviewDiffTool struct {
	repo *gitctx.Repository
	mode ReviewMode
}

const maxReviewChangesPage = 500

type reviewChangesTool struct {
	mode  ReviewMode
	scope ReviewScope
}

func (t reviewChangesTool) Definition() Definition {
	return Definition{Name: "review_changes", Description: "Return one page of the authoritative changed-path inventory with status and line stats.", Schema: schema(map[string]any{
		"offset": map[string]any{"type": "integer", "description": "Zero-based page offset.", "minimum": 0},
		"limit":  intProp("Maximum changes to return.", 1, maxReviewChangesPage),
	}), Strict: true}
}

func (t reviewChangesTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Offset int `json:"offset"`
		Limit  int `json:"limit"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	if args.Offset < 0 {
		return Result{}, fmt.Errorf("offset must be non-negative")
	}
	if args.Limit < 1 || args.Limit > maxReviewChangesPage {
		return Result{}, fmt.Errorf("limit must be between 1 and %d", maxReviewChangesPage)
	}
	start := min(args.Offset, len(t.scope.Changes))
	end := min(start+args.Limit, len(t.scope.Changes))
	hasMore := end < len(t.scope.Changes)
	return jsonResult("review_changes", map[string]any{
		"mode": t.mode, "changes": t.scope.Changes[start:end], "total": len(t.scope.Changes),
		"next_offset": end, "has_more": hasMore,
	}, hasMore)
}

func (t reviewDiffTool) Definition() Definition {
	return Definition{Name: "review_diff", Description: "Return the authoritative review diff with caps.", Schema: cappedSchema(), Strict: true}
}

func (t reviewDiffTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[capArgs](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	maxBytes, maxLines := normalizeCaps(args.MaxBytes, args.MaxLines)
	var diff string
	var truncated bool
	switch t.mode {
	case ReviewModeUncommitted:
		diff, truncated, err = t.repo.UncommittedDiff(maxBytes, maxLines)
	case ReviewModeStaged:
		diff, truncated, err = t.repo.StagedDiff(maxBytes, maxLines)
	default:
		return Result{}, fmt.Errorf("review_diff is unavailable in %s mode", t.mode)
	}
	if err != nil {
		return Result{}, err
	}
	return jsonResult("review_diff", map[string]any{"mode": t.mode, "diff": diff}, truncated)
}

type reviewDiffForPathsTool struct {
	repo *gitctx.Repository
	mode ReviewMode
}

func (t reviewDiffForPathsTool) Definition() Definition {
	return Definition{Name: "review_diff_for_paths", Description: "Return the authoritative review diff for selected repository-relative paths with caps.", Schema: schema(map[string]any{
		"paths":     stringArrayProp("Repository-relative paths to include."),
		"max_bytes": intProp("Maximum bytes to return.", 1, 65536),
		"max_lines": intProp("Maximum lines to return.", 1, 2000),
	}, "paths"), Strict: true}
}

func (t reviewDiffForPathsTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
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
	for _, path := range args.Paths {
		if _, err := cleanRepoPath(path); err != nil {
			return Result{}, err
		}
	}
	maxBytes, maxLines := normalizeCaps(args.MaxBytes, args.MaxLines)
	var diff string
	var truncated bool
	switch t.mode {
	case ReviewModeUncommitted:
		diff, truncated, err = t.repo.UncommittedDiffForPaths(args.Paths, maxBytes, maxLines)
	case ReviewModeStaged:
		diff, truncated, err = t.repo.StagedDiffForPaths(args.Paths, maxBytes, maxLines)
	default:
		return Result{}, fmt.Errorf("review_diff_for_paths is unavailable in %s mode", t.mode)
	}
	if err != nil {
		return Result{}, err
	}
	return jsonResult("review_diff_for_paths", map[string]any{"mode": t.mode, "paths": args.Paths, "diff": diff}, truncated)
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
	return Definition{Name: "git_staged_status", Description: "Return index status for paths changed versus HEAD.", Schema: emptySchema(), Strict: true}
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
	root, err := os.OpenRoot(t.repo.RootPath)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	content, err := root.ReadFile(".gitmodules")
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
	rel, err := cleanRepoPath(args.Path)
	if err != nil {
		return Result{}, err
	}
	root, err := os.OpenRoot(t.repo.RootPath)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	subRoot, err := root.OpenRoot(rel)
	if err != nil {
		return jsonResult("submodule_log_range", map[string]any{"available": false, "error": err.Error()}, false)
	}
	defer subRoot.Close()
	sub, err := gitctx.Open(subRoot.Name())
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

func cleanRepoPath(rel string) (string, error) {
	if rel == "" {
		return ".", nil
	}
	cleaned := filepath.Clean(rel)
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("absolute paths are not allowed: %s", rel)
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes repository: %s", rel)
	}
	return filepath.ToSlash(cleaned), nil
}
