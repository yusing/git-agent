package cli

import (
	"testing"

	skillctx "github.com/yusing/git-agent/internal/skills"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
)

func TestInspectionSkillApplies(t *testing.T) {
	tests := []struct {
		name  string
		kind  reviewtask.Kind
		mode  reviewtask.Mode
		skill skillctx.Skill
		hint  string
		want  bool
	}{
		{
			name:  "code review skill",
			kind:  reviewtask.KindReview,
			mode:  reviewtask.ModeUncommitted,
			skill: skillctx.Skill{Name: "code-review", Description: "Review code, diffs, pull requests, or major changes."},
			want:  true,
		},
		{
			name:  "codebase review excluded from diff",
			kind:  reviewtask.KindReview,
			mode:  reviewtask.ModeStaged,
			skill: skillctx.Skill{Name: "codebase-review", Description: "Review an entire codebase rather than a diff."},
			want:  false,
		},
		{
			name:  "codebase review applies to codebase",
			kind:  reviewtask.KindReview,
			mode:  reviewtask.ModeCodebase,
			skill: skillctx.Skill{Name: "codebase-review", Description: "Review code, diffs, or an entire codebase."},
			want:  true,
		},
		{
			name:  "simplifier skill",
			kind:  reviewtask.KindSimplify,
			mode:  reviewtask.ModeUncommitted,
			skill: skillctx.Skill{Name: "code-simplifier", Description: "Simplify recently changed code while preserving behavior."},
			want:  true,
		},
		{
			name:  "unrelated skill",
			kind:  reviewtask.KindReview,
			mode:  reviewtask.ModeUncommitted,
			skill: skillctx.Skill{Name: "writing-readme", Description: "Write repository manuals."},
			want:  false,
		},
		{
			name:  "explicit skill",
			kind:  reviewtask.KindReview,
			mode:  reviewtask.ModeUncommitted,
			skill: skillctx.Skill{Name: "security-audit", Description: "Inspect a security design."},
			hint:  "Use $security-audit for this change",
			want:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := inspectionSkillApplies(test.kind, test.mode, test.skill, test.hint); got != test.want {
				t.Fatalf("inspectionSkillApplies() = %t, want %t", got, test.want)
			}
		})
	}
}
