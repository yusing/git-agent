package textutil

import (
	"strings"
	"testing"
)

func TestNormalizePrompt(t *testing.T) {
	t.Parallel()

	got := NormalizePrompt("  a\t\n\n\n b \n")
	want := "a\n\nb"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestLimitMarksTruncation(t *testing.T) {
	t.Parallel()

	got, truncated := Limit("one\ntwo\nthree\n", 0, 2)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if got != "one\ntwo\n[truncated]\n" {
		t.Fatalf("got %q", got)
	}
}

func TestWrapBodyReflowsSoftLineBreaks(t *testing.T) {
	t.Parallel()

	got := WrapBody(`first paragraph has a soft
line break

second paragraph`, 80)
	want := `first paragraph has a soft line break

second paragraph`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWrapBodyPreservesStructuralLines(t *testing.T) {
	t.Parallel()

	got := WrapBody(`first paragraph
continues

- first item
  indented block
- second item
Refs: #123`, 80)
	want := `first paragraph continues

- first item
  indented block
- second item
Refs: #123`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestWrapBodyWrapsStructuralProse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		firstPrefix        string
		continuationPrefix string
		text               string
	}{
		{
			name:               "unordered list",
			firstPrefix:        "- ",
			continuationPrefix: "  ",
			text:               "Include previous HEAD context while keeping current staged changes authoritative for the generated commit message.",
		},
		{
			name:               "multi-digit ordered list",
			firstPrefix:        "123. ",
			continuationPrefix: "     ",
			text:               "Keep generic ordered markers intact when later callers introduce wider sequence numbers.",
		},
		{
			name:               "blockquote",
			firstPrefix:        "> ",
			continuationPrefix: "> ",
			text:               "Preserve quote structure on every locally wrapped continuation line.",
		},
		{
			name:               "nested blockquote",
			firstPrefix:        "> > ",
			continuationPrefix: "> > ",
			text:               "Preserve every quote level when compound structural prefixes require wrapping.",
		},
		{
			name:               "quoted unordered list",
			firstPrefix:        "> - ",
			continuationPrefix: ">   ",
			text:               "Keep the quote and nested list structure on hanging continuation lines.",
		},
		{
			name:               "git trailer",
			firstPrefix:        "Refs: ",
			continuationPrefix: "      ",
			text:               "issue-123 and issue-456 with enough explanatory text to require a safe continuation.",
		},
	}

	const width = 48
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lines := strings.Split(WrapBody(tt.firstPrefix+tt.text, width), "\n")
			if len(lines) < 2 {
				t.Fatalf("expected wrapped output, got %q", lines)
			}
			var content []string
			for i, line := range lines {
				prefix := tt.continuationPrefix
				if i == 0 {
					prefix = tt.firstPrefix
				}
				if !strings.HasPrefix(line, prefix) {
					t.Fatalf("line %d = %q, want prefix %q", i+1, line, prefix)
				}
				if len(line) > width {
					t.Fatalf("line %d is %d bytes, want <= %d: %q", i+1, len(line), width, line)
				}
				content = append(content, strings.TrimPrefix(line, prefix))
			}
			if got := strings.Join(content, " "); got != tt.text {
				t.Fatalf("wrapped content = %q, want %q", got, tt.text)
			}
		})
	}
}

func TestWrapBodyDoesNotTreatMarkerCollisionsAsStructuralProse(t *testing.T) {
	t.Parallel()

	for _, input := range []string{
		"-not-a-list " + strings.Repeat("word ", 12),
		"Refs=value " + strings.Repeat("word ", 12),
	} {
		lines := strings.Split(WrapBody(input, 32), "\n")
		if len(lines) < 2 {
			t.Fatalf("expected wrapped output for %q", input)
		}
		if strings.HasPrefix(lines[1], "  ") {
			t.Fatalf("unrelated marker collision gained structural indentation: %q", lines)
		}
	}
}

func TestWrapBodyDoesNotSplitUnbreakableTokens(t *testing.T) {
	t.Parallel()

	token := "https://example.com/" + strings.Repeat("x", 90)
	got := WrapBody("see "+token, 20)
	want := "see\n" + token
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	got = WrapBody("- "+token, 20)
	want = "- " + token
	if got != want {
		t.Fatalf("structural token got:\n%s\nwant:\n%s", got, want)
	}
}
