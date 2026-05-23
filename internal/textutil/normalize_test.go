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
