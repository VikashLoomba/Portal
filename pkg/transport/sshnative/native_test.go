package sshnative

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestDescribe pins the Desc shape; Impl is always native-ssh and Pid semantics
// (Health) are covered by the conformance suite. The passthrough resolver splits
// the literal target verbatim, so the resolved endpoint equals the input.
func TestDescribe(t *testing.T) {
	c, err := New("alice@box:2222",
		WithConfigResolver(passthroughResolver),
		WithHostKeyCallback(insecureIgnoreHostKey))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d := c.Describe()
	if d.Impl != "native-ssh" {
		t.Errorf("Impl = %q, want native-ssh", d.Impl)
	}
	if d.Host != "box" {
		t.Errorf("Host = %q, want box", d.Host)
	}
	if d.Endpoint != "alice@box:2222" {
		t.Errorf("Endpoint = %q, want alice@box:2222", d.Endpoint)
	}
}

// TestNewNoDial proves New does not dial: constructing a Client for an
// unreachable target succeeds and reports Health.Up == false.
func TestNewNoDial(t *testing.T) {
	c, err := New("alice@203.0.113.1:22",
		WithConfigResolver(passthroughResolver),
		WithHostKeyCallback(insecureIgnoreHostKey))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Up {
		t.Error("Health.Up = true before Ensure, want false")
	}
	if h.Pid != 0 {
		t.Errorf("Health.Pid = %d, want 0 (native has no pid)", h.Pid)
	}
}

func TestEnsureDirectHandshakeHonorsContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	target := fmt.Sprintf("user@%s", ln.Addr().String())
	privateKey, _ := generateKeyPair(t)
	keyFile := writeIdentityFile(t, privateKey)
	c, err := New(target,
		WithConfigResolver(passthroughResolver),
		WithHostKeyCallback(insecureIgnoreHostKey),
		WithAgentSocket(""),
		WithIdentityFiles(keyFile))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = c.Ensure(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ensure error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Ensure returned after %s, want prompt context cancellation", elapsed)
	}

	select {
	case conn := <-accepted:
		conn.Close()
	case <-time.After(time.Second):
		t.Fatal("direct dial was not accepted")
	}
	done := make(chan struct{})
	go func() {
		_, _ = c.Close(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked behind canceled Ensure")
	}
}
