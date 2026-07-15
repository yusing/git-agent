package cli

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func localHTTPTestClient(t *testing.T, endpoint localHTTPEndpoint) *http.Client {
	t.Helper()
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, endpoint.Network, endpoint.Address)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func TestLocalHTTPListenerUsesPrivateSocketAndCleansUp(t *testing.T) {
	listener, endpoint, err := listenLocalHTTP("/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(endpoint.Address)
	if endpoint.Network != localHTTPNetwork || !filepath.IsAbs(endpoint.Address) || endpoint.URL != "http://localhost/status" {
		t.Fatalf("endpoint = %#v", endpoint)
	}
	info, err := os.Stat(endpoint.Address)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("mode = %v, want socket", info.Mode())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("socket permissions = %o", info.Mode().Perm())
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("runtime directory remains: %v", err)
	}
}
