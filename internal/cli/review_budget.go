package cli

import (
	"strings"
	"unicode"

	skillctx "github.com/yusing/git-agent/internal/skills"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
)

func applicableInspectionSkillCount(kind reviewtask.Kind, mode reviewtask.Mode, store *skillctx.Store, operatorHint string) int {
	count := 0
	for _, skill := range store.Skills() {
		if inspectionSkillApplies(kind, mode, skill, operatorHint) {
			count++
		}
	}
	return count
}

func inspectionSkillApplies(kind reviewtask.Kind, mode reviewtask.Mode, skill skillctx.Skill, operatorHint string) bool {
	if mentionsSkill(operatorHint, skill.Name) {
		return true
	}
	name := strings.ToLower(skill.Name)
	description := strings.ToLower(skill.Description)
	switch kind {
	case reviewtask.KindReview:
		if mode != reviewtask.ModeCodebase && strings.Contains(name, "codebase") {
			return false
		}
		return strings.Contains(name, "code-review") ||
			strings.Contains(description, "review code, diffs") ||
			strings.Contains(description, "review code and diffs")
	case reviewtask.KindSimplify:
		return strings.Contains(name, "code-simpl") ||
			strings.Contains(description, "simplify recently changed code")
	default:
		return false
	}
}

func mentionsSkill(text, name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	for _, token := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != ':' && r != '_'
	}) {
		if token == name {
			return true
		}
	}
	return false
}
