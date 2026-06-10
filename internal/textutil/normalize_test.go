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

func TestWrapBodyDoesNotSplitUnbreakableTokens(t *testing.T) {
	t.Parallel()

	token := "https://example.com/" + strings.Repeat("x", 90)
	got := WrapBody("see "+token, 20)
	want := "see\n" + token
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
