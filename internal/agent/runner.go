package agent

import "context"

type Runner interface {
	Run(context.Context, Request) (Result, error)
}

type Request struct {
	SystemPrompt      string
	ProjectGuidance   string
	UserPrompt        string
	AllowedToolNames  []string
	MaxSteps          int
	RepairOnValidator bool
}

type Result struct {
	Text        string
	ToolCalls   int
	RepairCalls int
}
