package releasenote

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yusing/git-agent/internal/gitctx"
)

const (
	preparedCommitLimit           = 200
	preparedCommitMessageMaxLines = 10
	preparedCommitMessageMaxWords = 1000
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
}

type PreparedCommit struct {
	SHA     string `json:"sha"`
	Summary string `json:"summary"`
	Message string `json:"message,omitempty"`
	URL     string `json:"url,omitempty"`
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

func PrepareContext(repo *gitctx.Repository, baseRef, releaseRef string) (PreparedContext, error) {
	return PrepareContextFromRevision(repo, baseRef, releaseRef, releaseRef)
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

	return context, nil
}

func (c PreparedContext) Render() string {
	data, err := json.MarshalIndent(c, "", "  ")
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
		prepared = append(prepared, PreparedCommit{
			SHA:     commit.SHA,
			Summary: commit.Summary,
			Message: clampCommitMessage(commit.Message),
			URL:     commitURL(repoURL, commit.SHA),
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

func ExpectedSubmoduleHeadings(prepared PreparedContext) map[string]string {
	headings := make(map[string]string, len(prepared.Submodules))
	for _, submodule := range prepared.Submodules {
		if submodule.LocalHistoryAvailable && submodule.GroupHeading != "" {
			headings[submodule.Path] = submodule.GroupHeading
		}
	}
	return headings
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
