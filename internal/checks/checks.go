package checks

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	MaxDiagnostics       = 100
	MaxDiagnosticMessage = 512
	MaxDiagnosticCode    = 128
	MaxResultReason      = 512
	MaxResultError       = 1024
)

var checkerNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type ScopeKind string

const (
	ScopeChanged  ScopeKind = "changed"
	ScopeCodebase ScopeKind = "codebase"
)

type Scope struct {
	kind       ScopeKind
	root       string
	paths      []string
	components []string
}

func NewChangedScope(root string, paths, components []string) (Scope, error) {
	if len(paths) == 0 {
		return Scope{}, fmt.Errorf("changed checker scope requires paths")
	}
	scope, err := newScope(ScopeChanged, root, components)
	if err != nil {
		return Scope{}, err
	}
	scope.paths = make([]string, 0, len(paths))
	for _, path := range paths {
		normalized, err := normalizeRepositoryPath(path)
		if err != nil {
			return Scope{}, fmt.Errorf("changed checker scope: %w", err)
		}
		scope.paths = append(scope.paths, normalized)
	}
	slices.Sort(scope.paths)
	scope.paths = slices.Compact(scope.paths)
	return scope, nil
}

func NewCodebaseScope(root string, components []string) (Scope, error) {
	return newScope(ScopeCodebase, root, components)
}

func newScope(kind ScopeKind, root string, components []string) (Scope, error) {
	if !filepath.IsAbs(root) {
		return Scope{}, fmt.Errorf("checker scope root must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return Scope{}, fmt.Errorf("resolve checker scope root: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return Scope{}, fmt.Errorf("inspect checker scope root: %w", err)
	}
	if !info.IsDir() {
		return Scope{}, fmt.Errorf("checker scope root must be a directory")
	}
	switch kind {
	case ScopeChanged, ScopeCodebase:
	default:
		return Scope{}, fmt.Errorf("unknown checker scope kind %q", kind)
	}
	if len(components) == 0 {
		return Scope{}, fmt.Errorf("checker scope requires repository components")
	}
	normalizedComponents := make([]string, 0, len(components))
	for _, component := range components {
		if component == "" {
			normalizedComponents = append(normalizedComponents, "")
			continue
		}
		normalized, err := normalizeRepositoryPath(component)
		if err != nil {
			return Scope{}, fmt.Errorf("checker scope component: %w", err)
		}
		normalizedComponents = append(normalizedComponents, normalized)
	}
	slices.Sort(normalizedComponents)
	normalizedComponents = slices.Compact(normalizedComponents)
	if normalizedComponents[0] != "" {
		return Scope{}, fmt.Errorf("checker scope components must include the root")
	}
	return Scope{kind: kind, root: resolved, components: normalizedComponents}, nil
}

func (s Scope) Kind() ScopeKind {
	return s.kind
}

func (s Scope) Root() string {
	return s.root
}

func (s Scope) Paths() []string {
	return slices.Clone(s.paths)
}

func (s Scope) Components() []string {
	return slices.Clone(s.components)
}

func (s Scope) ComponentRoot(path string) (string, bool) {
	normalized, err := normalizeRepositoryPath(path)
	if err != nil {
		return "", false
	}
	selected := ""
	for _, component := range s.components {
		if component == "" {
			continue
		}
		if normalized == component || strings.HasPrefix(normalized, component+"/") {
			if len(component) > len(selected) {
				selected = component
			}
		}
	}
	return filepath.Join(s.root, filepath.FromSlash(selected)), true
}

func (s Scope) Contains(path string) bool {
	normalized, err := normalizeRepositoryPath(path)
	if err != nil {
		return false
	}
	if s.kind == ScopeCodebase {
		return true
	}
	_, found := slices.BinarySearch(s.paths, normalized)
	return found
}

func (s Scope) validate() error {
	if s.root == "" || !filepath.IsAbs(s.root) {
		return fmt.Errorf("checker scope is uninitialized")
	}
	if len(s.components) == 0 || s.components[0] != "" ||
		!slices.IsSorted(s.components) {
		return fmt.Errorf("checker scope repository components are invalid")
	}
	switch s.kind {
	case ScopeChanged:
		if len(s.paths) == 0 {
			return fmt.Errorf("changed checker scope requires paths")
		}
	case ScopeCodebase:
		if len(s.paths) != 0 {
			return fmt.Errorf("codebase checker scope must not contain paths")
		}
	default:
		return fmt.Errorf("unknown checker scope kind %q", s.kind)
	}
	return nil
}

type Status string

const (
	StatusPass     Status = "pass"
	StatusFindings Status = "findings"
	StatusSkipped  Status = "skipped"
	StatusError    Status = "error"
)

type Diagnostic struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Column  int    `json:"column,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type Result struct {
	Name        string       `json:"name"`
	Status      Status       `json:"status"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	Omitted     int          `json:"omitted,omitempty"`
	Reason      string       `json:"reason,omitempty"`
	Error       string       `json:"error,omitempty"`
}

func NewResult(name string, diagnostics []Diagnostic) (Result, error) {
	if err := validateCheckerName(name); err != nil {
		return Result{}, err
	}
	normalized := make([]Diagnostic, 0, len(diagnostics))
	for index, diagnostic := range diagnostics {
		value, err := normalizeDiagnostic(diagnostic)
		if err != nil {
			return Result{}, fmt.Errorf("diagnostic %d: %w", index, err)
		}
		normalized = append(normalized, value)
	}
	slices.SortFunc(normalized, compareDiagnostics)
	normalized = slices.Compact(normalized)
	omitted := max(len(normalized)-MaxDiagnostics, 0)
	normalized = normalized[:min(len(normalized), MaxDiagnostics)]
	status := StatusPass
	if len(normalized) > 0 || omitted > 0 {
		status = StatusFindings
	}
	return Result{
		Name:        name,
		Status:      status,
		Diagnostics: normalized,
		Omitted:     omitted,
	}, nil
}

func NewSkipped(name, reason string) (Result, error) {
	result := Result{
		Name:        name,
		Status:      StatusSkipped,
		Diagnostics: []Diagnostic{},
		Reason:      singleLine(reason, MaxResultReason),
	}
	if err := ValidateResult(result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func NewError(name string, err error) (Result, error) {
	message := "static check failed"
	if err != nil {
		message = singleLine(err.Error(), MaxResultError)
	}
	if message == "" {
		message = "static check failed"
	}
	result := Result{
		Name:        name,
		Status:      StatusError,
		Diagnostics: []Diagnostic{},
		Error:       message,
	}
	if validationErr := ValidateResult(result); validationErr != nil {
		return Result{}, validationErr
	}
	return result, nil
}

func ValidateResult(result Result) error {
	if err := validateCheckerName(result.Name); err != nil {
		return err
	}
	if result.Diagnostics == nil {
		return fmt.Errorf("check %q diagnostics must be an array", result.Name)
	}
	if len(result.Diagnostics) > MaxDiagnostics {
		return fmt.Errorf("check %q diagnostics exceeds %d items", result.Name, MaxDiagnostics)
	}
	for index, diagnostic := range result.Diagnostics {
		normalized, err := normalizeDiagnostic(diagnostic)
		if err != nil {
			return fmt.Errorf("check %q diagnostics[%d]: %w", result.Name, index, err)
		}
		if normalized != diagnostic {
			return fmt.Errorf("check %q diagnostics[%d] is not normalized", result.Name, index)
		}
		if index > 0 && compareDiagnostics(result.Diagnostics[index-1], diagnostic) >= 0 {
			return fmt.Errorf("check %q diagnostics must be strictly ordered and unique", result.Name)
		}
	}

	switch result.Status {
	case StatusPass:
		if len(result.Diagnostics) != 0 || result.Omitted != 0 || result.Reason != "" || result.Error != "" {
			return fmt.Errorf("check %q pass result contains inapplicable fields", result.Name)
		}
	case StatusFindings:
		if len(result.Diagnostics) == 0 && result.Omitted == 0 {
			return fmt.Errorf("check %q findings result requires diagnostics or omitted count", result.Name)
		}
		if result.Omitted < 0 || result.Reason != "" || result.Error != "" {
			return fmt.Errorf("check %q findings result contains invalid fields", result.Name)
		}
	case StatusSkipped:
		if len(result.Diagnostics) != 0 || result.Omitted != 0 || result.Error != "" ||
			result.Reason == "" || result.Reason != singleLine(result.Reason, MaxResultReason) {
			return fmt.Errorf("check %q skipped result requires only a normalized reason", result.Name)
		}
	case StatusError:
		if len(result.Diagnostics) != 0 || result.Omitted != 0 || result.Reason != "" ||
			result.Error == "" || result.Error != singleLine(result.Error, MaxResultError) {
			return fmt.Errorf("check %q error result requires only a normalized error", result.Name)
		}
	default:
		return fmt.Errorf("check %q has unsupported status %q", result.Name, result.Status)
	}
	return nil
}

func validateResultForScope(result Result, scope Scope) error {
	if err := ValidateResult(result); err != nil {
		return err
	}
	for _, diagnostic := range result.Diagnostics {
		if !scope.Contains(diagnostic.Path) {
			return fmt.Errorf("check %q returned diagnostic path %q outside authoritative scope", result.Name, diagnostic.Path)
		}
	}
	return nil
}

func validateCheckerName(name string) error {
	if !checkerNamePattern.MatchString(name) {
		return fmt.Errorf("invalid checker name %q", name)
	}
	return nil
}

func normalizeDiagnostic(diagnostic Diagnostic) (Diagnostic, error) {
	path, err := normalizeRepositoryPath(diagnostic.Path)
	if err != nil {
		return Diagnostic{}, err
	}
	if diagnostic.Line < 1 {
		return Diagnostic{}, fmt.Errorf("line must be positive")
	}
	if diagnostic.Column < 0 {
		return Diagnostic{}, fmt.Errorf("column must not be negative")
	}
	code := singleLine(diagnostic.Code, MaxDiagnosticCode)
	if diagnostic.Code != "" && code == "" {
		return Diagnostic{}, fmt.Errorf("code must be nonempty when present")
	}
	message := singleLine(diagnostic.Message, MaxDiagnosticMessage)
	if message == "" {
		return Diagnostic{}, fmt.Errorf("message is required")
	}
	return Diagnostic{
		Path:    path,
		Line:    diagnostic.Line,
		Column:  diagnostic.Column,
		Code:    code,
		Message: message,
	}, nil
}

func normalizeRepositoryPath(path string) (string, error) {
	path = filepath.ToSlash(filepath.Clean(strings.TrimSpace(path)))
	if path == "" || path == "." || path == ".." || filepath.IsAbs(path) ||
		strings.HasPrefix(path, "../") || strings.ContainsRune(path, '\x00') {
		return "", fmt.Errorf("path %q must be repository-relative", path)
	}
	return path, nil
}

func compareDiagnostics(left, right Diagnostic) int {
	if value := strings.Compare(left.Path, right.Path); value != 0 {
		return value
	}
	if value := left.Line - right.Line; value != 0 {
		return value
	}
	if value := left.Column - right.Column; value != 0 {
		return value
	}
	if value := strings.Compare(left.Code, right.Code); value != 0 {
		return value
	}
	return strings.Compare(left.Message, right.Message)
}

func singleLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}
