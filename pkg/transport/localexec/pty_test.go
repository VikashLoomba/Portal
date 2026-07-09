package localexec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/pkg/transport"
)

func TestStreamPtySttySize(t *testing.T) {
	sess, err := New().StreamPty(context.Background(), transport.PtyRequest{Rows: 40, Cols: 100}, "stty", "size")
	if err != nil {
		t.Fatalf("StreamPty: %v", err)
	}
	defer sess.Close()

	got := readLocalPtyUntil(t, sess, "40 100", 5*time.Second)
	if !strings.Contains(normalizeLocalPtyOutput(got), "40 100") {
		t.Fatalf("pty output = %q, want size 40 100", got)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestStreamPtyEmptyArgvShell(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("ENV", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := New().StreamPty(ctx, transport.PtyRequest{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("StreamPty shell: %v", err)
	}
	defer sess.Close()

	drain := startLocalPtyDrain(sess)
	waitDone := make(chan error, 1)
	go func() { waitDone <- sess.Wait() }()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()

	var lastWriteErr error
	writeExit := func() {
		_, lastWriteErr = sess.Write([]byte("exit\n"))
	}
	writeExit()

	for {
		select {
		case err := <-waitDone:
			if err != nil {
				t.Fatalf("Wait shell: %v; output=%q", err, drain.output())
			}
			return
		case <-ticker.C:
			writeExit()
		case <-deadline.C:
			_ = sess.Close()
			if lastWriteErr != nil {
				t.Fatalf("Wait shell did not return after exit; last write: %v; output=%q", lastWriteErr, drain.output())
			}
			t.Fatalf("Wait shell did not return after exit; output=%q", drain.output())
		}
	}
}

func TestStreamPtyResizeConcurrentWithWait(t *testing.T) {
	const (
		iterations = 50
		workers    = 4
	)
	for i := 0; i < iterations; i++ {
		sess, err := New().StreamPty(context.Background(), transport.PtyRequest{Rows: 24, Cols: 80}, "true")
		if err != nil {
			t.Fatalf("iteration %d StreamPty: %v", i, err)
		}

		start := make(chan struct{})
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var resizeErrs []error
		for worker := 0; worker < workers; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				<-start
				rows := uint16(24 + worker)
				cols := uint16(80 + worker)
				for {
					select {
					case <-stop:
						return
					default:
					}
					if err := sess.Resize(rows, cols); err != nil {
						mu.Lock()
						resizeErrs = append(resizeErrs, err)
						mu.Unlock()
					}
					runtime.Gosched()
				}
			}(worker)
		}

		waitDone := make(chan error, 1)
		close(start)
		go func() { waitDone <- sess.Wait() }()

		var waitErr error
		select {
		case waitErr = <-waitDone:
		case <-time.After(2 * time.Second):
			close(stop)
			_ = sess.Close()
			wg.Wait()
			t.Fatalf("iteration %d Wait did not return", i)
		}
		close(stop)
		wg.Wait()
		if waitErr != nil {
			t.Fatalf("iteration %d Wait: %v", i, waitErr)
		}

		mu.Lock()
		errs := append([]error(nil), resizeErrs...)
		mu.Unlock()
		for _, err := range errs {
			assertLocalResizeSessionError(t, err)
		}
		if err := sess.Resize(40, 100); err == nil || err.Error() != "localexec: resize pty after session ended" {
			t.Fatalf("iteration %d Resize after Wait = %v, want session-ended error", i, err)
		}
	}
}

func assertLocalResizeSessionError(t *testing.T, err error) {
	t.Helper()
	switch err.Error() {
	case "localexec: resize pty after session ended", "localexec: resize pty after session closed":
	default:
		t.Fatalf("Resize returned unsynchronized pty error %q, want session-ended or session-closed sentinel", err.Error())
	}
}

func readLocalPtyUntil(t *testing.T, sess transport.PtySession, want string, timeout time.Duration) string {
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
				if strings.Contains(normalizeLocalPtyOutput(out.String()), want) {
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

type localPtyDrain struct {
	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
}

func startLocalPtyDrain(sess transport.PtySession) *localPtyDrain {
	d := &localPtyDrain{done: make(chan struct{})}
	go func() {
		defer close(d.done)
		buf := make([]byte, 256)
		for {
			n, err := sess.Read(buf)
			if n > 0 {
				d.mu.Lock()
				d.out.Write(buf[:n])
				d.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return d
}

func (d *localPtyDrain) output() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return normalizeLocalPtyOutput(d.out.String())
}

func normalizeLocalPtyOutput(s string) string {
	return strings.ReplaceAll(s, "\r", "")
}
