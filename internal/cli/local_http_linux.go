package cli

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// listenLocalSocket creates the listener directly because net.Listen applies
// SO_REUSEADDR to stream listeners, including AF_UNIX. Restricted sandboxes can
// allow local sockets while intentionally denying that unrelated option.
func listenLocalSocket(address string) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()
	if err := unix.Bind(fd, &unix.SockaddrUnix{Name: address}); err != nil {
		return nil, err
	}
	if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), address)
	listener, err := net.FileListener(file)
	fileErr := file.Close()
	closeFD = false
	if err != nil {
		return nil, err
	}
	if fileErr != nil {
		_ = listener.Close()
		return nil, fileErr
	}
	return listener, nil
}
