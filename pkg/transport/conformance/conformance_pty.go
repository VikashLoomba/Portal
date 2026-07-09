package conformance

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/pkg/transport"
)

func runPty(t *testing.T, newT func(*testing.T) transport.Transport) {
	ctx := context.Background()
	tr := newT(t)
	ps, ok := tr.(transport.PtyStreamer)
	if !ok {
		t.Skip("transport does not implement PtyStreamer")
	}

	t.Run("stty_size", func(t *testing.T) {
		sess := startPty(t, ctx, ps, transport.PtyRequest{Rows: 41, Cols: 101}, "stty", "size")
		defer sess.Close()

		got := readPtyUntil(t, sess, "41 101", 5*time.Second)
		if !strings.Contains(normalizePtyOutput(got), "41 101") {
			t.Fatalf("pty output = %q, want size 41 101", got)
		}
		if err := sess.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})

	t.Run("resize_observed", func(t *testing.T) {
		const resizePollScript = `stty size
read _
i=0
while [ "$i" -lt 100 ]; do
	s=$(stty size)
	echo "$s"
	if [ "$s" = "13 57" ]; then
		exit 0
	fi
	i=$((i + 1))
	sleep 0.05
done
exit 1`

		sess := startPty(t, ctx, ps, transport.PtyRequest{Rows: 32, Cols: 96},
			"sh", "-c", shellQuote(resizePollScript))
		defer sess.Close()

		first := readPtyUntil(t, sess, "32 96", 5*time.Second)
		if !strings.Contains(normalizePtyOutput(first), "32 96") {
			t.Fatalf("first pty output = %q, want size 32 96", first)
		}
		if err := sess.Resize(13, 57); err != nil {
			t.Fatalf("Resize: %v", err)
		}
		if _, err := sess.Write([]byte("\n")); err != nil {
			t.Fatalf("write newline: %v", err)
		}
		second := readPtyUntil(t, sess, "13 57", 5*time.Second)
		if !strings.Contains(normalizePtyOutput(second), "13 57") {
			t.Fatalf("second pty output = %q, want size 13 57", second)
		}
		if err := sess.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})

	t.Run("exit_code_fidelity", func(t *testing.T) {
		sess := startPty(t, ctx, ps, transport.PtyRequest{Rows: 24, Cols: 80},
			"sh", "-c", shellQuote("exit 5"))
		defer sess.Close()

		err := sess.Wait()
		code, ok := transport.ExitCode(err)
		if !ok || code != 5 {
			t.Fatalf("ExitCode(Wait) = (%d, %v), want (5, true); err=%v", code, ok, err)
		}
	})

	t.Run("merged_output", func(t *testing.T) {
		sess := startPty(t, ctx, ps, transport.PtyRequest{Rows: 24, Cols: 80},
			"sh", "-c", shellQuote("echo out; echo err >&2"))
		defer sess.Close()

		got := readPtyUntilAll(t, sess, []string{"out", "err"}, 5*time.Second)
		normalized := normalizePtyOutput(got)
		if !strings.Contains(normalized, "out") || !strings.Contains(normalized, "err") {
			t.Fatalf("pty output = %q, want merged stdout and stderr", got)
		}
		if err := sess.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
	})

	t.Run("close_teardown_no_goroutine_leak", func(t *testing.T) {
		if _, err := tr.Ensure(ctx); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		baseline := runtime.NumGoroutine()
		sess := startPty(t, ctx, ps, transport.PtyRequest{Rows: 24, Cols: 80}, "cat")

		if err := sess.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		_ = sess.Wait()
		settleGoroutines(t, baseline, 6, 5*time.Second)
	})
}

func startPty(t *testing.T, ctx context.Context, ps transport.PtyStreamer, req transport.PtyRequest, argv ...string) transport.PtySession {
	t.Helper()
	sess, err := ps.StreamPty(ctx, req, argv...)
	if err != nil {
		t.Fatalf("StreamPty(%v): %v", argv, err)
	}
	return sess
}

func readPtyUntil(t *testing.T, sess transport.PtySession, want string, timeout time.Duration) string {
	t.Helper()
	return readPtyUntilAll(t, sess, []string{want}, timeout)
}

func readPtyUntilAll(t *testing.T, sess transport.PtySession, wants []string, timeout time.Duration) string {
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
				normalized := normalizePtyOutput(out.String())
				all := true
				for _, want := range wants {
					if !strings.Contains(normalized, want) {
						all = false
						break
					}
				}
				if all {
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
			t.Fatalf("%v before %q; output=%q", got.err, wants, got.out)
		}
		return got.out
	case <-time.After(timeout):
		_ = sess.Close()
		t.Fatalf("timed out waiting for %q from pty", wants)
		return ""
	}
}

func normalizePtyOutput(s string) string {
	return strings.ReplaceAll(s, "\r", "")
}

func settleGoroutines(t *testing.T, baseline, tolerance int, timeout time.Duration) {
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
