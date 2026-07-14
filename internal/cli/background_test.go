package cli

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestBackgroundProcessKeepsEventEndpointAliveAfterAdvertisement(t *testing.T) {
	if os.Getenv("GIT_AGENT_BACKGROUND_TEST_HELPER") == "1" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "review: agent events listening on http://%s/events?token=test\n", listener.Addr())
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
	output, err := startBackgroundProcess(
		executable,
		[]string{"-test.run=^TestBackgroundProcessKeepsEventEndpointAliveAfterAdvertisement$"},
		append(os.Environ(), "GIT_AGENT_BACKGROUND_TEST_HELPER=1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 2 {
		t.Fatalf("background instructions = %q, want advertisement and stop command", output)
	}
	parts := strings.SplitN(lines[0], " on ", 2)
	if len(parts) != 2 {
		t.Fatalf("advertisement = %q", lines[0])
	}
	stopPrefix := "review: stop background agent: kill -- "
	if runtime.GOOS == "windows" {
		stopPrefix = "review: stop background agent: taskkill /PID "
	}
	if !strings.HasPrefix(lines[1], stopPrefix) {
		t.Fatalf("stop instruction = %q, want prefix %q", lines[1], stopPrefix)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(lines[1], " /T /F") {
		t.Fatalf("stop instruction = %q, want process-tree termination", lines[1])
	}
	response, err := http.Get(parts[1])
	if err != nil {
		t.Fatalf("event endpoint unavailable after launcher returned: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("event endpoint status = %s", response.Status)
	}
}

func TestBackgroundChildEnvironmentReplacesExistingMarker(t *testing.T) {
	got := backgroundChildEnvironment([]string{"KEEP=value", backgroundReviewChildEnv + "=0", backgroundReviewChildEnv + "=stale"})
	if strings.Join(got, "\n") != "KEEP=value\n"+backgroundReviewChildEnv+"=1" {
		t.Fatalf("background child environment = %#v", got)
	}
}
