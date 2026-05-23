package releasenote

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yusing/git-agent/internal/textutil"
)

type Request struct {
	BaseRef    string
	ReleaseRef string
}

type ValidationOptions struct {
	RequireFullChangelog bool
	RequiredSubmodules   []string
}

func SystemPrompt() string {
	return textutil.NormalizePrompt(`
You generate GitHub release bodies for deployers and operators.
Use tools to inspect the requested range before writing.
Peel and validate both refs.
Inspect parent repository commits first.
Inspect submodule gitlink changes and include submodule commit groups only when the gitlink moved and local history is available.
When submodule gitlink changes are present and local submodule history is available, the Full Changelog must include a linked subgroup for each changed submodule.
Use repo_summary remotes to infer repository ownership for links; do not invent links when ownership is unavailable.
Return only the release body.
First printable line must start with "### ".
No preamble.
No duplicate section narratives.
Use plain ASCII punctuation.
Prioritize user-facing and operator-facing impact over internal implementation churn.
Avoid narrating internal sync noise, generated docs churn, schema churn, or mechanical dependency bumps unless they materially change deployment behavior.
Prefer heading priority based on deployer value. Avoid filler headings that add no operator signal.
Include "### Full Changelog" when the range touched code.
Parent-repo commits appear first in the full changelog.
Submodule groups appear after parent commits.
Use example Full Changelog shape:
- parent repository entries as top-level "- ..." bullets
- submodule groups as linked subgroup headings like "[**name**](url)"
- subgroup commit entries as indented "  - ..." bullets under that linked heading
Keep parent SHAs in the parent list and submodule SHAs in their own submodule groups.
Respect link ownership rules. Parent links stay with parent entries. Submodule links stay inside submodule groups.
Avoid common misoutputs: preambles, duplicate summary stories, forbidden headings, mixed parent/submodule ownership, and invented links.
`)
}

func UserPrompt(base, release string, maxSteps, maxToolCalls int) string {
	return textutil.NormalizePrompt(fmt.Sprintf(`
Current limits: %d total model steps, %d total tool calls. Spend budget carefully and finish within it.

Generate a release note for range %s..%s.
Audience: deployers and operators upgrading a live deployment.
Rules:
- first printable line starts with a level-3 heading
- plain ASCII punctuation only
- no forbidden preamble
- forbidden headings: avoid generic filler such as "Overview", "Notes", or "Misc"
- omit Summary when patch/minor range already has a clearer story in concrete sections
- parent commits first
- submodule groups only after parent commits in Full Changelog
- subgroup format: linked subgroup heading on its own line, then indented subgroup bullets below it
- each changed submodule with available local history must get its own subgroup in Full Changelog
- preserve parent/submodule ownership of SHAs and links
- downplay internal sync, generated docs, and schema churn unless deployers must act on them
- follow link ownership rules; do not invent repo or commit links when ownership is unclear
- misoutputs to avoid: repeated narrative across sections, parent bullets after submodule groups, mixed ownership of links/SHAs
Include ### Full Changelog when the range touched code.
Use resolve_ref for both refs, git_log_range for parent commits, repo_kind, gitmodules_table, and submodule_gitlink_range.
Use submodule_log_range only for changed submodule gitlinks.
Return only the release body.
`, maxSteps, maxToolCalls, base, release))
}

func Validate(output string) []string {
	return ValidateWithOptions(output, ValidationOptions{})
}

func ValidateWithRequirements(output string, requireFullChangelog bool) []string {
	return ValidateWithOptions(output, ValidationOptions{RequireFullChangelog: requireFullChangelog})
}

func ValidateWithOptions(output string, options ValidationOptions) []string {
	var errs []string
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		errs = append(errs, "output is empty")
		return errs
	}
	if !strings.HasPrefix(trimmed, "### ") {
		errs = append(errs, "first printable line must start with ###")
	}
	lower := strings.ToLower(trimmed)
	for _, phrase := range []string{"here is", "release note:", "preamble:"} {
		if strings.HasPrefix(lower, phrase) || strings.Contains(lower, "\n"+phrase) {
			errs = append(errs, fmt.Sprintf("forbidden preamble/commentary %q", phrase))
		}
	}
	if strings.Contains(trimmed, "```") {
		errs = append(errs, "output contains code fences")
	}
	if options.RequireFullChangelog && !strings.Contains(trimmed, "### Full Changelog") {
		errs = append(errs, "missing ### Full Changelog")
	}
	for _, heading := range duplicateHeadings(trimmed) {
		errs = append(errs, fmt.Sprintf("duplicate heading %q", heading))
	}
	errs = append(errs, validateHeadingStructure(trimmed)...)
	if options.RequireFullChangelog {
		errs = append(errs, validateFullChangelogOrder(trimmed)...)
		errs = append(errs, validateRequiredSubmoduleGroups(trimmed, options.RequiredSubmodules)...)
	}
	return errs
}

func duplicateHeadings(text string) []string {
	seen := map[string]bool{}
	var duplicates []string
	for line := range strings.SplitSeq(text, "\n") {
		if !strings.HasPrefix(line, "### ") {
			continue
		}
		heading := strings.TrimSpace(line)
		if seen[heading] && !slices.Contains(duplicates, heading) {
			duplicates = append(duplicates, heading)
		}
		seen[heading] = true
	}
	return duplicates
}

func validateHeadingStructure(text string) []string {
	lines := strings.Split(text, "\n")
	var errs []string
	lastHeading := ""
	sectionHasContent := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "### ") {
			if lastHeading != "" && !sectionHasContent {
				errs = append(errs, fmt.Sprintf("heading %q has no content", lastHeading))
			}
			lastHeading = line
			sectionHasContent = false
			continue
		}
		if line != "" {
			sectionHasContent = true
		}
	}
	if lastHeading != "" && !sectionHasContent {
		errs = append(errs, fmt.Sprintf("heading %q has no content", lastHeading))
	}
	return errs
}

func validateFullChangelogOrder(text string) []string {
	lines := strings.Split(text, "\n")
	inFullChangelog := false
	currentSection := fullChangelogNone
	var errs []string
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "### ") {
			inFullChangelog = line == "### Full Changelog"
			currentSection = fullChangelogNone
			continue
		}
		if !inFullChangelog || line == "" {
			continue
		}
		if isSubmoduleGroupHeading(line) {
			currentSection = fullChangelogSubmodule
			continue
		}
		if isSubmoduleBullet(raw) {
			currentSection = fullChangelogSubmodule
			continue
		}
		if strings.HasPrefix(line, "- ") {
			switch currentSection {
			case fullChangelogNone, fullChangelogParent:
				currentSection = fullChangelogParent
			case fullChangelogSubmodule:
				if looksLikeParentBullet(line) {
					errs = append(errs, "parent full changelog entries must appear before submodule groups")
					return errs
				}
			}
		}
	}
	return errs
}

type fullChangelogSection int

const (
	fullChangelogNone fullChangelogSection = iota
	fullChangelogParent
	fullChangelogSubmodule
)

func looksLikeParentBullet(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if strings.Contains(lower, "/") || strings.Contains(lower, ":") {
		return false
	}
	if strings.Contains(lower, "[") || strings.Contains(lower, "]") || strings.Contains(lower, "(") || strings.Contains(lower, ")") {
		return false
	}
	if strings.Contains(lower, "submodule") {
		return false
	}
	if strings.Contains(lower, "commit") {
		return false
	}
	if strings.HasPrefix(lower, "- update ") || strings.HasPrefix(lower, "- bump ") {
		return true
	}
	return false
}

func isSubmoduleGroupHeading(line string) bool {
	return strings.HasPrefix(line, "#### ") || (strings.HasPrefix(line, "[**") && strings.Contains(line, "](") && strings.HasSuffix(line, ")"))
}

func isSubmoduleBullet(raw string) bool {
	return strings.HasPrefix(raw, "  - ") || strings.HasPrefix(raw, "\t- ")
}

func validateRequiredSubmoduleGroups(text string, required []string) []string {
	if len(required) == 0 {
		return nil
	}
	found := map[string]bool{}
	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !isSubmoduleGroupHeading(trimmed) {
			continue
		}
		for _, name := range required {
			if trimmed == "#### "+name || strings.Contains(trimmed, "[**"+name+"**]") {
				found[name] = true
			}
		}
	}
	var errs []string
	for _, name := range required {
		if !found[name] {
			errs = append(errs, fmt.Sprintf("missing submodule full changelog group for %q", name))
		}
	}
	return errs
}
