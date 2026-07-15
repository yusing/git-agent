package cli

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync"
)

const localHTTPNetwork = "unix"

type localHTTPEndpoint struct {
	Network string `json:"network"`
	Address string `json:"address"`
	URL     string `json:"url"`
}

type localHTTPListener struct {
	net.Listener
	dir      string
	close    sync.Once
	closeErr error
}

func listenLocalHTTP(path string, query url.Values) (*localHTTPListener, localHTTPEndpoint, error) {
	if path == "" || path[0] != '/' {
		return nil, localHTTPEndpoint{}, errors.New("local HTTP path must be absolute")
	}
	dir, err := os.MkdirTemp("", "git-agent-")
	if err != nil {
		return nil, localHTTPEndpoint{}, fmt.Errorf("create local HTTP runtime directory: %w", err)
	}
	address := filepath.Join(dir, "http.sock")
	listener, err := listenLocalSocket(address)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, localHTTPEndpoint{}, fmt.Errorf("listen on local HTTP socket: %w", err)
	}
	local := &localHTTPListener{Listener: listener, dir: dir}
	if err := os.Chmod(address, 0o600); err != nil {
		_ = local.Close()
		return nil, localHTTPEndpoint{}, fmt.Errorf("secure local HTTP socket: %w", err)
	}
	requestURL := url.URL{Scheme: "http", Host: "localhost", Path: path, RawQuery: query.Encode()}
	return local, localHTTPEndpoint{
		Network: localHTTPNetwork,
		Address: address,
		URL:     requestURL.String(),
	}, nil
}

func (l *localHTTPListener) Close() error {
	l.close.Do(func() {
		listenerErr := l.Listener.Close()
		if errors.Is(listenerErr, net.ErrClosed) {
			listenerErr = nil
		}
		l.closeErr = errors.Join(listenerErr, os.RemoveAll(l.dir))
	})
	return l.closeErr
}
