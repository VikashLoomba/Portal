package localapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/hub"
)

// newEventsServer serves a Server with a real hub.Hub on a real unix socket in a
// short temp dir, returning the hub (for Publish) and socket path. tick sets the
// u4-defaulted TickInterval to a test value before Serve. The server is torn
// down on test cleanup.
func newEventsServer(t *testing.T, tick time.Duration) (*Server, *hub.Hub, string) {
	t.Helper()
	h := hub.New()
	path := filepath.Join(shortTempDir(t), "api.sock")
	s := New(Deps{
		Version: VersionInfo{Version: "9.9", GitSHA: "deadbeef", ProtoVersion: 3},
		Config:  config.New(t.TempDir()),
		Hub:     h,
	})
	s.TickInterval = tick // override the u4 default (30s).

	ln, err := Listen(path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitVersion(t, path)
	return s, h, path
}

// streamingClient dials the unix socket with NO overall client timeout — an
// events stream is long-lived, so a Timeout would tear it down mid-stream.
func streamingClient(path string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}
}

// lineReader decodes ndjson eventLines off a stream body on a goroutine so tests
// can bound each read with a timeout instead of blocking on Scanner.Scan.
type lineReader struct {
	lines chan eventLine
	errc  chan error
}

func readLines(body io.Reader) *lineReader {
	lr := &lineReader{lines: make(chan eventLine, 64), errc: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var el eventLine
			if err := json.Unmarshal(sc.Bytes(), &el); err != nil {
				lr.errc <- err
				return
			}
			lr.lines <- el
		}
		lr.errc <- sc.Err()
	}()
	return lr
}

// next returns the next decoded line or fails on timeout.
func (lr *lineReader) next(t *testing.T, timeout time.Duration) eventLine {
	t.Helper()
	select {
	case el := <-lr.lines:
		return el
	case err := <-lr.errc:
		t.Fatalf("stream ended before a line arrived: %v", err)
	case <-time.After(timeout):
		t.Fatal("timed out waiting for an event line")
	}
	return eventLine{}
}

// waitType returns the next line whose Type == typ, skipping other lines (e.g.
// interleaved ticks), or fails on timeout.
func (lr *lineReader) waitType(t *testing.T, typ string, timeout time.Duration) eventLine {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case el := <-lr.lines:
			if el.Type == typ {
				return el
			}
		case err := <-lr.errc:
			t.Fatalf("stream ended before a %q line: %v", typ, err)
		case <-deadline:
			t.Fatalf("timed out waiting for a %q line", typ)
		}
	}
}

// openStream connects to /v1/events and returns the response, a lineReader, and
// a cancel that disconnects the client by cancelling the request context.
func openStream(t *testing.T, path string) (*http.Response, *lineReader, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/v1/events", nil)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	resp, err := streamingClient(path).Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /v1/events: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		resp.Body.Close()
		cancel()
		t.Fatalf("Content-Type = %q, want application/x-ndjson", ct)
	}
	return resp, readLines(resp.Body), cancel
}

// TestEventsSnapshotFirst (EC3.1): the FIRST line is a populated snapshot.
func TestEventsSnapshotFirst(t *testing.T) {
	_, _, path := newEventsServer(t, time.Hour)
	resp, lr, cancel := openStream(t, path)
	defer cancel()
	defer resp.Body.Close()

	first := lr.next(t, 2*time.Second)
	if first.Type != "snapshot" {
		t.Fatalf("first line type = %q, want snapshot", first.Type)
	}
	if first.Status == nil {
		t.Fatal("snapshot line has nil status")
	}
	if first.Status.Version.Version != "9.9" {
		t.Errorf("snapshot status version = %q, want 9.9", first.Status.Version.Version)
	}
}

// TestEventsCoalescedState (EC3.2): a Coalesced hub signal yields a full-Status
// "state" line; a rapid burst still yields coherent latest-Status lines with no
// backlog stall.
func TestEventsCoalescedState(t *testing.T) {
	_, h, path := newEventsServer(t, time.Hour)
	resp, lr, cancel := openStream(t, path)
	defer cancel()
	defer resp.Body.Close()

	// The snapshot is written only after both Subscribe calls, so receiving it
	// proves the handler is subscribed and Publish will reach it.
	if first := lr.next(t, 2*time.Second); first.Type != "snapshot" {
		t.Fatalf("first line type = %q, want snapshot", first.Type)
	}

	h.Publish(hub.Event{Class: hub.Coalesced})
	state := lr.waitType(t, "state", 2*time.Second)
	if state.Status == nil || state.Status.Version.Version != "9.9" {
		t.Fatalf("state line status = %+v, want populated current Status", state.Status)
	}

	// A rapid burst must not stall: coalescing may collapse lines, but the next
	// state line still carries coherent current truth.
	for i := 0; i < 50; i++ {
		h.Publish(hub.Event{Class: hub.Coalesced})
	}
	state = lr.waitType(t, "state", 2*time.Second)
	if state.Status == nil || state.Status.Version.Version != "9.9" {
		t.Fatalf("post-burst state status = %+v, want populated current Status", state.Status)
	}
}

// TestEventsNotifyTeed (EC3.3): a Queued hub Event yields a "notify" line with
// the exact Notify fields.
func TestEventsNotifyTeed(t *testing.T) {
	_, h, path := newEventsServer(t, time.Hour)
	resp, lr, cancel := openStream(t, path)
	defer cancel()
	defer resp.Body.Close()

	if first := lr.next(t, 2*time.Second); first.Type != "snapshot" {
		t.Fatalf("first line type = %q, want snapshot", first.Type)
	}

	want := &hub.Notify{Title: "deploy done", Body: "build 42", Subtitle: "ci", Urgency: 1, Verified: true, Source: "hook", Sound: "ping", Seq: 7}
	h.Publish(hub.Event{Class: hub.Queued, Notify: want})

	notify := lr.waitType(t, "notify", 2*time.Second)
	if notify.Notify == nil {
		t.Fatal("notify line has nil notify")
	}
	if *notify.Notify != *want {
		t.Errorf("notify = %+v, want %+v", *notify.Notify, *want)
	}
}

// TestEventsTick (EC3.4): with a short TickInterval, at least one "tick" line
// arrives.
func TestEventsTick(t *testing.T) {
	_, _, path := newEventsServer(t, 20*time.Millisecond)
	resp, lr, cancel := openStream(t, path)
	defer cancel()
	defer resp.Body.Close()

	if first := lr.next(t, 2*time.Second); first.Type != "snapshot" {
		t.Fatalf("first line type = %q, want snapshot", first.Type)
	}
	lr.waitType(t, "tick", 2*time.Second)
}

// TestEventsDisconnect (EC3.5): closing the client mid-stream makes the handler
// return and subCount fall back to 0 — proving no leak/backpressure.
func TestEventsDisconnect(t *testing.T) {
	s, _, path := newEventsServer(t, time.Hour)
	resp, lr, cancel := openStream(t, path)

	if first := lr.next(t, 2*time.Second); first.Type != "snapshot" {
		t.Fatalf("first line type = %q, want snapshot", first.Type)
	}
	// The handler incremented subCount on entry.
	waitSubCount(t, s, 1)

	// Disconnect: cancel the request context and close the body.
	cancel()
	resp.Body.Close()

	// The handler's r.Context() is now Done; it returns and decrements.
	waitSubCount(t, s, 0)
}

// waitSubCount polls s.subCount until it equals want or fails.
func waitSubCount(t *testing.T, s *Server, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.subCount.Load() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subCount = %d, want %d", s.subCount.Load(), want)
}
