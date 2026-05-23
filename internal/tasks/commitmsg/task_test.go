package commitmsg

import (
	"strings"
	"testing"
)

func TestValidateRejectsFencesAndAmendProcessPhrasing(t *testing.T) {
	t.Parallel()

	errs := Validate(ModeAmend, "Update parser\n\nThis amend also fixes docs.\n```")
	if len(errs) < 2 {
		t.Fatalf("expected multiple validation errors, got %v", errs)
	}
}

func TestPromptsNameRequiredScope(t *testing.T) {
	t.Parallel()

	if got := UserPrompt(ModeNormal, 30, 24); !containsAll(got, "Current limits: 30 total model steps, 24 total tool calls.", "staged diff", "git_staged_diff", "ignore unstaged", "task IDs") {
		t.Fatalf("normal prompt missing staged scope: %s", got)
	}
	if got := SystemPrompt(ModeAmend); !containsAll(got, "final amended commit", "versus its parent", "one commit", "Do not narrate a delta or process") {
		t.Fatalf("amend prompt missing final commit scope: %s", got)
	}
	if got := UserPrompt(ModeAmend, 12, 9); !containsAll(got, "Current limits: 12 total model steps, 9 total tool calls.", "How to read the evidence", "authoritative", "do not dual-narrate", "tone / scope / task IDs") {
		t.Fatalf("amend user prompt missing evidence framing: %s", got)
	}
}

func TestShapeWrapsBodyAndKeepsSubject(t *testing.T) {
	t.Parallel()

	got := Shape("Add parser\n\n" + strings.Repeat("word ", 30))
	if !strings.HasPrefix(got, "Add parser\n\n") {
		t.Fatalf("missing subject/body split: %q", got)
	}
	for _, line := range strings.Split(got, "\n")[2:] {
		if len(line) > 72 {
			t.Fatalf("line too long: %d %q", len(line), line)
		}
	}
}

func TestPromptsReflectExampleStyleExpectations(t *testing.T) {
	t.Parallel()

	if got := SystemPrompt(ModeNormal); !containsAll(got, "type, scope, and impact", "Body optional", "three short paragraphs") {
		t.Fatalf("normal system prompt missing style guidance: %s", got)
	}
	if got := UserPrompt(ModeNormal, 30, 24); !containsAll(got, "git_staged_status", "recent commits", "full staged diff") {
		t.Fatalf("normal user prompt missing structured context guidance: %s", got)
	}
	if got := UserPrompt(ModeAmend, 30, 24); !containsAll(got, "Previous HEAD message is reference only", "polish wording only") {
		t.Fatalf("amend prompt missing example-aligned reuse guidance: %s", got)
	}
}

func containsAll(text string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			return false
		}
	}
	return true
}
