package golangci

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yusing/git-agent/internal/checks"
)

func TestPlanChangedUsesOnlyExactGoFilesAndNearestModulePackage(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "go.mod", "module root\n")
	writePlanFile(t, root, "a/a.go", "package a\n")
	writePlanFile(t, root, "a/other.go", "package a\n")
	writePlanFile(t, root, "b/b.go", "package b\n")
	writePlanFile(t, root, "nested/go.mod", "module nested\n")
	writePlanFile(t, root, "nested/c/c.go", "package c\n")
	writePlanFile(t, root, "README.md", "not Go\n")
	scope, err := checks.NewChangedScope(root, []string{
		"nested/c/c.go", "a/a.go", "a/other.go", "b/b.go", "README.md", "deleted.go",
	}, []string{""})
	if err != nil {
		t.Fatal(err)
	}

	genericPlan, err := New().Plan(scope)
	if err != nil {
		t.Fatal(err)
	}
	plan := genericPlan.(*checkerPlan)
	want := []invocation{
		{moduleRoot: root, targets: []string{"a/a.go", "a/other.go"}},
		{moduleRoot: root, targets: []string{"b/b.go"}},
		{moduleRoot: filepath.Join(root, "nested"), targets: []string{"c/c.go"}},
	}
	if !reflect.DeepEqual(plan.invocations, want) {
		t.Fatalf("invocations:\n got %#v\nwant %#v", plan.invocations, want)
	}
}

func TestPlanChangedSkipsNonGoOutsideModulesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "plain.go", "package plain\n")
	writePlanFile(t, root, "module/go.mod", "module example\n")
	writePlanFile(t, root, "outside.go", "package outside\n")
	if err := os.Symlink(filepath.Join(root, "outside.go"), filepath.Join(root, "module", "linked.go")); err != nil {
		t.Fatal(err)
	}
	scope, err := checks.NewChangedScope(root, []string{"plain.go", "notes.md", "module/linked.go", "missing.go"}, []string{""})
	if err != nil {
		t.Fatal(err)
	}
	genericPlan, err := New().Plan(scope)
	if err != nil {
		t.Fatal(err)
	}
	plan := genericPlan.(*checkerPlan)
	if plan.Runnable() || plan.SkipReason() == "" || len(plan.invocations) != 0 {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestPlanCodebaseFindsEveryModuleWithoutRootFallback(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "one/go.mod", "module one\n")
	writePlanFile(t, root, "one/main.go", "package one\n")
	writePlanFile(t, root, "two/go.mod", "module two\n")
	writePlanFile(t, root, "two/nested/go.mod", "module nested\n")
	writePlanFile(t, root, "vendor/ignored/go.mod", "module ignored\n")
	writePlanFile(t, root, ".git/collision/go.mod", "module ignored\n")
	writePlanFile(t, root, "linked/go.mod", "module linked\n")
	if err := os.Symlink(filepath.Join(root, "linked"), filepath.Join(root, "symlinked")); err != nil {
		t.Fatal(err)
	}
	scope, err := checks.NewCodebaseScope(root, []string{""})
	if err != nil {
		t.Fatal(err)
	}
	genericPlan, err := New().Plan(scope)
	if err != nil {
		t.Fatal(err)
	}
	plan := genericPlan.(*checkerPlan)
	want := []invocation{
		{moduleRoot: filepath.Join(root, "linked"), targets: []string{"./..."}},
		{moduleRoot: filepath.Join(root, "one"), targets: []string{"./..."}},
		{moduleRoot: filepath.Join(root, "two"), targets: []string{"./..."}},
		{moduleRoot: filepath.Join(root, "two", "nested"), targets: []string{"./..."}},
	}
	if !reflect.DeepEqual(plan.invocations, want) {
		t.Fatalf("invocations:\n got %#v\nwant %#v", plan.invocations, want)
	}
}

func TestPlanChangedDoesNotCrossRepositoryComponentBoundary(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "go.mod", "module root\n")
	writePlanFile(t, root, "sub/component.go", "package component\n")
	scope, err := checks.NewChangedScope(
		root,
		[]string{"sub/component.go"},
		[]string{"", "sub"},
	)
	if err != nil {
		t.Fatal(err)
	}
	genericPlan, err := New().Plan(scope)
	if err != nil {
		t.Fatal(err)
	}
	plan := genericPlan.(*checkerPlan)
	if plan.Runnable() {
		t.Fatalf("submodule file borrowed superproject go.mod: %#v", plan.invocations)
	}

	writePlanFile(t, root, "sub/go.mod", "module component\n")
	genericPlan, err = New().Plan(scope)
	if err != nil {
		t.Fatal(err)
	}
	plan = genericPlan.(*checkerPlan)
	if !plan.Runnable() || len(plan.invocations) != 1 || plan.invocations[0].moduleRoot != filepath.Join(root, "sub") {
		t.Fatalf("submodule-owned module not planned: %#v", plan.invocations)
	}
}

func TestValidateInvocationRejectsMalformedMixedAndEscapingTargets(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "module")
	writePlanFile(t, root, "module/go.mod", "module example\n")
	writePlanFile(t, root, "module/main.go", "package example\n")

	for _, target := range []invocation{
		{moduleRoot: module},
		{moduleRoot: module, targets: []string{"./...", "main.go"}},
		{moduleRoot: module, targets: []string{"-config.go"}},
		{moduleRoot: module, targets: []string{"../outside.go"}},
		{moduleRoot: root, targets: []string{"./..."}},
	} {
		if err := validateInvocation(root, target); err == nil {
			t.Fatalf("invocation accepted: %#v", target)
		}
	}
	for _, target := range []invocation{
		{moduleRoot: module, targets: []string{"main.go"}},
		{moduleRoot: module, targets: []string{"./..."}},
	} {
		if err := validateInvocation(root, target); err != nil {
			t.Fatalf("valid invocation rejected: %v", err)
		}
	}
}

func writePlanFile(t *testing.T, root, path, content string) {
	t.Helper()
	absolutePath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(absolutePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(absolutePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
