package tools

import (
	"context"
	"strings"

	"github.com/yusing/git-agent/internal/skillcmd"
)

const SkillsReadToolName = "skills_read"

type skillsReadTool struct {
	manager *skillcmd.Manager
}

func skillTools(manager *skillcmd.Manager) []Tool {
	if !manager.Available() {
		return nil
	}
	return []Tool{skillsReadTool{manager: manager}}
}

func (t skillsReadTool) Definition() Definition {
	return Definition{
		Name:        SkillsReadToolName,
		Description: "Read an enabled skill or its reference file through skills-mgr. Use an empty range to read the complete file.",
		Schema: schema(map[string]any{
			"locator": stringProp("Skill name or skill-relative reference locator."),
			"range":   stringProp("Optional inclusive start:end line range; use an empty string for the complete file."),
		}, "locator", "range"),
		Strict: true,
	}
}

func (t skillsReadTool) Execute(ctx context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		Locator string `json:"locator"`
		Range   string `json:"range"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	output, err := t.manager.Read(ctx, args.Locator, args.Range)
	if err != nil {
		return Result{}, err
	}
	return jsonResult(SkillsReadToolName, map[string]any{
		"stdout": output.Stdout,
		"stderr": output.Stderr,
	}, output.Truncated)
}

// UsedSkill returns the skill named by a skills_read invocation.
func UsedSkill(invocation Invocation) (string, bool) {
	if invocation.Name != SkillsReadToolName {
		return "", false
	}
	args, err := parseArgs[struct {
		Locator string `json:"locator"`
	}](invocation.Arguments)
	if err != nil {
		return "", false
	}
	name, _, _ := strings.Cut(strings.TrimSpace(args.Locator), "/")
	return name, name != ""
}
