package cli

import (
	_ "embed"
	"strings"
	"text/template"

	"github.com/yusing/git-agent/internal/textutil"
)

//go:embed prompts/operator-hint.md.tmpl
var operatorHintPromptSource string

//go:embed prompts/tool-policy.md
var toolPolicyPrompt string

//go:embed prompts/review-tool-policy.md
var reviewToolPolicyPrompt string

//go:embed prompts/environment.md.tmpl
var environmentPromptSource string

var (
	operatorHintPromptTemplate = template.Must(template.New("operator-hint").Parse(operatorHintPromptSource))
	environmentPromptTemplate  = template.Must(template.New("environment").Parse(environmentPromptSource))
)

type environmentPromptData struct {
	WorkPath       string
	RootPath       string
	Command        string
	Mode           string
	GuidanceFamily string
	MaxSteps       int
	MaxToolCalls   int
}

func renderOperatorHintPrompt(hint string) string {
	return strings.TrimSpace(textutil.ExecuteTemplate(operatorHintPromptTemplate, struct{ Hint string }{Hint: hint}))
}

func renderEnvironmentPrompt(data environmentPromptData) string {
	return strings.TrimSpace(textutil.ExecuteTemplate(environmentPromptTemplate, data))
}
