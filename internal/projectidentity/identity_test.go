package projectidentity

import (
	"path/filepath"
	"testing"

	git "github.com/go-git/go-git/v6"
	gitconfig "github.com/go-git/go-git/v6/config"
	"github.com/yusing/git-agent/internal/metadata"
)

func TestResolveSharesNormalizedOriginAcrossClonesAndURLSpellings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	first := initRepository(t, "git@GitHub.com:Acme/Widget.git")
	second := initRepository(t, "https://token@github.com/Acme/Widget.git?secret=value")

	firstIdentity, err := Resolve(first)
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := Resolve(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstIdentity.OriginIdentity != "github.com/Acme/Widget" || firstIdentity.OriginIdentity != secondIdentity.OriginIdentity {
		t.Fatalf("origin identities = %q and %q", firstIdentity.OriginIdentity, secondIdentity.OriginIdentity)
	}
	firstDir, err := firstIdentity.Dir()
	if err != nil {
		t.Fatal(err)
	}
	secondDir, err := secondIdentity.Dir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".git-agent", metadata.IdentitySHA("github.com/Acme/Widget"))
	if firstDir != want || secondDir != want {
		t.Fatalf("metadata dirs = %q and %q, want %q", firstDir, secondDir, want)
	}
}

func TestResolveFallsBackToCleanedProjectPathWithoutOrigin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, test := range []struct {
		name string
		root func() string
	}{
		{name: "Git without origin", root: func() string { return initRepository(t, "") }},
		{name: "non-Git", root: t.TempDir},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := test.root()
			identity, err := Resolve(filepath.Join(root, "."))
			if err != nil {
				t.Fatal(err)
			}
			if identity.OriginIdentity != "" || identity.Root != filepath.Clean(root) {
				t.Fatalf("identity = %#v", identity)
			}
			dir, err := identity.Dir()
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(home, ".git-agent", metadata.PathSHA(filepath.Clean(root)))
			if dir != want {
				t.Fatalf("metadata dir = %q, want %q", dir, want)
			}
		})
	}
}

func initRepository(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		cfg, err := repo.Config()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Remotes["origin"] = &gitconfig.RemoteConfig{Name: "origin", URLs: []string{origin}}
		if err := repo.SetConfig(cfg); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
