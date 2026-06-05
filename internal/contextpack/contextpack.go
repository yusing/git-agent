package contextpack

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	GoGeneratedRule = "go-generated-comment"
)

var goGeneratedLinePattern = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

type ArtifactRef struct {
	Ref       string `json:"ref"`
	Path      string `json:"path,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type ChangeOverview struct {
	Files          int            `json:"files"`
	Adds           int            `json:"adds"`
	Deletes        int            `json:"deletes"`
	Statuses       map[string]int `json:"statuses,omitempty"`
	Extensions     map[string]int `json:"extensions,omitempty"`
	GeneratedFiles int            `json:"generated_files,omitempty"`
	DominantScope  string         `json:"dominant_scope,omitempty"`
	Summary        string         `json:"summary,omitempty"`
}

type FileSummary struct {
	Path            string `json:"path"`
	Status          string `json:"status,omitempty"`
	Adds            int    `json:"adds,omitempty"`
	Deletes         int    `json:"deletes,omitempty"`
	Extension       string `json:"extension,omitempty"`
	Generated       bool   `json:"generated,omitempty"`
	GeneratedByRule string `json:"generated_by_rule,omitempty"`
}

type ChangeGroup struct {
	ID              string        `json:"id"`
	Match           string        `json:"match"`
	Count           int           `json:"count"`
	Statuses        []string      `json:"statuses,omitempty"`
	Extensions      []string      `json:"extensions,omitempty"`
	Adds            int           `json:"adds"`
	Deletes         int           `json:"deletes"`
	Generated       bool          `json:"generated,omitempty"`
	GeneratedByRule string        `json:"generated_by_rule,omitempty"`
	Summary         string        `json:"summary"`
	Samples         []FileSummary `json:"samples,omitempty"`
	TopChurn        []FileSummary `json:"top_churn,omitempty"`
	RawRefs         []ArtifactRef `json:"raw_refs,omitempty"`
}

type ContextPack struct {
	Overview  ChangeOverview `json:"overview"`
	Groups    []ChangeGroup  `json:"groups,omitempty"`
	Outliers  []FileSummary  `json:"outliers,omitempty"`
	Artifacts []ArtifactRef  `json:"artifacts,omitempty"`
}

type FileFact struct {
	Path       string
	Status     string
	Adds       int
	Deletes    int
	IsBinary   bool
	Header     string
	DiffRef    ArtifactRef
	ContentRef ArtifactRef
}

type Options struct {
	OutlierThreshold int
	SampleLimit      int
	TopChurnLimit    int
}

func Build(files []FileFact, opts Options) ContextPack {
	opts = withDefaults(opts)
	facts := normalizeFacts(files)
	pack := ContextPack{
		Overview:  overview(facts),
		Artifacts: artifacts(facts),
	}
	if len(facts) == 0 {
		return pack
	}

	grouped := map[string][]FileSummary{}
	for _, fact := range facts {
		summary := summarizeFile(fact)
		key := groupKey(summary)
		grouped[key] = append(grouped[key], summary)
	}

	keys := mapsKeys(grouped)
	for _, key := range keys {
		items := grouped[key]
		if len(facts) > 20 && len(items) <= opts.OutlierThreshold {
			pack.Outliers = append(pack.Outliers, items...)
			continue
		}
		pack.Groups = append(pack.Groups, buildGroup(items, opts))
	}
	sortFiles(pack.Outliers)
	return pack
}

func IsGoGenerated(path, header string) bool {
	if filepath.Ext(path) != ".go" {
		return false
	}
	inBlock := false
	for line := range strings.SplitSeq(header, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if goGeneratedLinePattern.MatchString(trimmed) {
			return true
		}
		if inBlock {
			if strings.Contains(trimmed, "*/") {
				inBlock = false
			}
			continue
		}
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, "/*") {
			if !strings.Contains(trimmed, "*/") {
				inBlock = true
			}
			continue
		}
		return false
	}
	return false
}

func IsLargeGeneratedHeavy(pack ContextPack) bool {
	return pack.Overview.Files > 100 &&
		pack.Overview.GeneratedFiles*100 >= pack.Overview.Files*80
}

func withDefaults(opts Options) Options {
	if opts.OutlierThreshold <= 0 {
		opts.OutlierThreshold = 1
	}
	if opts.SampleLimit <= 0 {
		opts.SampleLimit = 3
	}
	if opts.TopChurnLimit <= 0 {
		opts.TopChurnLimit = 3
	}
	return opts
}

func normalizeFacts(files []FileFact) []FileFact {
	facts := make([]FileFact, 0, len(files))
	for _, file := range files {
		if file.Path == "" {
			continue
		}
		file.Path = filepath.ToSlash(file.Path)
		if file.Status == "" {
			file.Status = "modified"
		}
		facts = append(facts, file)
	}
	slices.SortFunc(facts, func(a, b FileFact) int {
		return strings.Compare(a.Path, b.Path)
	})
	return facts
}

func summarizeFile(fact FileFact) FileSummary {
	summary := FileSummary{
		Path:      fact.Path,
		Status:    fact.Status,
		Adds:      fact.Adds,
		Deletes:   fact.Deletes,
		Extension: extension(fact.Path),
	}
	if IsGoGenerated(fact.Path, fact.Header) {
		summary.Generated = true
		summary.GeneratedByRule = GoGeneratedRule
	}
	return summary
}

func overview(facts []FileFact) ChangeOverview {
	out := ChangeOverview{
		Files:      len(facts),
		Statuses:   map[string]int{},
		Extensions: map[string]int{},
	}
	dirCounts := map[string]int{}
	for _, fact := range facts {
		summary := summarizeFile(fact)
		out.Adds += fact.Adds
		out.Deletes += fact.Deletes
		out.Statuses[summary.Status]++
		out.Extensions[summary.Extension]++
		if summary.Generated {
			out.GeneratedFiles++
		}
		dirCounts[dirName(fact.Path)]++
	}
	out.DominantScope = dominantKey(dirCounts)
	out.Summary = overviewSummary(out)
	return out
}

func overviewSummary(out ChangeOverview) string {
	if out.Files == 0 {
		return "No changed files."
	}
	if out.GeneratedFiles > 0 {
		return fmt.Sprintf("%d files changed (+%d/-%d); %d match standards-based generated-file markers.", out.Files, out.Adds, out.Deletes, out.GeneratedFiles)
	}
	return fmt.Sprintf("%d files changed (+%d/-%d).", out.Files, out.Adds, out.Deletes)
}

func artifacts(facts []FileFact) []ArtifactRef {
	seen := map[string]bool{}
	var out []ArtifactRef
	for _, fact := range facts {
		for _, ref := range []ArtifactRef{fact.DiffRef, fact.ContentRef} {
			if ref.Ref == "" || seen[ref.Ref] {
				continue
			}
			seen[ref.Ref] = true
			out = append(out, ref)
		}
	}
	slices.SortFunc(out, func(a, b ArtifactRef) int {
		return strings.Compare(a.Ref, b.Ref)
	})
	return out
}

func groupKey(file FileSummary) string {
	return strings.Join([]string{
		file.Status,
		fmt.Sprintf("generated=%t", file.Generated),
		file.GeneratedByRule,
		dirName(file.Path),
		file.Extension,
	}, "|")
}

func buildGroup(items []FileSummary, opts Options) ChangeGroup {
	sortFiles(items)
	group := ChangeGroup{
		ID:              groupID(items),
		Match:           groupMatch(items),
		Count:           len(items),
		Statuses:        sortedUnique(items, func(item FileSummary) string { return item.Status }),
		Extensions:      sortedUnique(items, func(item FileSummary) string { return item.Extension }),
		Generated:       items[0].Generated,
		GeneratedByRule: items[0].GeneratedByRule,
		Samples:         representativeSamples(items, opts.SampleLimit),
		TopChurn:        topChurn(items, opts.TopChurnLimit),
	}
	for _, item := range items {
		group.Adds += item.Adds
		group.Deletes += item.Deletes
	}
	group.Summary = groupSummary(group)
	return group
}

func groupID(items []FileSummary) string {
	input := groupMatch(items) + "|" + strings.Join(sortedUnique(items, func(item FileSummary) string { return item.Status }), ",")
	sum := sha256.Sum256([]byte(input))
	return "group_" + hex.EncodeToString(sum[:])[:12]
}

func groupMatch(items []FileSummary) string {
	dir := dirName(items[0].Path)
	ext := items[0].Extension
	if dir == "." {
		return "*" + ext
	}
	return dir + "/*" + ext
}

func groupSummary(group ChangeGroup) string {
	status := "Changed"
	if len(group.Statuses) == 1 {
		status = statusVerb(group.Statuses[0])
	}
	generated := ""
	if group.Generated {
		generated = " generated"
	}
	return fmt.Sprintf("%s %d%s %s files under %s (+%d/-%d).", status, group.Count, generated, strings.Join(group.Extensions, ","), strings.TrimSuffix(group.Match, "/*"+strings.Join(group.Extensions, ",")), group.Adds, group.Deletes)
}

func representativeSamples(items []FileSummary, limit int) []FileSummary {
	if len(items) <= limit {
		return slices.Clone(items)
	}
	candidates := []FileSummary{
		items[0],
		items[len(items)/2],
		topChurn(items, 1)[0],
	}
	return uniqueFiles(candidates, limit)
}

func topChurn(items []FileSummary, limit int) []FileSummary {
	copied := slices.Clone(items)
	slices.SortFunc(copied, func(a, b FileSummary) int {
		left := a.Adds + a.Deletes
		right := b.Adds + b.Deletes
		if left != right {
			return right - left
		}
		return strings.Compare(a.Path, b.Path)
	})
	if len(copied) > limit {
		copied = copied[:limit]
	}
	return copied
}

func uniqueFiles(items []FileSummary, limit int) []FileSummary {
	seen := map[string]bool{}
	var out []FileSummary
	for _, item := range items {
		if seen[item.Path] {
			continue
		}
		seen[item.Path] = true
		out = append(out, item)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func sortedUnique(items []FileSummary, value func(FileSummary) string) []string {
	seen := map[string]bool{}
	for _, item := range items {
		text := value(item)
		if text != "" {
			seen[text] = true
		}
	}
	return mapsKeys(seen)
}

func sortFiles(items []FileSummary) {
	slices.SortFunc(items, func(a, b FileSummary) int {
		return strings.Compare(a.Path, b.Path)
	})
}

func mapsKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func dominantKey(counts map[string]int) string {
	var best string
	var bestCount int
	for key, count := range counts {
		if count > bestCount || (count == bestCount && key < best) {
			best = key
			bestCount = count
		}
	}
	return best
}

func dirName(path string) string {
	dir := filepath.ToSlash(filepath.Dir(path))
	if dir == "" {
		return "."
	}
	return dir
}

func extension(path string) string {
	ext := filepath.Ext(path)
	if ext == "" {
		return "<none>"
	}
	return ext
}

func statusVerb(status string) string {
	switch status {
	case "A", "added":
		return "Added"
	case "D", "deleted":
		return "Deleted"
	case "R", "renamed":
		return "Renamed"
	case "M", "modified":
		return "Modified"
	default:
		return "Changed"
	}
}
