// Package followup owns shared replay, lineage, and cache continuity.
package followup

import (
	"slices"

	"github.com/yusing/git-agent/internal/openai"
)

const (
	MaxDepth      = 3
	MaxStateBytes = 64 << 20
)

// Lineage identifies one persisted turn in a follow-up tree.
type Lineage struct {
	ID             string
	ParentID       string
	Depth          int
	PromptCacheKey string
}

// Next creates a context-preserving descendant or resets an exhausted lineage.
func Next(parent *Lineage, id, promptCacheKey string) Lineage {
	next := Lineage{ID: id, PromptCacheKey: promptCacheKey}
	if parent != nil && CanReplay(parent.Depth) {
		next.ParentID = parent.ID
		next.Depth = parent.Depth + 1
		next.PromptCacheKey = parent.PromptCacheKey
	}
	return next
}

// CanReplay reports whether another context-preserving follow-up is available.
func CanReplay(depth int) bool {
	return depth < MaxDepth
}

// Leaf is one terminal conversation in a possibly branched operation.
type Leaf struct {
	Scope           string        `json:"scope,omitempty"`
	Model           string        `json:"model"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
	TurnState       string        `json:"turn_state,omitempty"`
	Input           []openai.Item `json:"input,omitempty"`
}

// Tree stores the common replay prefix once and branch-specific suffixes per leaf.
type Tree struct {
	Input  []openai.Item `json:"input,omitempty"`
	Leaves []Leaf        `json:"leaves,omitempty"`
}

// Compact stores the longest common input prefix once.
func Compact(leaves []Leaf) Tree {
	if len(leaves) == 0 {
		return Tree{}
	}
	commonLength := len(leaves[0].Input)
	for _, leaf := range leaves[1:] {
		commonLength = min(commonLength, len(leaf.Input))
		for index := range commonLength {
			if leaf.Input[index] != leaves[0].Input[index] {
				commonLength = index
				break
			}
		}
	}
	tree := Tree{
		Input:  slices.Clone(leaves[0].Input[:commonLength]),
		Leaves: make([]Leaf, len(leaves)),
	}
	for index, leaf := range leaves {
		tree.Leaves[index] = leaf
		tree.Leaves[index].Input = slices.Clone(leaf.Input[commonLength:])
	}
	return tree
}

// Expanded reconstructs every terminal conversation without mutating the tree.
func (t Tree) Expanded() []Leaf {
	leaves := make([]Leaf, len(t.Leaves))
	for index, leaf := range t.Leaves {
		leaves[index] = leaf
		leaves[index].Input = slices.Concat(t.Input, leaf.Input)
	}
	return leaves
}
