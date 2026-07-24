package review

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/tools"
)

const (
	BranchToolName       = "branch"
	BranchHelpToolName   = "branch_help"
	maxBranchScopeBytes  = 2048
	maxBranchScopeLines  = 20
	maxBranchResultBytes = 4096
	maxBranchResultLines = 40
)

var (
	branchModels  = []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}
	branchEfforts = []string{"low", "medium", "high", "xhigh"}
)

type BranchPolicy struct {
	MaxDepth    int
	MaxChildren int
}

type Branch struct {
	Scope           string   `json:"scope"`
	PathHints       []string `json:"path_hints"`
	Model           string   `json:"model"`
	ReasoningEffort string   `json:"reasoning_effort"`
}

type BranchRequest struct {
	Branches []Branch `json:"branches"`
}

type BranchResult struct {
	BranchID        string   `json:"branch_id"`
	ParentBranchID  string   `json:"parent_branch_id"`
	Message         string   `json:"message"`
	PathHints       []string `json:"path_hints"`
	SiblingScopes   []string `json:"sibling_scopes"`
	Model           string   `json:"model"`
	ReasoningEffort string   `json:"reasoning_effort"`
	Depth           int      `json:"depth"`
}

type branchHelpTool struct {
	kind Kind
}

type branchHelpData struct {
	Models                  []branchHelpModel  `json:"models"`
	ReasoningEffortMapping  []branchHelpEffort `json:"reasoning_effort_mapping"`
	AllowedModels           []string           `json:"allowed_models"`
	AllowedReasoningEfforts []string           `json:"allowed_reasoning_efforts"`
}

type branchHelpModel struct {
	Model        string `json:"model"`
	SuitableJobs string `json:"suitable_jobs"`
}

type branchHelpEffort struct {
	ScopeDifficulty string `json:"scope_difficulty"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type LeafReport struct {
	Scope         string
	Text          string
	Summary       string
	Findings      []Finding
	Opportunities []Opportunity
}

func Policy(depth Depth) BranchPolicy {
	switch depth {
	case DepthFast:
		return BranchPolicy{MaxDepth: 1, MaxChildren: 2}
	case DepthThorough:
		return BranchPolicy{MaxDepth: 2, MaxChildren: 4}
	default:
		return BranchPolicy{MaxDepth: 1, MaxChildren: 3}
	}
}

func BranchHelp(kind Kind) tools.Tool {
	return branchHelpTool{kind: kind}
}

func (t branchHelpTool) Definition() tools.Definition {
	return tools.Definition{
		Name:        BranchHelpToolName,
		Description: "Use before deciding to use `branch`",
		Schema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"required":             []string{},
			"additionalProperties": false,
		},
		Strict: true,
	}
}

func (t branchHelpTool) Execute(_ context.Context, invocation tools.Invocation) (tools.Result, error) {
	var arguments struct{}
	if err := decodeStrict(invocation.Arguments, &arguments); err != nil {
		return tools.Result{}, fmt.Errorf("invalid branch_help call: %w", err)
	}
	return tools.JSONResult(BranchHelpToolName, branchHelpData{
		Models: []branchHelpModel{
			{Model: "gpt-5.6-sol", SuitableJobs: "Defect review, correctness, security, reliability, performance, concurrency, lifecycle, and invariant-heavy analysis."},
			{Model: "gpt-5.6-terra", SuitableJobs: "Behavior-preserving simplification, reuse, clarity, efficiency, overengineering, and redundant-state analysis."},
			{Model: "gpt-5.6-luna", SuitableJobs: "General or mixed inspection."},
		},
		ReasoningEffortMapping: []branchHelpEffort{
			{ScopeDifficulty: "Local, direct, or mechanical.", ReasoningEffort: "low"},
			{ScopeDifficulty: "Cross-file, caller/test, ordinary correctness, or structural reuse.", ReasoningEffort: "medium"},
			{ScopeDifficulty: "Security, concurrency, lifecycle, data integrity, state machine, or cross-boundary invariant.", ReasoningEffort: "high"},
			{ScopeDifficulty: "Exceptional cryptographic or adversarial multi-boundary analysis.", ReasoningEffort: "xhigh"},
		},
		AllowedModels:           append([]string{"inherit"}, branchModels...),
		AllowedReasoningEfforts: append([]string{"inherit"}, allowedBranchEfforts(t.kind)...),
	}, false)
}

func BranchDefinition(kind Kind, depth Depth, nodeDepth int) (tools.Definition, bool) {
	policy := Policy(depth)
	if nodeDepth >= policy.MaxDepth {
		return tools.Definition{}, false
	}
	models := append([]string{"inherit"}, branchModels...)
	effortValues := append([]string{"inherit"}, allowedBranchEfforts(kind)...)
	child := objectSchema(map[string]any{
		"scope":            stringSchema(),
		"path_hints":       map[string]any{"type": "array", "items": stringSchema()},
		"model":            enumSchema(models...),
		"reasoning_effort": enumSchema(effortValues...),
	})
	return tools.Definition{
		Name:        BranchToolName,
		Description: "Fork this large, independently partitionable inspection into child conversations and permanently retire this conversation. Submit all immediate children together. Give each child a distinct natural-language reporting responsibility. Path hints accelerate discovery but never restrict inspection or evidence. Do not branch for repeated opinions, duplicate work, higher effort alone, or work this conversation can finish. Inherit model and effort unless the branch_help catalog and scope difficulty justify an override.",
		Schema: objectSchema(map[string]any{
			"branches": map[string]any{
				"type": "array", "minItems": 2, "maxItems": policy.MaxChildren, "items": child,
			},
		}),
		Strict: true,
	}, true
}

func ParseBranchRequest(kind Kind, depth Depth, nodeDepth int, arguments string) (BranchRequest, error) {
	policy := Policy(depth)
	if nodeDepth >= policy.MaxDepth {
		return BranchRequest{}, fmt.Errorf("branch is unavailable at conversation depth %d", nodeDepth)
	}
	var request BranchRequest
	if err := decodeStrict(arguments, &request); err != nil {
		return BranchRequest{}, fmt.Errorf("invalid branch call: %w", err)
	}
	if len(request.Branches) < 2 || len(request.Branches) > policy.MaxChildren {
		return BranchRequest{}, fmt.Errorf("branch call requires 2 to %d children", policy.MaxChildren)
	}
	allowedEfforts := allowedBranchEfforts(kind)
	for index, branch := range request.Branches {
		if err := validateBoundedText(branch.Scope, maxBranchScopeBytes, maxBranchScopeLines); err != nil {
			return BranchRequest{}, fmt.Errorf("branches[%d].scope %w", index, err)
		}
		if branch.PathHints == nil {
			return BranchRequest{}, fmt.Errorf("branches[%d].path_hints must be an array", index)
		}
		for hintIndex, hint := range branch.PathHints {
			if !validEvidencePath(hint) {
				return BranchRequest{}, fmt.Errorf("branches[%d].path_hints[%d] must be a safe repository-relative path", index, hintIndex)
			}
		}
		if branch.Model != "inherit" && !slices.Contains(branchModels, branch.Model) {
			return BranchRequest{}, fmt.Errorf("branches[%d].model is invalid", index)
		}
		if branch.ReasoningEffort != "inherit" && !slices.Contains(allowedEfforts, branch.ReasoningEffort) {
			return BranchRequest{}, fmt.Errorf("branches[%d].reasoning_effort is invalid", index)
		}
	}
	return request, nil
}

func EncodeBranchResult(result BranchResult) (string, error) {
	data, err := sonic.MarshalString(result)
	if err != nil {
		return "", fmt.Errorf("encode branch result: %w", err)
	}
	return data, nil
}

func Aggregate(kind Kind, leaves []LeafReport) (string, error) {
	switch kind {
	case KindReview:
		report := ReviewReport{Findings: []Finding{}}
		summaries := make([]string, 0, len(leaves))
		for _, leaf := range leaves {
			summaries = append(summaries, leaf.Scope+": "+leaf.Summary)
			report.Findings = append(report.Findings, leaf.Findings...)
		}
		slices.SortStableFunc(report.Findings, func(left, right Finding) int {
			return reviewSeverityRank[right.Severity] - reviewSeverityRank[left.Severity]
		})
		report.Summary = strings.Join(summaries, "\n")
		report.Recommendation = recommendation(report.Findings)
		return encodeAggregated(kind, report)
	case KindSimplify:
		report := SimplifyReport{Opportunities: []Opportunity{}}
		summaries := make([]string, 0, len(leaves))
		for _, leaf := range leaves {
			summaries = append(summaries, leaf.Scope+": "+leaf.Summary)
			report.Opportunities = append(report.Opportunities, leaf.Opportunities...)
		}
		report.Summary = strings.Join(summaries, "\n")
		return encodeAggregated(kind, report)
	default:
		return "", fmt.Errorf("unknown report kind %q", kind)
	}
}

func ParseLeaf(kind Kind, scope, text string) (LeafReport, error) {
	leaf := LeafReport{Scope: scope, Text: text}
	switch kind {
	case KindReview:
		var report ReviewReport
		if err := decodeStrict(text, &report); err != nil {
			return LeafReport{}, err
		}
		leaf.Summary = report.Summary
		leaf.Findings = report.Findings
	case KindSimplify:
		var report SimplifyReport
		if err := decodeStrict(text, &report); err != nil {
			return LeafReport{}, err
		}
		leaf.Summary = report.Summary
		leaf.Opportunities = report.Opportunities
	default:
		return LeafReport{}, fmt.Errorf("unknown report kind %q", kind)
	}
	return leaf, nil
}

func allowedBranchEfforts(kind Kind) []string {
	if kind == KindSimplify {
		return branchEfforts[:3]
	}
	return branchEfforts
}

func BoundedBranchMessage(message string) string {
	return boundText(message, maxBranchResultBytes, maxBranchResultLines)
}

func encodeAggregated(kind Kind, report any) (string, error) {
	text, err := sonic.MarshalString(report)
	if err != nil {
		return "", fmt.Errorf("encode aggregated %s report: %w", kind, err)
	}
	if errs := Validate(kind, text); len(errs) > 0 {
		return "", fmt.Errorf("validate aggregated %s report: %s", kind, strings.Join(errs, "; "))
	}
	return Shape(kind, text), nil
}

func recommendation(findings []Finding) string {
	result := "APPROVE"
	for _, finding := range findings {
		if reviewSeverityRank[finding.Severity] >= reviewSeverityRank["HIGH"] {
			return "FIX"
		}
		result = "COMMENT"
	}
	return result
}

func validateBoundedText(value string, maxBytes, maxLines int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("is required")
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("must be valid UTF-8")
	}
	if len(value) > maxBytes {
		return fmt.Errorf("exceeds %d bytes", maxBytes)
	}
	if strings.Count(value, "\n")+1 > maxLines {
		return fmt.Errorf("exceeds %d lines", maxLines)
	}
	return nil
}

func boundText(value string, maxBytes, maxLines int) string {
	value = strings.ToValidUTF8(value, "")
	lines := strings.Split(value, "\n")
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		value = strings.Join(lines, "\n")
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
