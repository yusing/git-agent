package explore

import (
	"strings"
	"testing"
)

func TestValidateAnswersRequiresExactBatchIDs(t *testing.T) {
	first := "AAAAAAAAAAAAAAAAAAAAAAAAAA"
	second := "BBBBBBBBBBBBBBBBBBBBBBBBBB"
	validator := ValidateAnswers([]string{first, second})
	valid := `{"answers":[{"item_id":"` + second + `","answer":"two"},{"item_id":"` + first + `","answer":"one"}]}`
	if errs := validator(valid); len(errs) != 0 {
		t.Fatalf("valid answers rejected: %v", errs)
	}
	missing := `{"answers":[{"item_id":"` + first + `","answer":"one"}]}`
	if errs := validator(missing); len(errs) != 1 || !strings.Contains(errs[0], "do not match") {
		t.Fatalf("missing answer errors = %v", errs)
	}
}

func TestFollowUpPromptSelectsParentBranch(t *testing.T) {
	parent := &Session{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAA", Answer: "prior branch answer"}
	prompt, err := UserPrompt(parent, []PromptItem{{
		ItemID: "BBBBBBBBBBBBBBBBBBBBBBBBBB", Question: "what changed?",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{parent.ID, parent.Answer, "what changed?"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
}
