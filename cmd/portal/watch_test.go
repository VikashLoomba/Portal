package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/hub"
)

// syncBuffer is a mutex-guarded bytes.Buffer so the test goroutine polling the
// rendered output never races runStatusWatch's writes (assertions run under
// -race).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// EC5: status --watch renders on the initial snapshot, re-renders on a state
// event, and exits cleanly (nil) when the daemon shuts down.
func TestRunStatusWatch_RerenderThenShutdown(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	d := startFakeDaemon(t, cfg, withMasterPID(4242))
	a := newDaemonTestApp(t, d.path, cfg)

	var out syncBuffer
	var errb syncBuffer
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- runStatusWatch(ctx, &out, &errb, a) }()

	// The first line of GET /v1/events is always a populated snapshot, so the
	// first status block must appear on its own without any publish.
	waitCount(t, &out, "dev box:", 1, 3*time.Second)

	// A Coalesced publish yields a full-Status state event → a SECOND render.
	d.hub.Publish(hub.Event{Class: hub.Coalesced})
	waitCount(t, &out, "dev box:", 2, 3*time.Second)

	if errb.String() != "" {
		t.Errorf("unexpected stderr during watch: %q", errb.String())
	}

	// Shut the daemon down (cancel its Serve ctx). The u1 BaseContext fixup
	// cancels the /v1/events handler, the stream EOFs, and runStatusWatch exits
	// nil. Without that fixup this would hang.
	d.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runStatusWatch on shutdown = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runStatusWatch did not exit after daemon shutdown")
	}
}

// TestWatchLoop_DrainsFinalStateOnStreamEnd proves the errc case renders any
// state event still buffered on `events` before returning. The client enqueues
// every decoded line onto `events` and only THEN sends the terminal to `errc`,
// so at stream end BOTH a final state event and the terminal can be ready; a
// bare `return nil` on `errc` would let select drop that last render ~50% of the
// time. We reproduce that exact simultaneity (a state event pre-buffered on
// `events`, a terminal on `errc`) many times: with the drain, every iteration
// renders exactly once regardless of which ready case select picks; without it,
// some iteration renders zero times and the test fails.
func TestWatchLoop_DrainsFinalStateOnStreamEnd(t *testing.T) {
	cfg := newTestConfig(t, "devbox")
	a := newDaemonTestApp(t, filepath.Join(t.TempDir(), "unused.sock"), cfg)
	st := api.Status{Host: "devbox"}

	for i := 0; i < 200; i++ {
		events := make(chan api.Event, 16)
		events <- api.Event{Type: "state", Status: &st}
		errc := make(chan error, 1)
		errc <- nil // clean EOF terminal, ready simultaneously with the buffered state

		var out bytes.Buffer
		if err := watchLoop(context.Background(), &out, a, events, errc); err != nil {
			t.Fatalf("iter %d: watchLoop = %v, want nil", i, err)
		}
		if got := strings.Count(out.String(), "dev box:"); got != 1 {
			t.Fatalf("iter %d: rendered %d times, want exactly 1 — a final buffered state was dropped at stream end", i, got)
		}
	}
}

// waitCount polls buf until marker appears at least n times, or fails.
func waitCount(t *testing.T, buf *syncBuffer, marker string, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if strings.Count(buf.String(), marker) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("marker %q appeared %d times, want >= %d\n--- got ---\n%s",
				marker, strings.Count(buf.String(), marker), n, buf.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// EC2: a watch has nothing to watch when the daemon is down. A nonexistent, a
// dead (plain file), and a hung socket each print exactly the one-line
// "needs the running daemon" message to stderr, nothing to stdout, and return
// errSilent.
func TestRunStatusWatch_DaemonDown(t *testing.T) {
	const wantMsg = "portal status --watch needs the running daemon; run `portal status` instead\n"

	assertDown := func(t *testing.T, ctx context.Context, a *app.App) {
		t.Helper()
		var out, errb bytes.Buffer
		err := runStatusWatch(ctx, &out, &errb, a)
		if !errors.Is(err, errSilent) {
			t.Errorf("runStatusWatch = %v, want errSilent", err)
		}
		if out.Len() != 0 {
			t.Errorf("stdout must be empty when daemon is down, got %q", out.String())
		}
		if errb.String() != wantMsg {
			t.Errorf("stderr mismatch:\n--- got ---\n%s\n--- want ---\n%s", errb.String(), wantMsg)
		}
	}

	t.Run("nonexistent_socket", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		a := newDaemonTestApp(t, filepath.Join(t.TempDir(), "nope.sock"), cfg)
		assertDown(t, context.Background(), a)
	})

	t.Run("dead_plain_file", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		f := filepath.Join(t.TempDir(), "notasocket")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		a := newDaemonTestApp(t, f, cfg)
		assertDown(t, context.Background(), a)
	})

	t.Run("hung_listener", func(t *testing.T) {
		cfg := newTestConfig(t, "devbox")
		// A listener that never accepts: the dial succeeds but the request stalls,
		// so Available's ProbeTimeout fires and the watch reports the daemon down.
		dir, err := os.MkdirTemp("/tmp", "portal-hung-watch-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		sock := filepath.Join(dir, "api.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })

		a := newDaemonTestApp(t, sock, cfg)
		assertDown(t, context.Background(), a)
	})
}
