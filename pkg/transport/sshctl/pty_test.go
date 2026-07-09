package sshctl_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/pkg/run"
	"github.com/VikashLoomba/Portal/pkg/transport"
	"github.com/VikashLoomba/Portal/pkg/transport/sshctl"
)

func TestStreamPtyPathShimTTYResizeAndArgs(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	sizeFile := filepath.Join(dir, "size")
	outFile := filepath.Join(dir, "out")
	t.Setenv("ARGS_FILE", argsFile)
	t.Setenv("SIZE_FILE", sizeFile)
	t.Setenv("OUT_FILE", outFile)

	installFakeSSH(t, `printf '%s\n' "$@" > "$ARGS_FILE"
# Install the trap before the SIZE_FILE sync point or Resize's single SIGWINCH can be lost.
trap 'stty size > "$OUT_FILE"' WINCH
stty size > "$SIZE_FILE" || exit 97
while :; do sleep 0.1; done
`)

	sock := filepath.Join(t.TempDir(), "cm.sock")
	s := sshctl.New(sock, "shimhost", []string{"-o", "BatchMode=yes"}, run.OSRunner{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := s.StreamPty(ctx, transport.PtyRequest{Rows: 24, Cols: 80}, "echo", "hello world")
	if err != nil {
		t.Fatalf("StreamPty: %v", err)
	}
	defer sess.Close()

	if got := waitForFileContent(t, sizeFile, "24 80", 5*time.Second); got != "24 80" {
		t.Fatalf("initial size = %q, want 24 80", got)
	}
	if err := sess.Resize(40, 100); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if got := waitForFileContent(t, outFile, "40 100", 5*time.Second); got != "40 100" {
		t.Fatalf("resized size = %q, want 40 100", got)
	}

	gotArgs := readLines(t, argsFile)
	wantArgs := []string{"-S", sock, "-o", "BatchMode=yes", "-tt", "shimhost", "echo", "hello world"}
	assertStringSlice(t, gotArgs, wantArgs)

	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = waitPty(t, sess, 5*time.Second)
}

func TestStreamPtyExitCodeMappingWithPathShim(t *testing.T) {
	tests := []struct {
		name string
		body string
		argv []string
		want int
	}{
		{
			name: "transport_failure_passthrough",
			body: "exit 255\n",
			argv: []string{"ignored"},
			want: 255,
		},
		{
			name: "remote_status",
			body: `while [ "$#" -gt 0 ]; do
	case "$1" in
		-S|-o|-p|-l|-i|-F|-J)
			shift 2
			;;
		-*)
			shift
			;;
		*)
			shift
			break
			;;
	esac
done
if [ "$#" -gt 0 ]; then
	trap '' TTOU TTIN TSTP 2>/dev/null || true
	sh -c "$*" </dev/null
fi
exit 7
`,
			argv: []string{"echo done"},
			want: 7,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installFakeSSH(t, tt.body)

			s := sshctl.New(filepath.Join(t.TempDir(), "cm.sock"), "shimhost", nil, run.OSRunner{})
			sess, err := s.StreamPty(context.Background(), transport.PtyRequest{Rows: 24, Cols: 80}, tt.argv...)
			if err != nil {
				t.Fatalf("StreamPty: %v", err)
			}
			defer sess.Close()
			drainDone := startPtyDiscard(sess)

			werr := waitPty(t, sess, 5*time.Second)
			waitForPtyDiscard(t, drainDone, 5*time.Second)
			code, ok := transport.ExitCode(werr)
			if !ok || code != tt.want {
				t.Fatalf("ExitCode(Wait err) = (%d, %v), want (%d, true); err=%v", code, ok, tt.want, werr)
			}
		})
	}
}

func TestStreamPtyContextCancelTearsDown(t *testing.T) {
	installFakeSSH(t, `stty size >/dev/null 2>&1 || exit 97
while :; do sleep 0.1; done
`)

	ctx, cancel := context.WithCancel(context.Background())
	s := sshctl.New(filepath.Join(t.TempDir(), "cm.sock"), "shimhost", nil, run.OSRunner{})
	sess, err := s.StreamPty(ctx, transport.PtyRequest{Rows: 24, Cols: 80}, "ignored")
	if err != nil {
		cancel()
		t.Fatalf("StreamPty: %v", err)
	}
	defer sess.Close()

	readDone := make(chan readResult, 1)
	go func() {
		var buf [1]byte
		n, err := sess.Read(buf[:])
		readDone <- readResult{n: n, err: err}
	}()

	waitDone := make(chan error, 1)
	go func() { waitDone <- sess.Wait() }()

	cancel()

	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after context cancellation")
	}

	select {
	case rr := <-readDone:
		if rr.err == nil {
			t.Fatalf("Read after context cancellation = (%d, nil), want EOF or error", rr.n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not unblock after context cancellation")
	}
}

func TestStreamPtyResizeAfterEnd(t *testing.T) {
	installFakeSSH(t, "exit 0\n")

	s := sshctl.New(filepath.Join(t.TempDir(), "cm.sock"), "shimhost", nil, run.OSRunner{})
	sess, err := s.StreamPty(context.Background(), transport.PtyRequest{Rows: 24, Cols: 80}, "ignored")
	if err != nil {
		t.Fatalf("StreamPty: %v", err)
	}
	defer sess.Close()

	if err := waitPty(t, sess, 5*time.Second); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	err = sess.Resize(40, 100)
	if err == nil {
		t.Fatal("Resize after Wait returned nil, want descriptive error")
	}
	if !strings.Contains(err.Error(), "after session ended") {
		t.Fatalf("Resize after Wait error = %q, want session-ended message", err.Error())
	}
}

type readResult struct {
	n   int
	err error
}

func installFakeSSH(t *testing.T, body string) {
	t.Helper()

	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "ssh")
	if err := os.WriteFile(shimPath, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write ssh shim: %v", err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func waitForFileContent(t *testing.T, path, want string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			last = normalizePTYText(string(b))
			if last == want {
				return last
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read %s: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to contain %q; last=%q", path, want, last)
	return ""
}

func readLines(t *testing.T, path string) []string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := strings.TrimSuffix(strings.ReplaceAll(string(b), "\r", ""), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func assertStringSlice(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %q, want %q", got, want)
		}
	}
}

func waitPty(t *testing.T, sess transport.PtySession, timeout time.Duration) error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = sess.Close()
		t.Fatalf("Wait did not return within %s", timeout)
		return nil
	}
}

func startPtyDiscard(sess transport.PtySession) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 256)
		for {
			if _, err := sess.Read(buf); err != nil {
				return
			}
		}
	}()
	return done
}

func waitForPtyDiscard(t *testing.T, done <-chan struct{}, timeout time.Duration) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("pty discard did not return within %s", timeout)
	}
}

func normalizePTYText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\r", ""))
}
