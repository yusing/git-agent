package explore

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/openai"
)

// Source: docs/spec.md "git-agent explore". The answer contract mirrors the
// installed search_code explorer while keeping each CLI result machine-readable.
const SystemPrompt = `You produce agent-ready codebase context for one or more independent exploration items. Each answer is injected into another coding agent as a ready-to-use context pack. That agent must act without rediscovering ownership, behavior, contracts, or blast radius already established here.

Treat semantic_results as unverified leads only. Use the available read-only repository tools to inspect the relevant implementation and its contract-defining tests until every answer is self-contained.

For each item:
- Prefer primary owners: entry points, packages, types, functions, CLI surfaces, and real call sites. Skip incidental mentions.
- Establish ownership and the change boundary implied by the question before collecting supporting edges.
- Read implementation with tests that specify its contract. Prefer tests over prose documentation for behavior.
- Include only direct callers and consumers, interfaces or schemas, configuration, and success or failure paths needed for this question.
- Capture relevant invariants, assumptions, and external contracts.
- When adapting or editing is in scope, name the minimal coherent file and symbol set plus validation that would prove the change. Do not invent an implementation plan.
- When the answer is negative, state what was inspected and the concrete absence.
- Answer every item independently. Do not merge facts across items.

Every answer must begin with the direct answer, then use dense labeled blocks as applicable: Owner, Behavior, Contracts, Blast radius, Boundary, and Absences. Cite concrete claims with repository-relative path and line ranges as path/to/file.ext:START-END. Do not use markdown links, absolute paths, progress chatter, dangling leads, or closing restatements.

Return only the strict JSON object required by the response schema. Emit exactly one answer for every input item, copying each opaque item_id exactly.`

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
	Answer   string `json:"selected_answer"`
}

type Answer struct {
	ItemID string `json:"item_id"`
	Answer string `json:"answer"`
}

type answerEnvelope struct {
	Answers []Answer `json:"answers"`
}

func UserPrompt(parent *Session, items []PromptItem) (string, error) {
	envelope := promptEnvelope{Items: items}
	if parent != nil {
		envelope.Parent = &promptParent{SearchID: parent.ID, Answer: parent.Answer}
	}
	data, err := sonic.ConfigStd.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("encode explore prompt: %w", err)
	}
	return "Exploration input JSON:\n" + string(data), nil
}

func TextFormat() *openai.TextFormat {
	return &openai.TextFormat{
		Name:        "explore_answers",
		Description: "Independent agent-ready answers for a synchronous explore batch.",
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
							"answer":  map[string]any{"type": "string"},
						},
						"required":             []string{"item_id", "answer"},
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
		for id, answer := range answers {
			got = append(got, id)
			if strings.TrimSpace(answer) == "" {
				return []string{fmt.Sprintf("answer for item %s is empty", id)}
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			return []string{fmt.Sprintf("answer item IDs %v do not match requested IDs %v", got, want)}
		}
		return nil
	}
}

func ParseAnswers(text string) (map[string]string, error) {
	var envelope answerEnvelope
	if err := sonic.ConfigStd.UnmarshalFromString(text, &envelope); err != nil {
		return nil, fmt.Errorf("decode explore answers: %w", err)
	}
	answers := make(map[string]string, len(envelope.Answers))
	for _, item := range envelope.Answers {
		if err := validateID(item.ItemID); err != nil {
			return nil, err
		}
		if _, exists := answers[item.ItemID]; exists {
			return nil, fmt.Errorf("duplicate explore answer for item %s", item.ItemID)
		}
		answers[item.ItemID] = item.Answer
	}
	return answers, nil
}

func FollowUpInput(parent Session, prompt string) []openai.Item {
	items := slices.Clone(parent.History)
	return append(items, openai.NewMessage("user", prompt))
}
