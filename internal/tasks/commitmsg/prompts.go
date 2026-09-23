package commitmsg

import (
	_ "embed"
	"text/template"

	"github.com/yusing/git-agent/internal/textutil"
)

//go:embed prompts/system.md.tmpl
var systemPromptSource string

//go:embed prompts/system-mode-normal.md
var systemModeNormalPrompt string

//go:embed prompts/system-mode-amend.md
var systemModeAmendPrompt string

//go:embed prompts/system-mode-pr.md
var systemModePRPrompt string

//go:embed prompts/user-prepared-commit.md.tmpl
var userPreparedCommitPromptSource string

//go:embed prompts/user-prepared-amend.md.tmpl
var userPreparedAmendPromptSource string

//go:embed prompts/user-prepared-pr.md.tmpl
var userPreparedPRPromptSource string

var (
	systemPromptTemplate             = template.Must(template.New("commit-system").Parse(systemPromptSource))
	userPreparedCommitPromptTemplate = template.Must(template.New("commit-user-prepared").Parse(userPreparedCommitPromptSource))
	userPreparedAmendPromptTemplate  = template.Must(template.New("amend-user-prepared").Parse(userPreparedAmendPromptSource))
	userPreparedPRPromptTemplate     = template.Must(template.New("pr-user-prepared").Parse(userPreparedPRPromptSource))
)

func renderSystemPrompt(modeInstructions string) string {
	return textutil.ExecutePromptTemplate(systemPromptTemplate, struct {
		ModeInstructions string
	}{ModeInstructions: modeInstructions})
}

type userPromptData struct {
	MaxSteps        int
	MaxToolCalls    int
	StyleReferences string
	PreparedContext string
}

func executeUserPrompt(tmpl *template.Template, data userPromptData) string {
	return textutil.ExecutePromptTemplate(tmpl, data)
}
