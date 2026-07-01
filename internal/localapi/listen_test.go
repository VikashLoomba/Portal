package localapi

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortTempDir returns a short-path temp dir (under /tmp, not the long macOS
// $TMPDIR) so unix socket paths stay under the ~104-byte sun_path limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "papi")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// unixClient returns an http.Client dialing the unix socket at path.
func unixClient(path string) *http.Client {
	return &http.Client{
		Timeout: time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}
}

// waitVersion blocks until GET /v1/version answers on path, or fails the test.
func waitVersion(t *testing.T, path string) {
	t.Helper()
	c := unixClient(path)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := c.Get("http://unix/v1/version")
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("version endpoint never came up")
}

// TestListenPermsAndSingleInstance covers EC5 (socket 0600 + parent dir 0700)
// and EC6 (a second Listen against a live responder fails).
func TestListenPermsAndSingleInstance(t *testing.T) {
	dir := shortTempDir(t)
	// Nest one level so MkdirAll+Chmod(0700) is actually exercised.
	path := filepath.Join(dir, "portal", "api.sock")

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if got := fi.Mode() & 0o777; got != 0o600 {
		t.Errorf("socket mode = %o, want 0600", got)
	}
	di, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat parent dir: %v", err)
	}
	if got := di.Mode() & 0o777; got != 0o700 {
		t.Errorf("parent dir mode = %o, want 0700", got)
	}

	s := New(Deps{Version: VersionInfo{Version: "test"}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	waitVersion(t, path)

	if _, err := Listen(path); err == nil {
		t.Fatal("second Listen against a live socket must fail (single-instance lock)")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
}

// TestListenStaleTakeover drives the probe→dial-failure→os.Remove branch. A
// plain net.Listen+Close would unlink the socket (unlinkOnClose defaults true)
// and leave nothing to take over, so we hand-craft a GENUINE stale entry with
// SetUnlinkOnClose(false): a socket file on disk with nothing accepting.
func TestListenStaleTakeover(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "api.sock")

	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected a stale socket file before takeover: %v", err)
	}

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen over stale file: %v", err)
	}
	if ln == nil {
		t.Fatal("Listen returned a nil listener")
	}
	defer ln.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket file missing after takeover: %v", err)
	}

	// Prove the takeover listener actually accepts (same-uid peer passes the
	// peer-cred gate).
	accepted := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Close()
		}
		accepted <- err
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial taken-over socket: %v", err)
	}
	c.Close()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("Accept on taken-over socket: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept on taken-over socket timed out")
	}
}
