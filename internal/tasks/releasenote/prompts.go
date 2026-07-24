package releasenote

import (
	_ "embed"
	"text/template"

	"github.com/yusing/git-agent/internal/textutil"
)

//go:embed prompts/system.md
var systemPromptMarkdown string

//go:embed prompts/user.md.tmpl
var userPromptMarkdown string

var userPromptTemplate = template.Must(template.New("release-note-user").Parse(userPromptMarkdown))

type userPromptData struct {
	MaxSteps        int
	MaxToolCalls    int
	Range           string
	PreparedContext string
}

func renderUserPrompt(data userPromptData) string {
	return textutil.ExecutePromptTemplate(userPromptTemplate, data)
}
