package textutil

import "testing"

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
