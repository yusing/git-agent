package followup

import "testing"

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
