package ignore

import "testing"

func TestTrimUnescapedTrailingSpaces(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		input string
		want  string
	}{
		{input: "plain   ", want: "plain"},
		{input: `escaped\  `, want: `escaped\ `},
		{input: `even\\ `, want: `even\\`},
		{input: `odd\\\ `, want: `odd\\\ `},
	} {
		if got := trimUnescapedTrailingSpaces(tt.input); got != tt.want {
			t.Errorf("trimUnescapedTrailingSpaces(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestMatcherRequiresEveryExcludedParentToBeReincluded(t *testing.T) {
	t.Parallel()

	matcher := New().Append("*\n!.local/\n!.local/share/\n!.local/share/keep.txt\n", nil)
	tests := []struct {
		name    string
		path    []string
		isDir   bool
		ignored bool
	}{
		{name: "positive allowlisted file", path: []string{".local", "share", "keep.txt"}},
		{name: "negative ignored sibling", path: []string{".local", "share", "containers"}, isDir: true, ignored: true},
		{name: "ignored sibling descendant", path: []string{".local", "share", "containers", "locked"}, isDir: true, ignored: true},
		{name: "unrelated basename collision", path: []string{"other", "keep.txt"}, ignored: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := matcher.Match(tt.path, tt.isDir); got != tt.ignored {
				t.Fatalf("Match(%q, %v) = %v, want %v", tt.path, tt.isDir, got, tt.ignored)
			}
		})
	}
}

func TestMatcherHandlesMalformedAndUnknownRulesConservatively(t *testing.T) {
	t.Parallel()

	matcher := New().Append("[\nfuture-ignore-format::value\nignored/\n", nil)
	if !matcher.Match([]string{"ignored", "file.txt"}, false) {
		t.Fatal("valid rule after malformed and unknown rules was lost")
	}
	if matcher.Match([]string{"unrelated.txt"}, false) {
		t.Fatal("malformed or unknown rule ignored an unrelated path")
	}
}

func TestMatcherRestoresReincludedDirectory(t *testing.T) {
	t.Parallel()

	matcher := New().Append("restored/\n!restored/\n", nil)
	if matcher.Match([]string{"restored", "nested", "file.txt"}, false) {
		t.Fatal("re-included directory still excluded its descendants")
	}
}

func TestNestedBasenameRulesStayBelowTheirBase(t *testing.T) {
	t.Parallel()

	matcher := New().Append("*.secret\n", []string{"a"}).Append("!file.secret\n", []string{"b"})
	if !matcher.Match([]string{"a", "file.secret"}, false) {
		t.Fatal("nested exclusion was overridden by an unrelated sibling rule")
	}
	if matcher.Match([]string{"b", "file.secret"}, false) {
		t.Fatal("nested exclusion leaked into a sibling directory")
	}
	if matcher.Match([]string{"file.secret"}, false) {
		t.Fatal("nested exclusion leaked into the repository root")
	}
}

func TestMatcherPreservesEscapedTrailingSpaceBeforePadding(t *testing.T) {
	t.Parallel()

	matcher := New().Append("secret\\  \n", nil)
	if !matcher.Match([]string{"secret "}, false) {
		t.Fatal("escaped trailing space was removed with unescaped padding")
	}
	if matcher.Match([]string{"secret"}, false) {
		t.Fatal("escaped trailing-space rule matched the unspaced name")
	}
}
