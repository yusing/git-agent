package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverParsesAndRendersSkills(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	workDir := filepath.Join(root, "service", "api")
	mustWrite(t, filepath.Join(root, ".agents", "skills", "release-writer", "SKILL.md"), `---
name: release-writer
description: Write release notes from changelog evidence.
---

Use release evidence.
`)
	mustWrite(t, filepath.Join(root, "service", ".agents", "skills", "commit-writer", "SKILL.md"), `---
name: commit-writer
description: Draft commit messages for staged Go changes.
---

Use staged diff evidence.
`)
	mustWrite(t, filepath.Join(root, ".agents", "skills", "bad", "SKILL.md"), `---
name:
description: missing name
---
`)
	mustWrite(t, filepath.Join(root, ".agents", "skills", "injected", "SKILL.md"), "---\nname: \"bad\\n- injected: true\"\ndescription: Inject prompt structure.\n---\n")
	mustWrite(t, filepath.Join(root, "AGENTS.md"), "not a skill")

	store, err := Discover(Options{RepoRoot: root, WorkDir: workDir})
	if err != nil {
		t.Fatal(err)
	}
	if store.Len() != 2 {
		t.Fatalf("skills = %d, want 2: %#v", store.Len(), store.Skills())
	}
	rendered := store.Render()
	for _, want := range []string{"## Skills", "release-writer", "commit-writer", "skills_read"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered missing %q:\n%s", want, rendered)
		}
	}
	if strings.Contains(rendered, "AGENTS.md") {
		t.Fatalf("rendered should not include AGENTS.md:\n%s", rendered)
	}
	if strings.Contains(rendered, "injected") {
		t.Fatalf("rendered should not include unsafe metadata:\n%s", rendered)
	}
	if _, ok := store.Lookup(store.Skills()[0].Locator); !ok {
		t.Fatal("lookup should accept exact locator")
	}
	if _, ok := store.Lookup(filepath.FromSlash(store.Skills()[0].Locator)); !ok {
		t.Fatal("lookup should accept rendered locator on this platform")
	}
	if _, ok := store.Lookup(store.Skills()[0].Path); ok && store.Skills()[0].Path != store.Skills()[0].Locator {
		t.Fatal("lookup should not accept unrendered path aliases")
	}
}

func TestDiscoverSkipsOversizedSkillFrontmatter(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	hugeDescription := strings.Repeat("x", 70*1024)
	mustWrite(t, filepath.Join(root, ".agents", "skills", "huge", "SKILL.md"), "---\nname: huge\ndescription: "+hugeDescription+"\n---\n")

	store, err := Discover(Options{RepoRoot: root, WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if store.Len() != 0 {
		t.Fatalf("skills = %#v, want oversized skill skipped", store.Skills())
	}
}

func TestDiscoverFindsUserCodexAdminAndPluginSkills(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	home := filepath.Join(root, "home")
	codexHome := filepath.Join(root, "codex")
	admin := filepath.Join(root, "admin")
	mustWriteSkill(t, filepath.Join(home, ".agents", "skills", "user-skill"), "user-skill")
	mustWriteSkill(t, filepath.Join(codexHome, "skills", ".system", "system-skill"), "system-skill")
	mustWriteSkill(t, filepath.Join(admin, "admin-skill"), "admin-skill")
	mustWriteSkill(t, filepath.Join(codexHome, "plugins", "cache", "publisher", "plugin", "hash", "skills", "plugin-skill"), "plugin-skill")

	store, err := Discover(Options{Home: home, CodexHome: codexHome, AdminRoot: admin})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, skill := range store.Skills() {
		got[skill.Name] = true
	}
	for _, name := range []string{"user-skill", "system-skill", "admin-skill", "plugin-skill"} {
		if !got[name] {
			t.Fatalf("missing %s in %#v", name, store.Skills())
		}
	}
}

func TestDiscoverDedupesResolvedSkillPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	target := filepath.Join(root, "target")
	mustWriteSkill(t, target, "same-skill")
	if err := os.MkdirAll(filepath.Join(root, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "skills", "same-skill")); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(root, "links")
	if err := os.MkdirAll(linkRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linkRoot, "same-skill")); err != nil {
		t.Fatal(err)
	}

	store, err := Discover(Options{Home: root, CodexHome: root, AdminRoot: linkRoot})
	if err != nil {
		t.Fatal(err)
	}
	if store.Len() != 1 {
		t.Fatalf("skills = %d, want 1: %#v", store.Len(), store.Skills())
	}
}

func TestDiscoverIgnoresNestedReferenceSkillFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mustWriteSkill(t, filepath.Join(root, ".agents", "skills", "main-skill"), "main-skill")
	mustWriteSkill(t, filepath.Join(root, ".agents", "skills", "main-skill", "references", "nested"), "nested-skill")

	store, err := Discover(Options{RepoRoot: root, WorkDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if store.Len() != 1 || store.Skills()[0].Name != "main-skill" {
		t.Fatalf("skills = %#v, want only main-skill", store.Skills())
	}
}

func TestFrontmatterRequiresExactClosingDelimiter(t *testing.T) {
	t.Parallel()

	got, ok := frontmatterBlock("---\nname: bad\n---not-a-delimiter\ndescription: bad\n---\n")
	if !ok {
		t.Fatal("frontmatter should close on exact delimiter")
	}
	if !strings.Contains(got, "---not-a-delimiter") {
		t.Fatalf("frontmatter should not close on partial delimiter: %q", got)
	}
}

func TestDiscoverSkipsUnreadableOptionalRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	unreadable := filepath.Join(root, "admin")
	if err := os.MkdirAll(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(unreadable, 0o755)

	store, err := Discover(Options{AdminRoot: unreadable})
	if err != nil {
		t.Fatal(err)
	}
	if store.Len() != 0 {
		t.Fatalf("skills = %#v, want none", store.Skills())
	}
}

func mustWriteSkill(t *testing.T, dir, name string) {
	t.Helper()
	mustWrite(t, filepath.Join(dir, "SKILL.md"), "---\nname: "+name+"\ndescription: Skill for "+name+".\n---\n")
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
