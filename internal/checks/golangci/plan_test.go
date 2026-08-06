package golangci

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yusing/git-agent/internal/checks"
	ignorectx "github.com/yusing/git-agent/internal/ignore"
)

func TestPlanChangedTargetsEachAffectedPackageOnce(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "go.mod", "module root\n")
	writePlanFile(t, root, "root.go", "package root\n")
	writePlanFile(t, root, "a/a.go", "package a\n")
	writePlanFile(t, root, "a/other.go", "package a\n")
	writePlanFile(t, root, "a/helpers_test.go", "package a\n")
	writePlanFile(t, root, "b/b.go", "package b\n")
	writePlanFile(t, root, "b-copy/b.go", "package bcopy\n")
	writePlanFile(t, root, "nested/go.mod", "module nested\n")
	writePlanFile(t, root, "nested/c/c.go", "package c\n")
	writePlanFile(t, root, "README.md", "not Go\n")
	scope, err := checks.NewChangedScope(root, []string{
		"nested/c/c.go", "root.go", "a/a.go", "a/other.go", "a/helpers_test.go",
		"b/b.go", "b-copy/b.go", "README.md", "deleted.go", "a/a.go",
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
		{moduleRoot: root, targets: []string{"."}},
		{moduleRoot: root, targets: []string{"./a"}},
		{moduleRoot: root, targets: []string{"./b"}},
		{moduleRoot: root, targets: []string{"./b-copy"}},
		{moduleRoot: filepath.Join(root, "nested"), targets: []string{"./c"}},
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
	if plan.Runnable() || len(plan.invocations) != 0 {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestPlanChangedSkipsDeletedRenameSourceAndTargetsDestinationPackage(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "go.mod", "module example\n")
	writePlanFile(t, root, "new/destination.go", "package new\n")
	scope, err := checks.NewChangedScope(
		root,
		[]string{"old/source.go", "new/destination.go"},
		[]string{""},
	)
	if err != nil {
		t.Fatal(err)
	}
	genericPlan, err := New().Plan(scope)
	if err != nil {
		t.Fatal(err)
	}
	plan := genericPlan.(*checkerPlan)
	want := []invocation{{moduleRoot: root, targets: []string{"./new"}}}
	if !reflect.DeepEqual(plan.invocations, want) {
		t.Fatalf("invocations:\n got %#v\nwant %#v", plan.invocations, want)
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
	scope, err := checks.NewCodebaseScope(root, []string{""}, map[string]ignorectx.Matcher{"": ignorectx.New()}, nil)
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

func TestPlanCodebasePrunesIgnoredUntrackedModuleTree(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "go.mod", "module root\n")
	writePlanFile(t, root, ".local/share/containers/storage/overlay/cache/go.mod", "module ignored\n")
	writePlanFile(t, root, ".local/share/tracked/go.mod", "module tracked\n")
	matcher := ignorectx.New().Append("*\n!go.mod\n!.local/\n!.local/share/\n!.local/share/keep.txt\n", nil)
	scope, err := checks.NewCodebaseScope(root, []string{""}, map[string]ignorectx.Matcher{"": matcher}, []string{".local/share/tracked/go.mod"})
	if err != nil {
		t.Fatal(err)
	}

	genericPlan, err := New().Plan(scope)
	if err != nil {
		t.Fatalf("ignored container subtree blocked planning: %v", err)
	}
	plan := genericPlan.(*checkerPlan)
	want := []invocation{
		{moduleRoot: root, targets: []string{"./..."}},
		{moduleRoot: filepath.Join(root, ".local", "share", "tracked"), targets: []string{"./..."}},
	}
	if !reflect.DeepEqual(plan.invocations, want) {
		t.Fatalf("invocations:\n got %#v\nwant %#v", plan.invocations, want)
	}
}

func TestPlanCodebaseAppliesIgnoreRulesWithinRepositoryComponents(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "go.mod", "module root\n")
	writePlanFile(t, root, "sub/go.mod", "module sub\n")
	writePlanFile(t, root, "sub/generated/go.mod", "module generated\n")
	writePlanFile(t, root, "sub/cache/go.mod", "module cache\n")

	matchers := map[string]ignorectx.Matcher{
		"":    ignorectx.New().Append("generated/\n", nil),
		"sub": ignorectx.New().Append("cache/\n", nil),
	}
	scope, err := checks.NewCodebaseScope(root, []string{"", "sub"}, matchers, nil)
	if err != nil {
		t.Fatal(err)
	}

	genericPlan, err := New().Plan(scope)
	if err != nil {
		t.Fatal(err)
	}
	plan := genericPlan.(*checkerPlan)
	want := []invocation{
		{moduleRoot: root, targets: []string{"./..."}},
		{moduleRoot: filepath.Join(root, "sub"), targets: []string{"./..."}},
		{moduleRoot: filepath.Join(root, "sub", "generated"), targets: []string{"./..."}},
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
	writePlanFile(t, root, "module/pkg/pkg.go", "package pkg\n")
	writePlanFile(t, root, "module/not-go/README.md", "not Go\n")
	writePlanFile(t, root, "outside/outside.go", "package outside\n")
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(module, "linked")); err != nil {
		t.Fatal(err)
	}

	for _, target := range []invocation{
		{moduleRoot: module},
		{moduleRoot: module, targets: []string{"./...", "./pkg"}},
		{moduleRoot: module, targets: []string{"main.go"}},
		{moduleRoot: module, targets: []string{"-config.go"}},
		{moduleRoot: module, targets: []string{"../outside"}},
		{moduleRoot: module, targets: []string{"./../outside"}},
		{moduleRoot: module, targets: []string{"./missing"}},
		{moduleRoot: module, targets: []string{"./not-go"}},
		{moduleRoot: module, targets: []string{"./linked"}},
		{moduleRoot: module, targets: []string{filepath.Join(module, "pkg")}},
		{moduleRoot: root, targets: []string{"./..."}},
	} {
		if err := validateInvocation(root, target); err == nil {
			t.Fatalf("invocation accepted: %#v", target)
		}
	}
	for _, target := range []invocation{
		{moduleRoot: module, targets: []string{"."}},
		{moduleRoot: module, targets: []string{"./pkg"}},
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
