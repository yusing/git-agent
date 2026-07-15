package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	backgroundtask "github.com/yusing/git-agent/internal/background"
)

const (
	detachedChildEnv  = "GIT_AGENT_DETACHED_CHILD"
	detachedTaskIDEnv = "GIT_AGENT_DETACHED_TASK_ID"
)

func isDetachedChild() bool {
	return os.Getenv(detachedChildEnv) == "1"
}

func detachedTaskID() string {
	return os.Getenv(detachedTaskIDEnv)
}

type detachedLaunch struct {
	Command  string            `json:"command"`
	ID       string            `json:"id"`
	PID      int               `json:"pid"`
	Endpoint localHTTPEndpoint `json:"endpoint"`
}

const maxDetachedLaunchBytes = 4096

func startDetachedTask(command string, args []string, stdout io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate git-agent executable: %w", err)
	}
	taskID := backgroundtask.NewID()
	launch, err := startDetachedProcess(executable, append([]string{command}, args...), detachedChildEnvironment(os.Environ(), taskID))
	if err != nil {
		return err
	}
	if launch.Command != command || launch.ID != taskID {
		return errors.New("detached task advertised mismatched identity")
	}
	return writeDetachedLaunch(stdout, launch)
}

func startDetachedProcess(executable string, args, env []string) (detachedLaunch, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return detachedLaunch{}, fmt.Errorf("create background startup pipe: %w", err)
	}
	defer reader.Close()

	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		writer.Close()
		return detachedLaunch{}, fmt.Errorf("open null device: %w", err)
	}
	defer null.Close()

	process, err := os.StartProcess(executable, append([]string{executable}, args...), &os.ProcAttr{
		Env:   env,
		Files: []*os.File{null, null, writer},
		Sys:   detachedProcessAttributes(),
	})
	writer.Close()
	if err != nil {
		return detachedLaunch{}, fmt.Errorf("start detached task: %w", err)
	}

	launch, readErr := readDetachedLaunch(reader)
	if readErr != nil {
		state, waitErr := process.Wait()
		return detachedLaunch{}, errors.Join(fmt.Errorf("detached task exited before advertising launch metadata: %s", state), readErr, waitErr)
	}
	if launch.PID != process.Pid {
		state, waitErr := process.Wait()
		return detachedLaunch{}, errors.Join(fmt.Errorf("detached task advertised PID %d, started PID %d", launch.PID, process.Pid), waitErr, fmt.Errorf("process state: %s", state))
	}
	if err := process.Release(); err != nil {
		return detachedLaunch{}, fmt.Errorf("release detached task: %w", err)
	}
	return launch, nil
}

func writeDetachedLaunch(writer io.Writer, launch detachedLaunch) error {
	if err := json.NewEncoder(writer).Encode(launch); err != nil {
		return fmt.Errorf("encode detached task launch metadata: %w", err)
	}
	return nil
}

func readDetachedLaunch(reader io.Reader) (detachedLaunch, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxDetachedLaunchBytes+1))
	if err != nil {
		return detachedLaunch{}, fmt.Errorf("read detached task launch metadata: %w", err)
	}
	if len(data) > maxDetachedLaunchBytes {
		_, drainErr := io.Copy(io.Discard, reader)
		return detachedLaunch{}, errors.Join(fmt.Errorf("detached task launch metadata exceeds %d bytes", maxDetachedLaunchBytes), drainErr)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] != '{' {
		return detachedLaunch{}, fmt.Errorf("detached task startup: %q", string(trimmed))
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var launch detachedLaunch
	if err := decoder.Decode(&launch); err != nil {
		return detachedLaunch{}, fmt.Errorf("decode detached task launch metadata: %w", err)
	}
	if launch.Command == "" || launch.ID == "" || launch.PID <= 0 || launch.Endpoint.Network == "" || launch.Endpoint.Address == "" || launch.Endpoint.URL == "" {
		return detachedLaunch{}, errors.New("detached task launch metadata is incomplete")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return detachedLaunch{}, fmt.Errorf("decode detached task launch metadata: %w", err)
	}
	return launch, nil
}

func detachedChildEnvironment(env []string, taskID string) []string {
	childMarker := detachedChildEnv + "="
	idMarker := detachedTaskIDEnv + "="
	child := make([]string, 0, len(env)+2)
	for _, variable := range env {
		if !strings.HasPrefix(variable, childMarker) && !strings.HasPrefix(variable, idMarker) {
			child = append(child, variable)
		}
	}
	return append(child, childMarker+"1", idMarker+taskID)
}
