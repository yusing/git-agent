package releasenote

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	json "encoding/json/v2"

	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/textutil"
)

const (
	maxNarrativeSummaryRunes = 160
	maxChildSummaryRunes     = 140
)

type Request struct {
	BaseRef    string
	ReleaseRef string
}

type ValidationOptions struct {
	RequireFullChangelog bool
	RequiredSubmodules   []string
}

type Document struct {
	Sections        []Section          `json:"sections"`
	ParentChangelog []ChangelogEntry   `json:"parent_changelog,omitempty"`
	Submodules      []SubmoduleSection `json:"submodules,omitempty"`
}

type Section struct {
	Heading string   `json:"heading"`
	Bullets []Bullet `json:"bullets"`
}

type Bullet struct {
	Label    string        `json:"label,omitempty"`
	Summary  string        `json:"summary"`
	Refs     []Reference   `json:"refs"`
	Children []ChildBullet `json:"children,omitempty"`
}

type ChildBullet struct {
	Summary string      `json:"summary"`
	Refs    []Reference `json:"refs,omitempty"`
}

type Reference struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	URL   string `json:"url,omitempty"`
}

type ChangelogEntry struct {
	SHA     string `json:"sha"`
	Summary string `json:"summary"`
	URL     string `json:"url,omitempty"`
}

type SubmoduleSection struct {
	Path    string           `json:"path"`
	Heading string           `json:"heading"`
	Entries []ChangelogEntry `json:"entries,omitempty"`
}

func SystemPrompt() string {
	return textutil.NormalizePrompt(systemPromptMarkdown)
}

func UserPrompt(prepared PreparedContext, maxSteps, maxToolCalls int) string {
	return renderUserPrompt(userPromptData{
		MaxSteps:        maxSteps,
		MaxToolCalls:    maxToolCalls,
		Range:           prepared.Range,
		PreparedContext: prepared.RenderForPrompt(),
	})
}

func TextFormat() *openai.TextFormat {
	return &openai.TextFormat{
		Name:        "release_note",
		Description: "Structured release-note narrative sections for local markdown rendering.",
		Schema:      OutputSchema(),
		Strict:      true,
	}
}

func OutputSchema() map[string]any {
	sectionEnum := []string{"Breaking Changes", "Security", "New Features", "Improvements", "Bug Fixes"}
	refItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"type": map[string]any{
				"type": "string",
				"enum": []string{"commit", "pr", "issue"},
			},
			"value": map[string]any{
				"type": "string",
			},
		},
		"required":             []string{"type", "value"},
		"additionalProperties": false,
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"sections": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"heading": map[string]any{
							"type": "string",
							"enum": sectionEnum,
						},
						"bullets": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"label": map[string]any{
										"type": []string{"string", "null"},
									},
									"summary": map[string]any{
										"type": "string",
									},
									"refs": map[string]any{
										"type":     "array",
										"minItems": 1,
										"items":    refItem,
									},
									"children": map[string]any{
										"type": "array",
										"items": map[string]any{
											"type": "object",
											"properties": map[string]any{
												"summary": map[string]any{
													"type": "string",
												},
												"refs": map[string]any{
													"type":  "array",
													"items": refItem,
												},
											},
											"required":             []string{"summary", "refs"},
											"additionalProperties": false,
										},
									},
								},
								"required":             []string{"label", "summary", "refs", "children"},
								"additionalProperties": false,
							},
						},
					},
					"required":             []string{"heading", "bullets"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"sections"},
		"additionalProperties": false,
	}
}

func ParseDocument(raw string) (Document, error) {
	var doc Document
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return Document{}, err
	}
	return doc, nil
}

func Validate(output string) []string {
	doc, errs := parseAndValidate(output)
	if len(errs) > 0 {
		return errs
	}
	return validateNarrativeDocument(doc)
}

func ValidateDocument(doc Document, options ValidationOptions) []string {
	errs := validateNarrativeDocument(doc)
	if options.RequireFullChangelog {
		errs = append(errs, validateRenderedFullChangelogRequirements(doc, options.RequiredSubmodules)...)
	}
	return errs
}

func BuildDocument(raw string, prepared PreparedContext) (Document, error) {
	doc, errs := parseAndValidate(raw)
	if len(errs) > 0 {
		return Document{}, fmt.Errorf("invalid release-note json: %s", strings.Join(errs, "; "))
	}

	doc.Sections = sortSections(doc.Sections, prepared.RecommendedSections)
	enrichNarrativeCommitRefs(&doc, prepared)
	doc.ParentChangelog = make([]ChangelogEntry, 0, len(prepared.ParentCommits))
	for _, commit := range prepared.ParentCommits {
		doc.ParentChangelog = append(doc.ParentChangelog, ChangelogEntry{
			SHA:     commit.SHA,
			Summary: commit.Summary,
			URL:     commit.URL,
		})
	}

	doc.Submodules = make([]SubmoduleSection, 0, len(prepared.Submodules))
	for _, submodule := range prepared.Submodules {
		if !submodule.LocalHistoryAvailable {
			continue
		}
		entries := make([]ChangelogEntry, 0, len(submodule.Commits))
		for _, commit := range submodule.Commits {
			entries = append(entries, ChangelogEntry{
				SHA:     commit.SHA,
				Summary: commit.Summary,
				URL:     commit.URL,
			})
		}
		doc.Submodules = append(doc.Submodules, SubmoduleSection{
			Path:    submodule.Path,
			Heading: submodule.GroupHeading,
			Entries: entries,
		})
	}

	return doc, nil
}

func Render(doc Document) string {
	var out []string

	for _, sec := range doc.Sections {
		if len(sec.Bullets) == 0 {
			continue
		}
		out = append(out, "### "+sec.Heading, "")
		for _, bullet := range sec.Bullets {
			out = append(out, "- "+renderBullet(bullet))
			for _, child := range bullet.Children {
				out = append(out, "  - "+renderChildBullet(child))
			}
		}
		out = append(out, "")
	}

	if len(doc.ParentChangelog) == 0 && len(doc.Submodules) == 0 {
		return strings.TrimSpace(strings.Join(out, "\n"))
	}

	out = append(out, "### Full Changelog", "")
	for _, entry := range doc.ParentChangelog {
		out = append(out, "- "+renderEntry(entry))
	}

	for _, submodule := range doc.Submodules {
		if len(out) > 0 && out[len(out)-1] != "" {
			out = append(out, "")
		}
		out = append(out, submodule.Heading, "")
		for _, entry := range submodule.Entries {
			out = append(out, "  - "+renderEntry(entry))
		}
	}

	return strings.TrimSpace(strings.Join(out, "\n"))
}

func parseAndValidate(output string) (Document, []string) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return Document{}, []string{"output is empty"}
	}
	if strings.Contains(trimmed, "```") {
		return Document{}, []string{"output contains code fences"}
	}
	doc, err := ParseDocument(trimmed)
	if err != nil {
		return Document{}, []string{fmt.Sprintf("output is not valid json: %v", err)}
	}
	return doc, nil
}

func validateNarrativeDocument(doc Document) []string {
	var errs []string
	errs = append(errs, validateSections(doc.Sections)...)
	errs = append(errs, validateDuplicateSectionHeadings(doc.Sections)...)
	errs = append(errs, validateBullets(doc.Sections)...)
	return errs
}

func sortSections(sections []Section, recommended []string) []Section {
	if len(recommended) == 0 {
		return sections
	}
	ordered := make([]Section, 0, len(sections))
	used := make([]bool, len(sections))
	for _, heading := range recommended {
		heading = strings.TrimPrefix(heading, "### ")
		for i, sec := range sections {
			if used[i] || sec.Heading != heading {
				continue
			}
			ordered = append(ordered, sec)
			used[i] = true
		}
	}
	for i, sec := range sections {
		if used[i] {
			continue
		}
		ordered = append(ordered, sec)
	}
	return ordered
}

func enrichNarrativeCommitRefs(doc *Document, prepared PreparedContext) {
	if doc == nil {
		return
	}
	submoduleURLs := submoduleCommitURLs(prepared)
	if len(submoduleURLs) == 0 {
		return
	}
	for sectionIdx := range doc.Sections {
		for bulletIdx := range doc.Sections[sectionIdx].Bullets {
			enrichCommitRefs(doc.Sections[sectionIdx].Bullets[bulletIdx].Refs, submoduleURLs)
			for childIdx := range doc.Sections[sectionIdx].Bullets[bulletIdx].Children {
				enrichCommitRefs(doc.Sections[sectionIdx].Bullets[bulletIdx].Children[childIdx].Refs, submoduleURLs)
			}
		}
	}
}

func enrichCommitRefs(refs []Reference, urls map[string]string) {
	for i := range refs {
		ref := &refs[i]
		if ref.Type != "commit" || ref.URL != "" {
			continue
		}
		if url := resolveCommitURL(ref.Value, urls); url != "" {
			ref.URL = url
		}
	}
}

func submoduleCommitURLs(prepared PreparedContext) map[string]string {
	urls := map[string]string{}
	for _, submodule := range prepared.Submodules {
		for _, commit := range submodule.Commits {
			if commit.SHA == "" || commit.URL == "" {
				continue
			}
			urls[strings.ToLower(commit.SHA)] = commit.URL
		}
	}
	return urls
}

func resolveCommitURL(ref string, urls map[string]string) string {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if ref == "" {
		return ""
	}
	if url := urls[ref]; url != "" {
		return url
	}
	if len(ref) < 7 {
		return ""
	}
	matchedURL := ""
	for sha, url := range urls {
		if !strings.HasPrefix(sha, ref) {
			continue
		}
		if matchedURL != "" && matchedURL != url {
			return ""
		}
		matchedURL = url
	}
	return matchedURL
}

func renderBullet(bullet Bullet) string {
	var b strings.Builder
	if label := strings.TrimSpace(bullet.Label); label != "" {
		b.WriteString("**")
		b.WriteString(label)
		b.WriteString("**: ")
	}
	b.WriteString(strings.TrimSpace(bullet.Summary))
	if refs := renderRefs(bullet.Refs); len(refs) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(refs, ", "))
		b.WriteString(")")
	}
	return b.String()
}

func renderChildBullet(child ChildBullet) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(child.Summary))
	if refs := renderRefs(child.Refs); len(refs) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(refs, ", "))
		b.WriteString(")")
	}
	return b.String()
}

func renderRefs(refs []Reference) []string {
	rendered := make([]string, 0, len(refs))
	for _, ref := range refs {
		switch ref.Type {
		case "commit":
			if ref.URL != "" {
				rendered = append(rendered, ref.URL)
				continue
			}
			rendered = append(rendered, shortSHA(ref.Value))
		case "pr", "issue":
			rendered = append(rendered, "#"+strings.TrimSpace(ref.Value))
		}
	}
	return rendered
}

func renderEntry(entry ChangelogEntry) string {
	short := shortSHA(entry.SHA)
	if entry.URL != "" {
		return fmt.Sprintf("[%s](%s) %s", short, entry.URL, entry.Summary)
	}
	return fmt.Sprintf("%s %s", short, entry.Summary)
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func validateSections(sections []Section) []string {
	var errs []string
	allowed := []string{"Breaking Changes", "Security", "New Features", "Improvements", "Bug Fixes"}
	for _, sec := range sections {
		if strings.TrimSpace(sec.Heading) == "" {
			errs = append(errs, "section heading is empty")
		}
		if !slices.Contains(allowed, sec.Heading) {
			errs = append(errs, fmt.Sprintf("forbidden section heading %q", sec.Heading))
		}
		if len(sec.Bullets) == 0 {
			errs = append(errs, fmt.Sprintf("section %q has no bullets", sec.Heading))
		}
	}
	return errs
}

func validateDuplicateSectionHeadings(sections []Section) []string {
	seen := map[string]bool{}
	var errs []string
	for _, sec := range sections {
		if seen[sec.Heading] {
			errs = append(errs, fmt.Sprintf("duplicate section heading %q", sec.Heading))
		}
		seen[sec.Heading] = true
	}
	return errs
}

func validateBullets(sections []Section) []string {
	var errs []string
	for _, sec := range sections {
		for _, bullet := range sec.Bullets {
			if strings.TrimSpace(bullet.Label) == "" && bullet.Label != "" {
				errs = append(errs, fmt.Sprintf("section %q has blank bullet label", sec.Heading))
			}
			if strings.TrimSpace(bullet.Summary) == "" {
				errs = append(errs, fmt.Sprintf("section %q has empty bullet summary", sec.Heading))
			}
			if strings.Contains(bullet.Summary, "```") {
				errs = append(errs, fmt.Sprintf("section %q bullet summary contains code fence", sec.Heading))
			}
			if hasLowSignalContinuation(bullet.Summary) {
				errs = append(errs, fmt.Sprintf("section %q bullet %q has low-signal continuation; keep the parent line as a short headline and move extra facts into children", sec.Heading, bullet.Summary))
			}
			if err := summaryLengthError(sec.Heading, "bullet", bullet.Summary, maxNarrativeSummaryRunes); err != "" {
				errs = append(errs, err)
			}
			if len(bullet.Refs) == 0 {
				errs = append(errs, fmt.Sprintf("section %q bullet %q has no refs", sec.Heading, bullet.Summary))
			}
			errs = append(errs, validateRefs(sec.Heading, bullet.Summary, bullet.Refs)...)
			errs = append(errs, validateChildBullets(sec.Heading, bullet)...)
		}
	}
	return errs
}

func hasLowSignalContinuation(summary string) bool {
	for _, clause := range strings.Split(summary, ",") {
		if isLowSignalContinuation(clause) {
			return true
		}
	}
	return false
}

func isLowSignalContinuation(clause string) bool {
	clause = strings.ToLower(strings.TrimSpace(clause))
	if clause == "" {
		return false
	}
	for {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(clause, "and "), "while ")
		if trimmed == clause {
			break
		}
		clause = trimmed
	}

	for _, prefix := range []string{
		"enabling operators to ",
		"enabling users to ",
		"enabling admins to ",
		"enabling teams to ",
		"which enables operators to ",
		"which enables users to ",
		"which enables admins to ",
		"which enables teams to ",
		"that enables operators to ",
		"that enables users to ",
		"that enables admins to ",
		"that enables teams to ",
		"allowing operators to ",
		"allowing users to ",
		"allowing admins to ",
		"allowing teams to ",
		"helping operators ",
		"helping users ",
		"helping admins ",
		"helping teams ",
		"making it easier for operators ",
		"making it easier for users ",
		"making it easier for admins ",
		"making it easier for teams ",
	} {
		if strings.HasPrefix(clause, prefix) {
			return true
		}
	}

	for _, fragment := range []string{
		"reducing editing errors",
		"reducing configuration errors",
		"reducing operator errors",
		"reducing user errors",
		"reducing mistakes",
		"reducing confusion",
		"reducing friction",
		"improving usability",
		"improving operator confidence",
		"improving user confidence",
	} {
		if strings.Contains(clause, fragment) {
			return true
		}
	}

	return false
}

func validateChildBullets(heading string, bullet Bullet) []string {
	var errs []string
	for _, child := range bullet.Children {
		if strings.TrimSpace(child.Summary) == "" {
			errs = append(errs, fmt.Sprintf("section %q bullet %q has empty child summary", heading, bullet.Summary))
			continue
		}
		if strings.Contains(child.Summary, "```") {
			errs = append(errs, fmt.Sprintf("section %q bullet %q child summary contains code fence", heading, bullet.Summary))
		}
		if hasLowSignalContinuation(child.Summary) {
			errs = append(errs, fmt.Sprintf("section %q child %q has low-signal continuation; keep child lines to one concrete operator-facing fact", heading, child.Summary))
		}
		if err := summaryLengthError(heading, "child", child.Summary, maxChildSummaryRunes); err != "" {
			errs = append(errs, err)
		}
		errs = append(errs, validateRefs(heading, child.Summary, child.Refs)...)
	}
	return errs
}

func summaryLengthError(heading, role, summary string, limit int) string {
	n := utf8.RuneCountInString(strings.TrimSpace(summary))
	if n <= limit {
		return ""
	}
	if role == "child" {
		return fmt.Sprintf("section %q child %q is too long (%d characters); shorten the child line to one concrete fact", heading, summary, n)
	}
	return fmt.Sprintf("section %q bullet %q is too long (%d characters); keep the parent line as a short headline and move extra facts into children", heading, summary, n)
}

func validateRefs(heading, summary string, refs []Reference) []string {
	var errs []string
	seen := map[string]bool{}
	for _, ref := range refs {
		key := ref.Type + ":" + ref.Value
		if seen[key] {
			errs = append(errs, fmt.Sprintf("section %q bullet %q has duplicate ref %q", heading, summary, key))
			continue
		}
		seen[key] = true

		switch ref.Type {
		case "commit":
			if !isCommitSHA(ref.Value) {
				errs = append(errs, fmt.Sprintf("section %q bullet %q has invalid commit ref %q", heading, summary, ref.Value))
			}
		case "pr", "issue":
			if !isDigits(ref.Value) {
				errs = append(errs, fmt.Sprintf("section %q bullet %q has invalid %s ref %q", heading, summary, ref.Type, ref.Value))
			}
		default:
			errs = append(errs, fmt.Sprintf("section %q bullet %q has invalid ref type %q", heading, summary, ref.Type))
		}
	}
	return errs
}

func validateRenderedFullChangelogRequirements(doc Document, required []string) []string {
	var errs []string
	if len(doc.ParentChangelog) == 0 && len(doc.Submodules) == 0 {
		errs = append(errs, "missing rendered full changelog data")
	}
	found := map[string]bool{}
	for _, submodule := range doc.Submodules {
		found[submodule.Path] = true
	}
	for _, name := range required {
		if !found[name] {
			errs = append(errs, fmt.Sprintf("missing submodule full changelog group for %q", name))
		}
	}
	return errs
}

func isCommitSHA(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 7 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func isDigits(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
