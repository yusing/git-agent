package golangci

import (
	"encoding/json"
	"os"
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
		{"--temp-root", privateRoot, "--workspace-root", root, "--module-root", root, "--result", "../escape.json", "--", "selected.go"},
		{"--temp-root", privateRoot, "--workspace-root", root, "--module-root", root, "--result", resultPath, "--", "-config.go"},
		{"--temp-root", privateRoot, "--workspace-root", root, "--module-root", root, "--result", resultPath, "--", "./...", "selected.go"},
	}
	for _, request := range requests {
		if err := New().RunHelper(request); err == nil {
			t.Fatalf("malformed helper request accepted: %#v", request)
		}
	}
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
