package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/localapi"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/client"
	"github.com/VikashLoomba/Portal/pkg/protocol"
	"github.com/VikashLoomba/Portal/pkg/transport/localexec"
	"github.com/VikashLoomba/Portal/pkg/transport/ptyx"
	"github.com/VikashLoomba/Portal/pkg/wsbits"
	"golang.org/x/sys/unix"
)

const (
	execTTYRestoreHelperEnv = "PORTAL_EXEC_TTY_RESTORE_HELPER"
	execTTYRestoreSockEnv   = "PORTAL_EXEC_TTY_RESTORE_SOCK"
	execTTYRestoreTTYEnv    = "PORTAL_EXEC_TTY_RESTORE_TTY_FILE"
	execTTYRestoreGoEnv     = "PORTAL_EXEC_TTY_RESTORE_GO_FILE"
	execTTYSignalHelperEnv  = "PORTAL_EXEC_TTY_SIGNAL_HELPER"
	execTTYSignalSockEnv    = "PORTAL_EXEC_TTY_SIGNAL_SOCK"
	execTTYSignalMarkerEnv  = "PORTAL_EXEC_TTY_SIGNAL_MARKER"
)

func TestMain(m *testing.M) {
	if os.Getenv(execTTYRestoreHelperEnv) == "1" {
		os.Exit(runExecTTYRestoreHelper())
	}
	if os.Getenv(execTTYSignalHelperEnv) == "1" {
		os.Exit(runExecTTYSignalHelper())
	}
	os.Exit(m.Run())
}

func TestExecCmd(t *testing.T) {
	setExecTermSeams(t, func(int) bool { return false }, nil, nil, nil)
	path := serveExecDaemon(t)

	tests := []struct {
		name     string
		args     []string
		wantOut  string
		wantCode int
		wantUse  bool
	}{
		{name: "true", args: []string{"--", "true"}},
		{name: "false", args: []string{"--", "false"}, wantCode: 1},
		{name: "stdout", args: []string{"--", "printf", "ok"}, wantOut: "ok"},
		{name: "missing dash", args: []string{"true"}, wantUse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &app.App{Paths: app.Paths{APISock: path}}
			cmd := newExecCmd(a)
			var out, errb bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&errb)
			cmd.SetArgs(tt.args)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := cmd.ExecuteContext(ctx)

			switch {
			case tt.wantUse:
				var ue usageErr
				if !errors.As(err, &ue) {
					t.Fatalf("error = %v, want usageErr", err)
				}
				return
			case tt.wantCode != 0:
				var ece exitCodeErr
				if !errors.As(err, &ece) {
					t.Fatalf("error = %v, want exitCodeErr", err)
				}
				if ece.code != tt.wantCode {
					t.Fatalf("exit code = %d, want %d", ece.code, tt.wantCode)
				}
			default:
				if err != nil {
					t.Fatalf("ExecuteContext returned %v", err)
				}
			}
			if out.String() != tt.wantOut {
				t.Fatalf("stdout = %q, want %q", out.String(), tt.wantOut)
			}
		})
	}
}

func TestExecCmdNoTTYForcesPipeMode(t *testing.T) {
	path := serveExecDaemon(t)

	t.Run("empty argv stays usage error", func(t *testing.T) {
		a := &app.App{Paths: app.Paths{APISock: path}}
		cmd := newExecCmd(a)
		cmd.SetArgs([]string{"-T"})

		err := cmd.ExecuteContext(context.Background())
		var ue usageErr
		if !errors.As(err, &ue) {
			t.Fatalf("error = %v, want usageErr", err)
		}
	})

	t.Run("argv runs without pty", func(t *testing.T) {
		a := &app.App{Paths: app.Paths{APISock: path}}
		cmd := newExecCmd(a)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"-T", "--", "printf", "pipe"})

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := cmd.ExecuteContext(ctx); err != nil {
			t.Fatalf("ExecuteContext returned %v", err)
		}
		if out.String() != "pipe" {
			t.Fatalf("stdout = %q, want pipe", out.String())
		}
	})
}

func TestExecCmdTTYRequiresTerminal(t *testing.T) {
	setExecTermSeams(t, func(int) bool { return false }, nil, nil, nil)

	a := &app.App{Paths: app.Paths{APISock: filepath.Join(t.TempDir(), "missing.sock")}}
	cmd := newExecCmd(a)
	cmd.SetArgs([]string{"-t", "--", "true"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil {
		t.Fatal("ExecuteContext error = nil, want terminal allocation error")
	}
	if err.Error() != "cannot allocate tty: stdin/stdout is not a terminal" {
		t.Fatalf("error = %q, want terminal allocation error", err.Error())
	}
}

func TestExecCmdMissingDashRejectsTerminalArgs(t *testing.T) {
	makeRawCalled := false
	setExecTermSeams(t,
		func(int) bool { return true },
		func(int) (func() error, error) {
			makeRawCalled = true
			return func() error { return nil }, nil
		},
		func(int) (uint16, uint16, error) { return 24, 80, nil },
		func(context.Context) <-chan struct{} {
			ch := make(chan struct{})
			close(ch)
			return ch
		},
	)

	tests := []struct {
		name string
		args []string
	}{
		{name: "auto tty", args: []string{"true"}},
		{name: "forced tty", args: []string{"-t", "true"}},
		{name: "no tty", args: []string{"-T", "true"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &app.App{Paths: app.Paths{APISock: filepath.Join(t.TempDir(), "missing.sock")}}
			cmd := newExecCmd(a)
			cmd.SetArgs(tt.args)

			err := cmd.ExecuteContext(context.Background())
			var ue usageErr
			if !errors.As(err, &ue) {
				t.Fatalf("error = %v, want usageErr", err)
			}
			if makeRawCalled {
				t.Fatal("MakeRaw called before missing-separator usage check")
			}
		})
	}
}

func TestExecCmdAutoTTYEmptyArgvSelectsPTY(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("ENV", "")

	path := serveExecDaemon(t)
	setExecTermSeams(t,
		func(int) bool { return true },
		func(int) (func() error, error) { return func() error { return nil }, nil },
		func(int) (uint16, uint16, error) { return 24, 80, nil },
		func(context.Context) <-chan struct{} {
			ch := make(chan struct{})
			close(ch)
			return ch
		},
	)

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		t.Fatalf("stdout pipe: %v", err)
	}

	oldStdin, oldStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})

	if _, err := inW.Write([]byte("exit\n")); err != nil {
		t.Fatalf("write stdin pipe: %v", err)
	}
	if err := inW.Close(); err != nil {
		t.Fatalf("close stdin writer: %v", err)
	}

	outputDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, outR)
		close(outputDone)
	}()

	a := &app.App{Paths: app.Paths{APISock: path}}
	cmd := newExecCmd(a)
	cmd.SetArgs(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = cmd.ExecuteContext(ctx)
	_ = outW.Close()
	select {
	case <-outputDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stdout pipe reader did not stop")
	}
	if err != nil {
		t.Fatalf("ExecuteContext returned %v", err)
	}
}

func TestExecCmdTTYWinchPumpSendsCurrentSize(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")

	path := serveExecDaemon(t)
	var sizeCalls atomic.Int64
	var resizeFD atomic.Int64
	resizeFD.Store(-1)
	setExecTermSeams(t,
		func(int) bool { return true },
		func(int) (func() error, error) { return func() error { return nil }, nil },
		func(fd int) (uint16, uint16, error) {
			if sizeCalls.Add(1) == 1 {
				return 40, 100, nil
			}
			resizeFD.Store(int64(fd))
			return 31, 97, nil
		},
		func(context.Context) <-chan struct{} {
			ch := make(chan struct{}, 1)
			ch <- struct{}{}
			close(ch)
			return ch
		},
	)

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		t.Fatalf("stdout pipe: %v", err)
	}

	oldStdin, oldStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = inR, outW
	stdinFD := int(os.Stdin.Fd())
	t.Cleanup(func() {
		os.Stdin, os.Stdout = oldStdin, oldStdout
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	if err := inW.Close(); err != nil {
		t.Fatalf("close stdin writer: %v", err)
	}

	var out bytes.Buffer
	outputDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&out, outR)
		close(outputDone)
	}()

	a := &app.App{Paths: app.Paths{APISock: path}}
	cmd := newExecCmd(a)
	script := `i=0; while [ $i -lt 30 ]; do size=$(stty size); echo "$size"; [ "$size" = "31 97" ] && exit 0; i=$((i+1)); sleep 0.1; done`
	cmd.SetArgs([]string{"-t", "--", "sh", "-c", shQuote(script)})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = cmd.ExecuteContext(ctx)
	_ = outW.Close()
	select {
	case <-outputDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stdout pipe reader did not stop")
	}
	if err != nil {
		t.Fatalf("ExecuteContext returned %v", err)
	}

	gotOut := out.String()
	if strings.Contains(gotOut, "97 31") {
		t.Fatalf("stdout = %q, contains swapped stty size 97 31", gotOut)
	}
	if !strings.Contains(gotOut, "31 97") {
		t.Fatalf("stdout = %q, want resized stty size 31 97", gotOut)
	}
	if got := int(resizeFD.Load()); got != stdinFD {
		t.Fatalf("resize getSize fd = %d, want current stdin fd %d", got, stdinFD)
	}
}

func TestExecCmdTTYRestoresTermios(t *testing.T) {
	path := serveExecDaemon(t)
	dir := t.TempDir()
	ttyfile := filepath.Join(dir, "tty")
	gofile := filepath.Join(dir, "go")

	helper := exec.Command(os.Args[0])
	helper.Env = append(os.Environ(),
		execTTYRestoreHelperEnv+"=1",
		execTTYRestoreSockEnv+"="+path,
		execTTYRestoreTTYEnv+"="+ttyfile,
		execTTYRestoreGoEnv+"="+gofile,
		"TERM=xterm-256color",
	)
	master, err := ptyx.Start(helper, 24, 80)
	if err != nil {
		t.Fatalf("ptyx.Start: %v", err)
	}
	defer master.Close()

	outputDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, master)
		close(outputDone)
	}()

	_ = waitExecTextFile(t, ttyfile)

	before, err := unix.IoctlGetTermios(int(master.Fd()), execGetTermiosReq())
	if err != nil {
		_ = helper.Process.Kill()
		t.Fatalf("read initial termios: %v", err)
	}
	if before.Lflag&unix.ECHO == 0 {
		_ = helper.Process.Kill()
		t.Fatalf("test pty started with ECHO disabled in Lflag %#x", before.Lflag)
	}

	if err := os.WriteFile(gofile, []byte("go"), 0o600); err != nil {
		_ = helper.Process.Kill()
		t.Fatalf("write go file: %v", err)
	}
	if err := waitExecCommand(t, helper, 8*time.Second); err != nil {
		t.Fatalf("helper exited with error: %v", err)
	}

	restored, err := unix.IoctlGetTermios(int(master.Fd()), execGetTermiosReq())
	if err != nil {
		t.Fatalf("read restored termios: %v", err)
	}
	if restored.Lflag&unix.ECHO == 0 {
		t.Fatalf("portal exec -t returned with ECHO disabled in Lflag %#x", restored.Lflag)
	}

	_ = master.Close()
	select {
	case <-outputDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pty output reader did not stop")
	}
}

func TestExecCmdTTYSignalRestoresAndReraises(t *testing.T) {
	sock, ready := serveExecPTYUpgradeHang(t)
	marker := filepath.Join(t.TempDir(), "restore.log")

	helper := exec.Command(os.Args[0])
	var out, errb bytes.Buffer
	helper.Stdout = &out
	helper.Stderr = &errb
	helper.Env = append(os.Environ(),
		execTTYSignalHelperEnv+"=1",
		execTTYSignalSockEnv+"="+sock,
		execTTYSignalMarkerEnv+"="+marker,
		"TERM=xterm-256color",
	)
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		_ = helper.Process.Kill()
		_ = waitExecCommand(t, helper, 2*time.Second)
		t.Fatalf("helper did not enter raw-mode exec window\nstdout:\n%s\nstderr:\n%s", out.String(), errb.String())
	}

	if err := helper.Process.Signal(syscall.SIGTERM); err != nil {
		_ = helper.Process.Kill()
		t.Fatalf("send SIGTERM: %v", err)
	}
	err := waitExecCommand(t, helper, 5*time.Second)
	if err == nil {
		t.Fatal("helper exited cleanly, want SIGTERM")
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("helper error = %T %v, want *exec.ExitError", err, err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("helper wait status = %T, want syscall.WaitStatus", exitErr.Sys())
	}
	if !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("helper wait status = %v, want signaled SIGTERM\nstdout:\n%s\nstderr:\n%s", status, out.String(), errb.String())
	}

	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read restore marker: %v", err)
	}
	if count := strings.Count(string(b), "restore\n"); count != 1 {
		t.Fatalf("restore marker count = %d in %q, want 1", count, string(b))
	}
}

func serveExecDaemon(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "portal-exec-api-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "api.sock")

	srv := localapi.New(localapi.Deps{
		Version:    api.VersionInfo{Version: "test", GitSHA: "exec", ProtoVersion: protocol.ProtoVersion},
		Config:     config.New(t.TempDir()),
		ExecStream: localexec.New(),
		Audit:      audit.New(t.TempDir()),
	})
	ln, err := localapi.Listen(path)
	if err != nil {
		t.Fatalf("localapi.Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("exec daemon did not stop")
		}
	})

	lc := client.New(path)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lc.Available(context.Background()) {
			return path
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("exec daemon did not come up")
	return path
}

func serveExecPTYUpgradeHang(t *testing.T) (string, <-chan struct{}) {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "portal-exec-signal-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	ready := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()

		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			serverDone <- err
			return
		}
		if got := req.URL.Query().Get("pty"); got != "1" {
			serverDone <- fmt.Errorf("pty query = %q, want 1", got)
			return
		}
		key := strings.TrimSpace(req.Header.Get("Sec-WebSocket-Key"))
		if _, err := fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nX-Portal-Exec-Pty: 1\r\n\r\n", wsbits.AcceptKey(key)); err != nil {
			serverDone <- err
			return
		}
		close(ready)
		_, _ = io.Copy(io.Discard, br)
		serverDone <- nil
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case err := <-serverDone:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.Fatalf("fake exec signal server: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("fake exec signal server did not stop")
		}
	})

	return path, ready
}

func setExecTermSeams(t *testing.T, isTerminal func(int) bool, makeRaw func(int) (func() error, error), getSize func(int) (uint16, uint16, error), watchWinch func(context.Context) <-chan struct{}) {
	t.Helper()

	oldIsTerminal := execIsTerminal
	oldMakeRaw := execMakeRaw
	oldGetSize := execGetSize
	oldWatchWinch := execWatchWinch
	if isTerminal != nil {
		execIsTerminal = isTerminal
	}
	if makeRaw != nil {
		execMakeRaw = makeRaw
	}
	if getSize != nil {
		execGetSize = getSize
	}
	if watchWinch != nil {
		execWatchWinch = watchWinch
	}
	t.Cleanup(func() {
		execIsTerminal = oldIsTerminal
		execMakeRaw = oldMakeRaw
		execGetSize = oldGetSize
		execWatchWinch = oldWatchWinch
	})
}

func runExecTTYRestoreHelper() int {
	sock := os.Getenv(execTTYRestoreSockEnv)
	ttyfile := os.Getenv(execTTYRestoreTTYEnv)
	gofile := os.Getenv(execTTYRestoreGoEnv)
	if sock == "" || ttyfile == "" || gofile == "" {
		fmt.Fprintln(os.Stderr, "exec tty restore helper: missing environment")
		return 1
	}

	ttyCmd := exec.Command("tty")
	ttyCmd.Stdin = os.Stdin
	ttyOut, err := ttyCmd.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "exec tty restore helper: tty:", err)
		return 1
	}
	if err := os.WriteFile(ttyfile, bytes.TrimSpace(ttyOut), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "exec tty restore helper: write tty file:", err)
		return 1
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(gofile); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "exec tty restore helper: stat go file:", err)
			return 1
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "exec tty restore helper: timed out waiting for go file")
			return 1
		}
		time.Sleep(20 * time.Millisecond)
	}

	a := &app.App{Paths: app.Paths{APISock: sock}}
	cmd := newExecCmd(a)
	cmd.SetArgs([]string{"-t", "--", "true"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cmd.ExecuteContext(ctx); err != nil {
		var ece exitCodeErr
		if errors.As(err, &ece) {
			return ece.code
		}
		fmt.Fprintln(os.Stderr, "exec tty restore helper:", err)
		return 1
	}
	return 0
}

func runExecTTYSignalHelper() int {
	sock := os.Getenv(execTTYSignalSockEnv)
	marker := os.Getenv(execTTYSignalMarkerEnv)
	if sock == "" || marker == "" {
		fmt.Fprintln(os.Stderr, "exec tty signal helper: missing environment")
		return 1
	}

	execIsTerminal = func(int) bool { return true }
	execMakeRaw = func(int) (func() error, error) {
		return func() error {
			return appendExecTTYSignalMarker(marker)
		}, nil
	}
	execGetSize = func(int) (uint16, uint16, error) { return 24, 80, nil }
	execWatchWinch = func(ctx context.Context) <-chan struct{} {
		ch := make(chan struct{})
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch
	}

	a := &app.App{Paths: app.Paths{APISock: sock}}
	cmd := newExecCmd(a)
	cmd.SetArgs([]string{"-t", "--", "sleep", "60"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "exec tty signal helper:", err)
		return 1
	}
	return 0
}

func appendExecTTYSignalMarker(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString("restore\n")
	return err
}

func waitExecTextFile(t *testing.T, path string) string {
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

func waitExecCommand(t *testing.T, cmd *exec.Cmd, timeout time.Duration) error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("timed out after %s", timeout)
	}
}

func execGetTermiosReq() uint {
	switch runtime.GOOS {
	case "darwin":
		return 0x40487413
	case "linux":
		switch runtime.GOARCH {
		case "mips", "mipsle", "mips64", "mips64le":
			return 0x540d
		case "ppc", "ppc64", "ppc64le":
			return 0x402c7413
		case "sparc64":
			return 0x40245408
		default:
			return 0x5401
		}
	default:
		return 0x5401
	}
}
