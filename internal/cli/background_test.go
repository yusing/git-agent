package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestDetachedProcessKeepsEventEndpointAliveAfterAdvertisement(t *testing.T) {
	if os.Getenv("GIT_AGENT_DETACHED_TEST_HELPER") == "1" {
		listener, endpoint, err := listenLocalHTTP("/events", url.Values{"token": []string{"test"}})
		if err != nil {
			os.Exit(2)
		}
		_ = writeDetachedLaunch(os.Stderr, detachedLaunch{
			Command:  "review",
			ID:       "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
			PID:      os.Getpid(),
			Endpoint: endpoint,
		})
		_ = os.Stderr.Close()
		conn, err := listener.Accept()
		if err != nil {
			os.Exit(3)
		}
		request := bufio.NewReader(conn)
		for {
			line, err := request.ReadString('\n')
			if err != nil || line == "\r\n" {
				break
			}
		}
		_, _ = fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\n\r\nready")
		_ = conn.Close()
		_ = listener.Close()
		os.Exit(0)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launch, err := startDetachedProcess(
		executable,
		[]string{"-test.run=^TestDetachedProcessKeepsEventEndpointAliveAfterAdvertisement$"},
		append(os.Environ(), "GIT_AGENT_DETACHED_TEST_HELPER=1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Command != "review" || launch.ID != "ABCDEFGHIJKLMNOPQRSTUVWXYZ" || launch.PID <= 0 || launch.Endpoint.Address == "" {
		t.Fatalf("launch = %#v", launch)
	}
	response, err := localHTTPTestClient(t, launch.Endpoint).Get(launch.Endpoint.URL)
	if err != nil {
		t.Fatalf("event endpoint unavailable after launcher returned: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("event endpoint status = %s", response.Status)
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
