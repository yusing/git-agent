package cli

import (
	_ "embed"
	"strings"
	"text/template"

	"github.com/yusing/git-agent/internal/textutil"
)

//go:embed prompts/orchestration.md.tmpl
var orchestrationPromptSource string

//go:embed prompts/operator-hint.md.tmpl
var operatorHintPromptSource string

//go:embed prompts/tool-policy.md
var toolPolicyPrompt string

//go:embed prompts/review-tool-policy.md
var reviewToolPolicyPrompt string

//go:embed prompts/environment.md.tmpl
var environmentPromptSource string

var (
	orchestrationPromptTemplate = template.Must(template.New("orchestration").Parse(orchestrationPromptSource))
	operatorHintPromptTemplate  = template.Must(template.New("operator-hint").Parse(operatorHintPromptSource))
	environmentPromptTemplate   = template.Must(template.New("environment").Parse(environmentPromptSource))
)

type orchestrationPromptData struct {
	Inventory string
	ToolName  string
}

type environmentPromptData struct {
	WorkPath       string
	RootPath       string
	Command        string
	Mode           string
	GuidanceFamily string
	MaxSteps       int
	MaxToolCalls   int
}

func renderOrchestrationPrompt(inventory, toolName string) string {
	return strings.TrimSpace(textutil.ExecuteTemplate(orchestrationPromptTemplate, orchestrationPromptData{
		Inventory: inventory,
		ToolName:  toolName,
	}))
}

func renderOperatorHintPrompt(hint string) string {
	return strings.TrimSpace(textutil.ExecuteTemplate(operatorHintPromptTemplate, struct{ Hint string }{Hint: hint}))
}

func renderEnvironmentPrompt(data environmentPromptData) string {
	return strings.TrimSpace(textutil.ExecuteTemplate(environmentPromptTemplate, data))
}
