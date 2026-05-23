package guidance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Family string

const (
	FamilyAuto   Family = "auto"
	FamilyAgents Family = "agents"
	FamilyClaude Family = "claude"
	FamilyNone   Family = "none"
)

type Source struct {
	Path    string
	Content string
	RelPath string
}

type Resolved struct {
	TargetPath string
	Family     Family
	Sources    []Source
	Rendered   string
}

func ResolveForTargets(repoRoot string, targetPaths []string, requested Family) (Resolved, error) {
	if len(targetPaths) == 0 {
		return Resolve(repoRoot, repoRoot, requested)
	}
	if requested == FamilyAuto || requested == "" {
		absRoot, err := filepath.Abs(repoRoot)
		if err != nil {
			return Resolved{}, err
		}
		for _, targetPath := range targetPaths {
			absTarget, err := filepath.Abs(targetPath)
			if err != nil {
				return Resolved{}, err
			}
			if hasFamily(absRoot, absTarget, FamilyAgents) {
				requested = FamilyAgents
				break
			}
		}
	}

	var family Family
	sourcesByPath := map[string]Source{}
	var order []string
	var target string
	for _, targetPath := range targetPaths {
		resolved, err := Resolve(repoRoot, targetPath, requested)
		if err != nil {
			return Resolved{}, err
		}
		if target == "" {
			target = resolved.TargetPath
		}
		if resolved.Family == FamilyNone {
			continue
		}
		if family == "" {
			family = resolved.Family
		}
		if resolved.Family != family {
			continue
		}
		for _, source := range resolved.Sources {
			if _, ok := sourcesByPath[source.Path]; ok {
				continue
			}
			sourcesByPath[source.Path] = source
			order = append(order, source.Path)
		}
	}
	if family == "" {
		family = FamilyNone
	}
	result := Resolved{TargetPath: target, Family: family}
	for _, path := range order {
		result.Sources = append(result.Sources, sourcesByPath[path])
	}
	result.Rendered = Render(result)
	return result, nil
}

func Resolve(repoRoot, targetPath string, requested Family) (Resolved, error) {
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return Resolved{}, err
	}
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return Resolved{}, err
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return Resolved{}, err
	}
	if strings.HasPrefix(rel, "..") {
		return Resolved{}, fmt.Errorf("target path %q is outside repository root %q", absTarget, absRoot)
	}

	family := requested
	if family == "" {
		family = FamilyAuto
	}
	if family == FamilyNone {
		return Resolved{TargetPath: absTarget, Family: FamilyNone}, nil
	}
	if family == FamilyAuto {
		if hasFamily(absRoot, absTarget, FamilyAgents) {
			family = FamilyAgents
		} else if hasFamily(absRoot, absTarget, FamilyClaude) {
			family = FamilyClaude
		} else {
			family = FamilyNone
		}
	}
	if family != FamilyAgents && family != FamilyClaude && family != FamilyNone {
		return Resolved{}, fmt.Errorf("unknown guidance family %q", requested)
	}

	resolved := Resolved{TargetPath: absTarget, Family: family}
	if family == FamilyNone {
		return resolved, nil
	}

	for _, dir := range scopeDirs(absRoot, absTarget) {
		source, ok, err := readFirstFamilyFile(dir, family)
		if err != nil {
			return Resolved{}, err
		}
		if ok {
			source.RelPath = relPath(absRoot, source.Path)
			resolved.Sources = append(resolved.Sources, source)
		}
	}
	resolved.Rendered = Render(resolved)
	return resolved, nil
}

func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

func ParseFamily(value string) (Family, error) {
	switch Family(strings.ToLower(strings.TrimSpace(value))) {
	case "", FamilyAuto:
		return FamilyAuto, nil
	case FamilyAgents:
		return FamilyAgents, nil
	case FamilyClaude:
		return FamilyClaude, nil
	case FamilyNone:
		return FamilyNone, nil
	default:
		return "", fmt.Errorf("unknown guidance family %q", value)
	}
}

func Render(resolved Resolved) string {
	if len(resolved.Sources) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# AGENTS.md instructions for %s\n\n", resolved.TargetPath)
	b.WriteString("<INSTRUCTIONS>\n")
	for i, source := range resolved.Sources {
		if i > 0 {
			b.WriteString("\n")
		}
		path := source.RelPath
		if path == "" {
			path = source.Path
		}
		fmt.Fprintf(&b, "<PROJECT_DOC path=%q>\n", path)
		b.WriteString(strings.TrimRight(source.Content, "\n"))
		b.WriteString("\n</PROJECT_DOC>\n")
	}
	b.WriteString("</INSTRUCTIONS>")
	return b.String()
}

func hasFamily(root, target string, family Family) bool {
	for _, dir := range scopeDirs(root, target) {
		_, ok, err := readFirstFamilyFile(dir, family)
		if err == nil && ok {
			return true
		}
	}
	return false
}

func readFirstFamilyFile(dir string, family Family) (Source, bool, error) {
	for _, name := range fileNames(family) {
		path := filepath.Join(dir, name)
		content, err := os.ReadFile(path)
		if err == nil {
			return Source{Path: path, Content: string(content)}, true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return Source{}, false, err
		}
	}
	return Source{}, false, nil
}

func fileNames(family Family) []string {
	switch family {
	case FamilyAgents:
		return []string{"AGENTS.override.md", "AGENTS.md"}
	case FamilyClaude:
		return []string{"CLAUDE.md"}
	default:
		return nil
	}
}

func scopeDirs(root, target string) []string {
	info, err := os.Stat(target)
	if err == nil && !info.IsDir() {
		target = filepath.Dir(target)
	}

	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." {
		return []string{root}
	}

	dirs := []string{root}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		dirs = append(dirs, current)
	}
	return dirs
}
