package golangci

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/checks"
)

func TestRunFiltersToAuthoritativeChangedScope(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "go.mod", "module example\n")
	writePlanFile(t, root, "selected.go", "package example\n")
	writePlanFile(t, root, "unrelated.go", "package example\n")
	scope, err := checks.NewChangedScope(root, []string{"selected.go"}, []string{""})
	if err != nil {
		t.Fatal(err)
	}
	genericPlan, err := New().Plan(scope)
	if err != nil {
		t.Fatal(err)
	}
	resultJSON := marshalHelperResult(t, []map[string]any{
		{
			"FromLinter": "selected-rule", "Text": " selected issue\n",
			"Pos": map[string]any{"Filename": "selected.go", "Line": 1, "Column": 2},
		},
		{
			"FromLinter": "collision", "Text": "must be filtered",
			"Pos": map[string]any{"Filename": "unrelated.go", "Line": 1, "Column": 1},
		},
	})
	executable := writeHelperScript(t, resultJSON, 1)

	result, err := New().Run(t.Context(), executable, genericPlan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != checks.StatusFindings || len(result.Diagnostics) != 1 {
		t.Fatalf("result = %#v", result)
	}
	diagnostic := result.Diagnostics[0]
	if diagnostic.Path != "selected.go" || diagnostic.Code != "selected-rule" ||
		diagnostic.Message != "selected issue" {
		t.Fatalf("diagnostic = %#v", diagnostic)
	}
}

func TestRunClassifiesPassAndExecutionError(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "go.mod", "module example\n")
	writePlanFile(t, root, "selected.go", "package example\n")
	scope, err := checks.NewChangedScope(root, []string{"selected.go"}, []string{""})
	if err != nil {
		t.Fatal(err)
	}
	genericPlan, err := New().Plan(scope)
	if err != nil {
		t.Fatal(err)
	}

	pass, err := New().Run(t.Context(), writeHelperScript(t, marshalHelperResult(t, []map[string]any{}), 0), genericPlan)
	if err != nil {
		t.Fatal(err)
	}
	if pass.Status != checks.StatusPass || len(pass.Diagnostics) != 0 {
		t.Fatalf("pass = %#v", pass)
	}

	checkErr, err := New().Run(t.Context(), writeHelperScript(t, "", 2), genericPlan)
	if err != nil {
		t.Fatal(err)
	}
	if checkErr.Status != checks.StatusError || checkErr.Error == "" || strings.Contains(checkErr.Error, "\n") {
		t.Fatalf("error result = %#v", checkErr)
	}
}

func TestBundledCheckerAnalyzesCompleteAffectedPackageContext(t *testing.T) {
	executable := buildGitAgent(t)

	for _, test := range []struct {
		name        string
		files       map[string]string
		changedPath string
		wantFinding bool
	}{
		{
			name: "production sibling",
			files: map[string]string{
				"subject.go":       "package example\n\nfunc Subject() string { return sibling() }\n",
				"sibling.go":       "package example\n\nfunc sibling() string { return \"ok\" }\n",
				"unrelated/bad.go": "package unrelated\n\nfunc Broken( {\n",
			},
			changedPath: "subject.go",
		},
		{
			name: "test sibling",
			files: map[string]string{
				"subject.go":      "package example\n\nfunc Subject() string { return \"ok\" }\n",
				"subject_test.go": "package example\n\nimport \"testing\"\n\nfunc TestSubject(t *testing.T) { testHelper(t) }\n",
				"helper_test.go":  "package example\n\nimport \"testing\"\n\nfunc testHelper(t *testing.T) { t.Helper(); _ = Subject() }\n",
			},
			changedPath: "subject_test.go",
		},
		{
			name: "genuine typecheck finding",
			files: map[string]string{
				"subject.go": "package example\n\nfunc Subject() { unused := \"value\" }\n",
			},
			changedPath: "subject.go",
			wantFinding: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writePlanFile(t, root, "go.mod", "module example\n\ngo 1.26.0\n")
			for path, content := range test.files {
				writePlanFile(t, root, path, content)
			}
			scope, err := checks.NewChangedScope(root, []string{test.changedPath}, []string{""})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := New().Plan(scope)
			if err != nil {
				t.Fatal(err)
			}
			result, err := New().Run(t.Context(), executable, plan)
			if err != nil {
				t.Fatal(err)
			}
			if !test.wantFinding {
				if result.Status != checks.StatusPass || len(result.Diagnostics) != 0 {
					t.Fatalf("result = %#v", result)
				}
				return
			}
			if result.Status != checks.StatusFindings || len(result.Diagnostics) == 0 {
				t.Fatalf("result = %#v", result)
			}
			if diagnostic := result.Diagnostics[0]; diagnostic.Path != test.changedPath ||
				diagnostic.Code != "typecheck" || !strings.Contains(diagnostic.Message, "declared and not used: unused") {
				t.Fatalf("diagnostic = %#v", diagnostic)
			}
		})
	}
}

func TestRunHonorsCancellationBeforeStartingChecker(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "go.mod", "module example\n")
	writePlanFile(t, root, "selected.go", "package example\n")
	scope, err := checks.NewChangedScope(root, []string{"selected.go"}, []string{""})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := New().Plan(scope)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = New().Run(ctx, filepath.Join(root, "never-started"), plan)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestReadJSONResultAllowsUnknownFutureFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	data := `{"Issues":[{"FromLinter":"future-linter","Text":"future finding","Pos":{"Filename":"subject.go","Line":1,"Column":1,"FuturePosition":true},"FutureIssue":{"value":1}}],"Report":{"FutureReport":true},"FutureTopLevel":[1,2,3]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := readJSONResult(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Issues) != 1 || result.Issues[0].FromLinter != "future-linter" ||
		result.Issues[0].Text != "future finding" {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunHelperRejectsMalformedAndEscapingRequestsBeforeUpstream(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "go.mod", "module example\n")
	writePlanFile(t, root, "selected.go", "package example\n")
	privateRoot := t.TempDir()
	if err := os.Chmod(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	invocationRoot := filepath.Join(privateRoot, "invocation")
	if err := os.Mkdir(invocationRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(invocationRoot, "result.json")

	requests := [][]string{
		nil,
		{"--temp-root", privateRoot, "--workspace-root", root, "--module-root", root, "--result", "../escape.json", "--", "."},
		{"--temp-root", privateRoot, "--workspace-root", root, "--module-root", root, "--result", resultPath, "--", "-config.go"},
		{"--temp-root", privateRoot, "--workspace-root", root, "--module-root", root, "--result", resultPath, "--", "./...", "."},
	}
	for _, request := range requests {
		if err := New().RunHelper(request); err == nil {
			t.Fatalf("malformed helper request accepted: %#v", request)
		}
	}
}

func buildGitAgent(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "git-agent")
	command := exec.CommandContext(t.Context(), "go", "build", "-o", executable, "./cmd/git-agent")
	command.Dir = root
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build git-agent: %v\n%s", err, output)
	}
	return executable
}

func marshalHelperResult(t *testing.T, issues []map[string]any) string {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"Issues": issues,
		"Report": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeHelperScript(t *testing.T, result string, exitCode int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "checker-helper")
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
result_path=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --result)
      result_path=$2
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
`
	if result != "" {
		script += "printf '%s\\n' " + string(encoded) + " > \"$result_path\"\n"
	}
	script += "exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
