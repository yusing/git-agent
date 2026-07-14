//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package cli

import (
	"fmt"
	"runtime"
	"syscall"
)

func backgroundProcessAttributes() *syscall.SysProcAttr {
	return nil
}

func backgroundStopCommand(pid int) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("taskkill /PID %d /T /F", pid)
	}
	return fmt.Sprintf("kill %d", pid)
}
