package checks

import (
	"context"
	"fmt"
	"reflect"
	"slices"
)

const PrivateCommand = "__git-agent-check"

type Plan interface {
	CheckerName() string
	Runnable() bool
	SkipReason() string
}

type Runner interface {
	Name() string
	Plan(Scope) (Plan, error)
	Run(context.Context, string, Plan) (Result, error)
}

type Helper interface {
	Runner
	RunHelper([]string) error
}

type Progress func(string) error

type Set struct {
	ordered []Runner
	byName  map[string]Runner
}

func NewSet(runners ...Runner) (*Set, error) {
	if len(runners) == 0 {
		return nil, fmt.Errorf("checker set requires at least one runner")
	}
	set := &Set{
		ordered: make([]Runner, 0, len(runners)),
		byName:  make(map[string]Runner, len(runners)),
	}
	for index, runner := range runners {
		if nilInterface(runner) {
			return nil, fmt.Errorf("checker registration %d is nil", index)
		}
		name := runner.Name()
		if err := validateCheckerName(name); err != nil {
			return nil, fmt.Errorf("checker registration %d: %w", index, err)
		}
		if _, exists := set.byName[name]; exists {
			return nil, fmt.Errorf("duplicate checker registration %q", name)
		}
		set.ordered = append(set.ordered, runner)
		set.byName[name] = runner
	}
	return set, nil
}

type PreparedSet struct {
	scope Scope
	items []preparedRunner
}

type preparedRunner struct {
	name   string
	runner Runner
	plan   Plan
}

type PlanSummary struct {
	Name       string
	Runnable   bool
	SkipReason string
}

func (s *Set) Prepare(scope Scope) (*PreparedSet, error) {
	if s == nil || len(s.ordered) == 0 {
		return nil, fmt.Errorf("checker set is uninitialized")
	}
	if err := scope.validate(); err != nil {
		return nil, err
	}
	prepared := &PreparedSet{scope: scope, items: make([]preparedRunner, 0, len(s.ordered))}
	for _, runner := range s.ordered {
		name := runner.Name()
		plan, err := runner.Plan(scope)
		if err != nil {
			return nil, fmt.Errorf("plan check %q: %w", name, err)
		}
		if nilInterface(plan) {
			return nil, fmt.Errorf("plan check %q: runner returned nil plan", name)
		}
		if plan.CheckerName() != name {
			return nil, fmt.Errorf("plan check %q: plan names checker %q", name, plan.CheckerName())
		}
		if plan.Runnable() {
			if plan.SkipReason() != "" {
				return nil, fmt.Errorf("plan check %q: runnable plan contains skip reason", name)
			}
		} else if _, err := NewSkipped(name, plan.SkipReason()); err != nil {
			return nil, fmt.Errorf("plan check %q: %w", name, err)
		}
		prepared.items = append(prepared.items, preparedRunner{name: name, runner: runner, plan: plan})
	}
	return prepared, nil
}

func (p *PreparedSet) Summaries() []PlanSummary {
	if p == nil {
		return nil
	}
	summaries := make([]PlanSummary, 0, len(p.items))
	for _, item := range p.items {
		summaries = append(summaries, PlanSummary{
			Name: item.name, Runnable: item.plan.Runnable(), SkipReason: item.plan.SkipReason(),
		})
	}
	return summaries
}

func (p *PreparedSet) Run(ctx context.Context, executable string, progress Progress) ([]Result, error) {
	if p == nil || len(p.items) == 0 {
		return nil, fmt.Errorf("prepared checker set is uninitialized")
	}
	if err := p.scope.validate(); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(p.items))
	for _, item := range p.items {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !item.plan.Runnable() {
			result, err := NewSkipped(item.name, item.plan.SkipReason())
			if err != nil {
				return nil, err
			}
			results = append(results, result)
			continue
		}
		if progress != nil {
			if err := progress(item.name); err != nil {
				return nil, fmt.Errorf("report check %q progress: %w", item.name, err)
			}
		}
		result, err := item.runner.Run(ctx, executable, item.plan)
		if err != nil {
			return nil, fmt.Errorf("run check %q: %w", item.name, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if result.Name != item.name {
			return nil, fmt.Errorf("run check %q: result names checker %q", item.name, result.Name)
		}
		if err := validateResultForScope(result, p.scope); err != nil {
			return nil, fmt.Errorf("run check %q: %w", item.name, err)
		}
		results = append(results, result)
	}
	return results, nil
}

func (p *PreparedSet) SyntheticResults(diagnostics func(string) []Diagnostic) ([]Result, error) {
	if p == nil || len(p.items) == 0 {
		return nil, fmt.Errorf("prepared checker set is uninitialized")
	}
	results := make([]Result, 0, len(p.items))
	for _, item := range p.items {
		if !item.plan.Runnable() {
			result, err := NewSkipped(item.name, item.plan.SkipReason())
			if err != nil {
				return nil, err
			}
			results = append(results, result)
			continue
		}
		result, err := NewResult(item.name, diagnostics(item.name))
		if err != nil {
			return nil, err
		}
		if err := validateResultForScope(result, p.scope); err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (s *Set) DispatchHelper(args []string) error {
	if s == nil || len(s.ordered) == 0 {
		return fmt.Errorf("checker set is uninitialized")
	}
	if len(args) == 0 {
		return fmt.Errorf("private checker request requires a registered checker name")
	}
	name := args[0]
	if err := validateCheckerName(name); err != nil {
		return fmt.Errorf("private checker request: %w", err)
	}
	runner, exists := s.byName[name]
	if !exists {
		return fmt.Errorf("private checker request names unknown checker %q", name)
	}
	helper, ok := runner.(Helper)
	if !ok {
		return fmt.Errorf("checker %q has no private helper", name)
	}
	return helper.RunHelper(slices.Clone(args[1:]))
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
