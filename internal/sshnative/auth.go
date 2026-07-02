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
// An ENCRYPTED key is treated as an UNUSABLE candidate, not a fatal error: it
// is skipped so any other method (agent or an unencrypted key) can still
// authenticate. Only when NO usable credential is found at all does the failure
// surface, and then the error names the exact agent-socket and identity-file
// paths that were tried plus the workaround (decrypt or add to ssh-agent) for
// any key that was skipped solely because it is passphrase-encrypted. This
// never prompts for passphrases.
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
	var encrypted []string // identity files skipped solely because they are passphrase-encrypted
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
				// An encrypted key is an unusable candidate, not a fatal
				// error: record it and keep going so the agent (or an
				// unencrypted key) can still authenticate. The guidance is
				// surfaced below only if nothing else works.
				encrypted = append(encrypted, path)
			}
			continue // encrypted or otherwise unparseable: skip this candidate
		}
		signers = append(signers, signer)
	}
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}

	if len(methods) == 0 {
		if agentConn != nil {
			agentConn.Close()
		}
		if len(encrypted) > 0 {
			// No agent and no unencrypted key: the encrypted key(s) are the
			// only credentials present, so name the workaround.
			return nil, nil, fmt.Errorf("sshnative: no usable ssh credentials for %s@%s; identity file %s is passphrase-encrypted; decrypt it or add it to ssh-agent (ssh-add %s) — sshnative does not prompt for passphrases", c.user, c.host, strings.Join(encrypted, ", "), strings.Join(encrypted, ", "))
		}
		return nil, nil, fmt.Errorf("sshnative: no usable ssh credentials for %s@%s; tried %s", c.user, c.host, strings.Join(tried, ", "))
	}
	return methods, agentConn, nil
}
