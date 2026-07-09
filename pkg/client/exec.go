package client

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/wsbits"
)

type readDeadlineSetter interface {
	SetReadDeadline(time.Time) error
}

// ErrPTYUnsupported means a PTY was requested but the upgraded daemon did not
// confirm PTY support. Restart the daemon so the CLI and daemon versions match.
var ErrPTYUnsupported = errors.New("daemon does not support PTY (restart the daemon after upgrading)")

type ExecOptions struct {
	PTY   bool
	Term  string
	Rows  uint16
	Cols  uint16
	Winch <-chan [2]uint16
}

// Exec opens POST /v1/exec as an in-tree WebSocket client, pumps std streams,
// and returns the remote process exit code.
func (c *Client) Exec(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return c.ExecWithOptions(ctx, argv, stdin, stdout, stderr, ExecOptions{})
}

// ExecWithOptions opens POST /v1/exec as an in-tree WebSocket client, pumps std
// streams, and returns the remote process exit code.
func (c *Client) ExecWithOptions(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer, opts ExecOptions) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	conn, err := net.Dial("unix", c.sock)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	ctxDone := make(chan struct{})
	ctxCloserDone := make(chan struct{})
	go func() {
		defer close(ctxCloserDone)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-ctxDone:
		}
	}()
	defer func() {
		close(ctxDone)
		<-ctxCloserDone
	}()

	key, err := execWSKey()
	if err != nil {
		return 0, err
	}
	target := execPath(argv, opts)
	req, err := http.NewRequest(http.MethodPost, "http://unix"+target, nil)
	if err != nil {
		return 0, err
	}
	if err := writeAll(conn, []byte(execUpgradeRequest(target, key))); err != nil {
		return 0, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		return 0, apiError(resp)
	}
	if got, want := strings.TrimSpace(resp.Header.Get("Sec-WebSocket-Accept")), execWSAccept(key); got != want {
		return 0, fmt.Errorf("websocket: Sec-WebSocket-Accept mismatch")
	}
	if opts.PTY && resp.Header.Get("X-Portal-Exec-Pty") != "1" {
		return 0, ErrPTYUnsupported
	}

	writeMu := &sync.Mutex{}
	stdinDone := pumpExecStdin(conn, writeMu, stdin, opts.PTY)
	sessionDone := make(chan struct{})
	var winchDone <-chan error
	if opts.PTY {
		winchDone = pumpExecWinch(conn, writeMu, opts.Winch, sessionDone)
	}
	var pumpErr error

	exitCode := 0
	var terminalErr error
	var transportErr error
	gotExit := false

readLoop:
	for {
		op, payload, err := wsbits.ReadFrame(br, false)
		if err != nil {
			if ctx.Err() != nil {
				transportErr = ctx.Err()
			} else {
				transportErr = err
			}
			break
		}
		switch op {
		case wsbits.OpBinary:
			f, err := api.DecodeExecFrame(payload)
			if err != nil {
				transportErr = err
				break readLoop
			}
			switch f.Stream {
			case api.ExecStreamStdout:
				if err := writeAll(stdout, f.Data); err != nil {
					transportErr = err
					break readLoop
				}
			case api.ExecStreamStderr:
				if err := writeAll(stderr, f.Data); err != nil {
					transportErr = err
					break readLoop
				}
			case api.ExecStreamExit:
				exitCode = f.Code
				gotExit = true
				break readLoop
			case api.ExecStreamError:
				msg := string(f.Data)
				if msg == "" {
					msg = "exec stream error"
				}
				terminalErr = errors.New(msg)
				break readLoop
			}
		case wsbits.OpPing:
			writeMu.Lock()
			err := wsbits.WriteFrame(conn, wsbits.OpPong, payload, true)
			writeMu.Unlock()
			if err != nil {
				transportErr = err
				break readLoop
			}
		case wsbits.OpClose:
			transportErr = errors.New("websocket: close before exec terminal frame")
			break readLoop
		}
	}

	close(sessionDone)
	// A terminal stdin read can block after the remote command has already
	// exited; doc §8.1 requires `portal exec -- uname -sm` to return promptly.
	if stdin != nil {
		if d, ok := stdin.(readDeadlineSetter); ok {
			_ = d.SetReadDeadline(time.Now())
		}
	}
	_ = conn.Close()
	if winchDone != nil {
		<-winchDone
	}
	if gotExit {
		// Non-deadline terminal readers can remain blocked after a terminal
		// frame. The pump's done channel is buffered, so abandoning it cannot
		// strand a sender; the goroutine exits when the Read eventually unblocks.
		return exitCode, nil
	}
	if terminalErr != nil {
		// Same blocked-reader constraint as clean exit: the remote terminal
		// frame is authoritative, so return it without waiting for stdin.
		return 0, terminalErr
	}
	if stdinDone != nil {
		select {
		case pumpErr = <-stdinDone:
		case <-ctx.Done():
			// A non-deadline-aware stdin reader can remain blocked after the
			// connection closes; that pump exits only when its Read unblocks.
			return 0, ctx.Err()
		}
	}

	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if pumpErr != nil && (transportErr == nil || errors.Is(transportErr, net.ErrClosed)) {
		return 0, pumpErr
	}
	if transportErr != nil {
		return 0, transportErr
	}
	if pumpErr != nil {
		return 0, pumpErr
	}
	return 0, io.ErrUnexpectedEOF
}

func execPath(argv []string, opts ExecOptions) string {
	var b strings.Builder
	b.WriteString("/v1/exec")
	for i, arg := range argv {
		if i == 0 {
			b.WriteByte('?')
		} else {
			b.WriteByte('&')
		}
		b.WriteString("arg=")
		b.WriteString(url.QueryEscape(arg))
	}
	if opts.PTY {
		appendExecQuery(&b, len(argv) == 0, "pty", "1")
		appendExecQuery(&b, false, "term", opts.Term)
		appendExecQuery(&b, false, "rows", strconv.FormatUint(uint64(opts.Rows), 10))
		appendExecQuery(&b, false, "cols", strconv.FormatUint(uint64(opts.Cols), 10))
	}
	return b.String()
}

func appendExecQuery(b *strings.Builder, first bool, key, value string) {
	if first {
		b.WriteByte('?')
	} else {
		b.WriteByte('&')
	}
	b.WriteString(key)
	b.WriteByte('=')
	b.WriteString(url.QueryEscape(value))
}

func execUpgradeRequest(target, key string) string {
	return "POST " + target + " HTTP/1.1\r\n" +
		"Host: unix\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Content-Length: 0\r\n" +
		"\r\n"
}

func execWSKey() (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b[:]), nil
}

func execWSAccept(key string) string {
	return wsbits.AcceptKey(key)
}

func pumpExecStdin(w net.Conn, writeMu *sync.Mutex, stdin io.Reader, pty bool) <-chan error {
	done := make(chan error, 1)
	if stdin == nil {
		if pty {
			return nil
		}
		go func() { done <- writeExecStdinFrame(w, writeMu, nil) }()
		return done
	}
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := stdin.Read(buf)
			if n > 0 {
				if err := writeExecStdinFrame(w, writeMu, buf[:n]); err != nil {
					done <- err
					return
				}
			}
			if rerr != nil {
				if errors.Is(rerr, io.EOF) {
					if pty {
						done <- nil
						return
					}
					done <- writeExecStdinFrame(w, writeMu, nil)
					return
				}
				_ = w.Close()
				done <- rerr
				return
			}
		}
	}()
	return done
}

func pumpExecWinch(w io.Writer, writeMu *sync.Mutex, winch <-chan [2]uint16, done <-chan struct{}) <-chan error {
	if winch == nil {
		return nil
	}
	errc := make(chan error, 1)
	go func() {
		for {
			select {
			case <-done:
				errc <- nil
				return
			case size, ok := <-winch:
				if !ok {
					errc <- nil
					return
				}
				payload, err := api.EncodeExecFrame(api.ExecFrame{
					Stream: api.ExecStreamWinch,
					Rows:   size[0],
					Cols:   size[1],
				})
				if err != nil {
					errc <- err
					return
				}
				writeMu.Lock()
				err = wsbits.WriteFrame(w, wsbits.OpBinary, payload, true)
				writeMu.Unlock()
				if err != nil {
					errc <- err
					return
				}
			}
		}
	}()
	return errc
}

func writeExecStdinFrame(w io.Writer, writeMu *sync.Mutex, data []byte) error {
	payload, err := api.EncodeExecFrame(api.ExecFrame{Stream: api.ExecStreamStdin, Data: data})
	if err != nil {
		return err
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	return wsbits.WriteFrame(w, wsbits.OpBinary, payload, true)
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
