package client

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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/localapi"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/protocol"
	"github.com/VikashLoomba/Portal/pkg/transport/localexec"
	"github.com/VikashLoomba/Portal/pkg/wsbits"
)

func TestExec(t *testing.T) {
	tests := []struct {
		name       string
		argv       []string
		stdin      io.Reader
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "stdout exit zero",
			argv:       []string{"printf", "hi"},
			wantStdout: "hi",
		},
		{
			name:     "nonzero exit",
			argv:     []string{"sh", "-c", "'exit 5'"},
			wantCode: 5,
		},
		{
			name:       "stdin half close",
			argv:       []string{"cat"},
			stdin:      strings.NewReader("payload\n"),
			wantStdout: "payload\n",
		},
		{
			name:       "stderr",
			argv:       []string{"sh", "-c", "'echo E >&2'"},
			wantStderr: "E",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, _ := startExecClientServer(t, config.New(t.TempDir()))
			var out, errb bytes.Buffer
			code, err := New(path).Exec(context.Background(), tt.argv, tt.stdin, &out, &errb)
			if err != nil {
				t.Fatalf("Exec returned error: %v", err)
			}
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tt.wantCode)
			}
			if out.String() != tt.wantStdout {
				t.Fatalf("stdout = %q, want %q", out.String(), tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(errb.String(), tt.wantStderr) {
				t.Fatalf("stderr = %q, want substring %q", errb.String(), tt.wantStderr)
			}
			if tt.wantStderr == "" && errb.String() != "" {
				t.Fatalf("stderr = %q, want empty", errb.String())
			}
		})
	}
}

func TestExecFeatureOff(t *testing.T) {
	cfg := config.New(t.TempDir())
	if err := cfg.SetFeature(config.FeatureExec, false); err != nil {
		t.Fatalf("SetFeature(exec,false): %v", err)
	}
	path, _ := startExecClientServer(t, cfg)

	var out, errb bytes.Buffer
	code, err := New(path).Exec(context.Background(), []string{"printf", "hi"}, nil, &out, &errb)
	if err == nil {
		t.Fatal("Exec error = nil, want feature_disabled APIError")
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *api.APIError", err)
	}
	if apiErr.Code != "feature_disabled" {
		t.Fatalf("APIError.Code = %q, want feature_disabled", apiErr.Code)
	}
}

func TestExecWithOptionsPTYSkewRequiresGrantedHeader(t *testing.T) {
	path := filepath.Join(shortExecClientTempDir(t), "api.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

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
		if _, err := fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsbits.AcceptKey(key)); err != nil {
			serverDone <- err
			return
		}
		if err := conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
			serverDone <- err
			return
		}
		if b, err := br.ReadByte(); err == nil {
			serverDone <- fmt.Errorf("client sent frame byte %#x after missing PTY grant", b)
			return
		} else if !errors.Is(err, io.EOF) {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()
	defer func() {
		_ = ln.Close()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Fatalf("fake exec server: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("fake exec server did not stop")
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	code, err := New(path).ExecWithOptions(ctx, []string{"cat"}, strings.NewReader("payload"), io.Discard, io.Discard, ExecOptions{PTY: true})
	if !errors.Is(err, ErrPTYUnsupported) {
		t.Fatalf("ExecWithOptions error = %v, want ErrPTYUnsupported", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}

func TestExecWithOptionsPTYFullStackSttySize(t *testing.T) {
	path, _ := startExecClientServer(t, config.New(t.TempDir()))

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := New(path).ExecWithOptions(ctx, []string{"stty", "size"}, nil, &out, nil, ExecOptions{PTY: true, Rows: 40, Cols: 100})
	if err != nil {
		t.Fatalf("ExecWithOptions returned error: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "40 100") {
		t.Fatalf("stdout = %q, want stty size 40 100", out.String())
	}
}

func TestExecWithOptionsPTYFullStackResize(t *testing.T) {
	path, _ := startExecClientServer(t, config.New(t.TempDir()))

	winch := make(chan [2]uint16, 1)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	time.AfterFunc(100*time.Millisecond, func() {
		select {
		case winch <- [2]uint16{30, 90}:
		default:
		}
	})

	script := "sleep 0.2; i=0; while [ $i -lt 20 ]; do stty size; i=$((i+1)); sleep 0.1; done"
	code, err := New(path).ExecWithOptions(ctx, []string{"sh", "-c", shellSingleQuoteExecTest(script)}, nil, &out, nil, ExecOptions{
		PTY:   true,
		Rows:  40,
		Cols:  100,
		Winch: winch,
	})
	if err != nil {
		t.Fatalf("ExecWithOptions returned error: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "30 90") {
		t.Fatalf("stdout = %q, want resized stty size 30 90", out.String())
	}
}

func TestExecWithOptionsPTYDisconnectKillsForegroundProcess(t *testing.T) {
	path, _ := startExecClientServer(t, config.New(t.TempDir()))
	pidfile := filepath.Join(t.TempDir(), "pid")
	argv := []string{"sh", "-c", shellSingleQuoteExecTest("echo $$ >" + pidfile + "; exec sleep 300")}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, err := New(path).ExecWithOptions(ctx, argv, nil, io.Discard, io.Discard, ExecOptions{PTY: true, Rows: 24, Cols: 80})
		done <- struct {
			code int
			err  error
		}{code: code, err: err}
	}()

	pid := waitExecPIDFile(t, pidfile)
	cancel()
	if !waitExecNoProcess(pid, 5*time.Second) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("process %d still existed after PTY client disconnect", pid)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ExecWithOptions did not return after context cancellation")
	}
}

func TestExecWithOptionsTerminalFrameReturnsWithBlockedStdin(t *testing.T) {
	tests := []struct {
		name       string
		frames     []api.ExecFrame
		wantCode   int
		wantErr    string
		wantStdout string
	}{
		{
			name: "clean exit",
			frames: []api.ExecFrame{
				{Stream: api.ExecStreamStdout, Data: []byte("done\n")},
				{Stream: api.ExecStreamExit, Code: 17},
			},
			wantCode:   17,
			wantStdout: "done\n",
		},
		{
			name: "terminal error",
			frames: []api.ExecFrame{
				{Stream: api.ExecStreamError, Data: []byte("remote failed")},
			},
			wantErr: "remote failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdin := &blockingExecReader{started: make(chan struct{})}
			path := startExecTerminalFrameServer(t, stdin.started, tt.frames)
			var out bytes.Buffer
			done := make(chan struct {
				code int
				err  error
			}, 1)

			go func() {
				code, err := New(path).Exec(context.Background(), []string{"ignored"}, stdin, &out, io.Discard)
				done <- struct {
					code int
					err  error
				}{code: code, err: err}
			}()

			select {
			case got := <-done:
				if tt.wantErr != "" {
					if got.err == nil || got.err.Error() != tt.wantErr {
						t.Fatalf("Exec error = %v, want %q", got.err, tt.wantErr)
					}
				} else if got.err != nil {
					t.Fatalf("Exec error = %v, want nil", got.err)
				}
				if got.code != tt.wantCode {
					t.Fatalf("exit code = %d, want %d", got.code, tt.wantCode)
				}
				if out.String() != tt.wantStdout {
					t.Fatalf("stdout = %q, want %q", out.String(), tt.wantStdout)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Exec did not return after terminal frame with blocked stdin")
			}
		})
	}
}

func TestExecPreservesExitWhenContextCancelsBeforeBlockedStdinDone(t *testing.T) {
	path := filepath.Join(shortExecClientTempDir(t), "api.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	clientClosed := make(chan struct{})
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
		key := strings.TrimSpace(req.Header.Get("Sec-WebSocket-Key"))
		if _, err := fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsbits.AcceptKey(key)); err != nil {
			serverDone <- err
			return
		}
		payload, err := api.EncodeExecFrame(api.ExecFrame{Stream: api.ExecStreamExit, Code: 17})
		if err != nil {
			serverDone <- err
			return
		}
		if err := wsbits.WriteFrame(conn, wsbits.OpBinary, payload, false); err != nil {
			serverDone <- err
			return
		}
		_, _ = br.ReadByte()
		close(clientClosed)
		serverDone <- nil
	}()
	defer func() {
		_ = ln.Close()
		select {
		case <-serverDone:
		case <-time.After(2 * time.Second):
			t.Fatal("fake exec server did not stop")
		}
	}()

	stdin, stdinWriter := io.Pipe()
	defer stdin.Close()
	defer stdinWriter.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, err := New(path).Exec(ctx, []string{"printf", "ignored"}, stdin, io.Discard, io.Discard)
		done <- struct {
			code int
			err  error
		}{code: code, err: err}
	}()

	select {
	case <-clientClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not close after exit frame")
	}
	cancel()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Exec error = %v, want nil", got.err)
		}
		if got.code != 17 {
			t.Fatalf("exit code = %d, want 17", got.code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exec did not return after context cancellation")
	}
}

func TestExecReturnsContextCanceledWhenTransportClosesBeforeBlockedStdinDone(t *testing.T) {
	path := filepath.Join(shortExecClientTempDir(t), "api.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

	serverClosed := make(chan struct{})
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
		key := strings.TrimSpace(req.Header.Get("Sec-WebSocket-Key"))
		if _, err := fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsbits.AcceptKey(key)); err != nil {
			serverDone <- err
			return
		}
		if err := wsbits.WriteFrame(conn, wsbits.OpClose, nil, false); err != nil {
			serverDone <- err
			return
		}
		if err := conn.Close(); err != nil {
			serverDone <- err
			return
		}
		close(serverClosed)
		serverDone <- nil
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Fatalf("fake exec server: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("fake exec server did not stop")
		}
	})

	stdin, stdinWriter := io.Pipe()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = stdinWriter.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		code, err := New(path).Exec(ctx, []string{"cat"}, stdin, io.Discard, io.Discard)
		done <- struct {
			code int
			err  error
		}{code: code, err: err}
	}()

	select {
	case <-serverClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("fake exec server did not close upgraded connection")
	}
	cancel()

	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("Exec error = %v, want context.Canceled", got.err)
		}
		if got.code != 0 {
			t.Fatalf("exit code = %d, want 0", got.code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exec did not return after context cancellation")
	}
}

type blockingExecReader struct {
	once    sync.Once
	started chan struct{}
}

func (r *blockingExecReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	select {}
}

func startExecTerminalFrameServer(t *testing.T, stdinStarted <-chan struct{}, frames []api.ExecFrame) string {
	t.Helper()

	path := filepath.Join(shortExecClientTempDir(t), "api.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}

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
		key := strings.TrimSpace(req.Header.Get("Sec-WebSocket-Key"))
		if _, err := fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsbits.AcceptKey(key)); err != nil {
			serverDone <- err
			return
		}
		select {
		case <-stdinStarted:
		case <-time.After(2 * time.Second):
			serverDone <- errors.New("stdin reader did not block")
			return
		}
		for _, frame := range frames {
			payload, err := api.EncodeExecFrame(frame)
			if err != nil {
				serverDone <- err
				return
			}
			if err := wsbits.WriteFrame(conn, wsbits.OpBinary, payload, false); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Fatalf("fake exec server: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("fake exec server did not stop")
		}
	})

	return path
}

func startExecClientServer(t *testing.T, cfg *config.Store) (string, *localapi.Server) {
	t.Helper()
	path := filepath.Join(shortExecClientTempDir(t), "api.sock")
	srv := localapi.New(localapi.Deps{
		Version:    api.VersionInfo{Version: "test", GitSHA: "exec", ProtoVersion: protocol.ProtoVersion},
		Config:     cfg,
		ExecStream: localexec.New(),
		Audit:      audit.New(t.TempDir()),
	})
	ln, err := localapi.Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("exec server did not stop")
		}
	})

	waitExecClientAvailable(t, New(path))
	return path, srv
}

func waitExecClientAvailable(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Available(context.Background()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server never became available")
}

func shortExecClientTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lcli-exec-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func shellSingleQuoteExecTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func waitExecPIDFile(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			text := strings.TrimSpace(string(b))
			if text == "" {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			pid, err := strconv.Atoi(text)
			if err != nil {
				t.Fatalf("pidfile %s contains %q: %v", path, text, err)
			}
			return pid
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read pidfile %s: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for pidfile %s", path)
	return 0
}

func waitExecNoProcess(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
