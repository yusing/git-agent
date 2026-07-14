//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package cli

import "syscall"

func detachedProcessAttributes() *syscall.SysProcAttr {
	return nil
}
