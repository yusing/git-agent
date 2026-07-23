package golangci

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yusing/git-agent/internal/checks"
)

const Name = "golangci-lint"

type Checker struct{}

func New() *Checker {
	return &Checker{}
}

func (*Checker) Name() string {
	return Name
}

type invocation struct {
	moduleRoot string
	targets    []string
}

type checkerPlan struct {
	scope       checks.Scope
	invocations []invocation
	reason      string
}

func (p *checkerPlan) CheckerName() string {
	return Name
}

func (p *checkerPlan) Runnable() bool {
	return len(p.invocations) > 0
}

func (p *checkerPlan) SkipReason() string {
	return p.reason
}

func (*Checker) Plan(scope checks.Scope) (checks.Plan, error) {
	var invocations []invocation
	var err error
	switch scope.Kind() {
	case checks.ScopeChanged:
		invocations, err = planChanged(scope)
	case checks.ScopeCodebase:
		invocations, err = planCodebase(scope)
	default:
		return nil, fmt.Errorf("golangci-lint received unknown scope kind %q", scope.Kind())
	}
	if err != nil {
		return nil, err
	}
	plan := &checkerPlan{scope: scope, invocations: invocations}
	if len(invocations) == 0 {
		if scope.Kind() == checks.ScopeCodebase {
			plan.reason = "authoritative review scope contains no Go modules"
		} else {
			plan.reason = "authoritative review scope contains no changed Go files in a Go module"
		}
	}
	return plan, nil
}

func planChanged(scope checks.Scope) ([]invocation, error) {
	type groupKey struct {
		moduleRoot string
		packageDir string
	}
	groups := make(map[groupKey][]string)
	for _, repositoryPath := range scope.Paths() {
		absolutePath, eligible, err := changedGoFile(scope.Root(), repositoryPath)
		if err != nil {
			return nil, err
		}
		if !eligible {
			continue
		}
		componentRoot, found := scope.ComponentRoot(repositoryPath)
		if !found {
			return nil, fmt.Errorf("changed Go file %q has no repository component", repositoryPath)
		}
		moduleRoot, found, err := nearestModuleRoot(componentRoot, filepath.Dir(absolutePath))
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		modulePath, err := filepath.Rel(moduleRoot, absolutePath)
		if err != nil {
			return nil, fmt.Errorf("locate changed Go file %q in module: %w", repositoryPath, err)
		}
		modulePath = filepath.ToSlash(modulePath)
		if !validLocalGoTarget(modulePath) {
			return nil, fmt.Errorf("changed Go file %q produced unsafe module target %q", repositoryPath, modulePath)
		}
		key := groupKey{moduleRoot: moduleRoot, packageDir: filepath.Dir(absolutePath)}
		groups[key] = append(groups[key], modulePath)
	}

	keys := make([]groupKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(left, right groupKey) int {
		if value := strings.Compare(left.moduleRoot, right.moduleRoot); value != 0 {
			return value
		}
		return strings.Compare(left.packageDir, right.packageDir)
	})
	result := make([]invocation, 0, len(keys))
	for _, key := range keys {
		result = append(result, invocation{
			moduleRoot: key.moduleRoot,
			targets:    uniqueSorted(groups[key]),
		})
	}
	return result, nil
}

func planCodebase(scope checks.Scope) ([]invocation, error) {
	var moduleRoots []string
	err := filepath.WalkDir(scope.Root(), func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk checker workspace %q: %w", path, walkErr)
		}
		if entry.IsDir() {
			if path != scope.Root() && (entry.Name() == ".git" || entry.Name() == "vendor") {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Name() != "go.mod" || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect module file %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		moduleRoot := filepath.Dir(path)
		if err := ensureContained(scope.Root(), moduleRoot); err != nil {
			return err
		}
		moduleRoots = append(moduleRoots, moduleRoot)
		return nil
	})
	if err != nil {
		return nil, err
	}
	moduleRoots = uniqueSorted(moduleRoots)
	result := make([]invocation, 0, len(moduleRoots))
	for _, moduleRoot := range moduleRoots {
		result = append(result, invocation{moduleRoot: moduleRoot, targets: []string{"./..."}})
	}
	return result, nil
}

func changedGoFile(root, repositoryPath string) (string, bool, error) {
	if filepath.Ext(repositoryPath) != ".go" {
		return "", false, nil
	}
	absolutePath := filepath.Join(root, filepath.FromSlash(repositoryPath))
	if err := ensureContained(root, absolutePath); err != nil {
		return "", false, fmt.Errorf("changed path %q: %w", repositoryPath, err)
	}
	info, err := os.Lstat(absolutePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect changed Go file %q: %w", repositoryPath, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", false, nil
	}
	resolved, err := filepath.EvalSymlinks(absolutePath)
	if err != nil {
		return "", false, fmt.Errorf("resolve changed Go file %q: %w", repositoryPath, err)
	}
	if resolved != absolutePath {
		return "", false, nil
	}
	return absolutePath, true, nil
}

func nearestModuleRoot(root, directory string) (string, bool, error) {
	for {
		moduleFile := filepath.Join(directory, "go.mod")
		info, err := os.Lstat(moduleFile)
		switch {
		case err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0:
			resolved, err := filepath.EvalSymlinks(moduleFile)
			if err != nil {
				return "", false, fmt.Errorf("resolve module file %q: %w", moduleFile, err)
			}
			if resolved == moduleFile {
				return directory, true, nil
			}
		case err != nil && !os.IsNotExist(err):
			return "", false, fmt.Errorf("inspect module file %q: %w", moduleFile, err)
		}
		if directory == root {
			return "", false, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", false, fmt.Errorf("changed Go file escaped checker workspace")
		}
		directory = parent
	}
}

func validateInvocation(workspaceRoot string, target invocation) error {
	if err := ensureContained(workspaceRoot, target.moduleRoot); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(target.moduleRoot)
	if err != nil || resolved != target.moduleRoot {
		return fmt.Errorf("checker module root %q is unsafe", target.moduleRoot)
	}
	moduleFile := filepath.Join(target.moduleRoot, "go.mod")
	info, err := os.Lstat(moduleFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("checker module %q has no regular go.mod", target.moduleRoot)
	}
	if len(target.targets) == 0 {
		return fmt.Errorf("checker target list is empty")
	}
	if len(target.targets) == 1 && target.targets[0] == "./..." {
		return nil
	}
	for _, path := range target.targets {
		if !validLocalGoTarget(path) {
			return fmt.Errorf("unsafe checker target %q", path)
		}
		absolutePath := filepath.Join(target.moduleRoot, filepath.FromSlash(path))
		if err := ensureContained(target.moduleRoot, absolutePath); err != nil {
			return err
		}
		info, err := os.Lstat(absolutePath)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("checker target %q is not a regular Go file", path)
		}
	}
	return nil
}

func ensureContained(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("locate %q beneath %q: %w", path, root, err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("path %q escapes checker workspace", path)
	}
	return nil
}

func validLocalGoTarget(path string) bool {
	cleaned := filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	return path == cleaned && cleaned != "" && cleaned != "." && cleaned != ".." &&
		!filepath.IsAbs(cleaned) && !strings.HasPrefix(cleaned, "../") &&
		!strings.HasPrefix(cleaned, "-") && !strings.ContainsRune(cleaned, '\x00') &&
		filepath.Ext(cleaned) == ".go"
}

func uniqueSorted(values []string) []string {
	values = slices.Clone(values)
	slices.Sort(values)
	return slices.Compact(values)
}
