package checks

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type fakePlan struct {
	name     string
	runnable bool
	reason   string
}

func (p fakePlan) CheckerName() string { return p.name }
func (p fakePlan) Runnable() bool      { return p.runnable }
func (p fakePlan) SkipReason() string  { return p.reason }

type fakeRunner struct {
	name         string
	plan         Plan
	planErr      error
	result       Result
	runErr       error
	receivedPath []string
	plannedPath  []string
	runCount     int
	helperCount  int
	helperArgs   []string
}

func (r *fakeRunner) Name() string { return r.name }

func (r *fakeRunner) Plan(scope Scope) (Plan, error) {
	r.receivedPath = scope.Paths()
	r.plannedPath = scope.Paths()
	if len(r.plannedPath) > 0 {
		r.plannedPath[0] = "attempted-mutation.go"
	}
	return r.plan, r.planErr
}

func (r *fakeRunner) Run(context.Context, string, Plan) (Result, error) {
	r.runCount++
	return r.result, r.runErr
}

func (r *fakeRunner) RunHelper(args []string) error {
	r.helperCount++
	r.helperArgs = args
	return nil
}

func TestSetRunsRegisteredCheckersInOrderWithImmutableScope(t *testing.T) {
	scope, err := NewChangedScope(t.TempDir(), []string{"z.go", "./a.go", "a.go"}, []string{"", "nested"})
	if err != nil {
		t.Fatal(err)
	}
	firstResult, err := NewResult("first", []Diagnostic{{Path: "a.go", Line: 1, Code: "A1", Message: "first"}})
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := NewResult("second", nil)
	if err != nil {
		t.Fatal(err)
	}
	first := &fakeRunner{name: "first", plan: fakePlan{name: "first", runnable: true}, result: firstResult}
	second := &fakeRunner{name: "second", plan: fakePlan{name: "second", runnable: true}, result: secondResult}
	set, err := NewSet(first, second)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := set.Prepare(scope)
	if err != nil {
		t.Fatal(err)
	}
	var progress []string
	results, err := prepared.Run(context.Background(), "/current/executable", func(name string) error {
		progress = append(progress, name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{results[0].Name, results[1].Name}; !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("result order = %#v", got)
	}
	if !reflect.DeepEqual(progress, []string{"first", "second"}) {
		t.Fatalf("progress = %#v", progress)
	}
	if got := scope.Paths(); !reflect.DeepEqual(got, []string{"a.go", "z.go"}) {
		t.Fatalf("scope mutated through runner: %#v", got)
	}
	components := scope.Components()
	components[0] = "mutated"
	if got := scope.Components(); !reflect.DeepEqual(got, []string{"", "nested"}) {
		t.Fatalf("scope components mutated through accessor: %#v", got)
	}
	if !reflect.DeepEqual(second.receivedPath, []string{"a.go", "z.go"}) {
		t.Fatalf("second runner received mutated scope: %#v", second.receivedPath)
	}
}

func TestSetBuildsSkipWithoutProgressOrExecution(t *testing.T) {
	scope, err := NewCodebaseScope(t.TempDir(), []string{""})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		name: "future-checker",
		plan: fakePlan{name: "future-checker", reason: "no supported project"},
	}
	set, err := NewSet(runner)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := set.Prepare(scope)
	if err != nil {
		t.Fatal(err)
	}
	progressCalls := 0
	results, err := prepared.Run(context.Background(), "", func(string) error {
		progressCalls++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner.runCount != 0 || progressCalls != 0 {
		t.Fatalf("run count = %d, progress = %d", runner.runCount, progressCalls)
	}
	if len(results) != 1 || results[0].Status != StatusSkipped || results[0].Reason != "no supported project" {
		t.Fatalf("results = %#v", results)
	}
}

func TestSetRejectsMalformedDuplicateAndMismatchedContracts(t *testing.T) {
	valid := &fakeRunner{name: "valid", plan: fakePlan{name: "valid", runnable: true}}
	var nilRunner *fakeRunner
	for name, runners := range map[string][]Runner{
		"empty":     nil,
		"nil":       {nilRunner},
		"malformed": {&fakeRunner{name: "../bad"}},
		"duplicate": {valid, &fakeRunner{name: "valid"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewSet(runners...); err == nil {
				t.Fatalf("registrations accepted: %#v", runners)
			}
		})
	}

	scope, err := NewCodebaseScope(t.TempDir(), []string{""})
	if err != nil {
		t.Fatal(err)
	}
	for name, runner := range map[string]*fakeRunner{
		"nil-plan":        {name: "check"},
		"mismatched-plan": {name: "check", plan: fakePlan{name: "future", runnable: true}},
		"bad-skip":        {name: "check", plan: fakePlan{name: "check"}},
		"mixed-plan":      {name: "check", plan: fakePlan{name: "check", runnable: true, reason: "skip"}},
	} {
		t.Run(name, func(t *testing.T) {
			set, err := NewSet(runner)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := set.Prepare(scope); err == nil {
				t.Fatal("malformed plan accepted")
			}
		})
	}
}

func TestSetRejectsOutOfScopeAndFutureResultCollisions(t *testing.T) {
	scope, err := NewChangedScope(t.TempDir(), []string{"selected.go"}, []string{""})
	if err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]Result{
		"unrelated-path": {
			Name: "check", Status: StatusFindings,
			Diagnostics: []Diagnostic{{Path: "unrelated.go", Line: 1, Message: "collision"}},
		},
		"future-name": {Name: "future", Status: StatusPass, Diagnostics: []Diagnostic{}},
		"unknown-status": {
			Name: "check", Status: "future", Diagnostics: []Diagnostic{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{
				name: "check", plan: fakePlan{name: "check", runnable: true}, result: result,
			}
			set, err := NewSet(runner)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := set.Prepare(scope)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := prepared.Run(context.Background(), "", nil); err == nil {
				t.Fatalf("result accepted: %#v", result)
			}
		})
	}
}

func TestDispatchHelperRejectsUnknownFutureAndNonHelperRegistrations(t *testing.T) {
	helper := &fakeRunner{name: "helper", plan: fakePlan{name: "helper", runnable: true}}
	set, err := NewSet(helper)
	if err != nil {
		t.Fatal(err)
	}
	if err := set.DispatchHelper([]string{"helper", "--opaque", "value"}); err != nil {
		t.Fatal(err)
	}
	if helper.helperCount != 1 || !reflect.DeepEqual(helper.helperArgs, []string{"--opaque", "value"}) {
		t.Fatalf("helper calls = %d, args = %#v", helper.helperCount, helper.helperArgs)
	}
	for _, args := range [][]string{nil, {"../bad"}, {"future"}} {
		if err := set.DispatchHelper(args); err == nil {
			t.Fatalf("helper request accepted: %#v", args)
		}
	}

	plain := runnerWithoutHelper{name: "plain", plan: fakePlan{name: "plain", runnable: true}}
	plainSet, err := NewSet(&plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := plainSet.DispatchHelper([]string{"plain"}); err == nil {
		t.Fatal("non-helper runner accepted private helper dispatch")
	}
}

type runnerWithoutHelper struct {
	name string
	plan Plan
}

func (r *runnerWithoutHelper) Name() string { return r.name }
func (r *runnerWithoutHelper) Plan(Scope) (Plan, error) {
	return r.plan, nil
}
func (r *runnerWithoutHelper) Run(context.Context, string, Plan) (Result, error) {
	return NewResult(r.name, nil)
}

func TestResultNormalizationBoundsAndValidation(t *testing.T) {
	var diagnostics []Diagnostic
	for index := range 102 {
		diagnostics = append(diagnostics, Diagnostic{
			Path: "many.go", Line: index + 1, Code: " lint\n", Message: fmt.Sprintf(" message\t%03d ", index),
		})
	}
	diagnostics = append(diagnostics, diagnostics[0])
	result, err := NewResult("checker", diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusFindings || len(result.Diagnostics) != 100 || result.Omitted != 2 {
		t.Fatalf("result = %#v", result)
	}
	if result.Diagnostics[0].Code != "lint" || strings.Contains(result.Diagnostics[0].Message, "\t") {
		t.Fatalf("diagnostic not normalized: %#v", result.Diagnostics[0])
	}

	checkErr, err := NewError("checker", errors.New(strings.Repeat("é", 800)+"\nsecret"))
	if err != nil {
		t.Fatal(err)
	}
	if len(checkErr.Error) > MaxResultError || strings.Contains(checkErr.Error, "\n") {
		t.Fatalf("bounded error = %q (%d bytes)", checkErr.Error, len(checkErr.Error))
	}
}
