package followup

import (
	"slices"
	"testing"

	"github.com/yusing/git-agent/internal/openai"
)

func TestNextPreservesLineageAndResetsAtSharedLimit(t *testing.T) {
	parent := Lineage{ID: "parent", Depth: 0, PromptCacheKey: "cache:root"}
	for depth := 1; depth <= MaxDepth; depth++ {
		next := Next(&parent, "child", "cache:new")
		if next.ParentID != parent.ID || next.Depth != depth || next.PromptCacheKey != "cache:root" {
			t.Fatalf("depth %d lineage = %#v", depth, next)
		}
		parent = next
		parent.ID = "parent"
	}
	reset := Next(&parent, "reset", "cache:reset")
	if reset.ParentID != "" || reset.Depth != 0 || reset.PromptCacheKey != "cache:reset" {
		t.Fatalf("reset lineage = %#v", reset)
	}
}

func TestCompactTreeRoundTripsCommonAndBranchInput(t *testing.T) {
	common := []openai.Item{
		openai.NewMessage("developer", "shared"),
		openai.NewMessage("user", "inspect"),
	}
	leaves := []Leaf{
		{Scope: "first", Model: "model-a", Input: append(slices.Clone(common), openai.NewMessage("assistant", "first"))},
		{Scope: "second", Model: "model-b", Input: append(slices.Clone(common), openai.NewMessage("assistant", "second"))},
	}
	tree := Compact(leaves)
	if !slices.Equal(tree.Input, common) || len(tree.Leaves) != 2 {
		t.Fatalf("compacted tree = %#v", tree)
	}
	expanded := tree.Expanded()
	for index := range leaves {
		if !slices.Equal(expanded[index].Input, leaves[index].Input) {
			t.Fatalf("expanded leaf %d = %#v, want %#v", index, expanded[index], leaves[index])
		}
	}
	expanded[0].Input[0].Content = "mutated"
	if tree.Input[0].Content != "shared" {
		t.Fatal("expanded input mutated persisted common prefix")
	}
}
