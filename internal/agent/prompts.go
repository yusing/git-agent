package agent

import (
	_ "embed"
	"strings"
	"text/template"

	"github.com/yusing/git-agent/internal/textutil"
)

//go:embed prompts/no-tools.md
var noToolsPrompt string

//go:embed prompts/request.md.tmpl
var requestPromptSource string

//go:embed prompts/forced-finalization.md
var forcedFinalizationPrompt string

//go:embed prompts/repair.md.tmpl
var repairPromptSource string

//go:embed prompts/explore.md
var explorePrompt string

//go:embed prompts/explore-target.md
var exploreTargetPrompt string

var (
	requestPromptTemplate = template.Must(template.New("agent-request").Parse(requestPromptSource))
	repairPromptTemplate  = template.Must(template.New("repair").Parse(repairPromptSource))
)

type requestPromptTool struct {
	Name        string
	Description string
	Parameters  string
}

type requestPromptData struct {
	Tools             []requestPromptTool
	ReadFileAvailable bool
}

func renderRequestPrompt(data requestPromptData) string {
	return strings.TrimSpace(textutil.ExecuteTemplate(requestPromptTemplate, data))
}

func renderRepairPrompt(errs []string) string {
	return strings.TrimSpace(textutil.ExecuteTemplate(repairPromptTemplate, struct{ Errors []string }{Errors: errs}))
}

// ExploreSystemPrompt returns the embedded exploration prompt for the selected mode.
func ExploreSystemPrompt(targeted bool) string {
	if targeted {
		return strings.TrimSpace(exploreTargetPrompt)
	}
	return strings.TrimSpace(explorePrompt)
}
