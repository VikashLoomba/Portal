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

	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/hub"
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
		Version: api.VersionInfo{Version: "9.9", GitSHA: "deadbeef", ProtoVersion: 3},
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

// streamLine is one ndjson line: the raw wire bytes plus its decoded envelope.
// Retaining raw lets tests assert on the on-the-wire JSON keys directly instead
// of round-tripping through the same structs the server marshals with, which
// would mask field-name drift (e.g. PascalCase vs. the camelCase §4.6 contract).
type streamLine struct {
	raw []byte
	el  api.Event
}

// lineReader decodes ndjson api.Event values off a stream body on a goroutine so tests
// can bound each read with a timeout instead of blocking on Scanner.Scan.
type lineReader struct {
	lines chan streamLine
	errc  chan error
}

func readLines(body io.Reader) *lineReader {
	lr := &lineReader{lines: make(chan streamLine, 64), errc: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			raw := append([]byte(nil), sc.Bytes()...)
			var el api.Event
			if err := json.Unmarshal(raw, &el); err != nil {
				lr.errc <- err
				return
			}
			lr.lines <- streamLine{raw: raw, el: el}
		}
		lr.errc <- sc.Err()
	}()
	return lr
}

// next returns the next decoded line or fails on timeout.
func (lr *lineReader) next(t *testing.T, timeout time.Duration) api.Event {
	t.Helper()
	return lr.nextLine(t, timeout).el
}

// nextLine returns the next raw+decoded line or fails on timeout.
func (lr *lineReader) nextLine(t *testing.T, timeout time.Duration) streamLine {
	t.Helper()
	select {
	case sl := <-lr.lines:
		return sl
	case err := <-lr.errc:
		t.Fatalf("stream ended before a line arrived: %v", err)
	case <-time.After(timeout):
		t.Fatal("timed out waiting for an event line")
	}
	return streamLine{}
}

// waitType returns the next line whose Type == typ, skipping other lines (e.g.
// interleaved ticks), or fails on timeout.
func (lr *lineReader) waitType(t *testing.T, typ string, timeout time.Duration) api.Event {
	t.Helper()
	return lr.waitTypeLine(t, typ, timeout).el
}

// waitTypeLine returns the next raw+decoded line whose Type == typ, skipping
// other lines, or fails on timeout.
func (lr *lineReader) waitTypeLine(t *testing.T, typ string, timeout time.Duration) streamLine {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case sl := <-lr.lines:
			if sl.el.Type == typ {
				return sl
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

	line := lr.waitTypeLine(t, "notify", 2*time.Second)
	if line.el.Notify == nil {
		t.Fatal("notify line has nil notify")
	}
	if *line.el.Notify != *want {
		t.Errorf("notify = %+v, want %+v", *line.el.Notify, *want)
	}

	// Assert the on-the-wire keys are the camelCase DESIGN §4.6 contract, not
	// the PascalCase Go field names. Decoding through the same struct the server
	// marshals with (as *line.el above) round-trips either casing cleanly and so
	// cannot catch this drift — the raw bytes must be inspected directly.
	var raw struct {
		Notify map[string]json.RawMessage `json:"notify"`
	}
	if err := json.Unmarshal(line.raw, &raw); err != nil {
		t.Fatalf("unmarshal raw notify line: %v", err)
	}
	for _, key := range []string{"title", "body", "subtitle", "urgency", "verified", "source", "sound", "seq"} {
		if _, ok := raw.Notify[key]; !ok {
			t.Errorf("notify object missing camelCase key %q; got keys %v (raw: %s)", key, mapKeys(raw.Notify), line.raw)
		}
	}
}

// mapKeys returns the keys of m (for test failure messages).
func mapKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
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
// return, releasing BOTH its hub subscriptions (Coalesced + Queued) as well as
// decrementing subCount — proving no leak/backpressure. Asserting on the hub's
// own subscriber count (not just the handler-local subCount atomic) is what
// actually catches a missing cancel: a leaked subscription stays in h.subs and
// keeps receiving every Publish forever while subCount can still reach 0.
func TestEventsDisconnect(t *testing.T) {
	s, h, path := newEventsServer(t, time.Hour)

	base := h.SubscriberCount()

	resp, lr, cancel := openStream(t, path)

	if first := lr.next(t, 2*time.Second); first.Type != "snapshot" {
		t.Fatalf("first line type = %q, want snapshot", first.Type)
	}
	// The handler incremented subCount and registered both hub subscriptions.
	waitSubCount(t, s, 1)
	waitHubSubs(t, h, base+2)

	// Disconnect: cancel the request context and close the body.
	cancel()
	resp.Body.Close()

	// The handler's r.Context() is now Done; it returns, decrements subCount,
	// and its two cancel funcs remove both subscribers from the hub.
	waitSubCount(t, s, 0)
	waitHubSubs(t, h, base)

	// A Publish after disconnect must reach no leaked subscriber. If either
	// subscription leaked it would still be in h.subs; the drop counter proves
	// nothing is being delivered to a dead, un-drained Queued channel.
	before := h.DroppedNotify()
	for i := 0; i < queuedStress; i++ {
		h.Publish(hub.Event{Class: hub.Queued, Notify: &hub.Notify{Seq: uint64(i)}})
		h.Publish(hub.Event{Class: hub.Coalesced})
	}
	if got := h.DroppedNotify(); got != before {
		t.Fatalf("DroppedNotify advanced by %d after disconnect; a subscription leaked", got-before)
	}
}

// queuedStress overflows a Queued subscriber's cap-16 buffer many times over, so
// a leaked (un-drained) subscription would register drops.
const queuedStress = 128

// waitHubSubs polls the hub's own subscriber count until it equals want or fails.
func waitHubSubs(t *testing.T, h *hub.Hub, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.SubscriberCount() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("hub SubscriberCount = %d, want %d", h.SubscriberCount(), want)
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
