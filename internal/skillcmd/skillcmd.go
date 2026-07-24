// Package skillcmd delegates skill listing and reading to the skills-mgr executable.
package skillcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultTimeout = 20 * time.Second
	maxOutputBytes = 64 * 1024
)

type Output struct {
	Stdout    string
	Stderr    string
	Truncated bool
}

type Command struct {
	Path    string
	Args    []string
	Dir     string
	Timeout time.Duration
}

type Runner interface {
	Run(context.Context, Command) (Output, error)
}

type LookupFunc func(string) (string, error)

type Options struct {
	Dir     string
	Timeout time.Duration
	Lookup  LookupFunc
	Runner  Runner
}

type Manager struct {
	path    string
	dir     string
	timeout time.Duration
	runner  Runner
}

func Discover(dir string) *Manager {
	return DiscoverWithOptions(Options{Dir: dir})
}

func DiscoverWithOptions(options Options) *Manager {
	lookup := options.Lookup
	if lookup == nil {
		lookup = exec.LookPath
	}
	runner := options.Runner
	if runner == nil {
		runner = commandRunner{}
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	dir, _ := filepath.Abs(options.Dir)
	path := ""
	found, err := lookup("skills-mgr")
	if err == nil && found != "" {
		path, err = filepath.Abs(found)
		if err != nil {
			path = ""
		}
	}
	return &Manager{
		path:    filepath.Clean(path),
		dir:     filepath.Clean(dir),
		timeout: timeout,
		runner:  runner,
	}
}

func (m *Manager) Available() bool {
	return m != nil && m.path != "" && m.path != "."
}

func (m *Manager) List(ctx context.Context) (Output, error) {
	return m.run(ctx, []string{"list"}, m.timeout)
}

func (m *Manager) Read(ctx context.Context, locator, lineRange string) (Output, error) {
	args := []string{"get", locator}
	if lineRange != "" {
		args = append(args, lineRange)
	}
	return m.run(ctx, args, m.timeout)
}

func (m *Manager) run(ctx context.Context, args []string, timeout time.Duration) (Output, error) {
	if !m.Available() {
		return Output{}, errors.New("skills-mgr is unavailable")
	}
	result, err := m.runner.Run(ctx, Command{
		Path: m.path, Args: args, Dir: m.dir, Timeout: timeout,
	})
	if err != nil {
		return Output{}, err
	}
	return result, nil
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, command Command) (Output, error) {
	commandCtx, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()

	cmd := exec.CommandContext(commandCtx, command.Path, command.Args...)
	cmd.Dir = command.Dir
	cmd.Stdin = nil
	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := Output{
		Stdout: strings.TrimSpace(stdout.String()), Stderr: strings.TrimSpace(stderr.String()),
		Truncated: stdout.truncated || stderr.truncated,
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		return result, fmt.Errorf("skills-mgr command timed out after %s", command.Timeout)
	}
	if err != nil {
		if result.Stderr != "" {
			return result, fmt.Errorf("skills-mgr command failed: %s", result.Stderr)
		}
		return result, fmt.Errorf("skills-mgr command failed: %w", err)
	}
	return result, nil
}

type cappedBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := maxOutputBytes - b.buffer.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(data)
	return original, nil
}

func (b *cappedBuffer) String() string {
	return b.buffer.String()
}
