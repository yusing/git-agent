package explore

import (
	"slices"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/openai"
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

func TestSystemPromptForQueryTarget(t *testing.T) {
	if got := SystemPromptFor(QueryTargetUniversal); got != SystemPrompt {
		t.Fatal("universal query target changed the existing system prompt")
	}
	for _, target := range []QueryTarget{
		QueryTargetDiagnose, QueryTargetChange, QueryTargetBehavior, QueryTargetOwner,
	} {
		if got := SystemPromptFor(target); got != TargetSystemPrompt {
			t.Fatalf("system prompt for %q is not the stable target prompt: %s", target, got)
		}
		wantInstruction := "Query target: " + string(target) + "\n" + target.Instructions()
		if got := InitialTargetInstruction(target); got != wantInstruction {
			t.Fatalf("initial target instruction for %q = %q, want %q", target, got, wantInstruction)
		}
		parsed, err := ParseQueryTarget(string(target))
		if err != nil || parsed != target {
			t.Fatalf("parse target %q = %q, %v", target, parsed, err)
		}
	}
	if got := InitialTargetInstruction(QueryTargetUniversal); got != "" {
		t.Fatalf("universal initial target instruction = %q, want empty", got)
	}
	universalParent := &Session{InstructionTarget: QueryTargetUniversal}
	if got := SystemPromptTarget(universalParent, QueryTargetOwner); got != QueryTargetOwner {
		t.Fatalf("universal-to-target prompt target = %q, want owner", got)
	}
	targetedParent := &Session{InstructionTarget: QueryTargetDiagnose}
	if got := SystemPromptTarget(targetedParent, QueryTargetOwner); got != QueryTargetDiagnose {
		t.Fatalf("target-to-target prompt target = %q, want stable diagnose family", got)
	}
	if _, err := ParseQueryTarget("review"); err == nil {
		t.Fatal("unsupported query target was accepted")
	}
}

func TestFollowUpInputAppendsOneTargetChangeWithoutRewritingPrefix(t *testing.T) {
	parent := Session{
		ActiveTarget: QueryTargetDiagnose,
		History: []openai.Item{
			openai.NewMessage("user", "first"),
			openai.NewMessage("assistant", "answer"),
		},
	}
	same := FollowUpInput(parent, "same target", QueryTargetDiagnose)
	if len(same) != len(parent.History)+1 || !slices.Equal(same[:len(parent.History)], parent.History) {
		t.Fatalf("same-target input changed replay prefix: %#v", same)
	}
	changed := FollowUpInput(parent, "find the owner", QueryTargetOwner)
	if len(changed) != len(parent.History)+2 || !slices.Equal(changed[:len(parent.History)], parent.History) {
		t.Fatalf("changed-target input changed replay prefix: %#v", changed)
	}
	change := changed[len(parent.History)]
	want := "Query target changed: owner\n" + QueryTargetOwner.Instructions()
	if change.Role != "developer" || change.Content != want {
		t.Fatalf("target-change message = %#v, want developer %q", change, want)
	}
	if last := changed[len(changed)-1]; last.Role != "user" || last.Content != "find the owner" {
		t.Fatalf("follow-up user message = %#v", last)
	}
}
