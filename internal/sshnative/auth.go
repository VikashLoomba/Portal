package sshnative

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// authMethods builds the ssh auth methods from the RESOLVED config in order:
//
//  1. the ssh-agent at the resolved agent socket (skipped when the resolved
//     value is empty — WithAgentSocket("") disables it);
//  2. unencrypted keys from the resolved identity-file list, via
//     ssh.ParsePrivateKey.
//
// An ENCRYPTED key yields a CLEAR error naming the workaround (decrypt or add
// to ssh-agent) rather than prompting. When no usable credential is found at
// all, it returns an actionable error naming the exact agent-socket and
// identity-file paths that were tried.
//
// The returned agentConn (nil unless the agent method was added) is held open
// for the client's lifetime by the caller — the agent signer callback dials it
// lazily during the handshake — and closed on redial/Close.
func (c *Client) authMethods() ([]ssh.AuthMethod, net.Conn, error) {
	var methods []ssh.AuthMethod
	var agentConn net.Conn
	var tried []string

	if c.agentSocket != "" {
		tried = append(tried, "agent socket "+c.agentSocket)
		conn, err := net.Dial("unix", c.agentSocket)
		if err == nil {
			agentConn = conn
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
		// An unreachable agent is not fatal: fall through to identity files.
	}

	var signers []ssh.Signer
	for _, path := range c.identityFiles {
		tried = append(tried, "identity file "+path)
		data, err := os.ReadFile(path)
		if err != nil {
			continue // missing/unreadable key: skip, try the next candidate
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			var pmErr *ssh.PassphraseMissingError
			if errors.As(err, &pmErr) {
				if agentConn != nil {
					agentConn.Close()
				}
				return nil, nil, fmt.Errorf("sshnative: identity file %s is passphrase-encrypted; decrypt it or add it to ssh-agent (ssh-add %s) — sshnative does not prompt for passphrases", path, path)
			}
			continue // some other parse failure: skip this candidate
		}
		signers = append(signers, signer)
	}
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}

	if len(methods) == 0 {
		return nil, nil, fmt.Errorf("sshnative: no usable ssh credentials for %s@%s; tried %s", c.user, c.host, strings.Join(tried, ", "))
	}
	return methods, agentConn, nil
}
