package sshnative

import (
	"context"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/internal/transport"
	"github.com/VikashLoomba/Portal/internal/transport/conformance"
)

// TestConformance runs the shared T7 suite against the in-process T6 server via
// the injection-seam Options (temp known_hosts + temp key + WithAgentSocket("")),
// so nothing touches the runner's real ~/.ssh. Describe().Impl == "native-ssh"
// (!= system-ssh), so the suite treats forwards as loopback-reachable. The
// Client now asserts PortForwarder (forward.go), so the PortForwarder section
// runs fully: the always-on lifecycle steps AND the loopback-guarded echo +
// ForwardLines round-trip (direct-tcpip through the T6 server's handler).
func TestConformance(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey())
	kh := writeKnownHosts(t, srv.knownHostsLine())
	keyFile := writeIdentityFile(t, clientPriv)

	conformance.Run(t, "sshnative", func(t *testing.T) transport.Transport {
		c, err := New(srv.target("testuser"),
			WithConfigResolver(passthroughResolver),
			WithKnownHostsPath(kh),
			WithIdentityFiles(keyFile),
			WithAgentSocket(""))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { c.Close(context.Background()) })
		return c
	})
}

// TestKnownHostsStrictFailure (EC6): a temp known_hosts holding the WRONG host
// key makes Ensure fail with an error naming the host and the `ssh <host>`
// remediation, and NO session is opened.
func TestKnownHostsStrictFailure(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey())
	keyFile := writeIdentityFile(t, clientPriv)

	// Write a valid known_hosts line for the server's address but with a
	// DIFFERENT (wrong) host key.
	wrongHostKey := generateSSHKey(t)
	wrongLine := lineFor(srv.addr, wrongHostKey)
	kh := writeKnownHosts(t, wrongLine)

	c, err := New(srv.target("testuser"),
		WithConfigResolver(passthroughResolver),
		WithKnownHostsPath(kh),
		WithIdentityFiles(keyFile),
		WithAgentSocket(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure: want host-key error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "127.0.0.1") {
		t.Errorf("error %q must name the host 127.0.0.1", msg)
	}
	if !strings.Contains(msg, "ssh 127.0.0.1") {
		t.Errorf("error %q must contain the `ssh <host>` remediation", msg)
	}
	// No session opened: the client is still down.
	h, herr := c.Health(context.Background())
	if herr != nil {
		t.Fatalf("Health: %v", herr)
	}
	if h.Up {
		t.Error("Health.Up = true after host-key failure, want false")
	}
	if _, _, execErr := c.Exec(context.Background(), nil, "true"); execErr == nil {
		t.Error("Exec after host-key failure: want error, got nil")
	}
}
