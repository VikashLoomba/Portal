package localapi

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/VikashLoomba/Portal/pkg/wsbits"
)

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
	if !strings.Contains(got, "Sec-WebSocket-Accept: "+wsbits.AcceptKey(key)+"\r\n") {
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
