package localapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/transport/localexec"
)

func TestExecFrameEncodeDecodeRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   ExecFrame
	}{
		{name: "stdin data", in: ExecFrame{Stream: ExecStreamStdin, Data: []byte("input")}},
		{name: "stdout binary data", in: ExecFrame{Stream: ExecStreamStdout, Data: []byte{0x00, 0xff, 'o', 'k'}}},
		{name: "stderr data", in: ExecFrame{Stream: ExecStreamStderr, Data: []byte("warn")}},
		{name: "exit code", in: ExecFrame{Stream: ExecStreamExit, Code: 3}},
		{name: "error data", in: ExecFrame{Stream: ExecStreamError, Data: []byte("boom")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := EncodeExecFrame(tt.in)
			if err != nil {
				t.Fatalf("EncodeExecFrame: %v", err)
			}
			got, err := DecodeExecFrame(encoded)
			if err != nil {
				t.Fatalf("DecodeExecFrame: %v", err)
			}
			if got.Stream != tt.in.Stream {
				t.Fatalf("Stream = %q, want %q", got.Stream, tt.in.Stream)
			}
			if !bytes.Equal(got.Data, tt.in.Data) {
				t.Fatalf("Data = %v, want %v", got.Data, tt.in.Data)
			}
			if got.Code != tt.in.Code {
				t.Fatalf("Code = %d, want %d", got.Code, tt.in.Code)
			}
		})
	}
}

func TestDecodeExecFrameMalformedCBOR(t *testing.T) {
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("DecodeExecFrame panicked: %v", rec)
		}
	}()
	if _, err := DecodeExecFrame([]byte{0xa1, 0x61, 's'}); err == nil {
		t.Fatal("DecodeExecFrame returned nil error")
	}
}

func TestExecUpgradeReaches101(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWS(t, path, []string{"printf", "hello"})
	c.Close()
}

func TestExecPrintfStdoutExitZero(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWS(t, path, []string{"printf", "hello"})
	defer c.Close()

	frames := readExecFramesUntilExit(t, c, 2*time.Second)
	if got := joinFrameData(frames, ExecStreamStdout); got != "hello" {
		t.Fatalf("stdout = %q, want hello (bridge must pass argv verbatim)", got)
	}
	if got := lastExitCode(frames); got != 0 {
		t.Fatalf("exit code = %d, want 0", got)
	}
}

func TestExecNonZeroExitFrame(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWS(t, path, []string{"sh", "-c", "'exit 4'"})
	defer c.Close()

	frames := readExecFramesUntilExit(t, c, 2*time.Second)
	if got := lastExitCode(frames); got != 4 {
		t.Fatalf("exit code = %d, want 4", got)
	}
}

func TestExecStdinHalfClose(t *testing.T) {
	path, _ := startExecServer(t, config.New(t.TempDir()), audit.New(t.TempDir()), localexec.New())
	c := dialExecWS(t, path, []string{"cat"})
	defer c.Close()

	writeExecClientFrame(t, c, ExecFrame{Stream: ExecStreamStdin, Data: []byte("ping\n")})
	stdout := readExecFrameMatching(t, c, ExecStreamStdout, 2*time.Second)
	if string(stdout.Data) != "ping\n" {
		t.Fatalf("stdout = %q, want ping newline", string(stdout.Data))
	}

	writeExecClientFrame(t, c, ExecFrame{Stream: ExecStreamStdin, Data: []byte{}})
	frames := readExecFramesUntilExit(t, c, 2*time.Second)
	if got := lastExitCode(frames); got != 0 {
		t.Fatalf("exit code = %d, want 0", got)
	}
}

func TestExecFeatureOffNoUpgrade(t *testing.T) {
	cfg := config.New(t.TempDir())
	if err := cfg.SetFeature(config.FeatureExec, false); err != nil {
		t.Fatalf("SetFeature(exec,false): %v", err)
	}
	path, _ := startExecServer(t, cfg, audit.New(t.TempDir()), localexec.New())

	status, _, body := rawExecHTTP(t, path, []string{"printf", "hello"})
	if !strings.Contains(status, "403") {
		t.Fatalf("status = %q, want 403", status)
	}
	if strings.Contains(status, "101") {
		t.Fatalf("disabled exec upgraded unexpectedly: %q", status)
	}
	var eb errorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("decode error body %q: %v", string(body), err)
	}
	if eb.Error.Code != "feature_disabled" {
		t.Fatalf("error code = %q, want feature_disabled", eb.Error.Code)
	}
}

func TestExecAuditOpenCloseOnce(t *testing.T) {
	a := audit.New(t.TempDir())
	path, _ := startExecServer(t, config.New(t.TempDir()), a, localexec.New())
	c := dialExecWS(t, path, []string{"printf", "hello"})
	defer c.Close()

	_ = readExecFramesUntilExit(t, c, 2*time.Second)
	lines := waitAuditLines(t, a.Path(), 2, 2*time.Second)
	var opens, closes int
	wantUIDField := fmt.Sprintf("\tuid=%d\t", os.Getuid())
	for _, line := range lines {
		if strings.Contains(line, "\texec-open\t") {
			opens++
			for _, token := range []string{"host=local", wantUIDField, "argv=printf hello"} {
				if !strings.Contains(line, token) {
					t.Fatalf("exec-open missing %q: %s", token, line)
				}
			}
		}
		if strings.Contains(line, "\texec-close\t") {
			closes++
			for _, token := range []string{"host=local", "code=0", "err=", "dur="} {
				if !strings.Contains(line, token) {
					t.Fatalf("exec-close missing %q: %s", token, line)
				}
			}
		}
	}
	if opens != 1 || closes != 1 {
		t.Fatalf("audit exec-open=%d exec-close=%d, want 1 each\n%s", opens, closes, strings.Join(lines, "\n"))
	}
}

func TestExecClientDisconnectCancelsStream(t *testing.T) {
	a := audit.New(t.TempDir())
	path, _ := startExecServer(t, config.New(t.TempDir()), a, localexec.New())
	baseline := runtime.NumGoroutine()

	c := dialExecWS(t, path, []string{"sh", "-c", "'sleep 5; echo hi'"})
	_ = c.Close()

	_ = waitAuditLines(t, a.Path(), 2, 2*time.Second)
	deadline := time.Now().Add(2 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = runtime.NumGoroutine()
		if got <= baseline+4 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines after disconnect = %d, baseline = %d", got, baseline)
}

type wsTestConn struct {
	net.Conn
	br *bufio.Reader
}

func startExecServer(t *testing.T, cfg *config.Store, a *audit.Log, streamer ExecStreamer) (string, *Server) {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "api.sock")
	s := New(Deps{
		Version:    VersionInfo{Version: "9.9"},
		Config:     cfg,
		ExecStream: streamer,
		Audit:      a,
	})
	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("exec server did not stop")
		}
	})
	waitVersion(t, path)
	return path, s
}

func dialExecWS(t *testing.T, path string, argv []string) *wsTestConn {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	q := url.Values{}
	for _, arg := range argv {
		q.Add("arg", arg)
	}
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	target := "/v1/exec?" + q.Encode()
	if _, err := fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: unix\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", target, key); err != nil {
		c.Close()
		t.Fatalf("write upgrade request: %v", err)
	}
	br := bufio.NewReader(c)
	tp := textproto.NewReader(br)
	status, err := tp.ReadLine()
	if err != nil {
		c.Close()
		t.Fatalf("read status: %v", err)
	}
	hdr, err := tp.ReadMIMEHeader()
	if err != nil {
		c.Close()
		t.Fatalf("read headers: %v", err)
	}
	if status != "HTTP/1.1 101 Switching Protocols" {
		c.Close()
		t.Fatalf("status = %q, want 101", status)
	}
	if got, want := hdr.Get("Sec-WebSocket-Accept"), wsAccept(key); got != want {
		c.Close()
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q", got, want)
	}
	return &wsTestConn{Conn: c, br: br}
}

func rawExecHTTP(t *testing.T, path string, argv []string) (string, textproto.MIMEHeader, []byte) {
	t.Helper()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	defer c.Close()
	q := url.Values{}
	for _, arg := range argv {
		q.Add("arg", arg)
	}
	target := "/v1/exec?" + q.Encode()
	if _, err := fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: unix\r\nUpgrade: websocket\r\nConnection: close, Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", target); err != nil {
		t.Fatalf("write request: %v", err)
	}
	br := bufio.NewReader(c)
	req, err := http.NewRequest(http.MethodPost, "http://unix"+target, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.Status, textproto.MIMEHeader(resp.Header), body
}

func writeExecClientFrame(t *testing.T, c *wsTestConn, f ExecFrame) {
	t.Helper()
	payload, err := EncodeExecFrame(f)
	if err != nil {
		t.Fatalf("EncodeExecFrame: %v", err)
	}
	if err := writeClientFrame(c, opBinary, payload); err != nil {
		t.Fatalf("write client frame: %v", err)
	}
}

func writeClientFrame(c net.Conn, op wsOpcode, payload []byte) error {
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
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	masked := append([]byte(nil), payload...)
	for i := range masked {
		masked[i] ^= mask[i%4]
	}
	if _, err := c.Write(header); err != nil {
		return err
	}
	if _, err := c.Write(mask[:]); err != nil {
		return err
	}
	_, err := c.Write(masked)
	return err
}

func readExecFrameMatching(t *testing.T, c *wsTestConn, stream string, timeout time.Duration) ExecFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for %s frame", stream)
		}
		f := readExecFrame(t, c, remaining)
		if f.Stream == stream {
			return f
		}
		if f.Stream == ExecStreamError || f.Stream == ExecStreamExit {
			t.Fatalf("got terminal frame before %s: %+v", stream, f)
		}
	}
}

func readExecFramesUntilExit(t *testing.T, c *wsTestConn, timeout time.Duration) []ExecFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var frames []ExecFrame
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("timed out waiting for exit frame; got %+v", frames)
		}
		f := readExecFrame(t, c, remaining)
		frames = append(frames, f)
		switch f.Stream {
		case ExecStreamExit:
			return frames
		case ExecStreamError:
			t.Fatalf("error frame: %s", string(f.Data))
		}
	}
}

func readExecFrame(t *testing.T, c *wsTestConn, timeout time.Duration) ExecFrame {
	t.Helper()
	for {
		op, payload, err := readServerFrame(c, timeout)
		if err != nil {
			t.Fatalf("read server frame: %v", err)
		}
		if op == opClose {
			t.Fatal("server close before exec terminal frame")
		}
		if op != opBinary {
			continue
		}
		f, err := DecodeExecFrame(payload)
		if err != nil {
			t.Fatalf("DecodeExecFrame: %v", err)
		}
		return f
	}
}

func readServerFrame(c *wsTestConn, timeout time.Duration) (wsOpcode, []byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	defer c.SetReadDeadline(time.Time{})

	var header [2]byte
	if _, err := io.ReadFull(c.br, header[:]); err != nil {
		return 0, nil, err
	}
	op := wsOpcode(header[0] & 0x0f)
	n, err := readServerPayloadLen(c.br, header[1]&0x7f)
	if err != nil {
		return 0, nil, err
	}
	masked := header[1]&0x80 != 0
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, int(n))
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return op, payload, nil
}

func readServerPayloadLen(r io.Reader, len7 byte) (uint64, error) {
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

func joinFrameData(frames []ExecFrame, stream string) string {
	var b strings.Builder
	for _, f := range frames {
		if f.Stream == stream {
			b.Write(f.Data)
		}
	}
	return b.String()
}

func lastExitCode(frames []ExecFrame) int {
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].Stream == ExecStreamExit {
			return frames[i].Code
		}
	}
	return -1
}

func waitAuditLines(t *testing.T, path string, want int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) >= want && lines[0] != "" {
				return lines
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("audit log did not reach %d lines:\n%s", want, string(b))
	return nil
}
