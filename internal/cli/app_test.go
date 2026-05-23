package cli

import (
	"context"
	"testing"
)

func TestRunWithoutArgsReturnsUsage(t *testing.T) {
	t.Parallel()

	err := New().Run(context.Background(), nil)
	if err == nil {
		t.Fatal("expected usage error")
	}
}

func TestRunCommitMsgStub(t *testing.T) {
	t.Parallel()

	err := New().Run(context.Background(), []string{"commit-msg"})
	if err == nil {
		t.Fatal("expected not implemented error")
	}
}

func TestRunReleaseNoteRequiresRange(t *testing.T) {
	t.Parallel()

	err := New().Run(context.Background(), []string{"release-note"})
	if err == nil {
		t.Fatal("expected argument error")
	}
}
