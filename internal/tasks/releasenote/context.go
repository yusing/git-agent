package releasenote

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/textutil"
)

const (
	preparedCommitLimit                 = 200
	preparedCommitMessageMaxLines       = 10
	preparedCommitMessageMaxWords       = 1000
	preparedPatchExcerptMaxLines        = 40
	preparedPatchExcerptMaxBytes        = 6 * 1024
	preparedCandidateEvidenceMaxPaths   = 8
	preparedCandidateEvidenceMaxExcerpt = 1500
)

type PreparedContext struct {
	Range                   string              `json:"range"`
	BaseRef                 string              `json:"base_ref"`
	BaseSHA                 string              `json:"base_sha"`
	ReleaseRef              string              `json:"release_ref"`
	ReleaseSHA              string              `json:"release_sha"`
	RepoKind                string              `json:"repo_kind"`
	ParentRepositoryURL     string              `json:"parent_repository_url,omitempty"`
	RequireFullChangelog    bool                `json:"require_full_changelog"`
	RecommendedSections     []string            `json:"recommended_sections,omitempty"`
	ParentCommits           []PreparedCommit    `json:"parent_commits,omitempty"`
	RequiredSubmoduleGroups []string            `json:"required_submodule_groups,omitempty"`
	Submodules              []PreparedSubmodule `json:"submodules,omitempty"`
	CandidateItems          []PreparedCandidate `json:"candidate_items,omitempty"`
}

type PreparedCommit struct {
	SHA             string                `json:"sha"`
	Summary         string                `json:"summary"`
	Message         string                `json:"message,omitempty"`
	URL             string                `json:"url,omitempty"`
	Files           []PreparedChangedFile `json:"files,omitempty"`
	Diffstat        *PreparedDiffstat     `json:"diffstat,omitempty"`
	PatchExcerpt    string                `json:"patch_excerpt,omitempty"`
	OperatorSignals *OperatorSignals      `json:"operator_signals,omitempty"`
	Policy          *ReleaseNotePolicy    `json:"release_note_policy,omitempty"`
	EvidenceError   string                `json:"evidence_error,omitempty"`
}

type PreparedChangedFile struct {
	Path       string `json:"path"`
	OldPath    string `json:"old_path,omitempty"`
	Status     string `json:"status"`
	Additions  int    `json:"additions,omitempty"`
	Deletions  int    `json:"deletions,omitempty"`
	Binary     bool   `json:"binary,omitempty"`
	Submodule  bool   `json:"submodule,omitempty"`
	Generated  bool   `json:"generated,omitempty"`
	Test       bool   `json:"test,omitempty"`
	Docs       bool   `json:"docs,omitempty"`
	Dependency bool   `json:"dependency,omitempty"`
}

type PreparedDiffstat struct {
	FilesChanged           int `json:"files_changed,omitempty"`
	Additions              int `json:"additions,omitempty"`
	Deletions              int `json:"deletions,omitempty"`
	TestFilesChanged       int `json:"test_files_changed,omitempty"`
	DocsFilesChanged       int `json:"docs_files_changed,omitempty"`
	GeneratedFilesChanged  int `json:"generated_files_changed,omitempty"`
	DependencyFilesChanged int `json:"dependency_files_changed,omitempty"`
	SubmoduleFilesChanged  int `json:"submodule_files_changed,omitempty"`
}

type OperatorSignals struct {
	RuntimeChanged      bool `json:"runtime_changed,omitempty"`
	ConfigSchemaChanged bool `json:"config_schema_changed,omitempty"`
	APIChanged          bool `json:"api_changed,omitempty"`
	CLIChanged          bool `json:"cli_changed,omitempty"`
	DocsChanged         bool `json:"docs_changed,omitempty"`
	GeneratedOnly       bool `json:"generated_only,omitempty"`
	TestsOnly           bool `json:"tests_only,omitempty"`
	DependencyOnly      bool `json:"dependency_only,omitempty"`
	SubmoduleOnly       bool `json:"submodule_only,omitempty"`
	InternalOnly        bool `json:"internal_only,omitempty"`
}

type ReleaseNotePolicy struct {
	IncludeNarrative bool   `json:"include_narrative"`
	Reason           string `json:"reason,omitempty"`
}

type PreparedCandidate struct {
	ID                 string                      `json:"id"`
	RecommendedSection string                      `json:"recommended_section"`
	Label              string                      `json:"label,omitempty"`
	Include            bool                        `json:"include"`
	Confidence         string                      `json:"confidence"`
	DraftFact          string                      `json:"draft_fact"`
	OmitReason         string                      `json:"omit_reason,omitempty"`
	Refs               []PreparedCandidateRef      `json:"refs"`
	Evidence           []PreparedCandidateEvidence `json:"evidence,omitempty"`
}

type PreparedCandidateRef struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type PreparedCandidateEvidence struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type PreparedSubmodule struct {
	Path                  string           `json:"path"`
	BaseSHA               string           `json:"base_sha,omitempty"`
	ReleaseSHA            string           `json:"release_sha,omitempty"`
	RepositoryURL         string           `json:"repository_url,omitempty"`
	GroupHeading          string           `json:"group_heading"`
	LocalHistoryAvailable bool             `json:"local_history_available"`
	AvailabilityError     string           `json:"availability_error,omitempty"`
	Commits               []PreparedCommit `json:"commits,omitempty"`
}

func PrepareContextFromRevision(repo *gitctx.Repository, baseRef, releaseRef, releaseRevision string) (PreparedContext, error) {
	baseSHA, err := repo.ResolveRef(baseRef)
	if err != nil {
		return PreparedContext{}, err
	}
	releaseSHA, err := repo.ResolveRef(releaseRevision)
	if err != nil {
		return PreparedContext{}, err
	}

	parentCommits, err := repo.LogMessagesFrom(baseRef, releaseRevision, preparedCommitLimit)
	if err != nil {
		return PreparedContext{}, err
	}
	submoduleChanges, err := repo.SubmoduleGitlinkRange(baseRef, releaseRevision)
	if err != nil {
		return PreparedContext{}, err
	}

	gitmodulesURLs, err := parseGitmodulesURLs(filepath.Join(repo.RootPath, ".gitmodules"))
	if err != nil {
		return PreparedContext{}, err
	}

	context := PreparedContext{
		Range:                baseRef + ".." + releaseRef,
		BaseRef:              baseRef,
		BaseSHA:              baseSHA,
		ReleaseRef:           releaseRef,
		ReleaseSHA:           releaseSHA,
		RepoKind:             repo.RepoKind(),
		ParentRepositoryURL:  repositoryURL(repo.Summary()),
		RequireFullChangelog: len(parentCommits) > 0,
		RecommendedSections:  recommendedSections(parentCommits),
		ParentCommits:        preparedCommits(parentCommits, repositoryURL(repo.Summary())),
		Submodules:           make([]PreparedSubmodule, 0, len(submoduleChanges)),
	}

	for _, change := range submoduleChanges {
		submodule, err := prepareSubmodule(repo.RootPath, change, gitmodulesURLs[change.Path])
		if err != nil {
			return PreparedContext{}, err
		}
		context.Submodules = append(context.Submodules, submodule)
		if change.Old != "" && change.New != "" && submodule.LocalHistoryAvailable {
			context.RequiredSubmoduleGroups = append(context.RequiredSubmoduleGroups, change.Path)
		}
	}
	context.CandidateItems = candidateItems(context.ParentCommits, context.Submodules)

	return context, nil
}

func (c PreparedContext) Render() string {
	data, err := sonic.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"range":%q}`, c.Range)
	}
	return string(data)
}

func (c PreparedContext) RenderForPrompt() string {
	return c.Render()
}

func preparedCommits(commits []gitctx.CommitMessageInfo, repoURL string) []PreparedCommit {
	prepared := make([]PreparedCommit, 0, len(commits))
	for _, commit := range commits {
		files := preparedChangedFiles(commit.Files)
		diffstat := preparedDiffstat(files, commit.Diffstat)
		signals := operatorSignals(files)
		policy := releaseNotePolicy(commit.Summary, signals)
		prepared = append(prepared, PreparedCommit{
			SHA:             commit.SHA,
			Summary:         commit.Summary,
			Message:         clampCommitMessage(commit.Message),
			URL:             commitURL(repoURL, commit.SHA),
			Files:           files,
			Diffstat:        diffstat,
			PatchExcerpt:    clampPatchExcerpt(commit.PatchExcerpt),
			OperatorSignals: signals,
			Policy:          policy,
			EvidenceError:   commit.EvidenceError,
		})
	}
	return prepared
}

func recommendedSections(commits []gitctx.CommitMessageInfo) []string {
	hasBreaking := false
	hasBugFixes := false
	hasFeatures := false
	hasImprovements := false
	for _, commit := range commits {
		lower := strings.ToLower(commit.Summary)
		switch {
		case strings.Contains(lower, "breaking"), strings.Contains(lower, "remove "), strings.Contains(lower, "drop "), strings.Contains(lower, "rename "), strings.Contains(lower, "path_patterns"):
			hasBreaking = true
		}
		switch {
		case strings.HasPrefix(lower, "fix("), strings.HasPrefix(lower, "fix:"), strings.Contains(lower, " bug "), strings.Contains(lower, "middleware after rules"):
			hasBugFixes = true
		case strings.HasPrefix(lower, "feat("), strings.HasPrefix(lower, "feat:"):
			hasFeatures = true
		default:
			hasImprovements = true
		}
	}

	var sections []string
	if hasBreaking {
		sections = append(sections, "### Breaking Changes")
	}
	if hasBugFixes {
		sections = append(sections, "### Bug Fixes")
	}
	if hasFeatures {
		sections = append(sections, "### New Features")
	}
	if hasImprovements || len(sections) == 0 {
		sections = append(sections, "### Improvements")
	}
	sections = append(sections, "### Full Changelog")
	return sections
}

func prepareSubmodule(root string, change gitctx.SubmoduleChange, gitmodulesURL string) (PreparedSubmodule, error) {
	submodule := PreparedSubmodule{
		Path:       change.Path,
		BaseSHA:    change.Old,
		ReleaseSHA: change.New,
	}
	if repoURL := normalizeRepositoryURL(gitmodulesURL); repoURL != "" {
		submodule.RepositoryURL = repoURL
	}
	submodule.GroupHeading = submoduleHeading(change.Path, submodule.RepositoryURL)
	if change.Old == "" || change.New == "" {
		return submodule, nil
	}

	path, err := safeRepoPath(root, change.Path)
	if err != nil {
		return PreparedSubmodule{}, err
	}
	repo, err := gitctx.Open(path)
	if err != nil {
		submodule.AvailabilityError = err.Error()
		return submodule, nil
	}

	if repoURL := repositoryURL(repo.Summary()); repoURL != "" {
		submodule.RepositoryURL = repoURL
	}
	submodule.GroupHeading = submoduleHeading(change.Path, submodule.RepositoryURL)

	commits, err := repo.LogMessagesFrom(change.Old, change.New, preparedCommitLimit)
	if err != nil {
		return PreparedSubmodule{}, err
	}
	submodule.LocalHistoryAvailable = true
	submodule.Commits = preparedCommits(commits, submodule.RepositoryURL)
	return submodule, nil
}

func clampCommitMessage(message string) string {
	message = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(message, "\r\n", "\n"), "\r", "\n"))
	if message == "" {
		return ""
	}
	return clampCommitMessageWords(clampCommitMessageLines(message, preparedCommitMessageMaxLines), preparedCommitMessageMaxWords)
}

func clampCommitMessageLines(message string, limit int) string {
	if limit <= 0 {
		return ""
	}
	lines := strings.Split(message, "\n")
	if len(lines) <= limit {
		return message
	}
	return strings.Join(lines[:limit], "\n")
}

func clampCommitMessageWords(message string, limit int) string {
	if limit <= 0 {
		return ""
	}

	var b strings.Builder
	wordsUsed := 0
	for i, line := range strings.Split(message, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		words := strings.Fields(line)
		if len(words) == 0 {
			continue
		}
		remaining := limit - wordsUsed
		if remaining <= 0 {
			break
		}
		if len(words) > remaining {
			words = words[:remaining]
		}
		b.WriteString(strings.Join(words, " "))
		wordsUsed += len(words)
		if wordsUsed >= limit {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

func preparedChangedFiles(files []gitctx.CommitFileChange) []PreparedChangedFile {
	prepared := make([]PreparedChangedFile, 0, len(files))
	for _, file := range files {
		path := cleanSlashPath(file.Path)
		if path == "" {
			continue
		}
		prepared = append(prepared, PreparedChangedFile{
			Path:       path,
			OldPath:    cleanSlashPath(file.OldPath),
			Status:     file.Status,
			Additions:  file.Additions,
			Deletions:  file.Deletions,
			Binary:     file.Binary,
			Submodule:  file.Submodule,
			Generated:  isGeneratedPath(path),
			Test:       isTestPath(path),
			Docs:       isDocsPath(path),
			Dependency: isDependencyPath(path),
		})
	}
	return prepared
}

func preparedDiffstat(files []PreparedChangedFile, raw gitctx.CommitDiffstat) *PreparedDiffstat {
	if len(files) == 0 && raw.FilesChanged == 0 && raw.Additions == 0 && raw.Deletions == 0 {
		return nil
	}
	stat := PreparedDiffstat{
		FilesChanged: len(files),
		Additions:    raw.Additions,
		Deletions:    raw.Deletions,
	}
	if stat.FilesChanged == 0 {
		stat.FilesChanged = raw.FilesChanged
	}
	for _, file := range files {
		switch {
		case file.Submodule:
			stat.SubmoduleFilesChanged++
		case file.Test:
			stat.TestFilesChanged++
		case file.Docs:
			stat.DocsFilesChanged++
		case file.Generated:
			stat.GeneratedFilesChanged++
		case file.Dependency:
			stat.DependencyFilesChanged++
		}
	}
	return &stat
}

func operatorSignals(files []PreparedChangedFile) *OperatorSignals {
	if len(files) == 0 {
		return nil
	}
	signals := OperatorSignals{
		GeneratedOnly:  true,
		TestsOnly:      true,
		DependencyOnly: true,
		SubmoduleOnly:  true,
		InternalOnly:   true,
	}
	for _, file := range files {
		path := file.Path
		if !file.Generated {
			signals.GeneratedOnly = false
		}
		if !file.Test {
			signals.TestsOnly = false
		}
		if !file.Dependency {
			signals.DependencyOnly = false
		}
		if !file.Submodule {
			signals.SubmoduleOnly = false
		}
		if !strings.HasPrefix(path, "internal/") || file.Docs || file.Test || file.Generated || file.Dependency || file.Submodule {
			signals.InternalOnly = false
		}
		if file.Docs {
			signals.DocsChanged = true
		}
		if isCLIPath(path) {
			signals.CLIChanged = true
		}
		if isAPIPath(path) {
			signals.APIChanged = true
		}
		if isConfigSchemaPath(path) {
			signals.ConfigSchemaChanged = true
		}
		if !file.Test && !file.Docs && !file.Generated && !file.Dependency && !file.Submodule {
			signals.RuntimeChanged = true
		}
	}
	return &signals
}

func releaseNotePolicy(summary string, signals *OperatorSignals) *ReleaseNotePolicy {
	if signals == nil {
		if isChoreSummary(summary) {
			return &ReleaseNotePolicy{IncludeNarrative: false, Reason: "chore without changed-file evidence"}
		}
		return &ReleaseNotePolicy{IncludeNarrative: true, Reason: "no changed-file evidence available"}
	}
	switch {
	case signals.SubmoduleOnly:
		return &ReleaseNotePolicy{IncludeNarrative: false, Reason: "submodule pointer update; use submodule commits as narrative evidence"}
	case signals.TestsOnly:
		return &ReleaseNotePolicy{IncludeNarrative: false, Reason: "tests only"}
	case signals.GeneratedOnly && !signals.ConfigSchemaChanged && !signals.APIChanged && !signals.DocsChanged:
		return &ReleaseNotePolicy{IncludeNarrative: false, Reason: "generated files only"}
	case signals.DependencyOnly:
		return &ReleaseNotePolicy{IncludeNarrative: false, Reason: "dependency metadata only"}
	case signals.InternalOnly && isChoreSummary(summary):
		return &ReleaseNotePolicy{IncludeNarrative: false, Reason: "internal chore only"}
	default:
		return &ReleaseNotePolicy{IncludeNarrative: true}
	}
}

func candidateItems(parentCommits []PreparedCommit, submodules []PreparedSubmodule) []PreparedCandidate {
	items := make([]PreparedCandidate, 0, len(parentCommits))
	for _, commit := range parentCommits {
		if commit.Policy != nil && !commit.Policy.IncludeNarrative {
			continue
		}
		items = append(items, candidateForCommit(commit, ""))
	}
	for _, submodule := range submodules {
		if !submodule.LocalHistoryAvailable {
			continue
		}
		for _, commit := range submodule.Commits {
			if commit.Policy != nil && !commit.Policy.IncludeNarrative {
				continue
			}
			items = append(items, candidateForCommit(commit, submodule.Path))
		}
	}
	return items
}

func candidateForCommit(commit PreparedCommit, submodulePath string) PreparedCandidate {
	label := commitLabel(commit, submodulePath)
	fact := draftFact(commit)
	candidate := PreparedCandidate{
		ID:                 candidateID(commit, submodulePath),
		RecommendedSection: recommendedSectionForCommit(commit),
		Label:              label,
		Include:            commit.Policy == nil || commit.Policy.IncludeNarrative,
		Confidence:         candidateConfidence(commit),
		DraftFact:          fact,
		Refs:               []PreparedCandidateRef{{Type: "commit", Value: commit.SHA}},
		Evidence:           candidateEvidence(commit),
	}
	if commit.Policy != nil && !commit.Policy.IncludeNarrative {
		candidate.OmitReason = commit.Policy.Reason
	}
	return candidate
}

func candidateID(commit PreparedCommit, submodulePath string) string {
	prefix := "parent"
	if submodulePath != "" {
		prefix = submodulePath
	}
	sha := commit.SHA
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return strings.NewReplacer("/", "-", "_", "-").Replace(prefix + "-" + sha)
}

func recommendedSectionForCommit(commit PreparedCommit) string {
	lower := strings.ToLower(commit.Summary + "\n" + commit.Message)
	switch {
	case strings.Contains(lower, "breaking"), strings.Contains(lower, "remove "), strings.Contains(lower, "drop "):
		return "Breaking Changes"
	case strings.Contains(lower, "security"), strings.Contains(lower, "cve"):
		return "Security"
	case strings.HasPrefix(lower, "fix("), strings.HasPrefix(lower, "fix:"):
		return "Bug Fixes"
	case strings.HasPrefix(lower, "feat("), strings.HasPrefix(lower, "feat:"):
		return "New Features"
	default:
		return "Improvements"
	}
}

func candidateConfidence(commit PreparedCommit) string {
	switch {
	case commit.PatchExcerpt != "" && len(commit.Files) > 0:
		return "high"
	case len(commit.Files) > 0:
		return "medium"
	default:
		return "low"
	}
}

func candidateEvidence(commit PreparedCommit) []PreparedCandidateEvidence {
	evidence := []PreparedCandidateEvidence{{Type: "commit_message", Value: commit.Message}}
	paths := make([]string, 0, min(len(commit.Files), preparedCandidateEvidenceMaxPaths))
	for _, file := range commit.Files {
		paths = append(paths, file.Path)
		if len(paths) >= preparedCandidateEvidenceMaxPaths {
			break
		}
	}
	if len(paths) > 0 {
		evidence = append(evidence, PreparedCandidateEvidence{Type: "changed_paths", Value: strings.Join(paths, ", ")})
	}
	if commit.PatchExcerpt != "" {
		excerpt, _ := textutil.Limit(commit.PatchExcerpt, preparedCandidateEvidenceMaxExcerpt, 0)
		evidence = append(evidence, PreparedCandidateEvidence{Type: "patch_excerpt", Value: strings.TrimSpace(excerpt)})
	}
	return evidence
}

func commitLabel(commit PreparedCommit, submodulePath string) string {
	scope := conventionalScope(commit.Summary)
	if submodulePath != "" && scope != "" {
		return stableLabel(submodulePath) + "/" + stableLabel(scope)
	}
	if submodulePath != "" {
		return stableLabel(submodulePath)
	}
	if scope != "" {
		return stableLabel(scope)
	}
	for _, file := range commit.Files {
		if file.Submodule || file.Test || file.Generated || file.Dependency {
			continue
		}
		return labelFromPath(file.Path)
	}
	return ""
}

func conventionalScope(summary string) string {
	_, rest, ok := strings.Cut(summary, "(")
	if !ok {
		return ""
	}
	scope, _, ok := strings.Cut(rest, ")")
	if !ok || strings.Contains(scope, " ") {
		return ""
	}
	return scope
}

func stableLabel(value string) string {
	value = strings.Trim(value, " ./_-\t")
	if value == "" {
		return ""
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '-' || r == '_' || r == '/' || r == '.'
	})
	for i, part := range parts {
		if part == "" {
			continue
		}
		if acronym := stableAcronym(part); acronym != "" {
			parts[i] = acronym
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, "/")
}

func stableAcronym(part string) string {
	switch strings.ToLower(part) {
	case "accesslog":
		return "Access Logs"
	case "api":
		return "API"
	case "cli":
		return "CLI"
	case "tls":
		return "TLS"
	case "ui":
		return "UI"
	case "webui":
		return "WebUI"
	case "ovh":
		return "OVH"
	default:
		return ""
	}
}

func labelFromPath(path string) string {
	switch {
	case strings.HasPrefix(path, "cmd/"):
		return "CLI"
	case strings.HasPrefix(path, "internal/api/"):
		return "API"
	case strings.HasPrefix(path, "internal/config/"):
		return "Config"
	case strings.HasPrefix(path, "internal/"):
		parts := strings.Split(path, "/")
		if len(parts) >= 2 {
			return stableLabel(parts[1])
		}
	case strings.HasPrefix(path, "webui/"):
		return "WebUI"
	case strings.HasPrefix(path, "docs/") || strings.HasPrefix(path, "README"):
		return "Docs"
	}
	return ""
}

func draftFact(commit PreparedCommit) string {
	var paragraph []string
	for line := range strings.SplitSeq(commit.Message, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
		switch {
		case line == "" && len(paragraph) > 0:
			return normalizeDraftFact(paragraph)
		case line == "", line == commit.Summary, isTrailerLine(line):
			continue
		default:
			paragraph = append(paragraph, line)
		}
	}
	if len(paragraph) > 0 {
		return normalizeDraftFact(paragraph)
	}
	return strings.TrimSuffix(stripConventionalPrefix(commit.Summary), ".") + "."
}

func normalizeDraftFact(lines []string) string {
	fact := strings.TrimSpace(strings.Join(lines, " "))
	fact = strings.Join(strings.Fields(fact), " ")
	return strings.TrimSuffix(fact, ".") + "."
}

func stripConventionalPrefix(summary string) string {
	_, rest, ok := strings.Cut(summary, ": ")
	if ok {
		return rest
	}
	return summary
}

func isTrailerLine(line string) bool {
	key, _, ok := strings.Cut(line, ":")
	if !ok || key == "" {
		return false
	}
	for _, r := range key {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '-' {
			continue
		}
		return false
	}
	return true
}

func clampPatchExcerpt(excerpt string) string {
	excerpt = strings.TrimSpace(excerpt)
	if excerpt == "" {
		return ""
	}
	limited, _ := textutil.Limit(excerpt, preparedPatchExcerptMaxBytes, preparedPatchExcerptMaxLines)
	return strings.TrimSpace(limited)
}

func cleanSlashPath(path string) string {
	return strings.Trim(strings.ReplaceAll(path, "\\", "/"), "/")
}

func isGeneratedPath(path string) bool {
	base := filepath.Base(path)
	return strings.Contains(base, ".gen.") || strings.Contains(base, ".generated.") || strings.HasSuffix(base, "_gen.go") || base == "schema.json" || strings.HasSuffix(base, ".schema.json") || strings.HasPrefix(path, "internal/api/v1/docs/swagger.")
}

func isTestPath(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, ".test.ts") || strings.HasSuffix(base, ".test.tsx") || strings.Contains(path, "/test/") || strings.Contains(path, "/tests/")
}

func isDocsPath(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	ext := strings.ToLower(filepath.Ext(path))
	return strings.HasPrefix(path, "docs/") || strings.Contains(path, "/docs/") || strings.Contains(path, "/wiki/") || strings.HasPrefix(base, "readme") || ext == ".md" || ext == ".mdx" || ext == ".rst" || ext == ".txt"
}

func isDependencyPath(path string) bool {
	base := filepath.Base(path)
	switch base {
	case "go.mod", "go.sum", "package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb", "Cargo.toml", "Cargo.lock":
		return true
	default:
		return strings.HasPrefix(path, "vendor/") || strings.Contains(path, "/vendor/") || strings.HasPrefix(path, "node_modules/")
	}
}

func isCLIPath(path string) bool {
	return strings.HasPrefix(path, "cmd/") || strings.HasPrefix(path, "completions/")
}

func isAPIPath(path string) bool {
	return strings.Contains(path, "/api/") || strings.HasPrefix(path, "internal/api/") || strings.Contains(strings.ToLower(path), "openapi") || strings.Contains(path, "swagger.")
}

func isConfigSchemaPath(path string) bool {
	lower := strings.ToLower(path)
	return strings.Contains(lower, "schema") || strings.Contains(lower, "config")
}

func isChoreSummary(summary string) bool {
	lower := strings.ToLower(summary)
	return strings.HasPrefix(lower, "chore(") || strings.HasPrefix(lower, "chore:")
}

func parseGitmodulesURLs(path string) (map[string]string, error) {
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}

	urls := map[string]string{}
	currentPath := ""
	for raw := range strings.SplitSeq(string(content), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "", strings.HasPrefix(line, "#"), strings.HasPrefix(line, ";"):
			continue
		case strings.HasPrefix(line, "[submodule "):
			currentPath = ""
		case strings.HasPrefix(line, "path = "):
			currentPath = strings.TrimSpace(strings.TrimPrefix(line, "path = "))
		case strings.HasPrefix(line, "url = ") && currentPath != "":
			urls[currentPath] = strings.TrimSpace(strings.TrimPrefix(line, "url = "))
		}
	}
	return urls, nil
}

func repositoryURL(summary map[string]any) string {
	remotes, _ := summary["remotes"].(map[string][]string)
	if len(remotes) == 0 {
		return ""
	}
	if urls := remotes["origin"]; len(urls) > 0 {
		if normalized := normalizeRepositoryURL(urls[0]); normalized != "" {
			return normalized
		}
	}

	names := make([]string, 0, len(remotes))
	for name := range remotes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, raw := range remotes[name] {
			if normalized := normalizeRepositoryURL(raw); normalized != "" {
				return normalized
			}
		}
	}
	return ""
}

func normalizeRepositoryURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "https://"), strings.HasPrefix(raw, "ssh://"), strings.HasPrefix(raw, "git://"):
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			return ""
		}
		path := strings.TrimSuffix(strings.Trim(parsed.Path, "/"), ".git")
		if path == "" {
			return ""
		}
		return "https://" + parsed.Host + "/" + path
	case strings.Contains(raw, "@") && strings.Contains(raw, ":") && !strings.Contains(raw, "://"):
		parts := strings.SplitN(raw, "@", 2)
		hostPath := parts[len(parts)-1]
		host, path, ok := strings.Cut(hostPath, ":")
		if !ok {
			return ""
		}
		path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
		if host == "" || path == "" {
			return ""
		}
		return "https://" + host + "/" + path
	default:
		return ""
	}
}

func commitURL(repoURL, sha string) string {
	if repoURL == "" || sha == "" {
		return ""
	}
	return repoURL + "/commit/" + sha
}

func submoduleHeading(path, repoURL string) string {
	if repoURL != "" {
		return fmt.Sprintf("[**%s**](%s)", path, repoURL)
	}
	return "#### " + path
}

func safeRepoPath(root, rel string) (string, error) {
	cleaned := filepath.Clean(rel)
	path := filepath.Join(root, cleaned)
	resolvedRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolvedPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolvedPath != resolvedRoot && !strings.HasPrefix(resolvedPath, resolvedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes repository: %s", rel)
	}
	return resolvedPath, nil
}
