package cli

import (
	"fmt"
	"path/filepath"

	backgroundtask "github.com/yusing/git-agent/internal/background"
	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/projectidentity"
	reviewtask "github.com/yusing/git-agent/internal/tasks/review"
)

const maxFollowUpPromptBytes = 256 << 10

type reviewFollowUpParent struct {
	Mode   reviewtask.Mode
	Report any
	Turn   backgroundtask.TurnMetadata
}

func loadFollowUpParent(kind reviewtask.Kind, id string) (reviewFollowUpParent, error) {
	repo, err := gitctx.Open(".")
	if err != nil {
		return reviewFollowUpParent{}, err
	}
	metadataDir, err := projectidentity.FromRepository(repo).Dir()
	if err != nil {
		return reviewFollowUpParent{}, err
	}
	store, err := backgroundtask.NewStore(metadataDir)
	if err != nil {
		return reviewFollowUpParent{}, err
	}
	return readFollowUpParent(store, kind, id, repo.WorkPath)
}

func readFollowUpParent(
	store *backgroundtask.Store,
	kind reviewtask.Kind,
	id string,
	workspace string,
) (reviewFollowUpParent, error) {
	parent, err := store.Read(id)
	if err != nil {
		return reviewFollowUpParent{}, err
	}
	if parent.Command != string(kind) {
		return reviewFollowUpParent{}, fmt.Errorf("follow-up parent %s belongs to %s, not %s", id, parent.Command, kind)
	}
	if parent.Terminal == nil || parent.Terminal.Kind != "final" || parent.Turn == nil {
		return reviewFollowUpParent{}, fmt.Errorf("follow-up parent %s is not an eligible successful provider turn", id)
	}
	if filepath.Clean(parent.Turn.Workspace) != filepath.Clean(workspace) {
		return reviewFollowUpParent{}, fmt.Errorf("follow-up parent %s belongs to workspace %s, not %s", id, parent.Turn.Workspace, workspace)
	}
	mode := reviewtask.Mode(parent.Turn.Mode)
	switch mode {
	case reviewtask.ModeCodebase, reviewtask.ModeUncommitted, reviewtask.ModeStaged:
	default:
		return reviewFollowUpParent{}, fmt.Errorf("follow-up parent %s has invalid mode %q", id, mode)
	}
	if _, err := reviewtask.ParseDepth(parent.Turn.ReviewDepth); err != nil {
		return reviewFollowUpParent{}, fmt.Errorf("follow-up parent %s has invalid inspection depth: %w", id, err)
	}
	if len(parent.Turn.Leaves) == 0 {
		return reviewFollowUpParent{}, fmt.Errorf("follow-up parent %s has no replayable input", id)
	}
	report, ok := parent.Terminal.Value["text"]
	if !ok || report == nil {
		return reviewFollowUpParent{}, fmt.Errorf("follow-up parent %s has no report", id)
	}
	return reviewFollowUpParent{
		Mode: mode, Report: report, Turn: *parent.Turn,
	}, nil
}
