package sshnative

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// testServer is an in-process x/crypto/ssh SERVER on a random loopback port. It
// accepts publickey auth for a single test-generated key and runs each exec
// request LOCALLY via `sh -c` — matching the shell-join model the native client
// emits — piping stdout/stderr/exit-status back. Its direct-tcpip handler dials
// the requested host:port LOCALLY and copies bytes, so the native forward
// round-trip works fully in-process. It is _test-only (this file is never
// shipped).
type testServer struct {
	ln      net.Listener
	hostKey ssh.Signer
	addr    string // "127.0.0.1:<port>"

	// swallowGlobalRequests, when set, makes the server READ global requests
	// (e.g. keepalive@openssh.com) but NEVER reply — modeling a black-holed
	// half-open TCP link where packets are silently dropped and no RST arrives.
	// The TCP connection is left open, so a client that blocks on the reply
	// without a deadline never learns the peer is gone. Set before serving.
	swallowGlobalRequests bool

	mu    sync.Mutex
	conns []net.Conn // every accepted TCP conn, so dropConns can sever them
}

// serverOption mutates a testServer before it starts serving.
type serverOption func(*testServer)

// withSwallowGlobalRequests makes the server drain global requests without ever
// replying (black-hole), so a client keepalive with wantReply blocks until it
// hits its own reply deadline.
func withSwallowGlobalRequests() serverOption {
	return func(s *testServer) { s.swallowGlobalRequests = true }
}

// newTestServer starts a server whose publickey callback accepts exactly
// authorized. It generates a fresh host key and cleans up on test end.
func newTestServer(t *testing.T, authorized ssh.PublicKey, opts ...serverOption) *testServer {
	t.Helper()
	hostSigner := generateSSHKey(t)

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("unknown public key")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &testServer{ln: ln, hostKey: hostSigner, addr: ln.Addr().String()}
	for _, o := range opts {
		o(s)
	}
	go s.serve(cfg)
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *testServer) serve(cfg *ssh.ServerConfig) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn, cfg)
	}
}

// dropConns severs every accepted TCP connection, simulating a silent network
// death / server reboot so a client's in-flight keepalive@openssh.com SendRequest
// fails. The listener stays open so a subsequent Ensure can re-dial.
func (s *testServer) dropConns() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (s *testServer) handleConn(nConn net.Conn, cfg *ssh.ServerConfig) {
	s.mu.Lock()
	s.conns = append(s.conns, nConn)
	s.mu.Unlock()
	sconn, chans, reqs, err := ssh.NewServerConn(nConn, cfg)
	if err != nil {
		nConn.Close()
		return
	}
	defer sconn.Close()
	// Global requests include keepalive@openssh.com. Normally DiscardRequests
	// replies false (no error), which the client counts as a successful
	// keepalive. In swallow mode we drain the requests but NEVER reply, so a
	// client keepalive with wantReply blocks until its own reply deadline —
	// the black-holed half-open link the keepalive deadline exists to catch.
	if s.swallowGlobalRequests {
		go func() {
			for range reqs {
			}
		}()
	} else {
		go ssh.DiscardRequests(reqs)
	}
	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session":
			go s.handleSession(newChan)
		case "direct-tcpip":
			go s.handleDirectTCPIP(newChan)
		default:
			newChan.Reject(ssh.UnknownChannelType, "only session and direct-tcpip channels are supported")
		}
	}
}

// directTCPIPPayload is the RFC 4254 §7.2 direct-tcpip channel-open request:
// the host:port the client wants proxied plus its originator address.
type directTCPIPPayload struct {
	HostToConnect  string
	PortToConnect  uint32
	OriginatorHost string
	OriginatorPort uint32
}

// handleDirectTCPIP dials the requested host:port on THIS machine and copies
// bytes bidirectionally between the ssh channel and the local connection with
// TCP half-close semantics — exactly as a real ssh server does: a client FIN
// (channel EOF) is mapped to shutdown(SHUT_WR) on the remote conn while the
// reverse direction stays open, so a request/response service can still write
// its reply after reading the request to EOF. Both ends are fully closed only
// after BOTH copies finish. This makes the native client's direct-tcpip forwards
// round-trip faithfully in-process, including the half-close path.
func (s *testServer) handleDirectTCPIP(newChan ssh.NewChannel) {
	var p directTCPIPPayload
	if err := ssh.Unmarshal(newChan.ExtraData(), &p); err != nil {
		newChan.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
		return
	}
	dest := net.JoinHostPort(p.HostToConnect, strconv.Itoa(int(p.PortToConnect)))
	remote, err := net.Dial("tcp", dest)
	if err != nil {
		newChan.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := newChan.Accept()
	if err != nil {
		remote.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(ch, remote)
		ch.CloseWrite() // remote EOF -> channel EOF (client read side sees FIN)
	}()
	go func() {
		defer wg.Done()
		io.Copy(remote, ch)
		if cw, ok := remote.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite() // channel EOF -> remote SHUT_WR (half-close)
		}
	}()
	go func() {
		wg.Wait()
		ch.Close()
		remote.Close()
	}()
}

func (s *testServer) handleSession(newChan ssh.NewChannel) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	for req := range reqs {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
				req.Reply(false, nil)
				ch.Close()
				return
			}
			req.Reply(true, nil)
			s.runCommand(ch, payload.Command)
			return
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

// runCommand executes command via `sh -c` locally, wiring the channel as
// stdin/stdout and the extended-data stream as stderr, then reports exit-status.
func (s *testServer) runCommand(ch ssh.Channel, command string) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()

	var status uint32
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			status = uint32(ee.ExitCode())
		} else {
			status = 1
		}
	}
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	ch.Close()
}

// knownHostsLine renders the server's real host key as a known_hosts entry for
// its loopback address, for tests to write to a temp file.
func (s *testServer) knownHostsLine() string {
	return knownhosts.Line([]string{knownhosts.Normalize(s.addr)}, s.hostKey.PublicKey())
}

// target returns user@127.0.0.1:<port> for New.
func (s *testServer) target(user string) string { return user + "@" + s.addr }

// insecureIgnoreHostKey is a host-key callback that accepts any key. Used ONLY
// by unit tests that never dial (target-parse/Describe/no-dial), so no real
// verification is exercised.
var insecureIgnoreHostKey = ssh.InsecureIgnoreHostKey()

// lineFor renders a known_hosts entry binding addr to signer's public key —
// used to write a DELIBERATELY WRONG host key for the strict-failure test.
func lineFor(addr string, signer ssh.Signer) string {
	return knownhosts.Line([]string{knownhosts.Normalize(addr)}, signer.PublicKey())
}

// --- key/fixture helpers ---

// generateSSHKey returns a fresh ed25519 ssh.Signer.
func generateSSHKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer from key: %v", err)
	}
	return signer
}

// generateKeyPair returns a fresh ed25519 private key and its ssh.Signer.
func generateKeyPair(t *testing.T) (ed25519.PrivateKey, ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer from key: %v", err)
	}
	return priv, signer
}

// writeKnownHosts writes line to a temp known_hosts file and returns its path.
func writeKnownHosts(t *testing.T, line string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return path
}

// writeIdentityFile writes priv as an unencrypted OpenSSH PEM key to a temp
// file and returns its path.
func writeIdentityFile(t *testing.T, priv crypto.PrivateKey) string {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write identity file: %v", err)
	}
	return path
}

// writeEncryptedIdentityFile writes priv as a passphrase-encrypted OpenSSH PEM
// key to a temp file and returns its path.
func writeEncryptedIdentityFile(t *testing.T, priv crypto.PrivateKey) string {
	t.Helper()
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("hunter2"))
	if err != nil {
		t.Fatalf("marshal encrypted private key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write encrypted identity file: %v", err)
	}
	return path
}

// startFakeAgent serves an in-process ssh-agent holding priv over a temp unix
// socket (short path to stay under the OS sun_path limit) and returns the
// socket path.
func startFakeAgent(t *testing.T, priv crypto.PrivateKey) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "a")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen agent socket: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatalf("agent add key: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go agent.ServeAgent(keyring, conn)
		}
	}()
	return sock
}
