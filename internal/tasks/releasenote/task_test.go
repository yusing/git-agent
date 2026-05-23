package releasenote

import (
	"strings"
	"testing"
)

func TestValidateRequiresHeadingAndRejectsDuplicateHeadings(t *testing.T) {
	t.Parallel()

	errs := Validate("Release note:\n\n### Summary\nx\n### Summary\ny")
	if len(errs) < 2 {
		t.Fatalf("expected heading and duplicate errors, got %v", errs)
	}
}

func TestValidateRequiresFullChangelogWhenRangeTouchedCode(t *testing.T) {
	t.Parallel()

	errs := ValidateWithRequirements("### Summary\nChanged parser", true)
	if len(errs) == 0 {
		t.Fatal("expected missing full changelog error")
	}
}

func TestValidateRejectsEmptySectionsAndOutOfOrderFullChangelog(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"### Summary",
		"",
		"### Full Changelog",
		"- parent change",
		"#### ui/submodule",
		"- [ui/submodule] submodule change",
		"- update parser after subgroup",
	}, "\n")
	errs := ValidateWithRequirements(output, true)
	if len(errs) < 2 {
		t.Fatalf("expected section-content and ordering errors, got %v", errs)
	}
}

func TestValidateAllowsSubmoduleBulletsInsideSubgroup(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"### Summary",
		"Deploy safer parser updates.",
		"### Full Changelog",
		"- parent change",
		"",
		"[**ui/submodule**](https://example.com/ui/submodule)",
		"",
		"  - submodule change",
		"",
		"  - another submodule change",
	}, "\n")
	errs := ValidateWithRequirements(output, true)
	if len(errs) != 0 {
		t.Fatalf("expected valid subgroup bullets, got %v", errs)
	}
}

func TestValidateAllowsExampleStyleReleaseNote(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"### Breaking Changes",
		"",
		"- Remove path_patterns from route config and payloads.",
		"",
		"### Bug Fixes",
		"",
		"- FileServer routes now run middleware after rules settle.",
		"",
		"### Improvements",
		"",
		"- Setup script and README snippets use POSIX-friendly sh patterns.",
		"",
		"### Full Changelog",
		"",
		"- refactor(route): remove path_patterns (deadbeefdeadbeefdeadbeefdeadbeefdeadbeef)",
		"",
		"- fix(route): wrap FileServer handlers with middleware after rules (feedfacefeedfacefeedfacefeedfacefeedface)",
		"",
		"[**webui**](https://github.com/yusing/godoxy-webui)",
		"",
		"  - chore: update wiki (https://github.com/yusing/godoxy-webui/commit/f64b2e8f85eec683b8e9258c23e3398c4ba828ed)",
		"",
		"  - chore(types,ui): remove path_patterns from types and ui (https://github.com/yusing/godoxy-webui/commit/77af25899ff4c4de7de5bb81b9bf4ac7d276e4aa)",
	}, "\n")
	errs := ValidateWithRequirements(output, true)
	if len(errs) != 0 {
		t.Fatalf("expected example-style release note to validate, got %v", errs)
	}
}

func TestValidateRejectsParentBulletAfterLinkedSubgroup(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"### Summary",
		"Deploy safer parser updates.",
		"### Full Changelog",
		"- parent change",
		"",
		"[**webui**](https://github.com/yusing/godoxy-webui)",
		"",
		"  - submodule change",
		"",
		"- update parser after subgroup",
	}, "\n")
	errs := ValidateWithRequirements(output, true)
	if len(errs) == 0 {
		t.Fatal("expected ordering error after linked subgroup")
	}
}

func TestValidateRequiresChangedSubmoduleGroupsWhenRequested(t *testing.T) {
	t.Parallel()

	output := strings.Join([]string{
		"### Routing behavior changes",
		"",
		"- Operators should validate routing config after upgrade.",
		"",
		"### Full Changelog",
		"",
		"- fix(route): wrap FileServer handlers with middleware after rules (deadbeefdeadbeefdeadbeefdeadbeefdeadbeef)",
	}, "\n")

	errs := ValidateWithOptions(output, ValidationOptions{
		RequireFullChangelog: true,
		RequiredSubmodules:   []string{"webui", "goutils"},
	})
	if len(errs) < 2 {
		t.Fatalf("expected missing submodule group errors, got %v", errs)
	}
}

func TestUserPromptContainsRangeAndSubmoduleGuidance(t *testing.T) {
	t.Parallel()

	got := UserPrompt("v1.0.0", "v1.1.0", 30, 24)
	for _, want := range []string{
		"Current limits: 30 total model steps, 24 total tool calls.",
		"v1.0.0..v1.1.0",
		"submodule_gitlink_range",
		"submodule_log_range",
		"deployers and operators",
		"plain ASCII punctuation",
		"Full Changelog",
		"forbidden headings",
		"omit Summary",
		"misoutputs to avoid",
		"link ownership rules",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q: %s", want, got)
		}
	}
}
