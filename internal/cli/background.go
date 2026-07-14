package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const backgroundReviewChildEnv = "GIT_AGENT_BACKGROUND_REVIEW_CHILD"

func isBackgroundReviewChild() bool {
	return os.Getenv(backgroundReviewChildEnv) == "1"
}

func startBackgroundReview(command string, args []string, stderr io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate git-agent executable: %w", err)
	}
	output, err := startBackgroundProcess(executable, append([]string{command}, args...), os.Environ())
	if err != nil {
		return err
	}
	_, err = io.WriteString(stderr, output)
	return err
}

func startBackgroundProcess(executable string, args, env []string) (string, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return "", fmt.Errorf("create background startup pipe: %w", err)
	}
	defer reader.Close()

	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		writer.Close()
		return "", fmt.Errorf("open null device: %w", err)
	}
	defer null.Close()

	process, err := os.StartProcess(executable, append([]string{executable}, args...), &os.ProcAttr{
		Env:   backgroundChildEnvironment(env),
		Files: []*os.File{null, null, writer},
		Sys:   backgroundProcessAttributes(),
	})
	writer.Close()
	if err != nil {
		return "", fmt.Errorf("start background review: %w", err)
	}

	line, readErr := bufio.NewReader(reader).ReadString('\n')
	if readErr != nil {
		state, waitErr := process.Wait()
		return "", errors.Join(fmt.Errorf("background review exited before advertising events: %s", state), readErr, waitErr)
	}
	role, _, advertised := strings.Cut(line, ": agent events listening on ")
	if !advertised {
		state, waitErr := process.Wait()
		return "", errors.Join(fmt.Errorf("background review failed to advertise events: %s", strings.TrimSpace(line)), waitErr, fmt.Errorf("process state: %s", state))
	}
	if err := process.Release(); err != nil {
		return "", fmt.Errorf("release background review: %w", err)
	}
	return line + role + ": stop background agent: " + backgroundStopCommand(process.Pid) + "\n", nil
}

func backgroundChildEnvironment(env []string) []string {
	marker := backgroundReviewChildEnv + "="
	child := make([]string, 0, len(env)+1)
	for _, variable := range env {
		if !strings.HasPrefix(variable, marker) {
			child = append(child, variable)
		}
	}
	return append(child, marker+"1")
}

func closeBackgroundReviewStderr(stderr io.Writer) {
	if !isBackgroundReviewChild() {
		return
	}
	if file, ok := stderr.(*os.File); ok {
		_ = file.Close()
	}
}
