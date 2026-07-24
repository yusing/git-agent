package review

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/tools"
)

func TestBranchHelpPublishesCatalogAndKindSpecificEfforts(t *testing.T) {
	tests := []struct {
		kind      Kind
		wantXHigh bool
	}{
		{kind: KindReview, wantXHigh: true},
		{kind: KindSimplify},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			tool := BranchHelp(test.kind)
			definition := tool.Definition()
			if definition.Name != BranchHelpToolName ||
				definition.Description != "Use before deciding to use `branch`" ||
				!definition.Strict {
				t.Fatalf("definition = %#v", definition)
			}
			required, ok := definition.Schema["required"].([]string)
			if !ok || len(required) != 0 {
				t.Fatalf("required = %#v, want empty array", definition.Schema["required"])
			}
			result, err := tool.Execute(context.Background(), tools.Invocation{
				Name: BranchHelpToolName, Arguments: `{}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				OK   bool           `json:"ok"`
				Tool string         `json:"tool"`
				Data branchHelpData `json:"data"`
			}
			if err := json.Unmarshal([]byte(result.Content), &envelope); err != nil {
				t.Fatal(err)
			}
			if !envelope.OK || envelope.Tool != BranchHelpToolName || len(envelope.Data.Models) != len(branchModels) {
				t.Fatalf("envelope = %#v", envelope)
			}
			hasXHigh := slices.Contains(envelope.Data.AllowedReasoningEfforts, "xhigh")
			if hasXHigh != test.wantXHigh {
				t.Fatalf("allowed efforts = %#v, want xhigh=%v", envelope.Data.AllowedReasoningEfforts, test.wantXHigh)
			}
		})
	}
}

func TestBranchDefinitionFollowsDepthPolicyAndStrictCatalog(t *testing.T) {
	tests := []struct {
		name        string
		kind        Kind
		depth       Depth
		nodeDepth   int
		want        bool
		maxChildren int
		wantXHigh   bool
	}{
		{name: "fast root", kind: KindReview, depth: DepthFast, want: true, maxChildren: 2, wantXHigh: true},
		{name: "balanced root", kind: KindReview, depth: DepthBalanced, want: true, maxChildren: 3, wantXHigh: true},
		{name: "thorough child", kind: KindReview, depth: DepthThorough, nodeDepth: 1, want: true, maxChildren: 4, wantXHigh: true},
		{name: "thorough leaf", kind: KindReview, depth: DepthThorough, nodeDepth: 2},
		{name: "simplify omits xhigh", kind: KindSimplify, depth: DepthBalanced, want: true, maxChildren: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition, ok := BranchDefinition(test.kind, test.depth, test.nodeDepth)
			if ok != test.want {
				t.Fatalf("available = %v, want %v", ok, test.want)
			}
			if !ok {
				return
			}
			if definition.Name != BranchToolName || !definition.Strict {
				t.Fatalf("definition = %#v", definition)
			}
			if strings.Contains(definition.Description, "Model catalog:") || strings.Contains(definition.Description, "Effort guidance:") {
				t.Fatalf("branch description still embeds help content: %q", definition.Description)
			}
			branches := definition.Schema["properties"].(map[string]any)["branches"].(map[string]any)
			if branches["minItems"] != 2 || branches["maxItems"] != test.maxChildren {
				t.Fatalf("branches schema = %#v", branches)
			}
			item := branches["items"].(map[string]any)
			if item["additionalProperties"] != false {
				t.Fatalf("child schema is not strict: %#v", item)
			}
			efforts := item["properties"].(map[string]any)["reasoning_effort"].(map[string]any)["enum"].([]string)
			hasXHigh := false
			for _, effort := range efforts {
				hasXHigh = hasXHigh || effort == "xhigh"
			}
			if hasXHigh != test.wantXHigh {
				t.Fatalf("efforts = %#v, want xhigh=%v", efforts, test.wantXHigh)
			}
		})
	}
}

func TestParseBranchRequestValidatesCompleteShapeBeforeFanout(t *testing.T) {
	valid := `{"branches":[
		{"scope":"Review lifecycle behavior.","path_hints":["internal/cli"],"model":"gpt-5.6-sol","reasoning_effort":"high"},
		{"scope":"Review aggregation behavior.","path_hints":[],"model":"inherit","reasoning_effort":"inherit"}
	]}`
	request, err := ParseBranchRequest(KindReview, DepthBalanced, 0, valid)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Branches) != 2 || request.Branches[1].PathHints == nil {
		t.Fatalf("request = %#v", request)
	}

	tests := []struct {
		name string
		json string
		want string
	}{
		{name: "one child", json: `{"branches":[{"scope":"one","path_hints":[],"model":"inherit","reasoning_effort":"inherit"}]}`, want: "requires 2 to 3"},
		{name: "unknown field", json: strings.Replace(valid, `"scope":"Review lifecycle behavior."`, `"scope":"Review lifecycle behavior.","extra":true`, 1), want: "unknown field"},
		{name: "unsafe hint", json: strings.Replace(valid, `"internal/cli"`, `"../outside"`, 1), want: "safe repository-relative"},
		{name: "invented model", json: strings.Replace(valid, `"gpt-5.6-sol"`, `"gpt-next"`, 1), want: "model is invalid"},
		{name: "simplify xhigh", json: strings.Replace(valid, `"high"`, `"xhigh"`, 1), want: "reasoning_effort is invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind := KindReview
			if test.name == "simplify xhigh" {
				kind = KindSimplify
			}
			_, err := ParseBranchRequest(kind, DepthBalanced, 0, test.json)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAggregateMechanicallyMergesValidatedLeaves(t *testing.T) {
	reviewText, err := Aggregate(KindReview, []LeafReport{
		mustParseLeaf(t, KindReview, "first", `{"summary":"first summary","recommendation":"COMMENT","findings":[{"severity":"LOW","aspect":"tests","title":"low first","impact":"impact","evidences":[{"title":"line","path":"a.go","line_start":1,"line_end":1}],"proposed_fix":"fix"}]}`),
		mustParseLeaf(t, KindReview, "second", `{"summary":"second summary","recommendation":"FIX","findings":[{"severity":"HIGH","aspect":"correctness","title":"high second","impact":"impact","evidences":[{"title":"line","path":"b.go","line_start":1,"line_end":1}],"proposed_fix":"fix"},{"severity":"LOW","aspect":"style","title":"low second","impact":"impact","evidences":[{"title":"line","path":"b.go","line_start":2,"line_end":2}],"proposed_fix":"fix"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var review ReviewReport
	if err := decodeStrict(reviewText, &review); err != nil {
		t.Fatal(err)
	}
	if review.Recommendation != "FIX" || len(review.Findings) != 3 {
		t.Fatalf("review = %#v", review)
	}
	if review.Findings[0].Title != "high second" || review.Findings[1].Title != "low first" || review.Findings[2].Title != "low second" {
		t.Fatalf("finding order = %#v", review.Findings)
	}
	if review.Summary != "first: first summary\nsecond: second summary" {
		t.Fatalf("summary = %q", review.Summary)
	}

	simplifyText, err := Aggregate(KindSimplify, []LeafReport{
		mustParseLeaf(t, KindSimplify, "one", `{"summary":"one summary","opportunities":[]}`),
		mustParseLeaf(t, KindSimplify, "two", `{"summary":"two summary","opportunities":[{"aspect":"clarity","title":"flatten","body":"nested","evidences":[{"title":"line","path":"a.go","line_start":1,"line_end":1}],"proposed_change":"return early"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var simplify SimplifyReport
	if err := decodeStrict(simplifyText, &simplify); err != nil {
		t.Fatal(err)
	}
	if simplify.Summary != "one: one summary\ntwo: two summary" || len(simplify.Opportunities) != 1 {
		t.Fatalf("simplify = %#v", simplify)
	}
}

func mustParseLeaf(t *testing.T, kind Kind, scope, text string) LeafReport {
	t.Helper()
	leaf, err := ParseLeaf(kind, scope, text)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}
