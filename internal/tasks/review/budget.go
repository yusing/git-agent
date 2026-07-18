package review

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
)

type Depth string

const (
	DepthFast     Depth = "fast"
	DepthBalanced Depth = "balanced"
	DepthThorough Depth = "thorough"

	ReviewMaxSteps       = 60
	ReviewMaxToolCalls   = 48
	SimplifyMaxSteps     = 45
	SimplifyMaxToolCalls = 36

	maxBudgetSkills = 4
)

type BudgetInput struct {
	Kind             Kind
	Prepared         PreparedContext
	ToolNames        []string
	ApplicableSkills int
	Depth            Depth
	ExplicitMaxSteps int
}

type BudgetPlan struct {
	Policy              string  `json:"policy"`
	EffectiveLines      int     `json:"effective_lines"`
	EffectiveFiles      int     `json:"effective_files"`
	Scopes              int     `json:"scopes"`
	WorkUnits           int     `json:"work_units"`
	CapabilityCoverage  float64 `json:"capability_coverage"`
	ApplicableSkills    int     `json:"applicable_skills"`
	LowerSteps          int     `json:"lower_steps"`
	SelectedSteps       int     `json:"selected_steps"`
	UpperSteps          int     `json:"upper_steps"`
	MaxToolCalls        int     `json:"max_tool_calls"`
	HardStepCap         int     `json:"hard_step_cap"`
	HardToolCallCap     int     `json:"hard_tool_call_cap"`
	Automatic           bool    `json:"automatic"`
	CodebaseFixedBudget bool    `json:"codebase_fixed_budget,omitempty"`
}

func ParseDepth(value string) (Depth, error) {
	depth := Depth(strings.ToLower(strings.TrimSpace(value)))
	if depth == "" {
		return DepthBalanced, nil
	}
	if slices.Contains([]Depth{DepthFast, DepthBalanced, DepthThorough}, depth) {
		return depth, nil
	}
	return "", fmt.Errorf("--depth must be one of fast, balanced, or thorough")
}

func PlanBudget(input BudgetInput) (BudgetPlan, error) {
	depth, err := ParseDepth(string(input.Depth))
	if err != nil {
		return BudgetPlan{}, err
	}
	if input.ExplicitMaxSteps < 0 {
		return BudgetPlan{}, errors.New("explicit maximum steps must not be negative")
	}

	stepFloor, hardStepCap, toolFloor, hardToolCap, base, verification, err := budgetConstants(input.Kind)
	if err != nil {
		return BudgetPlan{}, err
	}
	plan := BudgetPlan{
		Policy:          string(depth),
		HardStepCap:     hardStepCap,
		HardToolCallCap: hardToolCap,
		Automatic:       input.ExplicitMaxSteps == 0,
	}

	coverage, err := capabilityCoverage(input.Prepared.Mode, input.ToolNames)
	if err != nil {
		return BudgetPlan{}, err
	}
	plan.CapabilityCoverage = coverage
	plan.ApplicableSkills = min(max(input.ApplicableSkills, 0), maxBudgetSkills)

	if input.Prepared.Mode == ModeCodebase {
		plan.LowerSteps = hardStepCap
		plan.UpperSteps = hardStepCap
		plan.SelectedSteps = hardStepCap
		plan.MaxToolCalls = hardToolCap
		plan.CodebaseFixedBudget = true
		if input.ExplicitMaxSteps > 0 {
			plan.Policy = "explicit"
			plan.SelectedSteps = input.ExplicitMaxSteps
		}
		return plan, nil
	}

	metrics := changeBudgetMetrics(input.Prepared)
	plan.EffectiveLines = metrics.effectiveLines
	plan.EffectiveFiles = metrics.effectiveFiles
	plan.Scopes = metrics.scopes
	plan.WorkUnits = metrics.workUnits

	lowMultiplier := 1 + 0.25*(1-coverage)
	highMultiplier := 1 + 0.75*(1-coverage)
	skillLow := ceilDiv(plan.ApplicableSkills, 3)
	rawLow := base + int(math.Ceil(0.5*float64(metrics.workUnits)*lowMultiplier)) + skillLow
	rawHigh := base + int(math.Ceil(float64(metrics.workUnits)*highMultiplier)) + plan.ApplicableSkills + verification
	plan.LowerSteps = min(max(rawLow, stepFloor), hardStepCap)
	plan.UpperSteps = min(max(rawHigh, plan.LowerSteps), hardStepCap)

	switch depth {
	case DepthFast:
		plan.SelectedSteps = plan.LowerSteps
	case DepthBalanced:
		plan.SelectedSteps = ceilDiv(plan.LowerSteps+plan.UpperSteps, 2)
	case DepthThorough:
		plan.SelectedSteps = plan.UpperSteps
	}
	plan.MaxToolCalls = min(max(toolFloor, ceilDiv(4*plan.SelectedSteps, 5)+plan.ApplicableSkills), hardToolCap)
	if input.ExplicitMaxSteps > 0 {
		plan.Policy = "explicit"
		plan.SelectedSteps = input.ExplicitMaxSteps
		plan.MaxToolCalls = hardToolCap
	}
	return plan, nil
}

func budgetConstants(kind Kind) (stepFloor, hardStepCap, toolFloor, hardToolCap, base, verification int, err error) {
	switch kind {
	case KindReview:
		return 8, ReviewMaxSteps, 6, ReviewMaxToolCalls, 6, 3, nil
	case KindSimplify:
		return 6, SimplifyMaxSteps, 5, SimplifyMaxToolCalls, 5, 2, nil
	default:
		return 0, 0, 0, 0, 0, 0, fmt.Errorf("unknown inspection kind %q", kind)
	}
}

type budgetMetrics struct {
	effectiveLines int
	effectiveFiles int
	scopes         int
	workUnits      int
}

func changeBudgetMetrics(prepared PreparedContext) budgetMetrics {
	totalLines := 0
	binaryFiles := 0
	for _, stat := range prepared.Stats {
		totalLines += stat.Adds + stat.Deletes
		if stat.IsBinary {
			binaryFiles++
		}
	}

	generatedLines := 0
	for _, group := range prepared.ContextPack.Groups {
		if group.Generated {
			generatedLines += group.Adds + group.Deletes
		}
	}
	for _, file := range prepared.ContextPack.Outliers {
		if file.Generated {
			generatedLines += file.Adds + file.Deletes
		}
	}
	generatedLines = min(generatedLines, totalLines)
	generatedFiles := min(prepared.ContextPack.Overview.GeneratedFiles, max(0, len(prepared.Paths)-binaryFiles))
	handwrittenLines := max(0, totalLines-generatedLines)
	handwrittenFiles := max(0, len(prepared.Paths)-generatedFiles-binaryFiles)

	effectiveLines := handwrittenLines + int(math.Ceil(0.15*float64(generatedLines))) + 50*binaryFiles
	effectiveFiles := handwrittenFiles + ceilDiv(generatedFiles, 4) + binaryFiles
	scopes := changedScopeCount(prepared.Paths)
	workUnits := 2*int(math.Ceil(math.Sqrt(float64(effectiveLines)/50))) +
		int(math.Ceil(math.Sqrt(float64(effectiveFiles)))) +
		int(math.Ceil(math.Log2(1+float64(max(0, scopes-1)))))
	return budgetMetrics{
		effectiveLines: effectiveLines,
		effectiveFiles: effectiveFiles,
		scopes:         scopes,
		workUnits:      workUnits,
	}
}

func changedScopeCount(paths []string) int {
	scopes := map[string]struct{}{}
	for _, path := range paths {
		path = filepath.ToSlash(strings.TrimSpace(path))
		if path == "" {
			continue
		}
		scope, _, nested := strings.Cut(path, "/")
		if !nested {
			scope = "."
		}
		scopes[scope] = struct{}{}
	}
	return len(scopes)
}

func capabilityCoverage(mode Mode, toolNames []string) (float64, error) {
	has := func(names ...string) bool {
		return slices.ContainsFunc(names, func(name string) bool {
			return slices.Contains(toolNames, name)
		})
	}
	if !has("read_file") {
		return 0, errors.New("inspection budget requires bounded source reading capability")
	}

	coverage := 0.30
	if has("grep", "inspect_file") {
		coverage += 0.20
	}
	if has("find", "list_files") {
		coverage += 0.10
	}
	if mode == ModeCodebase {
		if !has("list_files") {
			return 0, errors.New("codebase inspection budget requires scope discovery capability")
		}
		coverage += 0.20
		return coverage / 0.80, nil
	}
	if !has("review_changes") {
		return 0, errors.New("diff inspection budget requires authoritative scope enumeration capability")
	}
	coverage += 0.20
	if has("review_diff_for_paths") {
		coverage += 0.20
	}
	return coverage, nil
}

func ceilDiv(value, divisor int) int {
	if value <= 0 {
		return 0
	}
	return 1 + (value-1)/divisor
}
