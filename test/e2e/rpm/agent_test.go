//go:build e2e

package e2e

import (
	"net"
	"os"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// startAgent serves an in-process ssh-agent on a unix socket, holding the given
// private key. The CLI subprocess authenticates to the test sshd through it via
// SSH_AUTH_SOCK. It returns a stop function. This avoids depending on the
// ssh-agent binary and on mutating the test process's own agent.
func startAgent(t *testing.T, sockPath, keyFile string) func() {
	t.Helper()
	pem, err := os.ReadFile(keyFile)
	must(t, err)
	key, err := ssh.ParseRawPrivateKey(pem)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: key}); err != nil {
		t.Fatalf("add key to agent: %v", err)
	}

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen agent socket: %v", err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
					return
				}
			}
			go agent.ServeAgent(keyring, conn)
		}
	}()
	return func() {
		close(done)
		ln.Close()
	}
}
