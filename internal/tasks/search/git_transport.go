package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport"
	gitssh "github.com/go-git/go-git/v6/plumbing/transport/ssh"
	gossh "golang.org/x/crypto/ssh"
)

type defaultSSHAuth struct{}

func (defaultSSHAuth) ClientConfig(ctx context.Context, request *transport.Request) (*gossh.ClientConfig, error) {
	username := gitssh.DefaultUsername
	if request.URL.User != nil && request.URL.User.Username() != "" {
		username = request.URL.User.Username()
	}

	signers, encryptedKey := defaultSSHSigners()
	agent, agentErr := gitssh.NewSSHAgentAuth(username)
	if agent == nil && len(signers) == 0 {
		if encryptedKey {
			return nil, fmt.Errorf("SSH authentication unavailable: no usable SSH agent and default private keys require a passphrase: %w", agentErr)
		}
		return nil, fmt.Errorf("SSH authentication unavailable: no usable SSH agent or default private key: %w", agentErr)
	}

	auth := &gitssh.PublicKeysCallback{
		User: username,
		Callback: func() ([]gossh.Signer, error) {
			if agent == nil {
				return append([]gossh.Signer(nil), signers...), nil
			}
			agentSigners, err := agent.Callback()
			if err != nil {
				if len(signers) == 0 {
					return nil, err
				}
				return append([]gossh.Signer(nil), signers...), nil
			}
			return append(agentSigners, signers...), nil
		},
	}
	return auth.ClientConfig(ctx, request)
}

func defaultSSHSigners() (signers []gossh.Signer, encryptedKey bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, false
	}
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa", "id_dsa"} {
		data, err := os.ReadFile(filepath.Join(home, ".ssh", name))
		if err != nil {
			continue
		}
		signer, err := gossh.ParsePrivateKey(data)
		if _, ok := errors.AsType[*gossh.PassphraseMissingError](err); ok {
			encryptedKey = true
			continue
		}
		if err == nil {
			signers = append(signers, signer)
		}
	}
	return signers, encryptedKey
}

func remoteClientOptions() []client.Option {
	return []client.Option{client.WithSSHAuth(defaultSSHAuth{})}
}
