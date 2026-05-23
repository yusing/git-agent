package openai

import "context"

type Client interface {
	CreateResponse(context.Context, Request) (Response, error)
}

type Request struct {
	Model    string
	BaseURL  string
	Input    []Message
	ToolSpec []ToolSpec
}

type Message struct {
	Role    string
	Content string
}

type ToolSpec struct {
	Name        string
	Description string
	SchemaJSON  string
}

type Response struct {
	Text       string
	ToolCalls  []ToolCall
	FinishKind string
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}
