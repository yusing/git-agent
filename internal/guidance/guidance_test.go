package guidance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePrefersAgentsFamilyAndScopesFromRootToTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "root agents")
	mustWrite(t, filepath.Join(root, "frontend", "AGENTS.md"), "frontend agents")
	mustWrite(t, filepath.Join(root, "frontend", "admin", "CLAUDE.md"), "admin claude")

	resolved, err := Resolve(root, filepath.Join(root, "frontend", "admin"), FamilyAuto)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Family != FamilyAgents {
		t.Fatalf("family = %s, want %s", resolved.Family, FamilyAgents)
	}
	if len(resolved.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(resolved.Sources))
	}
	if !strings.Contains(resolved.Rendered, `<PROJECT_DOC path="AGENTS.md">`) {
		t.Fatalf("rendered missing root provenance:\n%s", resolved.Rendered)
	}
	if strings.Contains(resolved.Rendered, "admin claude") {
		t.Fatalf("rendered merged cross-family guidance:\n%s", resolved.Rendered)
	}
}

func TestResolveForTargetsUsesAgentsFamilyAcrossStagedPaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), "root claude")
	mustWrite(t, filepath.Join(root, "frontend", "AGENTS.md"), "frontend agents")
	mustWrite(t, filepath.Join(root, "docs", "CLAUDE.md"), "docs claude")

	resolved, err := ResolveForTargets(root, []string{
		filepath.Join(root, "docs", "guide.md"),
		filepath.Join(root, "frontend", "app.go"),
	}, FamilyAuto)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Family != FamilyAgents {
		t.Fatalf("family = %s, want %s", resolved.Family, FamilyAgents)
	}
	if strings.Contains(resolved.Rendered, "claude") || !strings.Contains(resolved.Rendered, "frontend agents") {
		t.Fatalf("unexpected rendered guidance:\n%s", resolved.Rendered)
	}
}

func TestResolveFallsBackToClaudeOnlyWhenNoAgentsFound(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "CLAUDE.md"), "root claude")

	resolved, err := Resolve(root, root, FamilyAuto)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Family != FamilyClaude {
		t.Fatalf("family = %s, want %s", resolved.Family, FamilyClaude)
	}
	if !strings.Contains(resolved.Rendered, "root claude") {
		t.Fatalf("rendered missing claude guidance:\n%s", resolved.Rendered)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
