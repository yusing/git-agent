package tools

import "context"

type Definition struct {
	Name        string
	Description string
	SchemaJSON  string
}

type Invocation struct {
	Name      string
	Arguments string
}

type Result struct {
	Content   string
	Truncated bool
}

type Tool interface {
	Definition() Definition
	Execute(context.Context, Invocation) (Result, error)
}
