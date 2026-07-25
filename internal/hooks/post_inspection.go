// Package hooks runs explicitly configured lifecycle hooks.
package hooks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"text/template"
	"time"

	"github.com/bytedance/sonic"
)

const SchemaVersion = 2

const hookWaitDelay = time.Second

type PostInspection struct {
	SchemaVersion int               `json:"schema_version"`
	Session       InspectionSession `json:"session"`
	Metrics       InspectionMetrics `json:"metrics"`
	Report        any               `json:"report"`
}

type InspectionMetrics struct {
	Usage           Usage            `json:"usage"`
	UsedSkills      []string         `json:"used_skills"`
	ToolCalls       []ToolCallMetric `json:"tool_calls"`
	BranchesCreated int              `json:"branches_created"`
	Branches        []BranchMetric   `json:"branches"`
}

type ToolCallMetric struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type BranchMetric struct {
	ID              string `json:"id"`
	ParentID        string `json:"parent_id"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	Usage           Usage  `json:"usage"`
}

type InspectionSession struct {
	ID              string         `json:"id"`
	Title           string         `json:"title"`
	Command         string         `json:"command"`
	Mode            string         `json:"mode"`
	Model           string         `json:"model"`
	ReasoningEffort string         `json:"reasoning_effort"`
	StartedAt       time.Time      `json:"started_at"`
	CompletedAt     time.Time      `json:"completed_at"`
	ElapsedMS       int64          `json:"elapsed_ms"`
	ToolCalls       int            `json:"tool_calls"`
	RepairCalls     int            `json:"repair_calls"`
	Repository      map[string]any `json:"repository"`
}

type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	CachedInputTokens   int64 `json:"cached_input_tokens"`
	UncachedInputTokens int64 `json:"uncached_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

func RunPostInspection(ctx context.Context, configured []string, payload PostInspection) error {
	data, err := sonic.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode post-inspection hook payload: %w", err)
	}
	for index, source := range configured {
		rendered, err := render(source, payload)
		if err != nil {
			return fmt.Errorf("post-inspection hook %d: %w", index+1, err)
		}
		command := exec.CommandContext(ctx, "sh", "-c", rendered)
		command.Stdin = bytes.NewReader(data)
		command.WaitDelay = hookWaitDelay
		stderr := limitedStringWriter{remaining: 4096}
		command.Stdout = io.Discard
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				return fmt.Errorf("post-inspection hook %d: %w", index+1, err)
			}
			return fmt.Errorf("post-inspection hook %d: %w: %s", index+1, err, message)
		}
	}
	return nil
}

func render(source string, payload PostInspection) (string, error) {
	tmpl, err := template.New("post_inspection").Option("missingkey=error").Funcs(template.FuncMap{
		"format_markdown": formatMarkdown,
		"json": func(value any) (string, error) {
			data, err := sonic.Marshal(value)
			return string(data), err
		},
		"shellquote": shellQuote,
	}).Parse(source)
	if err != nil {
		return "", fmt.Errorf("parse shell template: %w", err)
	}
	var rendered strings.Builder
	if err := tmpl.Execute(&rendered, payload); err != nil {
		return "", fmt.Errorf("render shell template: %w", err)
	}
	return rendered.String(), nil
}

func shellQuote(value any) string {
	text := fmt.Sprint(value)
	return "'" + strings.ReplaceAll(text, "'", "'\"'\"'") + "'"
}

type limitedStringWriter struct {
	builder   strings.Builder
	remaining int
}

func (w *limitedStringWriter) Write(data []byte) (int, error) {
	original := len(data)
	if w.remaining == 0 {
		return original, nil
	}
	data = data[:min(len(data), w.remaining)]
	written, err := w.builder.Write(data)
	w.remaining -= written
	return original, err
}

func (w *limitedStringWriter) String() string {
	return w.builder.String()
}
