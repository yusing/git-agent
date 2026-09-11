package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/config"
	"github.com/yusing/git-agent/internal/tasks/commitmsg"
)

// Opt in explicitly: this evaluation uses configured provider credentials and
// billable requests. Ordinary tests use fake servers and do not run it.
func TestLiveCommitIntent(t *testing.T) {
	if os.Getenv("GIT_AGENT_LIVE_COMMIT_INTENT") != "1" {
		t.Skip("set GIT_AGENT_LIVE_COMMIT_INTENT=1 to evaluate gpt-5.6-luna")
	}
	cfg, err := config.Resolve(config.Options{Model: "gpt-5.6-luna"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, hint, content string
		want, absent        []string
	}{
		{
			name:    "related_port",
			hint:    "port rF30625",
			content: "SnomLogo = /images/snom.svg\nSnomGroups = deskphone, DECT base, DECT handset, hospitality\n",
			want:    []string{"sync:", "port rF30625", "(T47286)"},
			absent:  []string{"rF30622", "rF30618"},
		},
		{
			name:    "explicit_task_overrides_history",
			hint:    "port rF30625 (T49999)",
			content: "SnomLogo = /images/snom.svg\nSnomGroups = deskphone, DECT base, DECT handset, hospitality\n",
			want:    []string{"sync:", "port rF30625", "(T49999)"},
			absent:  []string{"T47286"},
		},
		{
			name:    "unrelated_change",
			content: "func EscapeXML(s string) string { return xmlEscape(s) }\n",
			want:    []string{"XML"},
			absent:  []string{"T47286", "rF30622", "Snom"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := initRepo(t)
			t.Chdir(repoDir)
			for _, subject := range []string{
				"sync: port rF30618 handle hospitality H-series and the M-SC DECT series (T47286)",
				"sync: port rF30622 - support Snom M-series DECT bases (T47286)",
				"chore: tidy formatting",
			} {
				runGit(t, repoDir, "commit", "--allow-empty", "-m", subject)
			}
			writeFixtureFile(t, filepath.Join(repoDir, "config.txt"), tc.content)
			runGit(t, repoDir, "add", "config.txt")
			repo, state, err := openCommitRepository(repoDir)
			if err != nil {
				t.Fatal(err)
			}
			paths, err := repo.StagedPaths()
			if err != nil {
				t.Fatal(err)
			}
			caseCfg := cfg
			caseCfg.AppendPrompt = tc.hint
			var stdout, stderr bytes.Buffer
			app := &App{stdout: &stdout, stderr: &stderr}
			result, err := app.generateCommitMessage(t.Context(), caseCfg, repo, paths, commitmsg.ModeNormal, "commit-msg", nil, state)
			if err != nil {
				t.Fatal(err)
			}
			t.Log(result.Text)
			subject, _, _ := strings.Cut(result.Text, "\n")
			for _, want := range tc.want {
				if !strings.Contains(subject, want) {
					t.Errorf("subject missing %q: %s", want, subject)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(result.Text, absent) {
					t.Errorf("message contains unsupported %q: %s", absent, result.Text)
				}
			}
		})
	}
}

