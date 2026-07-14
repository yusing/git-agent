// Package projectidentity resolves the stable identity shared by project
// metadata consumers.
package projectidentity

import (
	"path/filepath"

	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/giturl"
	"github.com/yusing/git-agent/internal/metadata"
)

// Identity describes one local project's metadata identity.
type Identity struct {
	Root           string
	OriginIdentity string
}

// Resolve finds the containing Git repository when present. Non-Git paths use
// the cleaned absolute path as their identity.
func Resolve(start string) (Identity, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return Identity{}, err
	}
	if repo, err := gitctx.Open(abs); err == nil {
		return FromRepository(repo), nil
	}
	return Identity{Root: abs}, nil
}

// FromRepository resolves identity from an already-open repository.
func FromRepository(repo *gitctx.Repository) Identity {
	if repo == nil {
		return Identity{}
	}
	return Identity{
		Root:           filepath.Clean(repo.RootPath),
		OriginIdentity: Origin(repo),
	}
}

// Origin returns the normalized identity of the first origin URL.
func Origin(repo *gitctx.Repository) string {
	if repo == nil {
		return ""
	}
	cfg, err := repo.Repo.Config()
	if err != nil {
		return ""
	}
	remote := cfg.Remotes["origin"]
	if remote == nil || len(remote.URLs) == 0 {
		return ""
	}
	return giturl.Identity(remote.URLs[0])
}

// Dir returns the owner-only metadata directory for the identity.
func (i Identity) Dir() (string, error) {
	return metadata.ProjectDir(i.Root, i.OriginIdentity)
}
