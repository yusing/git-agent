//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package cli

import (
	"fmt"
	"syscall"
)

func backgroundProcessAttributes() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

func backgroundStopCommand(pid int) string {
	return fmt.Sprintf("kill -- %d", pid)
}
