//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package golangci

import (
	"errors"
	"os"
	"syscall"
)

func checkerProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func killCheckerProcess(process *os.Process) error {
	err := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
