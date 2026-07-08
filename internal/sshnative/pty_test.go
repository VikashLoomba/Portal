package sshnative

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/transport"
)

func TestStreamPtySttySize(t *testing.T) {
	c := dialedTestClient(t)
	sess, err := c.StreamPty(context.Background(), transport.PtyRequest{Rows: 40, Cols: 100}, "stty", "size")
	if err != nil {
		t.Fatalf("StreamPty: %v", err)
	}
	defer sess.Close()

	got := readNativePtyUntil(t, sess, "40 100", 5*time.Second)
	if !strings.Contains(normalizeNativePtyOutput(got), "40 100") {
		t.Fatalf("pty output = %q, want size 40 100", got)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestStreamPtyRegistrationDeregistration(t *testing.T) {
	c := dialedTestClient(t)

	sess, err := c.StreamPty(context.Background(), transport.PtyRequest{Rows: 24, Cols: 80}, "stty", "size")
	if err != nil {
		t.Fatalf("StreamPty stty: %v", err)
	}
	if got := c.ptySessionCount(); got != 1 {
		t.Fatalf("registered pty sessions = %d, want 1", got)
	}
	_ = readNativePtyUntil(t, sess, "24 80", 5*time.Second)
	if err := sess.Wait(); err != nil {
		t.Fatalf("Wait stty: %v", err)
	}
	if got := c.ptySessionCount(); got != 0 {
		t.Fatalf("registered pty sessions after Wait = %d, want 0", got)
	}

	sess, err = c.StreamPty(context.Background(), transport.PtyRequest{Rows: 24, Cols: 80}, "sleep", "300")
	if err != nil {
		t.Fatalf("StreamPty sleep: %v", err)
	}
	if got := c.ptySessionCount(); got != 1 {
		t.Fatalf("registered pty sessions = %d, want 1", got)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close sleep: %v", err)
	}
	if got := c.ptySessionCount(); got != 0 {
		t.Fatalf("registered pty sessions after Close = %d, want 0", got)
	}
	_ = sess.Wait()
}

func TestStreamPtyDeadClientPromptClose(t *testing.T) {
	c := dialedPtyClient(t, []serverOption{withSwallowGlobalRequests()}, WithKeepalive(50*time.Millisecond, 1))
	sess, err := c.StreamPty(context.Background(), transport.PtyRequest{Rows: 24, Cols: 80}, "sleep", "300")
	if err != nil {
		t.Fatalf("StreamPty: %v", err)
	}
	defer sess.Close()

	readDone := make(chan error, 1)
	go func() {
		var buf [1]byte
		_, err := sess.Read(buf[:])
		readDone <- err
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- sess.Wait() }()

	waitForNativeDead(t, c, 5*time.Second)
	deadline := time.After(5 * time.Second)
	for readDone != nil || waitDone != nil {
		select {
		case <-readDone:
			readDone = nil
		case <-waitDone:
			waitDone = nil
		case <-deadline:
			t.Fatal("pty Read/Wait did not return promptly after keepalive marked the client dead")
		}
	}
}

func TestStreamPtyCloseWithoutWaitDoesNotLeak(t *testing.T) {
	c := dialedTestClient(t)
	if _, err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	baseline := runtime.NumGoroutine()

	sess, err := c.StreamPty(context.Background(), transport.PtyRequest{Rows: 24, Cols: 80}, "sleep", "300")
	if err != nil {
		t.Fatalf("StreamPty: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	settleNativeGoroutines(t, baseline, 8, 5*time.Second)
}

func dialedPtyClient(t *testing.T, serverOpts []serverOption, clientOpts ...Option) *Client {
	t.Helper()
	clientPriv, clientSigner := generateKeyPair(t)
	srv := newTestServer(t, clientSigner.PublicKey(), serverOpts...)
	kh := writeKnownHosts(t, srv.knownHostsLine())
	keyFile := writeIdentityFile(t, clientPriv)

	opts := []Option{
		WithConfigResolver(passthroughResolver),
		WithKnownHostsPath(kh),
		WithIdentityFiles(keyFile),
		WithAgentSocket(""),
	}
	opts = append(opts, clientOpts...)
	c, err := New(srv.target("testuser"), opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	if _, err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return c
}

func readNativePtyUntil(t *testing.T, sess transport.PtySession, want string, timeout time.Duration) string {
	t.Helper()

	type readResult struct {
		out string
		err error
	}
	result := make(chan readResult, 1)
	go func() {
		var out bytes.Buffer
		buf := make([]byte, 256)
		for {
			n, err := sess.Read(buf)
			if n > 0 {
				out.Write(buf[:n])
				if strings.Contains(normalizeNativePtyOutput(out.String()), want) {
					result <- readResult{out: out.String()}
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					result <- readResult{out: out.String(), err: err}
					return
				}
				result <- readResult{out: out.String(), err: fmt.Errorf("read pty: %w", err)}
				return
			}
		}
	}()

	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("%v before %q; output=%q", got.err, want, got.out)
		}
		return got.out
	case <-time.After(timeout):
		_ = sess.Close()
		t.Fatalf("timed out waiting for %q from pty", want)
		return ""
	}
}

func normalizeNativePtyOutput(s string) string {
	return strings.ReplaceAll(s, "\r", "")
}

func waitForNativeDead(t *testing.T, c *Client, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h, err := c.Health(context.Background())
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if !h.Up {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("client was not marked dead before deadline")
}

func settleNativeGoroutines(t *testing.T, baseline, tolerance int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+tolerance {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines = %d, want <= %d after settle", runtime.NumGoroutine(), baseline+tolerance)
}
