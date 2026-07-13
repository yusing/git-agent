package giturl

import "testing"

func TestIdentityNormalizesCommonRemoteForms(t *testing.T) {
	for _, remote := range []string{
		"git@GitHub.com:Acme/Widget.git",
		"ssh://git@github.com/Acme/Widget.git",
		"https://token@github.com/Acme/Widget.git?x=secret#fragment",
	} {
		if got := Identity(remote); got != "github.com/Acme/Widget" {
			t.Errorf("Identity(%q) = %q", remote, got)
		}
	}
}

func TestSanitizeRemovesURLSecrets(t *testing.T) {
	got := Sanitize("https://user:secret@example.test/repo.git?token=value#private")
	if got != "https://example.test/repo.git" {
		t.Fatalf("Sanitize() = %q", got)
	}
}

func TestIdentityPreservesExplicitPort(t *testing.T) {
	first := Identity("ssh://git@example.test:2200/acme/repo.git")
	second := Identity("ssh://git@example.test:2222/acme/repo.git")
	if first == second {
		t.Fatalf("identities collide: %q", first)
	}
}

func TestIdentityDropsDefaultPort(t *testing.T) {
	if Identity("ssh://git@example.test:22/acme/repo.git") != Identity("git@example.test:acme/repo.git") {
		t.Fatal("default SSH port changed identity")
	}
}
