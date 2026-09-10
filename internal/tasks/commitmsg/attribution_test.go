package commitmsg

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/gitctx"
)

func TestHistoricalAttributionFixturePreservesOnlyCurrentEvidence(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/historical-attribution.json")
	if err != nil {
		t.Fatal(err)
	}
	var prepared PreparedCommitContext
	if err := json.Unmarshal(data, &prepared); err != nil {
		t.Fatal(err)
	}
	// Load the old history separately: normal prepared context no longer
	// serializes historical prose, but local formatting still uses its style.
	var historical struct {
		RecentCommits    []gitctx.CommitInfo `json:"recent_commits"`
		PreviousHeadDiff string              `json:"previous_head_diff"`
	}
	if err := json.Unmarshal(data, &historical); err != nil {
		t.Fatal(err)
	}
	prepared.RecentCommits = historical.RecentCommits
	if len(prepared.StagedPaths) != 19 || len(prepared.StagedSubmodules) != 0 ||
		!prepared.DiffTruncated || len(prepared.FocusDiffPaths) != 3 ||
		!strings.Contains(historical.PreviousHeadDiff, "AppendSubmoduleTrailer") {
		t.Fatal("fixture no longer represents the diagnosed parent/current pair")
	}

	rendered := prepared.RenderForPrompt()
	var got PreparedCommitContext
	if err := json.Unmarshal([]byte(rendered), &got); err != nil {
		t.Fatal(err)
	}
	// No filtering of submodule-related words or unchanged hunks: retain all
	// current evidence, including secondary changes and truncation signals.
	want := prepared
	want.RecentCommits = nil
	if !reflect.DeepEqual(got, want) {
		t.Fatal("normal rendering altered current staged evidence")
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(rendered), &fields); err != nil {
		t.Fatal(err)
	}
	for field := range fields {
		if strings.HasPrefix(field, "previous_head_") || field == "recent_commits" {
			t.Fatalf("normal request leaked %s", field)
		}
	}
	if !strings.Contains(rendered, `"commit_style": "conventional"`) {
		t.Fatal("missing locally derived style")
	}
	// Changing only historical subject matter must not change the request.
	prepared.RecentCommits = []gitctx.CommitInfo{{Summary: "feat(other): unrelated prior capability"}}
	var before, after any
	if err := json.Unmarshal([]byte(rendered), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(prepared.RenderForPrompt()), &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("historical subject matter changed normal request")
	}

}

func TestPrepareCommitContextKeepsHistoryLocal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		message string
		style   string
	}{
		{"feat(history): historical-only-capability", "conventional"},
		{"Introduce historical-only-capability", "title-case"},
	} {
		t.Run(tc.style, func(t *testing.T) {
			repoDir := filepath.Join(t.TempDir(), "repo")
			initGitRepo(t, repoDir)
			writeFile(t, filepath.Join(repoDir, "past.txt"), "historical-only-implementation\n")
			runGit(t, repoDir, "add", ".")
			runGit(t, repoDir, "commit", "-m", tc.message)
			writeFile(t, filepath.Join(repoDir, "current.txt"), "current staged behavior\n")
			writeFile(t, filepath.Join(repoDir, "secondary.txt"), "secondary staged behavior\n")
			runGit(t, repoDir, "add", ".")
			repo, err := gitctx.Open(repoDir)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := PrepareCommitContext(repo)
			if err != nil {
				t.Fatal(err)
			}
			got := UserPromptWithPreparedCommitContext(prepared, 30, 24)
			for _, forbidden := range []string{"historical-only", "past.txt", "previous_head_", "recent_commits"} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("normal request leaked %s", forbidden)
				}
			}
			if !containsAll(got, tc.style, "current staged behavior", "secondary staged behavior") {
				t.Fatal("request lost style or staged change cluster")
			}
		})
	}
}
