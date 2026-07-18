package review

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/contextpack"
	"github.com/yusing/git-agent/internal/gitctx"
)

var fullDiffBudgetTools = []string{
	"list_files", "read_file", "inspect_file", "grep", "find",
	"review_changes", "review_diff_for_paths",
}

func TestPlanBudgetExamples(t *testing.T) {
	tests := []struct {
		name       string
		kind       Kind
		prepared   PreparedContext
		skills     int
		coverage   []string
		wantLower  int
		wantMiddle int
		wantUpper  int
		wantTools  int
	}{
		{
			name:       "tiny review",
			kind:       KindReview,
			prepared:   preparedBudgetChange(40, 1, []string{"main.go"}),
			coverage:   fullDiffBudgetTools,
			wantLower:  8,
			wantMiddle: 10,
			wantUpper:  12,
			wantTools:  8,
		},
		{
			name:       "tiny simplify",
			kind:       KindSimplify,
			prepared:   preparedBudgetChange(40, 1, []string{"main.go"}),
			coverage:   fullDiffBudgetTools,
			wantLower:  7,
			wantMiddle: 9,
			wantUpper:  10,
			wantTools:  8,
		},
		{
			name:       "medium review with skill",
			kind:       KindReview,
			prepared:   preparedBudgetChange(600, 8, budgetPaths(8, 3)),
			skills:     1,
			coverage:   fullDiffBudgetTools,
			wantLower:  14,
			wantMiddle: 19,
			wantUpper:  23,
			wantTools:  17,
		},
		{
			name:       "medium simplify with skill",
			kind:       KindSimplify,
			prepared:   preparedBudgetChange(600, 8, budgetPaths(8, 3)),
			skills:     1,
			coverage:   fullDiffBudgetTools,
			wantLower:  13,
			wantMiddle: 17,
			wantUpper:  21,
			wantTools:  15,
		},
		{
			name:     "large review with partial coverage",
			kind:     KindReview,
			prepared: preparedBudgetChange(5000, 45, budgetPaths(45, 8)),
			skills:   2,
			coverage: []string{
				"list_files", "read_file", "grep", "find", "review_changes",
			},
			wantLower:  23,
			wantMiddle: 35,
			wantUpper:  46,
			wantTools:  30,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := PlanBudget(BudgetInput{
				Kind: test.kind, Prepared: test.prepared, ToolNames: test.coverage,
				ApplicableSkills: test.skills, Depth: DepthBalanced,
			})
			if err != nil {
				t.Fatal(err)
			}
			if plan.LowerSteps != test.wantLower || plan.SelectedSteps != test.wantMiddle || plan.UpperSteps != test.wantUpper {
				t.Fatalf("steps = %d/%d/%d, want %d/%d/%d; plan = %#v",
					plan.LowerSteps, plan.SelectedSteps, plan.UpperSteps,
					test.wantLower, test.wantMiddle, test.wantUpper, plan)
			}
			if plan.MaxToolCalls != test.wantTools {
				t.Fatalf("MaxToolCalls = %d, want %d", plan.MaxToolCalls, test.wantTools)
			}
		})
	}
}

func TestPlanBudgetSelectsDepthAndPreservesExplicitOverride(t *testing.T) {
	prepared := preparedBudgetChange(600, 8, budgetPaths(8, 3))
	for _, test := range []struct {
		depth Depth
		want  int
	}{
		{DepthFast, 13},
		{DepthBalanced, 18},
		{DepthThorough, 22},
	} {
		plan, err := PlanBudget(BudgetInput{Kind: KindReview, Prepared: prepared, ToolNames: fullDiffBudgetTools, Depth: test.depth})
		if err != nil {
			t.Fatal(err)
		}
		if plan.SelectedSteps != test.want || plan.Policy != string(test.depth) || !plan.Automatic {
			t.Fatalf("depth %s plan = %#v, want selected %d automatic plan", test.depth, plan, test.want)
		}
	}

	plan, err := PlanBudget(BudgetInput{
		Kind: KindReview, Prepared: prepared, ToolNames: fullDiffBudgetTools,
		Depth: DepthFast, ExplicitMaxSteps: 73,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.SelectedSteps != 73 || plan.MaxToolCalls != ReviewMaxToolCalls || plan.Policy != "explicit" || plan.Automatic {
		t.Fatalf("explicit plan = %#v", plan)
	}
}

func TestPlanBudgetDiscountsGeneratedChangesAndCountsBinaryFiles(t *testing.T) {
	prepared := PreparedContext{
		Mode:  ModeUncommitted,
		Paths: []string{"gen.go", "asset.bin"},
		Stats: []gitctx.FileStat{
			{Path: "gen.go", Adds: 1000},
			{Path: "asset.bin", IsBinary: true},
		},
		ContextPack: contextpack.ContextPack{
			Overview: contextpack.ChangeOverview{Files: 2, Adds: 1000, GeneratedFiles: 1},
			Groups:   []contextpack.ChangeGroup{{Count: 1, Adds: 1000, Generated: true}},
		},
	}
	plan, err := PlanBudget(BudgetInput{Kind: KindReview, Prepared: prepared, ToolNames: fullDiffBudgetTools})
	if err != nil {
		t.Fatal(err)
	}
	if plan.EffectiveLines != 200 || plan.EffectiveFiles != 2 {
		t.Fatalf("effective size = %d lines/%d files, want 200/2; plan = %#v", plan.EffectiveLines, plan.EffectiveFiles, plan)
	}
}

func TestPlanBudgetRequiresEssentialCapabilities(t *testing.T) {
	prepared := preparedBudgetChange(40, 1, []string{"main.go"})
	for _, test := range []struct {
		name  string
		tools []string
		want  string
	}{
		{"source reading", []string{"review_changes"}, "source reading"},
		{"scope enumeration", []string{"read_file"}, "scope enumeration"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := PlanBudget(BudgetInput{Kind: KindReview, Prepared: prepared, ToolNames: test.tools})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestPlanBudgetCodebaseUsesFixedCeilings(t *testing.T) {
	plan, err := PlanBudget(BudgetInput{
		Kind: KindSimplify, Prepared: PreparedContext{Mode: ModeCodebase},
		ToolNames: []string{"list_files", "read_file", "grep", "find"}, Depth: DepthFast,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.LowerSteps != SimplifyMaxSteps || plan.SelectedSteps != SimplifyMaxSteps || plan.UpperSteps != SimplifyMaxSteps ||
		plan.MaxToolCalls != SimplifyMaxToolCalls || !plan.CodebaseFixedBudget {
		t.Fatalf("codebase plan = %#v", plan)
	}
}

func TestPlanBudgetIsMonotonicAndClamped(t *testing.T) {
	previous := 0
	for _, lines := range []int{1, 50, 100, 500, 5000, 50_000, 500_000} {
		plan, err := PlanBudget(BudgetInput{
			Kind: KindReview, Prepared: preparedBudgetChange(lines, 1, []string{"main.go"}),
			ToolNames: fullDiffBudgetTools, Depth: DepthBalanced,
		})
		if err != nil {
			t.Fatal(err)
		}
		if plan.SelectedSteps < previous {
			t.Fatalf("%d lines selected %d steps after %d", lines, plan.SelectedSteps, previous)
		}
		if plan.SelectedSteps > ReviewMaxSteps || plan.MaxToolCalls > ReviewMaxToolCalls || plan.LowerSteps > plan.SelectedSteps || plan.SelectedSteps > plan.UpperSteps {
			t.Fatalf("invalid bounded plan for %d lines: %#v", lines, plan)
		}
		previous = plan.SelectedSteps
	}
}

func TestParseDepth(t *testing.T) {
	for input, want := range map[string]Depth{"": DepthBalanced, "FAST": DepthFast, " thorough ": DepthThorough} {
		got, err := ParseDepth(input)
		if err != nil || got != want {
			t.Fatalf("ParseDepth(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := ParseDepth("exhaustive"); err == nil {
		t.Fatal("ParseDepth accepted invalid depth")
	}
}

func preparedBudgetChange(lines, files int, paths []string) PreparedContext {
	stats := make([]gitctx.FileStat, files)
	for i := range files {
		stats[i] = gitctx.FileStat{Path: paths[i]}
	}
	if len(stats) > 0 {
		stats[0].Adds = lines
	}
	return PreparedContext{
		Mode: ModeUncommitted, Paths: paths, Stats: stats,
		ContextPack: contextpack.ContextPack{Overview: contextpack.ChangeOverview{Files: files, Adds: lines}},
	}
}

func budgetPaths(files, scopes int) []string {
	paths := make([]string, files)
	for i := range files {
		paths[i] = fmt.Sprintf("scope-%d/file-%d.go", i%scopes, i)
	}
	return paths
}
