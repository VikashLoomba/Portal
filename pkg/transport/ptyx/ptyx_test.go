package ptyx

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStartEchoRoundTrip(t *testing.T) {
	cmd := exec.Command("cat")
	master := startPTY(t, cmd, 24, 80)
	defer closeMasterAndWait(t, master, cmd)

	if _, err := master.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write to pty master: %v", err)
	}

	got := readUntilCount(t, master, []byte("ping"), 2, 2*time.Second)
	if count := bytes.Count(got, []byte("ping")); count < 2 {
		t.Fatalf("pty output %q contains %d ping occurrences, want at least 2", got, count)
	}
}

func TestStartChildSeesTTY(t *testing.T) {
	cmd := exec.Command("sh", "-c", "test -t 0 && echo TTY")
	master := startPTY(t, cmd, 24, 80)
	defer closeMasterAndWait(t, master, cmd)

	got := readUntil(t, master, []byte("TTY"), 2*time.Second)
	if !bytes.Contains(got, []byte("TTY")) {
		t.Fatalf("pty output %q does not show child stdin as a tty", got)
	}
}

func TestWinsizeSetGet(t *testing.T) {
	cmd := exec.Command("sh", "-c", "stty size")
	master := startPTY(t, cmd, 40, 100)
	got := readUntil(t, master, []byte("40 100"), 2*time.Second)
	closeMasterAndWait(t, master, cmd)

	if !bytes.Contains(got, []byte("40 100")) {
		t.Fatalf("stty size output %q, want 40 100", got)
	}

	cmd = exec.Command("sh", "-c", "sleep 300")
	master = startPTY(t, cmd, 24, 80)
	defer closeMasterAndWait(t, master, cmd)

	if err := Setsize(master, 50, 120); err != nil {
		t.Fatalf("Setsize: %v", err)
	}
	rows, cols, err := Getsize(master)
	if err != nil {
		t.Fatalf("Getsize: %v", err)
	}
	if rows != 50 || cols != 120 {
		t.Fatalf("Getsize = (%d, %d), want (50, 120)", rows, cols)
	}
}

func TestMasterCloseSendsSIGHUP(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	cmd := exec.Command("sh", "-c", `echo $$ > "$1"; exec sleep 300`, "sh", pidfile)
	master := startPTY(t, cmd, 24, 80)

	pid := waitForPIDFile(t, pidfile)
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	if err := master.Close(); err != nil {
		_ = cmd.Process.Kill()
		<-waitDone
		t.Fatalf("close pty master: %v", err)
	}

	if !waitForNoProcess(pid, 5*time.Second) {
		_ = cmd.Process.Kill()
		select {
		case <-waitDone:
		case <-time.After(time.Second):
		}
		t.Fatalf("process %d still existed after pty master close", pid)
	}

	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatalf("Wait did not return after process %d disappeared", pid)
	}
}

func startPTY(t *testing.T, cmd *exec.Cmd, rows, cols uint16) *os.File {
	t.Helper()

	master, err := Start(cmd, rows, cols)
	if err != nil {
		if isPTYUnavailable(err) {
			t.Skipf("pty unavailable: %v", err)
		}
		t.Fatalf("Start: %v", err)
	}
	return master
}

func readUntil(t *testing.T, f *os.File, want []byte, timeout time.Duration) []byte {
	t.Helper()

	return readUntilCount(t, f, want, 1, timeout)
}

func readUntilCount(t *testing.T, f *os.File, want []byte, count int, timeout time.Duration) []byte {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var got bytes.Buffer
	buf := make([]byte, 256)

	for time.Now().Before(deadline) {
		if err := f.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			t.Fatalf("set pty read deadline: %v", err)
		}

		n, err := f.Read(buf)
		if n > 0 {
			got.Write(buf[:n])
			if bytes.Count(got.Bytes(), want) >= count {
				return append([]byte(nil), got.Bytes()...)
			}
		}
		if err != nil {
			if os.IsTimeout(err) {
				continue
			}
			t.Fatalf("read pty: %v; got %q", err, got.Bytes())
		}
	}

	t.Fatalf("timed out waiting for %d occurrences of %q; got %q", count, want, got.Bytes())
	return nil
}

func closeMasterAndWait(t *testing.T, master *os.File, cmd *exec.Cmd) {
	t.Helper()

	if master != nil {
		_ = master.Close()
	}
	waitForCommand(t, cmd, 2*time.Second)
}

func waitForCommand(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatalf("command did not exit within %s", timeout)
	}
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			text := strings.TrimSpace(string(b))
			if text != "" {
				pid, err := strconv.Atoi(text)
				if err != nil {
					t.Fatalf("parse pid file %q: %v", text, err)
				}
				return pid
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read pid file: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for pid file %s", path)
	return 0
}

func waitForNoProcess(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func isPTYUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "pty") && !strings.Contains(msg, "/dev/ptmx") {
		return false
	}
	return errors.Is(err, os.ErrPermission) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ENODEV) ||
		errors.Is(err, syscall.ENXIO)
}
