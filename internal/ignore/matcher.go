package ignore

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-git/go-git/v6/plumbing/format/gitignore"
)

const matchEnd = "\x00git-agent-ignore-match-end"

// Matcher applies Git ignore rules to each path prefix. Once a directory is
// excluded, a rule for one of its descendants cannot re-include that
// descendant unless the directory itself was re-included first.
type Matcher struct {
	patterns []pattern
}

type pattern struct {
	parsed      gitignore.Pattern
	exact       gitignore.Pattern
	base        []string
	simpleExact bool
}

// New returns a matcher seeded with basename-only patterns.
func New(patterns ...gitignore.Pattern) Matcher {
	result := Matcher{patterns: make([]pattern, len(patterns))}
	for i, parsed := range patterns {
		result.patterns[i] = pattern{parsed: parsed, simpleExact: true}
	}
	return result
}

// Append adds rules from one ignore file whose directory is base.
func (m Matcher) Append(text string, base []string) Matcher {
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		normalized := trimUnescapedTrailingSpaces(line)
		exact := strings.TrimSuffix(strings.TrimPrefix(normalized, "!"), "/")
		entry := pattern{
			parsed:      gitignore.ParsePattern(normalized, base),
			base:        slices.Clone(base),
			simpleExact: !strings.Contains(exact, "/"),
		}
		if !entry.simpleExact {
			entry.exact = gitignore.ParsePattern(strings.TrimSuffix(normalized, "/")+"/"+matchEnd, base)
		}
		m.patterns = append(m.patterns, entry)
	}
	return m
}

// Match implements gitignore.Matcher.
func (m Matcher) Match(path []string, isDir bool) bool {
	ignored := false
	for end := 1; end <= len(path); end++ {
		prefix := path[:end]
		prefixIsDir := end < len(path) || isDir
		if matched, found := m.matchExact(prefix, prefixIsDir); found {
			ignored = matched == gitignore.Exclude
		}
		if ignored && end < len(path) {
			return true
		}
	}
	return ignored
}

func (m Matcher) matchExact(path []string, isDir bool) (gitignore.MatchResult, bool) {
	for i := len(m.patterns) - 1; i >= 0; i-- {
		if result := m.patterns[i].matchExact(path, isDir); result != gitignore.NoMatch {
			return result, true
		}
	}
	return gitignore.NoMatch, false
}

func (p pattern) matchExact(path []string, isDir bool) gitignore.MatchResult {
	if p.simpleExact {
		if len(path) <= len(p.base) || !slices.Equal(path[:len(p.base)], p.base) {
			return gitignore.NoMatch
		}
		direct := make([]string, len(p.base)+1)
		copy(direct, p.base)
		direct[len(p.base)] = path[len(path)-1]
		return p.parsed.Match(direct, isDir)
	}
	if p.exact == nil {
		return gitignore.NoMatch
	}
	exactPath := make([]string, len(path)+1)
	copy(exactPath, path)
	exactPath[len(path)] = matchEnd
	return p.exact.Match(exactPath, false)
}

func trimUnescapedTrailingSpaces(line string) string {
	for strings.HasSuffix(line, " ") {
		backslashes := 0
		for i := len(line) - 2; i >= 0 && line[i] == '\\'; i-- {
			backslashes++
		}
		if backslashes%2 == 1 {
			break
		}
		line = line[:len(line)-1]
	}
	return line
}

// PathParts converts a native or slash-separated repository path into the
// component form accepted by Matcher.Match.
func PathParts(path string) []string {
	path = strings.Trim(filepath.ToSlash(path), "/")
	if path == "" || path == "." {
		return nil
	}
	return strings.Split(path, "/")
}
