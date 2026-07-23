package cli

import (
	"fmt"

	backgroundtask "github.com/yusing/git-agent/internal/background"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/projectidentity"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
)

const maxFollowUpPromptBytes = 256 << 10

func loadFollowUpParent(kind reviewtask.Kind, id string) (reviewtask.Mode, any, error) {
	repo, err := gitctx.Open(".")
	if err != nil {
		return "", nil, err
	}
	metadataDir, err := projectidentity.FromRepository(repo).Dir()
	if err != nil {
		return "", nil, err
	}
	store, err := backgroundtask.NewStore(metadataDir)
	if err != nil {
		return "", nil, err
	}
	return readFollowUpParent(store, kind, id)
}

func readFollowUpParent(store *backgroundtask.Store, kind reviewtask.Kind, id string) (reviewtask.Mode, any, error) {
	parent, err := store.Read(id)
	if err != nil {
		return "", nil, err
	}
	if parent.Command != string(kind) {
		return "", nil, fmt.Errorf("follow-up parent %s belongs to %s, not %s", id, parent.Command, kind)
	}
	if parent.Terminal == nil || parent.Terminal.Kind != "final" || parent.Turn == nil {
		return "", nil, fmt.Errorf("follow-up parent %s is not an eligible successful provider turn", id)
	}
	mode := reviewtask.Mode(parent.Turn.Mode)
	switch mode {
	case reviewtask.ModeCodebase, reviewtask.ModeUncommitted, reviewtask.ModeStaged:
	default:
		return "", nil, fmt.Errorf("follow-up parent %s has invalid mode %q", id, mode)
	}
	report, ok := parent.Terminal.Value["text"]
	if !ok || report == nil {
		return "", nil, fmt.Errorf("follow-up parent %s has no report", id)
	}
	return mode, report, nil
}
