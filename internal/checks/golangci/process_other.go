//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package golangci

import (
	"fmt"
	"os"
	"syscall"
)

func checkerProcessAttributes() *syscall.SysProcAttr {
	return nil
}

func killCheckerProcess(process *os.Process) error {
	return fmt.Errorf("checker process-group cancellation is unsupported on this platform (pid %d)", process.Pid)
}
