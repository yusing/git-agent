package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/openai"
	"github.com/yusing/git-agent/internal/tasks/releasenote"
)

func TestRunnerRepairsAfterContextFinalization(t *testing.T) {
	for _, budget := range []int{10000, 50000} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			artifact := func(summary string) string {
				return fmt.Sprintf(`{"sections":[{"heading":"New Features","bullets":[{"label":null,"summary":%q,"refs":[{"type":"commit","value":%q}],"children":[]}]}]}`, summary, sha)
			}
			draft := artifact(strings.Repeat("Operators can configure routes. ", 8))
			valid := artifact("Operators can configure routes")
			registry := newTestToolExecutor()
			var order executionLog
			registry.add(recordingTool{name: "read_file", order: &order, content: strings.Repeat("gathered evidence\n", 6000)})
			client := &fakeClient{responses: []openai.Response{
				{ToolCalls: []openai.ToolCall{{ID: "fc_1", CallID: "call_1", Name: "read_file", Arguments: `{}`}}},
				{Text: draft},
				{Text: valid},
			}}
			recorder, traceOutput := newTraceBuffer(t, "release-note")
			runner := OpenAIRunner{
				Config: config.Config{Model: "test", MaxSteps: 1, MaxToolCalls: 2, ContextTokens: budget},
				Client: client, Tools: registry, ToolSpecs: registry.Definitions([]string{"read_file"}),
				Validator: releasenote.Validate, Trace: recorder, PromptCacheKey: "release-repair",
			}
			prompt := releasenote.UserPrompt(releasenote.PreparedContext{Range: "v1..v2", ParentCommits: []releasenote.PreparedCommit{{SHA: sha, Summary: "feat: configurable routes"}}}, 1, 2)
			format := releasenote.TextFormat()
			result, err := runner.Run(t.Context(), Request{
				SystemPrompt: releasenote.SystemPrompt(), UserPrompt: prompt,
				ProjectGuidance: "Use operator-facing headlines", DeveloperInstructions: "Keep upgrade guidance",
				TextFormat: format, MaxSteps: 1, RepairOnValidator: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != valid || result.RepairCalls != 1 || result.ToolCalls != 1 || len(client.requests) != 3 {
				t.Fatalf("result = %#v, requests = %d", result, len(client.requests))
			}
			repair := client.requests[2]
			if estimateRequestTokens(repair) >= budget || len(repair.Tools) != 0 || repair.TextFormat != format || repair.Instructions != client.requests[0].Instructions || repair.PromptCacheKey != "release-repair" {
				t.Fatal("repair exceeded budget or lost request contracts")
			}
			var contents []string
			var toolOutputs int
			for _, item := range repair.Input {
				contents = append(contents, item.Content)
				if item.Type == "function_call_output" {
					toolOutputs++
				}
			}
			for _, want := range []string{prompt, draft, "Use operator-facing headlines", "Keep upgrade guidance", "too long"} {
				if !strings.Contains(strings.Join(contents, "\n"), want) {
					t.Fatalf("repair lost %q", want)
				}
			}
			compacted := budget == 10000
			if (toolOutputs == 0) != compacted || strings.Contains(traceOutput.String(), "repair_history_compacted") != compacted {
				t.Fatal("repair must shorten only over-budget history")
			}
		})
	}
}
