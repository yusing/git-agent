//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package golangci

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/yusing/git-agent/internal/checks"
)

func TestRunCancelsActiveCheckerProcess(t *testing.T) {
	root := t.TempDir()
	writePlanFile(t, root, "go.mod", "module example\n")
	writePlanFile(t, root, "selected.go", "package example\n")
	scope, err := checks.NewChangedScope(root, []string{"selected.go"}, []string{""})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := New().Plan(scope)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("GIT_AGENT_CHECK_TEST_ADDR", listener.Addr().String())

	ctx, cancel := context.WithCancel(t.Context())
	executable := buildBlockingChecker(t)
	result := make(chan error, 1)
	go func() {
		_, err := New().Run(ctx, executable, plan)
		result <- err
	}()

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- connection
	}()

	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	var connection net.Conn
	select {
	case connection = <-accepted:
		defer connection.Close()
	case err := <-acceptErr:
		t.Fatalf("accept checker notification: %v", err)
	case err := <-result:
		t.Fatalf("checker exited before cancellation: %v", err)
	case <-timer.C:
		t.Fatal("checker did not start")
	}

	cancel()
	timer.Reset(10 * time.Second)
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	case <-timer.C:
		t.Fatal("active checker was not terminated after cancellation")
	}
}

func buildBlockingChecker(t *testing.T) string {
	t.Helper()
	source := filepath.Join(t.TempDir(), "main.go")
	program := `package main

import (
	"io"
	"net"
	"os"
)

func main() {
	connection, err := net.Dial("tcp", os.Getenv("GIT_AGENT_CHECK_TEST_ADDR"))
	if err != nil {
		os.Exit(2)
	}
	defer connection.Close()
	_, _ = io.Copy(io.Discard, connection)
}
`
	if err := os.WriteFile(source, []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "blocking-checker")
	command := exec.CommandContext(t.Context(), "go", "build", "-o", executable, source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build blocking checker: %v\n%s", err, output)
	}
	return executable
}
