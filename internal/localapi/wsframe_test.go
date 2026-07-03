package localapi

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestWSAcceptRFC6455Vector(t *testing.T) {
	got := wsAccept("dGhlIHNhbXBsZSBub25jZQ==")
	if got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("wsAccept = %q, want RFC 6455 vector", got)
	}
}

func TestWSReadMessageMaskedClientPayloadBoundaries(t *testing.T) {
	sizes := []int{0, 1, 125, 126, 127, 65535, 65536}
	for _, size := range sizes {
		t.Run(payloadSizeName(size), func(t *testing.T) {
			want := patternedPayload(size)
			server, client := net.Pipe()
			defer server.Close()
			defer client.Close()

			writeErr := make(chan error, 1)
			go func() {
				_, err := client.Write(maskedClientFrame(0x80|byte(opBinary), want))
				writeErr <- err
			}()

			rw := bufio.NewReadWriter(bufio.NewReader(server), bufio.NewWriter(server))
			op, got, err := wsReadMessage(rw)
			if err != nil {
				t.Fatalf("wsReadMessage: %v", err)
			}
			if op != opBinary {
				t.Fatalf("opcode = 0x%x, want binary", byte(op))
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("payload mismatch for size %d", size)
			}
			if err := <-writeErr; err != nil {
				t.Fatalf("client write: %v", err)
			}
		})
	}
}

func TestWSWriteBinaryLengthBoundaries(t *testing.T) {
	sizes := []int{0, 1, 125, 126, 127, 65535, 65536}
	for _, size := range sizes {
		t.Run(payloadSizeName(size), func(t *testing.T) {
			want := patternedPayload(size)
			var out bytes.Buffer
			if err := wsWriteBinary(&out, want); err != nil {
				t.Fatalf("wsWriteBinary: %v", err)
			}
			op, got, payload := parseServerFrame(t, out.Bytes())
			if op != opBinary {
				t.Fatalf("opcode = 0x%x, want binary", byte(op))
			}
			if got != uint64(size) {
				t.Fatalf("length = %d, want %d", got, size)
			}
			if !bytes.Equal(payload, want) {
				t.Fatalf("payload mismatch for size %d", size)
			}
		})
	}
}

func TestWSReadMessageMalformedFrames(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
	}{
		{
			name:  "unmasked client frame",
			frame: []byte{0x80 | byte(opBinary), 0x00},
		},
		{
			name:  "declared length exceeds max",
			frame: oversizedMaskedFrameHeader(wsMaxPayload + 1),
		},
		{
			name:  "reserved opcode",
			frame: maskedClientFrame(0x80|0x3, nil),
		},
		{
			name:  "continuation frame",
			frame: maskedClientFrame(0x80|byte(opContinuation), nil),
		},
		{
			name:  "truncated frame",
			frame: maskedClientFrame(0x80|byte(opBinary), []byte("hello"))[:4],
		},
		{
			name:  "fragmented data frame",
			frame: maskedClientFrame(byte(opBinary), []byte("hello")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rw := bufio.NewReadWriter(bufio.NewReader(bytes.NewReader(tt.frame)), bufio.NewWriter(io.Discard))
			if _, _, err := wsReadMessage(rw); err == nil {
				t.Fatal("wsReadMessage returned nil error")
			}
		})
	}
}

func TestWSWriteClose(t *testing.T) {
	var out bytes.Buffer
	if err := wsWriteClose(&out, 1000, "done"); err != nil {
		t.Fatalf("wsWriteClose: %v", err)
	}
	op, n, payload := parseServerFrame(t, out.Bytes())
	if op != opClose {
		t.Fatalf("opcode = 0x%x, want close", byte(op))
	}
	if n != uint64(len(payload)) {
		t.Fatalf("length = %d, want %d", n, len(payload))
	}
	if got := binary.BigEndian.Uint16(payload[:2]); got != 1000 {
		t.Fatalf("close code = %d, want 1000", got)
	}
	if got := string(payload[2:]); got != "done" {
		t.Fatalf("close reason = %q, want done", got)
	}
}

func TestWSWritePong(t *testing.T) {
	var out bytes.Buffer
	if err := wsWritePong(&out, []byte("ping")); err != nil {
		t.Fatalf("wsWritePong: %v", err)
	}
	op, n, payload := parseServerFrame(t, out.Bytes())
	if op != opPong {
		t.Fatalf("opcode = 0x%x, want pong", byte(op))
	}
	if n != 4 || !bytes.Equal(payload, []byte("ping")) {
		t.Fatalf("pong payload = %q length %d, want ping length 4", payload, n)
	}
}

func TestWSUpgradeDirectHijackerWritesSwitchingProtocols(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	rawResponse := make(chan string, 1)
	readErr := make(chan error, 1)
	go func() {
		resp, err := readRawHTTPResponse(client)
		if err != nil {
			readErr <- err
			return
		}
		rawResponse <- resp
		readErr <- nil
	}()

	key := "dGhlIHNhbXBsZSBub25jZQ=="
	w := newHijackResponseWriter(server)
	r := wsUpgradeRequest(key)
	conn, brw, err := wsUpgrade(w, r)
	if err != nil {
		t.Fatalf("wsUpgrade: %v", err)
	}
	if conn != server {
		t.Fatal("wsUpgrade returned a different connection")
	}
	if brw == nil {
		t.Fatal("wsUpgrade returned nil buffered reader/writer")
	}
	if !w.hijacked {
		t.Fatal("Hijack was not called")
	}
	got := <-rawResponse
	if err := <-readErr; err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.HasPrefix(got, "HTTP/1.1 101 Switching Protocols\r\n") {
		t.Fatalf("response = %q, want 101 Switching Protocols", got)
	}
	if !strings.Contains(got, "Sec-WebSocket-Accept: "+wsAccept(key)+"\r\n") {
		t.Fatalf("response missing accept header: %q", got)
	}
}

func TestWSUpgradeNoHijackerReturnsErrorWithout101(t *testing.T) {
	w := newPlainResponseWriter()
	_, _, err := wsUpgrade(w, wsUpgradeRequest("abc"))
	if !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("wsUpgrade error = %v, want http.ErrNotSupported", err)
	}
	if w.status != 0 {
		t.Fatalf("status = %d, want no write", w.status)
	}
	if strings.Contains(w.body.String(), "101 Switching Protocols") {
		t.Fatalf("body contains 101 response: %q", w.body.String())
	}
}

func TestWSUpgradeMissingKeyDoesNotHijack(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	w := newHijackResponseWriter(server)
	r := wsUpgradeRequest("")
	r.Header.Del("Sec-WebSocket-Key")
	if _, _, err := wsUpgrade(w, r); err == nil {
		t.Fatal("wsUpgrade returned nil error")
	}
	if w.hijacked {
		t.Fatal("Hijack was called for a bad request")
	}
}

func wsUpgradeRequest(key string) *http.Request {
	req, err := http.NewRequest(http.MethodGet, "http://unix/v1/exec", nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	if key != "" {
		req.Header.Set("Sec-WebSocket-Key", key)
	}
	return req
}

type hijackResponseWriter struct {
	header   http.Header
	conn     net.Conn
	hijacked bool
}

func newHijackResponseWriter(conn net.Conn) *hijackResponseWriter {
	return &hijackResponseWriter{
		header: make(http.Header),
		conn:   conn,
	}
}

func (w *hijackResponseWriter) Header() http.Header { return w.header }

func (w *hijackResponseWriter) Write(b []byte) (int, error) { return len(b), nil }

func (w *hijackResponseWriter) WriteHeader(int) {}

func (w *hijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return w.conn, bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn)), nil
}

type plainResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newPlainResponseWriter() *plainResponseWriter {
	return &plainResponseWriter{header: make(http.Header)}
}

func (w *plainResponseWriter) Header() http.Header { return w.header }

func (w *plainResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }

func (w *plainResponseWriter) WriteHeader(status int) { w.status = status }

func readRawHTTPResponse(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	var b strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "", err
		}
		b.WriteString(line)
		if line == "\r\n" {
			return b.String(), nil
		}
	}
}

func maskedClientFrame(first byte, payload []byte) []byte {
	const maskBit = 0x80
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	frame := []byte{first}
	n := len(payload)
	switch {
	case n <= 125:
		frame = append(frame, maskBit|byte(n))
	case n <= 0xffff:
		frame = append(frame, maskBit|126, 0, 0)
		binary.BigEndian.PutUint16(frame[2:4], uint16(n))
	default:
		frame = append(frame, maskBit|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(frame[2:10], uint64(n))
	}
	frame = append(frame, mask[:]...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	return frame
}

func oversizedMaskedFrameHeader(n uint64) []byte {
	frame := []byte{0x80 | byte(opBinary), 0x80 | 127, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint64(frame[2:10], n)
	frame = append(frame, 0, 0, 0, 0)
	return frame
}

func parseServerFrame(t *testing.T, frame []byte) (wsOpcode, uint64, []byte) {
	t.Helper()
	if len(frame) < 2 {
		t.Fatalf("frame too short: %d", len(frame))
	}
	if frame[0]&0x80 == 0 {
		t.Fatalf("FIN bit not set in first byte 0x%x", frame[0])
	}
	if frame[1]&0x80 != 0 {
		t.Fatal("server frame is masked")
	}
	op := wsOpcode(frame[0] & 0x0f)
	n, headerLen := serverFrameLength(t, frame)
	if uint64(len(frame)-headerLen) != n {
		t.Fatalf("payload length bytes = %d, header length says %d", len(frame)-headerLen, n)
	}
	return op, n, frame[headerLen:]
}

func serverFrameLength(t *testing.T, frame []byte) (uint64, int) {
	t.Helper()
	switch len7 := frame[1] & 0x7f; len7 {
	case 126:
		if len(frame) < 4 {
			t.Fatal("126-length frame missing extended length")
		}
		return uint64(binary.BigEndian.Uint16(frame[2:4])), 4
	case 127:
		if len(frame) < 10 {
			t.Fatal("127-length frame missing extended length")
		}
		return binary.BigEndian.Uint64(frame[2:10]), 10
	default:
		return uint64(len7), 2
	}
}

func patternedPayload(size int) []byte {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	return payload
}

func payloadSizeName(size int) string {
	return "size_" + strconv.Itoa(size)
}
