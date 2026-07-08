package termx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/ptyx"
	"golang.org/x/sys/unix"
)

type ptyFixture struct {
	master *os.File
	slave  *os.File
	cmd    *exec.Cmd
}

func TestIsTerminal(t *testing.T) {
	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pipeRead.Close()
	defer pipeWrite.Close()

	if IsTerminal(int(pipeRead.Fd())) {
		t.Fatalf("pipe read fd reported as terminal")
	}

	pty := newPTYFixture(t, 24, 80)
	defer pty.close(t)

	if !IsTerminal(int(pty.master.Fd())) {
		t.Fatalf("pty master fd was not reported as terminal")
	}
	if !IsTerminal(int(pty.slave.Fd())) {
		t.Fatalf("pty slave fd was not reported as terminal")
	}
}

func TestMakeRawRestore(t *testing.T) {
	pty := newPTYFixture(t, 24, 80)
	defer pty.close(t)

	fd := int(pty.slave.Fd())
	before, err := unix.IoctlGetTermios(fd, getTermiosReq)
	if err != nil {
		t.Fatalf("read termios before MakeRaw: %v", err)
	}
	original := *before

	restore, err := MakeRaw(fd)
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}

	raw, err := unix.IoctlGetTermios(fd, getTermiosReq)
	if err != nil {
		t.Fatalf("read raw termios: %v", err)
	}
	if raw.Lflag&unix.ECHO != 0 {
		t.Fatalf("MakeRaw left ECHO set in Lflag %#x", raw.Lflag)
	}

	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored, err := unix.IoctlGetTermios(fd, getTermiosReq)
	if err != nil {
		t.Fatalf("read restored termios: %v", err)
	}
	if *restored != original {
		t.Fatalf("restored termios mismatch: got %#v want %#v", *restored, original)
	}
}

func TestGetSize(t *testing.T) {
	pty := newPTYFixture(t, 33, 101)
	defer pty.close(t)

	rows, cols, err := GetSize(int(pty.slave.Fd()))
	if err != nil {
		t.Fatalf("GetSize: %v", err)
	}
	if rows != 33 || cols != 101 {
		t.Fatalf("GetSize = (%d, %d), want (33, 101)", rows, cols)
	}
}

func TestWatchWinch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := WatchWinch(ctx)
	if err := syscall.Kill(os.Getpid(), unix.SIGWINCH); err != nil {
		t.Fatalf("send SIGWINCH: %v", err)
	}

	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatalf("WatchWinch channel closed before delivering SIGWINCH")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for SIGWINCH tick")
	}

	cancel()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("WatchWinch channel did not close after context cancel")
		}
	}
}

func newPTYFixture(t *testing.T, rows, cols uint16) *ptyFixture {
	t.Helper()

	ttyfile := filepath.Join(t.TempDir(), "tty")
	cmd := exec.Command("sh", "-c", `tty > "$1"; sleep 300`, "sh", ttyfile)
	master, err := ptyx.Start(cmd, rows, cols)
	if err != nil {
		if isPTYUnavailable(err) {
			t.Skipf("pty unavailable: %v", err)
		}
		t.Fatalf("ptyx.Start: %v", err)
	}

	slaveName := waitForTextFile(t, ttyfile)
	slave, err := os.OpenFile(slaveName, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		cleanupStartedPTY(t, master, cmd)
		if isPTYUnavailable(err) {
			t.Skipf("pty slave unavailable: %v", err)
		}
		t.Fatalf("open pty slave %q: %v", slaveName, err)
	}

	return &ptyFixture{
		master: master,
		slave:  slave,
		cmd:    cmd,
	}
}

func (p *ptyFixture) close(t *testing.T) {
	t.Helper()

	if p.slave != nil {
		_ = p.slave.Close()
	}
	cleanupStartedPTY(t, p.master, p.cmd)
}

func waitForTextFile(t *testing.T, path string) string {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			text := strings.TrimSpace(string(b))
			if text != "" {
				return text
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read %s: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", path)
	return ""
}

func cleanupStartedPTY(t *testing.T, master *os.File, cmd *exec.Cmd) {
	t.Helper()

	if master != nil {
		_ = master.Close()
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("pty child did not exit")
	}
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
