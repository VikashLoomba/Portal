package sshnative

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dialedTestClient starts an in-process T6 server, points a native Client at it
// via the injection-seam Options (temp-dir known_hosts + key, agent disabled) so
// nothing touches the runner's real ~/.ssh, Ensures it, and returns the connected
// client. It fails the test on any setup error.
func dialedTestClient(t *testing.T) *Client {
	t.Helper()
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
	t.Cleanup(func() { c.Close(context.Background()) })
	if _, err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return c
}

// TestExecHonorsContextCancel: a ctx deadline must interrupt a started Exec, not
// wait for the remote command. Without the session watcher, sess.Run blocks for
// the full remote sleep regardless of ctx. `sleep 30` makes an ignored-ctx bug a
// stark ~30s hang against the sub-second honored path.
func TestExecHonorsContextCancel(t *testing.T) {
	c := dialedTestClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := c.Exec(ctx, nil, "sleep", "30")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Exec: want error from a ctx-canceled session, got nil")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Exec ignored ctx: returned after %v (remote command was not interrupted)", elapsed)
	}
	if ctx.Err() == nil {
		t.Fatalf("ctx should be done; err=%v", err)
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Errorf("Exec error %q should surface the ctx deadline", err.Error())
	}
	// The connection itself survives cancellation (only the session was torn down):
	// a fresh Exec on a live ctx still works.
	out, _, err := c.Exec(context.Background(), nil, "echo", "alive")
	if err != nil || !strings.Contains(out, "alive") {
		t.Errorf("post-cancel Exec: out=%q err=%v (connection should survive)", out, err)
	}
}

// TestStreamHonorsContextCancel: a ctx cancel must unblock a Stream consumer
// waiting on a hung remote session, mirroring exec.CommandContext.
func TestStreamHonorsContextCancel(t *testing.T) {
	c := dialedTestClient(t)

	ctx, cancel := context.WithCancel(context.Background())
	_, _, _, wait, err := c.Stream(ctx, "sleep", "30")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Cancel shortly after start; wait must return promptly, not after 30s.
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	waitErr := wait()
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("Stream ignored ctx: wait returned after %v", elapsed)
	}
	if waitErr == nil {
		t.Fatal("wait: want the ctx error after cancel, got nil")
	}
	if ctx.Err() == nil {
		t.Fatal("ctx should be canceled")
	}
}

// TestKnownHostsMissingFailsClosed (finding 4): with NO known_hosts entry for the
// box (a nonexistent file — the default posture for a fresh dev box), Ensure must
// fail CLOSED with a "known_hosts" error rather than accept any host key, and no
// session must open. This pins the security-sensitive os.ErrNotExist branch of
// buildHostKeyCallback so a regression to accept-any-host cannot pass CI.
func TestKnownHostsMissingFailsClosed(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey())
	keyFile := writeIdentityFile(t, clientPriv)

	// A path that does not exist -> knownhosts.New returns os.ErrNotExist ->
	// the strict reject-all callback must be synthesized (fail-closed).
	missing := filepath.Join(t.TempDir(), "no-such-dir", "known_hosts")

	c, err := New(srv.target("testuser"),
		WithKnownHostsPath(missing),
		WithIdentityFiles(keyFile),
		WithAgentSocket(""))
	if err != nil {
		t.Fatalf("New: %v (a missing known_hosts must not fail construction)", err)
	}

	_, err = c.Ensure(context.Background())
	if err == nil {
		t.Fatal("Ensure: want a fail-closed host-key error when known_hosts is absent, got nil (accept-any-host hole)")
	}
	if !strings.Contains(err.Error(), "known_hosts") {
		t.Errorf("error %q must mention known_hosts", err.Error())
	}
	// No session opened: the client is still down.
	h, herr := c.Health(context.Background())
	if herr != nil {
		t.Fatalf("Health: %v", herr)
	}
	if h.Up {
		t.Error("Health.Up = true after missing-known_hosts failure, want false")
	}
	if _, _, execErr := c.Exec(context.Background(), nil, "true"); execErr == nil {
		t.Error("Exec after missing-known_hosts failure: want error, got nil")
	}
}

// TestEnsureRedialsDeadClient (finding 6 / T5): the keepalive marks a broken
// connection dead; the next Ensure must re-dial (rebuilt=true) and re-establish a
// working session rather than reuse the stale client. We force the dead state the
// keepalive would set on 3 strikes via the markDead seam, then assert re-dial.
func TestEnsureRedialsDeadClient(t *testing.T) {
	c := dialedTestClient(t)

	// Sanity: the live connection works.
	if _, _, err := c.Exec(context.Background(), nil, "true"); err != nil {
		t.Fatalf("pre-dead Exec: %v", err)
	}

	c.mu.Lock()
	prev := c.client
	c.mu.Unlock()

	// Mark the current client dead — exactly what startKeepaliveLocked does after
	// keepaliveStrikes consecutive failures.
	c.markDead(prev)

	h, _ := c.Health(context.Background())
	if h.Up {
		t.Fatal("Health.Up = true for a dead client, want false")
	}

	rebuilt, err := c.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure after dead: %v", err)
	}
	if !rebuilt {
		t.Fatal("Ensure must re-dial a dead client (rebuilt=true); a stale dead client would be reused forever")
	}

	c.mu.Lock()
	next := c.client
	c.mu.Unlock()
	if next == prev {
		t.Error("Ensure must build a NEW *ssh.Client on re-dial, got the same stale client")
	}

	// The re-dialed connection serves a fresh session.
	out, _, err := c.Exec(context.Background(), nil, "echo", "redialed")
	if err != nil || !strings.Contains(out, "redialed") {
		t.Errorf("post-redial Exec: out=%q err=%v", out, err)
	}
}

// TestKeepaliveMarksDeadOnConnectionDrop (finding 6 / T5): drives the REAL
// startKeepaliveLocked detection loop end-to-end — no markDead seam. With a fast
// keepalive cadence, severing the server's TCP connection makes the in-flight
// keepalive@openssh.com SendRequest fail; after keepaliveStrikes consecutive
// failures the goroutine must mark the client dead so Health.Up flips false and
// the next Ensure self-heals by re-dialing. A regression that never trips the
// strike threshold (e.g. `strikes >= limit` weakened, the markDead call dropped,
// or the goroutine panics/early-returns) leaves Health.Up=true forever and fails
// here — the exact silent-death hole T5 exists to close.
func TestKeepaliveMarksDeadOnConnectionDrop(t *testing.T) {
	clientPriv, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey())
	kh := writeKnownHosts(t, srv.knownHostsLine())
	keyFile := writeIdentityFile(t, clientPriv)

	// 10ms period / 3 strikes: real detection fires in ~30ms yet the loop is the
	// same code the 15s/3 production path runs.
	c, err := New(srv.target("testuser"),
		WithKnownHostsPath(kh),
		WithIdentityFiles(keyFile),
		WithAgentSocket(""),
		WithKeepalive(10*time.Millisecond, 3))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	if _, err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if h, _ := c.Health(context.Background()); !h.Up {
		t.Fatal("Health.Up = false right after dial, want true")
	}

	// Silent network death: the client is not told; only its keepalive can notice.
	srv.dropConns()

	// The detection loop must flip Health.Up to false within a bounded window
	// (period*strikes plus generous slack), with NO markDead call from the test.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if h, _ := c.Health(context.Background()); !h.Up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Health.Up stayed true after the connection dropped: keepalive never detected the dead connection (T5 self-heal broken)")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Self-heal: the next Ensure re-dials a fresh, working client.
	rebuilt, err := c.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure after keepalive death: %v", err)
	}
	if !rebuilt {
		t.Fatal("Ensure must re-dial after keepalive marked the client dead (rebuilt=true)")
	}
	out, _, err := c.Exec(context.Background(), nil, "echo", "healed")
	if err != nil || !strings.Contains(out, "healed") {
		t.Errorf("post-heal Exec: out=%q err=%v", out, err)
	}
}
