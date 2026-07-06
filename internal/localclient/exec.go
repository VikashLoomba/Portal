package localclient

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/VikashLoomba/Portal/internal/localapi"
)

const (
	execWSGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	execWSMaxPayload = 16 << 20
)

type execWSOpcode byte

const (
	execOpContinuation execWSOpcode = 0x0
	execOpText         execWSOpcode = 0x1
	execOpBinary       execWSOpcode = 0x2
	execOpClose        execWSOpcode = 0x8
	execOpPing         execWSOpcode = 0x9
	execOpPong         execWSOpcode = 0xA
)

type readDeadlineSetter interface {
	SetReadDeadline(time.Time) error
}

// Exec opens POST /v1/exec as an in-tree WebSocket client, pumps std streams,
// and returns the remote process exit code.
func (c *Client) Exec(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
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
	target := execPath(argv)
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

	writeMu := &sync.Mutex{}
	stdinDone := pumpExecStdin(conn, writeMu, stdin)
	var pumpErr error

	exitCode := 0
	var terminalErr error
	var transportErr error
	gotExit := false

readLoop:
	for {
		op, payload, err := readExecServerMessage(br)
		if err != nil {
			if ctx.Err() != nil {
				transportErr = ctx.Err()
			} else {
				transportErr = err
			}
			break
		}
		switch op {
		case execOpBinary:
			f, err := localapi.DecodeExecFrame(payload)
			if err != nil {
				transportErr = err
				break readLoop
			}
			switch f.Stream {
			case localapi.ExecStreamStdout:
				if err := writeAll(stdout, f.Data); err != nil {
					transportErr = err
					break readLoop
				}
			case localapi.ExecStreamStderr:
				if err := writeAll(stderr, f.Data); err != nil {
					transportErr = err
					break readLoop
				}
			case localapi.ExecStreamExit:
				exitCode = f.Code
				gotExit = true
				break readLoop
			case localapi.ExecStreamError:
				msg := string(f.Data)
				if msg == "" {
					msg = "exec stream error"
				}
				terminalErr = errors.New(msg)
				break readLoop
			}
		case execOpPing:
			writeMu.Lock()
			err := writeMaskedFrame(conn, execOpPong, payload)
			writeMu.Unlock()
			if err != nil {
				transportErr = err
				break readLoop
			}
		case execOpClose:
			transportErr = errors.New("websocket: close before exec terminal frame")
			break readLoop
		}
	}

	// A terminal stdin read can block after the remote command has already
	// exited; doc §8.1 requires `portal exec -- uname -sm` to return promptly.
	if stdin != nil {
		if d, ok := stdin.(readDeadlineSetter); ok {
			_ = d.SetReadDeadline(time.Now())
		}
	}
	_ = conn.Close()
	if stdinDone != nil {
		pumpErr = <-stdinDone
	}

	if gotExit {
		return exitCode, nil
	}
	if terminalErr != nil {
		return 0, terminalErr
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

func execPath(argv []string) string {
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
	return b.String()
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
	sum := sha1.Sum([]byte(key + execWSGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func pumpExecStdin(w net.Conn, writeMu *sync.Mutex, stdin io.Reader) <-chan error {
	done := make(chan error, 1)
	if stdin == nil {
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

func writeExecStdinFrame(w io.Writer, writeMu *sync.Mutex, data []byte) error {
	payload, err := localapi.EncodeExecFrame(localapi.ExecFrame{Stream: localapi.ExecStreamStdin, Data: data})
	if err != nil {
		return err
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	return writeMaskedBinary(w, payload)
}

func writeMaskedBinary(w io.Writer, payload []byte) error {
	return writeMaskedFrame(w, execOpBinary, payload)
}

func writeMaskedFrame(w io.Writer, op execWSOpcode, payload []byte) error {
	header := []byte{0x80 | byte(op)}
	n := len(payload)
	switch {
	case n <= 125:
		header = append(header, 0x80|byte(n))
	case n <= 0xffff:
		header = append(header, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(header[2:4], uint16(n))
	default:
		header = append(header, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[2:10], uint64(n))
	}

	var mask [4]byte
	if _, err := io.ReadFull(rand.Reader, mask[:]); err != nil {
		return err
	}

	frame := make([]byte, 0, len(header)+len(mask)+len(payload))
	frame = append(frame, header...)
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	return writeAll(w, frame)
}

func readExecServerMessage(r *bufio.Reader) (execWSOpcode, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}

	fin := header[0]&0x80 != 0
	if header[0]&0x70 != 0 {
		return 0, nil, errors.New("websocket: reserved bits set")
	}
	op := execWSOpcode(header[0] & 0x0f)
	if !validExecWSOpcode(op) {
		return 0, nil, fmt.Errorf("websocket: reserved opcode 0x%x", byte(op))
	}
	if op == execOpContinuation {
		return 0, nil, errors.New("websocket: continuation frames are unsupported")
	}
	if !fin {
		return 0, nil, errors.New("websocket: fragmented messages are unsupported")
	}
	if header[1]&0x80 != 0 {
		return 0, nil, errors.New("websocket: server frame is masked")
	}

	n, err := readExecWSPayloadLen(r, header[1]&0x7f)
	if err != nil {
		return 0, nil, err
	}
	if n > execWSMaxPayload {
		return 0, nil, fmt.Errorf("websocket: payload length %d exceeds limit", n)
	}

	payload := make([]byte, int(n))
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return op, payload, nil
}

func validExecWSOpcode(op execWSOpcode) bool {
	switch op {
	case execOpContinuation, execOpText, execOpBinary, execOpClose, execOpPing, execOpPong:
		return true
	default:
		return false
	}
}

func readExecWSPayloadLen(r io.Reader, len7 byte) (uint64, error) {
	switch len7 {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		return uint64(binary.BigEndian.Uint16(b[:])), nil
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, err
		}
		return binary.BigEndian.Uint64(b[:]), nil
	default:
		return uint64(len7), nil
	}
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
