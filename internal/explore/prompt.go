package explore

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/agent"
	"github.com/yusing/git-agent/internal/openai"
)

// Source: docs/spec.md "git-agent explore". The answer contract mirrors the
// installed search_code explorer while keeping each CLI result machine-readable.
var (
	SystemPrompt       = agent.ExploreSystemPrompt(false)
	TargetSystemPrompt = agent.ExploreSystemPrompt(true)
)

type QueryTarget string

const (
	QueryTargetUniversal QueryTarget = ""
	QueryTargetDiagnose  QueryTarget = "diagnose"
	QueryTargetChange    QueryTarget = "change"
	QueryTargetBehavior  QueryTarget = "behavior"
	QueryTargetOwner     QueryTarget = "owner"

	queryTargetDiagnoseInstructions = "Prioritize the concrete reproducer, current failure path, immediate mechanism, and evidence-backed bottleneck or regression cause. Distinguish symptoms from the violated invariant and authoritative owner."
	queryTargetChangeInstructions   = "Prioritize operation-ready change context: the authoritative implementation boundary, affected contracts and call sites, edge cases, and focused validation that would falsify the change. Do not propose code that is not grounded in the repository."
	queryTargetBehaviorInstructions = "Prioritize current behavior: executable semantics, contract-defining tests, inputs and outputs, invariants, and success and failure paths. Separate observed behavior from unsupported assumptions."
	queryTargetOwnerInstructions    = "Prioritize ownership: the authoritative implementation, entry points, callers and consumers, subsystem boundaries, and relevant interfaces. Exclude incidental mentions and identify concrete absences."
)

func ParseQueryTarget(value string) (QueryTarget, error) {
	target := QueryTarget(value)
	switch target {
	case QueryTargetUniversal, QueryTargetDiagnose, QueryTargetChange, QueryTargetBehavior, QueryTargetOwner:
		return target, nil
	default:
		return QueryTargetUniversal, fmt.Errorf(
			"unsupported explore query target %q (want diagnose, change, behavior, or owner)",
			value,
		)
	}
}

func (target QueryTarget) Instructions() string {
	switch target {
	case QueryTargetDiagnose:
		return queryTargetDiagnoseInstructions
	case QueryTargetChange:
		return queryTargetChangeInstructions
	case QueryTargetBehavior:
		return queryTargetBehaviorInstructions
	case QueryTargetOwner:
		return queryTargetOwnerInstructions
	default:
		return ""
	}
}

func InitialTargetInstruction(target QueryTarget) string {
	if target.Instructions() == "" {
		return ""
	}
	return targetInstruction("Query target:", target)
}

func targetInstruction(prefix string, target QueryTarget) string {
	text := prefix + " " + string(target)
	if instructions := target.Instructions(); instructions != "" {
		text += "\n" + instructions
	}
	return text
}

func SystemPromptFor(target QueryTarget) string {
	if target.Instructions() == "" {
		return SystemPrompt
	}
	return TargetSystemPrompt
}

func SystemPromptTarget(parent *Session, selected QueryTarget) QueryTarget {
	if parent != nil && parent.InstructionTarget != QueryTargetUniversal {
		return parent.InstructionTarget
	}
	return selected
}

type PromptItem struct {
	ItemID          string `json:"item_id"`
	Question        string `json:"question"`
	SemanticResults string `json:"semantic_results,omitempty"`
}

type promptEnvelope struct {
	Parent *promptParent `json:"parent,omitempty"`
	Items  []PromptItem  `json:"items"`
}

type promptParent struct {
	SearchID string `json:"search_id"`
	Items    []Item `json:"selected_items"`
}

type Item struct {
	Description string   `json:"description"`
	References  []string `json:"references"`
}

type Answer struct {
	ItemID string `json:"item_id"`
	Items  []Item `json:"items"`
}

type answerEnvelope struct {
	Answers []Answer `json:"answers"`
}

func UserPrompt(parent *Session, items []PromptItem) (string, error) {
	envelope := promptEnvelope{Items: items}
	if parent != nil {
		envelope.Parent = &promptParent{SearchID: parent.ID, Items: parent.Items}
	}
	text, err := sonic.ConfigStd.MarshalToString(envelope)
	if err != nil {
		return "", fmt.Errorf("encode explore prompt: %w", err)
	}
	return "Exploration input JSON:\n" + text, nil
}

func TextFormat() *openai.TextFormat {
	return &openai.TextFormat{
		Name:        "explore_answers",
		Description: "Independent repository-grounded item sets for a synchronous explore batch.",
		Strict:      true,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"answers": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"item_id": map[string]any{"type": "string"},
							"items": map[string]any{
								"type":     "array",
								"minItems": 1,
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"description": map[string]any{"type": "string", "minLength": 1},
										"references": map[string]any{
											"type":     "array",
											"minItems": 1,
											"items":    map[string]any{"type": "string", "minLength": 1},
										},
									},
									"required":             []string{"description", "references"},
									"additionalProperties": false,
								},
							},
						},
						"required":             []string{"item_id", "items"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"answers"},
			"additionalProperties": false,
		},
	}
}

func ValidateAnswers(itemIDs []string) func(string) []string {
	want := slices.Clone(itemIDs)
	slices.Sort(want)
	return func(text string) []string {
		answers, err := ParseAnswers(text)
		if err != nil {
			return []string{err.Error()}
		}
		got := make([]string, 0, len(answers))
		for id := range answers {
			got = append(got, id)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			return []string{fmt.Sprintf("answer item IDs %v do not match requested IDs %v", got, want)}
		}
		return nil
	}
}

func ParseAnswers(text string) (map[string][]Item, error) {
	var envelope answerEnvelope
	if err := sonic.ConfigStd.UnmarshalFromString(text, &envelope); err != nil {
		return nil, fmt.Errorf("decode explore answers: %w", err)
	}

	answers := make(map[string][]Item, len(envelope.Answers))
	for _, answer := range envelope.Answers {
		if err := validateID(answer.ItemID); err != nil {
			return nil, err
		}
		if _, exists := answers[answer.ItemID]; exists {
			return nil, fmt.Errorf("duplicate explore answer for item %s", answer.ItemID)
		}
		if err := validateItems(answer.ItemID, answer.Items); err != nil {
			return nil, err
		}
		answers[answer.ItemID] = answer.Items
	}
	return answers, nil
}

func validateItems(itemID string, items []Item) error {
	if len(items) == 0 {
		return fmt.Errorf("answer for item %s requires at least one item", itemID)
	}
	for index, item := range items {
		if strings.TrimSpace(item.Description) == "" {
			return fmt.Errorf("answer for item %s item %d requires a description", itemID, index)
		}
		if len(item.References) == 0 {
			return fmt.Errorf("answer for item %s item %d requires at least one reference", itemID, index)
		}
		for referenceIndex, reference := range item.References {
			if strings.TrimSpace(reference) == "" {
				return fmt.Errorf("answer for item %s item %d reference %d is empty", itemID, index, referenceIndex)
			}
		}
	}
	return nil
}

func FollowUpInput(parent Session, prompt string, target QueryTarget) []openai.Item {
	items := slices.Clone(parent.History)
	if parent.ActiveTarget != target {
		items = append(items, openai.NewMessage("developer", targetInstruction("Query target changed:", target)))
	}
	return append(items, openai.NewMessage("user", prompt))
}
