//go:build !linux

package cli

import "net"

func listenLocalSocket(address string) (net.Listener, error) {
	return net.Listen(localHTTPNetwork, address)
}
