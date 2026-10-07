package releasenote

import (
	"strings"
	"testing"

	json "encoding/json/v2"
)

func TestPromptBudgetPreservesInventoryAndLocalEvidence(t *testing.T) {
	commit := PreparedCommit{SHA: strings.Repeat("a", 40), Summary: "feat: operator change", Message: strings.Repeat("配置 ", 1000), PatchExcerpt: strings.Repeat("+configuration\n", 1000)}
	for range 100 {
		commit.Files = append(commit.Files, PreparedChangedFile{Path: "config/example.go", Status: "M"})
	}
	subCommit := commit
	subCommit.SHA = strings.Repeat("b", 40)
	prepared := PreparedContext{Range: "base..release", BaseSHA: "base", ReleaseSHA: "release", ParentCommits: []PreparedCommit{commit}, Submodules: []PreparedSubmodule{{Path: "webui", LocalHistoryAvailable: true, Commits: []PreparedCommit{subCommit}}}}
	prepared.CandidateItems = candidateItems(prepared.ParentCommits, prepared.Submodules)
	before := prepared.Render()
	fullPrompt := UserPrompt(prepared, 30, 24)
	if strings.Contains(fullPrompt, `"evidence":`) || !strings.Contains(fullPrompt, commit.Message[:30]) {
		t.Fatal("prompt must deduplicate candidate evidence and keep commit messages")
	}
	if got := UserPromptWithinBudget(prepared, 30, 24, func(string) bool { return true }); got != fullPrompt {
		t.Fatal("fitting evidence changed")
	}
	for _, budget := range []int{8000, 1} {
		prompt := UserPromptWithinBudget(prepared, 30, 24, func(prompt string) bool {
			data, err := json.Marshal(prompt)
			if err != nil {
				t.Fatal(err)
			}
			return len(data) < budget
		})
		_, context, _ := strings.Cut(prompt, "<prepared_release_note_context format=\"json\">\n")
		context, _, _ = strings.Cut(context, "\n</prepared_release_note_context>")
		var compact PreparedContext
		if err := json.Unmarshal([]byte(context), &compact); err != nil {
			t.Fatal(err)
		}
		if !compact.EvidenceCompacted || compact.BaseSHA != prepared.BaseSHA || compact.ReleaseSHA != prepared.ReleaseSHA || len(compact.ParentCommits) != 1 || len(compact.Submodules) != 1 || compact.Submodules[0].Path != "webui" || len(compact.Submodules[0].Commits) != 1 || len(compact.CandidateItems) != 2 {
			t.Fatal("compaction lost range, submodule ownership, or inventory")
		}
		for i, got := range []PreparedCommit{compact.ParentCommits[0], compact.Submodules[0].Commits[0]} {
			want := []PreparedCommit{commit, subCommit}[i]
			if got.SHA != want.SHA || got.Summary != want.Summary || got.OmittedFiles != len(want.Files)-len(got.Files) {
				t.Fatal("compaction lost commit identity or omitted-file count")
			}
		}
		if budget > 1 {
			data, _ := json.Marshal(prompt)
			if len(data) >= budget {
				t.Fatalf("prompt has %d bytes, budget %d", len(data), budget)
			}
		}
	}
	if prepared.Render() != before {
		t.Fatal("prompt compaction changed full local evidence")
	}
}
