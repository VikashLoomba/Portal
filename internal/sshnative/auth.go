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
	return buildAuthMethods(c.agentSocket, c.identityFiles, c.user, c.host)
}

// hopAuthMethods builds a ProxyJump hop's auth from ITS OWN resolved
// IdentityFiles (stat-filtered, mirroring New's target logic) so per-hop
// `IdentityFile` ssh_config fidelity is honored: a hop that resolves usable
// identity files offers exactly those, matching OpenSSH; a hop that resolves
// none falls back to the client's identity files (the target's resolved list or
// the ~/.ssh defaults). The agent socket is shared across the chain. The "no
// usable credentials" error names the HOP (rh.User@rh.HostName), not the target,
// so a failure identifies which hop lacked a key.
func (c *Client) hopAuthMethods(rh ResolvedHost) ([]ssh.AuthMethod, net.Conn, error) {
	ids := statFilterIdentityFiles(rh.IdentityFiles)
	if len(ids) == 0 {
		ids = c.identityFiles
	}
	return buildAuthMethods(c.agentSocket, ids, rh.User, rh.HostName)
}

// statFilterIdentityFiles returns, in order, the paths that exist on disk. New
// and hopAuthMethods both use it so a resolved IdentityFile that is not present
// is dropped rather than offered.
func statFilterIdentityFiles(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// buildAuthMethods builds the ssh auth methods for a specific agent socket and
// identity-file list; user/host label the "no usable credentials" error only.
// See authMethods for the ordering and encrypted-key contract.
func buildAuthMethods(agentSocket string, identityFiles []string, user, host string) ([]ssh.AuthMethod, net.Conn, error) {
	var methods []ssh.AuthMethod
	var agentConn net.Conn
	var tried []string

	if agentSocket != "" {
		tried = append(tried, "agent socket "+agentSocket)
		conn, err := net.Dial("unix", agentSocket)
		if err == nil {
			agentConn = conn
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
		// An unreachable agent is not fatal: fall through to identity files.
	}

	var signers []ssh.Signer
	var encrypted []string // identity files skipped solely because they are passphrase-encrypted
	for _, path := range identityFiles {
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
			return nil, nil, fmt.Errorf("sshnative: no usable ssh credentials for %s@%s; identity file %s is passphrase-encrypted; decrypt it or add it to ssh-agent (ssh-add %s) — sshnative does not prompt for passphrases", user, host, strings.Join(encrypted, ", "), strings.Join(encrypted, ", "))
		}
		return nil, nil, fmt.Errorf("sshnative: no usable ssh credentials for %s@%s; tried %s", user, host, strings.Join(tried, ", "))
	}
	return methods, agentConn, nil
}
