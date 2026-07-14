//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package background

import (
	"errors"
	"os"
	"syscall"
)

func processIsAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
