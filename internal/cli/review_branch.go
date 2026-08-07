package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/yusing/git-agent/internal/agent"
	"github.com/yusing/git-agent/internal/openai"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
	"github.com/yusing/git-agent/internal/tools"
	"github.com/yusing/git-agent/internal/trace"
)

type branchNode struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	Depth    int    `json:"depth"`
}

type branchChild struct {
	branchNode
	Scope           string `json:"scope"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type branchProgress struct {
	Active     int `json:"active"`
	Completed  int `json:"completed"`
	Failed     int `json:"failed"`
	TotalKnown int `json:"total_known"`
}

type reviewTreeResult struct {
	Text            string
	ToolCalls       int
	RepairCalls     int
	ToolCallsByName map[string]int
	UsedSkills      []string
	Branches        []reviewBranchMetric
}

type reviewBranchMetric struct {
	ID              string
	ParentID        string
	Model           string
	ReasoningEffort string
	Usage           openai.Usage
}

type reviewTree struct {
	kind     reviewtask.Kind
	depth    reviewtask.Depth
	recorder *trace.Recorder
	cancel   context.CancelFunc

	mu           sync.Mutex
	nextID       int
	progress     branchProgress
	failure      error
	observeUsage func(openai.Usage)
	branches     map[string]*reviewBranchMetric
	branchOrder  []string
}

type reviewNodeResult struct {
	leaves          []reviewtask.LeafReport
	toolCalls       int
	repairCalls     int
	toolCallsByName map[string]int
	usedSkills      []string
}

func runReviewTree(
	ctx context.Context,
	kind reviewtask.Kind,
	depth reviewtask.Depth,
	runner agent.OpenAIRunner,
	request agent.Request,
	recorder *trace.Recorder,
) (reviewTreeResult, error) {
	treeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	tree := &reviewTree{
		kind: kind, depth: depth, recorder: recorder, cancel: cancel,
		progress:     branchProgress{Active: 1, TotalKnown: 1},
		observeUsage: runner.ObserveUsage, branches: map[string]*reviewBranchMetric{},
	}
	result, err := tree.runNode(treeCtx, runner, request, branchNode{ID: "root"}, "")
	if err != nil {
		return reviewTreeResult{}, err
	}
	if len(result.leaves) == 0 {
		return reviewTreeResult{}, errors.New("inspection returned no validated leaves")
	}
	if len(result.leaves) == 1 {
		return reviewTreeResult{
			Text: result.leaves[0].Text, ToolCalls: result.toolCalls, RepairCalls: result.repairCalls,
			ToolCallsByName: result.toolCallsByName, UsedSkills: result.usedSkills,
			Branches: tree.branchMetrics(),
		}, nil
	}
	if err := recorder.WriteExact("runtime.status", map[string]any{
		"phase": "aggregating_branches", "branch_progress": tree.snapshotProgress(),
	}); err != nil {
		return reviewTreeResult{}, err
	}
	text, err := reviewtask.Aggregate(kind, result.leaves)
	if err != nil {
		return reviewTreeResult{}, err
	}
	if runner.Validator != nil {
		if validationErrors := runner.Validator(text); len(validationErrors) > 0 {
			return reviewTreeResult{}, fmt.Errorf("validate aggregated branch report: %v", validationErrors)
		}
	}
	return reviewTreeResult{
		Text: text, ToolCalls: result.toolCalls, RepairCalls: result.repairCalls,
		ToolCallsByName: result.toolCallsByName, UsedSkills: result.usedSkills,
		Branches: tree.branchMetrics(),
	}, nil
}

func (t *reviewTree) runNode(
	ctx context.Context,
	runner agent.OpenAIRunner,
	request agent.Request,
	node branchNode,
	scope string,
) (reviewNodeResult, error) {
	runner.ObserveUsage = func(usage openai.Usage) {
		if t.observeUsage != nil {
			t.observeUsage(usage)
		}
		if node.ID != "root" {
			t.recordBranchUsage(node.ID, usage)
		}
	}
	if definition, ok := reviewtask.BranchDefinition(t.kind, t.depth, node.Depth); ok {
		request.ControlTool = &definition
	} else {
		request.ControlTool = nil
		runner.ToolSpecs = slices.DeleteFunc(slices.Clone(runner.ToolSpecs), func(definition tools.Definition) bool {
			return definition.Name == reviewtask.BranchHelpToolName
		})
	}
	outcome, err := runner.RunNode(ctx, request)
	if err != nil {
		t.failNode(node, err)
		return reviewNodeResult{}, err
	}
	if outcome.Final != nil {
		return t.completeLeaf(node, scope, *outcome.Final)
	}
	if outcome.Branch == nil {
		err := errors.New("inspection conversation returned no outcome")
		t.failNode(node, err)
		return reviewNodeResult{}, err
	}

	branchRequest, err := reviewtask.ParseBranchRequest(t.kind, t.depth, node.Depth, outcome.Branch.Arguments)
	if err != nil {
		t.failNode(node, err)
		return reviewNodeResult{}, err
	}
	children, err := t.acceptFanout(node, runner, branchRequest)
	if err != nil {
		t.failNode(node, err)
		return reviewNodeResult{}, err
	}

	results := make([]reviewNodeResult, len(children))
	errorsByChild := make([]error, len(children))
	var wait sync.WaitGroup
	for index, child := range children {
		wait.Go(func() {
			childRunner := runner
			childRunner.Config.Model = child.Model
			childRunner.Config.ThinkingEffort = child.ReasoningEffort
			childRunner.Trace = t.childTrace(child.branchNode)
			siblingScopes := make([]string, 0, len(children)-1)
			for siblingIndex, sibling := range children {
				if siblingIndex != index {
					siblingScopes = append(siblingScopes, sibling.Scope)
				}
			}
			branchResult, encodeErr := reviewtask.EncodeBranchResult(reviewtask.BranchResult{
				BranchID: child.ID, ParentBranchID: node.ID,
				Message: "In scope: " + child.Scope, PathHints: branchRequest.Branches[index].PathHints,
				SiblingScopes: siblingScopes, Model: child.Model,
				ReasoningEffort: child.ReasoningEffort, Depth: child.Depth,
			})
			if encodeErr != nil {
				errorsByChild[index] = encodeErr
				t.recordFailure(encodeErr)
				t.cancel()
				return
			}
			childRequest := request
			childRequest.Input = outcome.Branch.ForkInput(
				branchResult,
				child.Model == runner.Config.Model,
			)
			childRequest.TurnState = outcome.Branch.TurnState
			childRequest.ControlTool = nil
			results[index], errorsByChild[index] = t.runNode(ctx, childRunner, childRequest, child.branchNode, child.Scope)
			if errorsByChild[index] != nil {
				t.recordFailure(errorsByChild[index])
				t.cancel()
			}
		})
	}
	wait.Wait()
	for _, childErr := range errorsByChild {
		if childErr != nil {
			if failure := t.recordedFailure(); failure != nil {
				return reviewNodeResult{}, failure
			}
			return reviewNodeResult{}, childErr
		}
	}
	result := reviewNodeResult{
		toolCalls: outcome.Branch.ToolCalls, repairCalls: outcome.Branch.RepairCalls,
		toolCallsByName: maps.Clone(outcome.Branch.ToolCallsByName),
		usedSkills:      slices.Clone(outcome.Branch.UsedSkills),
	}
	for _, childResult := range results {
		result.leaves = append(result.leaves, childResult.leaves...)
		result.toolCalls += childResult.toolCalls
		result.repairCalls += childResult.repairCalls
		mergeToolCalls(result.toolCallsByName, childResult.toolCallsByName)
		result.usedSkills = appendUnique(result.usedSkills, childResult.usedSkills...)
	}
	return result, nil
}

func (t *reviewTree) acceptFanout(
	parent branchNode,
	runner agent.OpenAIRunner,
	request reviewtask.BranchRequest,
) ([]branchChild, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	children := make([]branchChild, len(request.Branches))
	for index, branch := range request.Branches {
		t.nextID++
		model := branch.Model
		if model == "inherit" {
			model = runner.Config.Model
		}
		effort := branch.ReasoningEffort
		if effort == "inherit" {
			effort = runner.Config.ThinkingEffort
		}
		children[index] = branchChild{
			branchNode: branchNode{ID: fmt.Sprintf("b%d", t.nextID), ParentID: parent.ID, Depth: parent.Depth + 1},
			Scope:      branch.Scope, Model: model, ReasoningEffort: effort,
		}
		t.branches[children[index].ID] = &reviewBranchMetric{
			ID: children[index].ID, ParentID: children[index].ParentID,
			Model: children[index].Model, ReasoningEffort: children[index].ReasoningEffort,
		}
		t.branchOrder = append(t.branchOrder, children[index].ID)
	}
	t.progress.Active--
	t.progress.Active += len(children)
	t.progress.TotalKnown += len(children)
	if err := t.recorder.WriteExact("branch.fanout", map[string]any{
		"parent": parent, "children": children,
	}); err != nil {
		return nil, err
	}
	if err := t.writeProgressLocked(); err != nil {
		return nil, err
	}
	return children, nil
}

func (t *reviewTree) recordBranchUsage(id string, usage openai.Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	metric := t.branches[id]
	metric.Usage.Add(usage)
}

func (t *reviewTree) branchMetrics() []reviewBranchMetric {
	t.mu.Lock()
	defer t.mu.Unlock()
	metrics := make([]reviewBranchMetric, 0, len(t.branchOrder))
	for _, id := range t.branchOrder {
		metrics = append(metrics, *t.branches[id])
	}
	return metrics
}

func (t *reviewTree) childTrace(node branchNode) *trace.Recorder {
	recorder, _ := trace.NewEventSink(func(event trace.Event) error {
		return t.recorder.WriteExact("branch.event", map[string]any{
			"node":  node,
			"event": map[string]any{"kind": event.Kind, "value": event.Value},
		})
	})
	return recorder
}

func (t *reviewTree) completeLeaf(node branchNode, scope string, result agent.Result) (reviewNodeResult, error) {
	leaf := reviewtask.LeafReport{Scope: scope, Text: result.Text}
	if node.ID != "root" {
		var err error
		leaf, err = reviewtask.ParseLeaf(t.kind, scope, result.Text)
		if err != nil {
			t.failNode(node, err)
			return reviewNodeResult{}, err
		}
		itemCount := len(leaf.Findings) + len(leaf.Opportunities)
		t.mu.Lock()
		t.progress.Active--
		t.progress.Completed++
		err = t.recorder.WriteExact("branch.completed", map[string]any{
			"node": node, "summary": reviewtask.BoundedBranchMessage(leaf.Summary), "item_count": itemCount,
		})
		if err == nil {
			err = t.writeProgressLocked()
		}
		t.mu.Unlock()
		if err != nil {
			return reviewNodeResult{}, err
		}
	}
	return reviewNodeResult{
		leaves: []reviewtask.LeafReport{leaf}, toolCalls: result.ToolCalls, repairCalls: result.RepairCalls,
		toolCallsByName: maps.Clone(result.ToolCallsByName), usedSkills: slices.Clone(result.UsedSkills),
	}, nil
}

func mergeToolCalls(destination, source map[string]int) {
	for name, count := range source {
		destination[name] += count
	}
}

func appendUnique(destination []string, values ...string) []string {
	for _, value := range values {
		if !slices.Contains(destination, value) {
			destination = append(destination, value)
		}
	}
	return destination
}

func (t *reviewTree) failNode(node branchNode, failure error) {
	t.recordFailure(failure)
	t.cancel()
	if node.ID == "root" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.progress.Active--
	t.progress.Failed++
	_ = t.recorder.WriteExact("branch.failed", map[string]any{
		"node": node, "message": reviewtask.BoundedBranchMessage(failure.Error()),
	})
	_ = t.writeProgressLocked()
}

func (t *reviewTree) writeProgressLocked() error {
	return t.recorder.WriteExact("runtime.status", map[string]any{
		"phase": "branches_running", "branch_progress": t.progress,
	})
}

func (t *reviewTree) snapshotProgress() branchProgress {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.progress
}

func (t *reviewTree) recordFailure(failure error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failure == nil || isCancellation(t.failure) && !isCancellation(failure) {
		t.failure = failure
	}
}

func (t *reviewTree) recordedFailure() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failure
}

func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
