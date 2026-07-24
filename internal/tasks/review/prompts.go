package review

import (
	_ "embed"
	"strings"
	"text/template"

	"github.com/yusing/git-agent/internal/textutil"
)

//go:embed prompts/system-review.md
var systemReviewPrompt string

//go:embed prompts/system-simplify.md
var systemSimplifyPrompt string

//go:embed prompts/user.md.tmpl
var userPromptSource string

//go:embed prompts/mission-review.md
var reviewMissionPrompt string

//go:embed prompts/mission-simplify.md
var simplifyMissionPrompt string

//go:embed prompts/scope-codebase.md
var codebaseScopePrompt string

//go:embed prompts/scope-diff.md
var diffScopePrompt string

var userPromptTemplate = template.Must(template.New("review-user").Parse(userPromptSource))

type userPromptData struct {
	Mission         string
	Scope           string
	PreparedContext string
}

func renderUserPrompt(data userPromptData) string {
	data.Mission = strings.TrimSpace(data.Mission)
	data.Scope = strings.TrimSpace(data.Scope)
	return textutil.ExecutePromptTemplate(userPromptTemplate, data)
}
