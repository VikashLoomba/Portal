package sshnative

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuthAgentPath (EC9): a fake in-process ssh-agent over a temp SSH_AUTH_SOCK
// (injected via WithAgentSocket) authenticates to the server. Identity files
// point at a nonexistent path so authentication can ONLY have come from the
// agent.
func TestAuthAgentPath(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey())
	kh := writeKnownHosts(t, srv.knownHostsLine())
	sock := startFakeAgent(t, clientPriv)
	missingKey := filepath.Join(t.TempDir(), "does-not-exist")

	c, err := New(srv.target("testuser"),
		WithKnownHostsPath(kh),
		WithAgentSocket(sock),
		WithIdentityFiles(missingKey))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close(context.Background())

	rebuilt, err := c.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure via agent: %v", err)
	}
	if !rebuilt {
		t.Error("Ensure rebuilt = false, want true on first dial")
	}
	stdout, _, err := c.Exec(context.Background(), []byte("agent-ok\n"), "cat")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if stdout != "agent-ok\n" {
		t.Errorf("stdout = %q, want %q", stdout, "agent-ok\n")
	}
}

// TestAuthIdentityFilePath (EC9): a temp unencrypted id_ed25519 (injected via
// WithIdentityFiles, with WithAgentSocket("") to force the key path)
// authenticates to the server.
func TestAuthIdentityFilePath(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey())
	kh := writeKnownHosts(t, srv.knownHostsLine())
	keyFile := writeIdentityFile(t, clientPriv)

	c, err := New(srv.target("testuser"),
		WithKnownHostsPath(kh),
		WithIdentityFiles(keyFile),
		WithAgentSocket(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close(context.Background())

	if _, err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure via identity file: %v", err)
	}
	stdout, _, err := c.Exec(context.Background(), []byte("key-ok\n"), "cat")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if stdout != "key-ok\n" {
		t.Errorf("stdout = %q, want %q", stdout, "key-ok\n")
	}
}

// TestAuthEncryptedKey (EC9): an encrypted identity file yields a CLEAR error
// naming the workaround (decrypt / add to ssh-agent) rather than prompting.
func TestAuthEncryptedKey(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey())
	kh := writeKnownHosts(t, srv.knownHostsLine())
	encKey := writeEncryptedIdentityFile(t, clientPriv)

	c, err := New(srv.target("testuser"),
		WithKnownHostsPath(kh),
		WithIdentityFiles(encKey),
		WithAgentSocket(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure with encrypted key: want error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "encrypted") {
		t.Errorf("error %q must state the key is encrypted", msg)
	}
	if !strings.Contains(msg, "ssh-agent") && !strings.Contains(msg, "ssh-add") {
		t.Errorf("error %q must name the ssh-agent/ssh-add workaround", msg)
	}
	if !strings.Contains(msg, encKey) {
		t.Errorf("error %q must name the offending key path %q", msg, encKey)
	}
}

// TestAuthNoCredentials (EC9): WithAgentSocket("") + WithIdentityFiles pointing
// at a nonexistent path yields an actionable error naming the tried paths.
func TestAuthNoCredentials(t *testing.T) {
	_, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey())
	kh := writeKnownHosts(t, srv.knownHostsLine())
	missingKey := filepath.Join(t.TempDir(), "does-not-exist")

	c, err := New(srv.target("testuser"),
		WithKnownHostsPath(kh),
		WithIdentityFiles(missingKey),
		WithAgentSocket(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure with no credentials: want error, got nil")
	}
	if !strings.Contains(err.Error(), missingKey) {
		t.Errorf("error %q must name the tried identity path %q", err.Error(), missingKey)
	}
}
