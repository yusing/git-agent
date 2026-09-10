package cli

import (
	"fmt"
	"os"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/yusing/git-agent/internal/gitctx"
)

// Git's repository redirection is not interpreted by the context collector.
// Never include values here: environment settings may contain credentials.
func checkCommitEnvironment() error {
	for _, name := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_NAMESPACE", "GIT_SHALLOW_FILE",
	} {
		if _, set := os.LookupEnv(name); set {
			return fmt.Errorf("commit commands do not support %s; unset it and rerun", name)
		}
	}
	return nil
}

type commitState struct {
	head        string
	ref         string
	fingerprint gitctx.ChangeFingerprint
}

func openCommitRepository(path string) (*gitctx.Repository, commitState, error) {
	if err := checkCommitEnvironment(); err != nil {
		return nil, commitState{}, err
	}
	repo, err := gitctx.Open(path)
	if err != nil {
		return nil, commitState{}, err
	}
	ref, err := repo.Repo.Reference(plumbing.HEAD, false)
	if err != nil {
		return nil, commitState{}, err
	}
	fingerprint, err := repo.StagedFingerprint()
	if err != nil {
		return nil, commitState{}, err
	}
	return repo, commitState{head: repo.HeadSHA, ref: ref.String(), fingerprint: fingerprint}, nil
}

func (s commitState) check(path string) error {
	// Reopen rather than consulting cached HEAD or index data. This detects
	// concurrent changes up to this check, not changes made later by Git hooks.
	_, current, err := openCommitRepository(path)
	if err != nil {
		return fmt.Errorf("verify commit repository state: %w", err)
	}
	if s != current {
		return gitctx.ErrChangeSnapshotStale
	}
	return nil
}
