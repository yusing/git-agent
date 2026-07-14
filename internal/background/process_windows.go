//go:build windows

package background

import "syscall"

const stillActive = 259

func processIsAlive(pid int) bool {
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var code uint32
	return syscall.GetExitCodeProcess(handle, &code) == nil && code == stillActive
}

func syncDirectory(string) error {
	return nil
}
