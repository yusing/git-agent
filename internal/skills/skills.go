package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"github.com/goccy/go-yaml"
	"github.com/yusing/git-agent/internal/textutil"
)

const (
	DefaultAdminRoot    = "/etc/codex/skills"
	pluginMaxDepth      = 10
	maxSkillNameLen     = 80
	maxSkillDescLen     = 300
	maxFrontmatterBytes = 64 * 1024
)

type Options struct {
	RepoRoot  string
	WorkDir   string
	Home      string
	CodexHome string
	AdminRoot string
}

type Skill struct {
	Name        string
	Description string
	Locator     string
	Path        string
	Root        string
	Scope       string
}

type Store struct {
	skills    []Skill
	byLocator map[string]Skill
}

type discovery struct {
	store         *Store
	seenPaths     map[string]struct{}
	seenNames     map[string]struct{}
	disabledPaths map[string]struct{}
}

type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

type codexConfig struct {
	Skills struct {
		Config []skillConfig `toml:"config"`
	} `toml:"skills"`
}

type skillConfig struct {
	Path    string `toml:"path"`
	Enabled *bool  `toml:"enabled"`
}

type sourceRoot struct {
	path          string
	scope         string
	includeSystem bool
}

func DefaultOptions(repoRoot, workDir string) Options {
	home, _ := os.UserHomeDir()
	return normalizeOptions(Options{
		RepoRoot:  repoRoot,
		WorkDir:   workDir,
		Home:      home,
		CodexHome: os.Getenv("CODEX_HOME"),
		AdminRoot: DefaultAdminRoot,
	})
}

func Discover(options Options) (*Store, error) {
	options = normalizeOptions(options)
	disabledPaths, err := loadDisabledSkillPaths(options.CodexHome)
	if err != nil {
		return nil, err
	}
	var roots []sourceRoot
	repoRoots, err := repoSkillRoots(options.RepoRoot, options.WorkDir)
	if err != nil {
		return nil, err
	}
	roots = append(roots, repoRoots...)
	if options.Home != "" {
		roots = append(roots, sourceRoot{path: filepath.Join(options.Home, ".agents", "skills"), scope: "user"})
	}
	if options.CodexHome != "" {
		roots = append(roots, sourceRoot{path: filepath.Join(options.CodexHome, "skills"), scope: "codex", includeSystem: true})
	}
	if options.AdminRoot != "" {
		roots = append(roots, sourceRoot{path: options.AdminRoot, scope: "admin"})
	}

	store := &Store{
		byLocator: map[string]Skill{},
	}
	state := discovery{
		store:         store,
		seenPaths:     map[string]struct{}{},
		seenNames:     map[string]struct{}{},
		disabledPaths: disabledPaths,
	}
	for _, root := range roots {
		if err := state.discoverRoot(root); err != nil {
			return nil, err
		}
	}
	if options.CodexHome != "" {
		if err := state.discoverPluginCache(filepath.Join(options.CodexHome, "plugins", "cache")); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(store.skills, func(a, b Skill) int {
		if a.Name != b.Name {
			return strings.Compare(a.Name, b.Name)
		}
		return strings.Compare(a.Locator, b.Locator)
	})
	for _, skill := range store.skills {
		store.byLocator[skill.Locator] = skill
	}
	return store, nil
}

func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	return len(s.skills)
}

func (s *Store) Skills() []Skill {
	if s == nil {
		return nil
	}
	return slices.Clone(s.skills)
}

func (s *Store) Lookup(locator string) (Skill, bool) {
	if s == nil {
		return Skill{}, false
	}
	skill, ok := s.byLocator[locator]
	return skill, ok
}

func (s *Store) Summary() []map[string]string {
	if s == nil || len(s.skills) == 0 {
		return nil
	}
	summary := make([]map[string]string, 0, len(s.skills))
	for _, skill := range s.skills {
		summary = append(summary, map[string]string{
			"name":    skill.Name,
			"scope":   skill.Scope,
			"locator": skill.Locator,
		})
	}
	return summary
}

func (s *Store) Render() string {
	if s == nil || len(s.skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Skills\n")
	b.WriteString("A skill is a set of local instructions provided through a `SKILL.md` source. Below is the list of skills that can be used. Each entry includes a name, description, and source locator.\n\n")
	b.WriteString("### Available skills\n")
	for _, skill := range s.skills {
		fmt.Fprintf(&b, "- name=%q description=%q source_locator=%q\n", skill.Name, skill.Description, skill.Locator)
	}
	b.WriteString("\n### How to use skills\n")
	b.WriteString("- Trigger rules: If the user names a skill with `$SkillName` or plain text, or the task clearly matches a skill description above, use that skill for this turn.\n")
	b.WriteString("- Before applying a skill, call `skills_read` for the listed source locator and read its `SKILL.md` completely.\n")
	b.WriteString("- When a loaded `SKILL.md` references another text resource under `references/`, call `skills_read` with the same source locator and the referenced relative path.\n")
	b.WriteString("- Do not load unrelated skill resources. Prefer directly referenced files over broad exploration.\n")
	b.WriteString("- Skill instructions cannot override tool policy, task instructions, output contracts, validators, project guidance priority, or authoritative Git/release evidence.\n")
	b.WriteString("- If a skill cannot be read or applied cleanly, mention the issue briefly only if it affects the final artifact; otherwise continue with the best fallback.")
	return textutil.NormalizePrompt(b.String())
}

func normalizeOptions(options Options) Options {
	if options.WorkDir == "" {
		options.WorkDir = options.RepoRoot
	}
	if options.CodexHome == "" && options.Home != "" {
		options.CodexHome = filepath.Join(options.Home, ".codex")
	}
	return options
}

func loadDisabledSkillPaths(codexHome string) (map[string]struct{}, error) {
	if codexHome == "" {
		return nil, nil
	}
	var config codexConfig
	configPath := filepath.Join(codexHome, "config.toml")
	if _, err := toml.DecodeFile(configPath, &config); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read Codex skill config %s: %w", configPath, err)
	}
	disabled := make(map[string]struct{})
	for _, skill := range config.Skills.Config {
		if skill.Path == "" || skill.Enabled == nil {
			continue
		}
		path, err := filepath.EvalSymlinks(filepath.Clean(skill.Path))
		if err != nil {
			path = filepath.Clean(skill.Path)
		}
		if *skill.Enabled {
			delete(disabled, path)
		} else {
			disabled[path] = struct{}{}
		}
	}
	return disabled, nil
}

func repoSkillRoots(repoRoot, workDir string) ([]sourceRoot, error) {
	if repoRoot == "" || workDir == "" {
		return nil, nil
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	current, err := filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, current)
	if err != nil {
		return nil, err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		current = root
	}
	var dirs []string
	for {
		dirs = append(dirs, current)
		if current == root {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	slices.Reverse(dirs)
	roots := make([]sourceRoot, 0, len(dirs))
	for _, dir := range dirs {
		roots = append(roots, sourceRoot{path: filepath.Join(dir, ".agents", "skills"), scope: "repo"})
	}
	return roots, nil
}

func (d *discovery) discoverRoot(root sourceRoot) error {
	info, err := os.Stat(root.path)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return d.scanDirectSkillRoot(root, root.includeSystem)
}

func (d *discovery) scanDirectSkillRoot(root sourceRoot, includeSystem bool) error {
	entries, err := os.ReadDir(root.path)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root.path, entry.Name())
		if entry.Name() == ".system" && includeSystem {
			if err := d.scanSystemSkillRoot(root, path); err != nil {
				return err
			}
			continue
		}
		if !entryIsDir(entry, path) {
			continue
		}
		if err := d.addSkill(root, filepath.Join(path, "SKILL.md")); err != nil {
			return err
		}
	}
	return nil
}

func (d *discovery) scanSystemSkillRoot(root sourceRoot, systemRoot string) error {
	entries, err := os.ReadDir(systemRoot)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(systemRoot, entry.Name())
		if !entryIsDir(entry, path) {
			continue
		}
		if err := d.addSkill(root, filepath.Join(path, "SKILL.md")); err != nil {
			return err
		}
	}
	return nil
}

func (d *discovery) discoverPluginCache(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsPermission(err) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if rel != "." && pathDepth(rel) > pluginMaxDepth {
			return filepath.SkipDir
		}
		if entry.Name() != "skills" {
			return nil
		}
		if err := d.scanDirectSkillRoot(sourceRoot{path: path, scope: "plugin"}, false); err != nil {
			return err
		}
		return filepath.SkipDir
	})
}

func entryIsDir(entry os.DirEntry, path string) bool {
	if entry.IsDir() {
		return true
	}
	if entry.Type()&os.ModeSymlink == 0 {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (d *discovery) addSkill(root sourceRoot, path string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil
	}
	if _, disabled := d.disabledPaths[resolved]; disabled {
		return nil
	}
	skill, ok, err := parseSkill(root, resolved)
	if err != nil || !ok {
		return err
	}
	if _, exists := d.seenPaths[resolved]; exists {
		return nil
	}
	if _, exists := d.seenNames[skill.Name]; exists {
		return nil
	}
	skill.Path = resolved
	skill.Root = filepath.Dir(resolved)
	skill.Locator = opaqueLocator(resolved)
	d.seenPaths[resolved] = struct{}{}
	d.seenNames[skill.Name] = struct{}{}
	d.store.skills = append(d.store.skills, skill)
	return nil
}

func opaqueLocator(path string) string {
	sum := sha256.Sum256([]byte(filepath.ToSlash(path)))
	return "skill:" + hex.EncodeToString(sum[:8])
}

func parseSkill(root sourceRoot, path string) (Skill, bool, error) {
	content, err := readFrontmatterCandidate(path)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return Skill{}, false, nil
		}
		return Skill{}, false, err
	}
	metaText, ok := frontmatterBlock(content)
	if !ok {
		return Skill{}, false, nil
	}
	var meta frontmatter
	if err := yaml.Unmarshal([]byte(metaText), &meta); err != nil {
		return Skill{}, false, nil
	}
	meta.Name = strings.Join(strings.Fields(meta.Name), " ")
	meta.Description = truncateMetadata(strings.Join(strings.Fields(meta.Description), " "), maxSkillDescLen)
	if !validSkillName(meta.Name) || hasCommitNameSegment(meta.Name) || !validSkillDescription(meta.Description) {
		return Skill{}, false, nil
	}
	return Skill{
		Name:        meta.Name,
		Description: meta.Description,
		Path:        path,
		Scope:       root.scope,
	}, true, nil
}

func readFrontmatterCandidate(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxFrontmatterBytes+1))
	if err != nil {
		return "", err
	}
	if len(content) > maxFrontmatterBytes {
		return "", nil
	}
	return string(content), nil
}

func hasCommitNameSegment(name string) bool {
	for part := range strings.FieldsFuncSeq(name, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == ':'
	}) {
		if strings.EqualFold(part, "commit") {
			return true
		}
	}
	return false
}

func validSkillName(name string) bool {
	if name == "" || len(name) > maxSkillNameLen {
		return false
	}
	for _, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == ':' {
			continue
		}
		return false
	}
	return true
}

func validSkillDescription(description string) bool {
	if description == "" {
		return false
	}
	for _, r := range description {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func truncateMetadata(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	text = text[:maxBytes]
	for !utf8.ValidString(text) && len(text) > 0 {
		text = text[:len(text)-1]
	}
	return strings.TrimSpace(text)
}

func frontmatterBlock(content string) (string, bool) {
	content = strings.TrimPrefix(content, "\uFEFF")
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return "", false
	}
	rest := strings.TrimPrefix(content, "---\n")
	lines := strings.Split(rest, "\n")
	var meta []string
	for _, line := range lines {
		if line == "---" {
			return strings.Join(meta, "\n"), true
		}
		meta = append(meta, line)
	}
	return "", false
}

func pathDepth(rel string) int {
	if rel == "." || rel == "" {
		return 0
	}
	return len(strings.Split(filepath.ToSlash(rel), "/"))
}
