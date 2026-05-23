package guidance

type Family string

const (
	FamilyAuto   Family = "auto"
	FamilyAgents Family = "agents"
	FamilyClaude Family = "claude"
	FamilyNone   Family = "none"
)

type Source struct {
	Path    string
	Content string
}

type Resolved struct {
	TargetPath string
	Family     Family
	Sources    []Source
	Rendered   string
}
