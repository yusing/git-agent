package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestDetachedProcessReadsLaunchMetadataWithoutEndpoint(t *testing.T) {
	if os.Getenv("GIT_AGENT_DETACHED_TEST_HELPER") == "1" {
		_ = advertiseDetachedLaunch(os.Stderr, detachedLaunch{
			Command: "review",
			ID:      "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
			PID:     os.Getpid(),
		})
		os.Exit(0)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launch, err := startDetachedProcess(
		executable,
		[]string{"-test.run=^TestDetachedProcessReadsLaunchMetadataWithoutEndpoint$"},
		append(os.Environ(), "GIT_AGENT_DETACHED_TEST_HELPER=1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Command != "review" || launch.ID != "ABCDEFGHIJKLMNOPQRSTUVWXYZ" || launch.PID <= 0 {
		t.Fatalf("launch = %#v", launch)
	}
}

func TestDetachedChildEnvironmentReplacesExistingMarker(t *testing.T) {
	taskID := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	got := detachedChildEnvironment([]string{
		"KEEP=value",
		detachedChildEnv + "=0",
		detachedChildEnv + "=stale",
		detachedTaskIDEnv + "=old",
	}, taskID)
	want := "KEEP=value\n" + detachedChildEnv + "=1\n" + detachedTaskIDEnv + "=" + taskID
	if strings.Join(got, "\n") != want {
		t.Fatalf("detached child environment = %#v", got)
	}
}

func TestDetachedLaunchRejectsAndDrainsOversizedJSON(t *testing.T) {
	input := bytes.NewBufferString(strings.Repeat("x", maxDetachedLaunchBytes*2))
	if _, err := readDetachedLaunch(input); err == nil {
		t.Fatal("oversized launch metadata accepted")
	}
	if input.Len() != 0 {
		t.Fatalf("launch bytes remaining = %d", input.Len())
	}
}

func TestDetachedLaunchReportsChildStartupDiagnostic(t *testing.T) {
	_, err := readDetachedLaunch(strings.NewReader("listen unix /tmp/git-agent/http.sock: permission denied\n"))
	if err == nil || !strings.Contains(err.Error(), `detached task startup: "listen unix /tmp/git-agent/http.sock: permission denied"`) {
		t.Fatalf("error = %v", err)
	}
}
