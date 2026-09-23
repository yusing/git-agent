// Package followup owns shared replay, lineage, and cache continuity.
package followup

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
