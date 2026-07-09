package client

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/localapi"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/doctor"
	"github.com/VikashLoomba/Portal/pkg/hub"
	"github.com/VikashLoomba/Portal/pkg/protocol"
)

// fakeAgent is a function-level localapi.AgentSource (no wire, no goroutines):
// canned HelloAck + a Snapshot whose ok flag drives the ports 503 boundary.
type fakeAgent struct {
	ack   *protocol.HelloAck
	seq   uint64
	ports []uint16
	ok    bool
}

func (f *fakeAgent) HelloAck() *protocol.HelloAck       { return f.ack }
func (f *fakeAgent) Snapshot() (uint64, []uint16, bool) { return f.seq, f.ports, f.ok }
func (f *fakeAgent) LastDisconnectErr() string          { return "" }

// shortTempDir returns a short-path (/tmp) temp dir so the unix socket sun_path
// stays under the ~104-byte limit; localapi's own tests do the same.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lcli")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// harness bundles a live localapi.Server (over a real UDS) with its fakes and a
// Client dialing it.
type harness struct {
	client   *Client
	hub      *hub.Hub
	cfg      *config.Store
	cfgDir   string
	agent    *fakeAgent
	pushed   *[][]int
	kicks    *int
	report   *doctor.Report
	sock     string
	stopSrv  context.CancelFunc
	srvError chan error
}

// cannedReport is the doctor report the fake Doctor closure returns; it spans all
// three statuses so the round-trip proves UnmarshalJSON maps each tag correctly.
func cannedReport() *doctor.Report {
	return &doctor.Report{
		Host: "devbox",
		Checks: []doctor.Check{
			{Name: "ssh master", Status: doctor.Pass, Detail: "UP (pid=1)"},
			{Name: "agent verb: clip", Status: doctor.Warn},
			{Name: "notify", Status: doctor.Fail, Detail: "no path"},
		},
	}
}

// newHarness starts a real localapi.Server on a temp socket and returns a Client
// wired to it, torn down on cleanup.
func newHarness(t *testing.T, agent *fakeAgent) *harness {
	t.Helper()
	h := &harness{
		hub:    hub.New(),
		agent:  agent,
		pushed: new([][]int),
		kicks:  new(int),
		report: cannedReport(),
	}
	h.cfgDir = t.TempDir()
	h.cfg = config.New(h.cfgDir)
	h.sock = filepath.Join(shortTempDir(t), "api.sock")

	srv := localapi.New(localapi.Deps{
		Version: api.VersionInfo{Version: "9.9", GitSHA: "deadbeef", ProtoVersion: 3},
		Host:    func() (string, error) { return "devbox", nil },
		Agent:   agent,
		Config:  h.cfg,
		Hub:     h.hub,
		PushAllow: func(ports []int) error {
			cp := append([]int(nil), ports...)
			*h.pushed = append(*h.pushed, cp)
			return nil
		},
		Kick:   func() { *h.kicks++ },
		Doctor: func(context.Context) *doctor.Report { return h.report },
	})

	ln, err := localapi.Listen(h.sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.stopSrv = cancel
	h.srvError = make(chan error, 1)
	go func() { h.srvError <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.srvError:
		case <-time.After(3 * time.Second):
			t.Error("Serve did not return after cancel")
		}
	})

	h.client = New(h.sock)
	waitAvailable(t, h.client)
	return h
}

// waitAvailable blocks until the server answers, so tests never race startup.
func waitAvailable(t *testing.T, c *Client) {
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

func TestClientHappyPath(t *testing.T) {
	agent := &fakeAgent{
		ack:   &protocol.HelloAck{AgentPID: 777, AgentGitSHA: "abc123", Kernel: "6.1", BootID: "boot-1"},
		seq:   5,
		ports: []uint16{5000, 6000},
		ok:    true,
	}
	h := newHarness(t, agent)
	ctx := context.Background()

	t.Run("Available", func(t *testing.T) {
		if !h.client.Available(ctx) {
			t.Fatal("Available = false, want true against a live server")
		}
	})

	t.Run("Status", func(t *testing.T) {
		st, err := h.client.Status(ctx)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.Agent == nil {
			t.Fatal("Status.Agent is nil, want the handshake identity")
		}
		if st.Agent.Pid != 777 || st.Agent.SHA != "abc123" {
			t.Errorf("Agent = %+v, want Pid=777 SHA=abc123", *st.Agent)
		}
		if st.Version.Version != "9.9" {
			t.Errorf("Version = %q, want 9.9", st.Version.Version)
		}
	})

	t.Run("Ports", func(t *testing.T) {
		ports, err := h.client.Ports(ctx)
		if err != nil {
			t.Fatalf("Ports: %v", err)
		}
		if len(ports) != 2 || ports[0].Port != 5000 || ports[1].Port != 6000 {
			t.Errorf("ports = %+v, want [5000 6000]", ports)
		}
	})

	t.Run("Allow then Unallow", func(t *testing.T) {
		before := len(*h.pushed)
		allowed, err := h.client.Allow(ctx, 40085)
		if err != nil {
			t.Fatalf("Allow: %v", err)
		}
		if !containsInt(allowed, 40085) {
			t.Errorf("allowed = %v, want to contain 40085", allowed)
		}
		if len(*h.pushed) != before+1 {
			t.Fatalf("PushAllow fired %d times, want 1 more (%d)", len(*h.pushed), before+1)
		}
		if last := (*h.pushed)[len(*h.pushed)-1]; !containsInt(last, 40085) {
			t.Errorf("last push = %v, want to contain 40085", last)
		}

		allowed, err = h.client.Unallow(ctx, 40085)
		if err != nil {
			t.Fatalf("Unallow: %v", err)
		}
		if containsInt(allowed, 40085) {
			t.Errorf("allowed = %v, want 40085 removed", allowed)
		}
		if len(*h.pushed) != before+2 {
			t.Errorf("PushAllow fired %d times total, want %d", len(*h.pushed), before+2)
		}
	})

	t.Run("Features and SetFeature round-trip through config.Store", func(t *testing.T) {
		feats, err := h.client.Features(ctx)
		if err != nil {
			t.Fatalf("Features: %v", err)
		}
		// Default posture: every gate ON.
		if !feats[config.FeatureNotify] {
			t.Errorf("feature %q = false by default, want true", config.FeatureNotify)
		}

		got, err := h.client.SetFeature(ctx, config.FeatureNotify, false)
		if err != nil {
			t.Fatalf("SetFeature: %v", err)
		}
		if got[config.FeatureNotify] {
			t.Errorf("SetFeature returned %q = true, want false", config.FeatureNotify)
		}
		// Assert the change hit the real config file on disk.
		b, err := os.ReadFile(filepath.Join(h.cfgDir, "feature."+config.FeatureNotify))
		if err != nil {
			t.Fatalf("read feature file: %v", err)
		}
		if trimmed := string(b); trimmed != "off\n" {
			t.Errorf("feature file = %q, want %q", trimmed, "off\n")
		}
	})

	t.Run("SetFeature unknown feature", func(t *testing.T) {
		_, err := h.client.SetFeature(ctx, "does-not-exist", true)
		if !errors.Is(err, ErrFeatureUnknown) {
			t.Fatalf("SetFeature unknown = %v, want ErrFeatureUnknown", err)
		}
	})

	t.Run("Reconcile advances the Kick counter", func(t *testing.T) {
		before := *h.kicks
		if err := h.client.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if *h.kicks != before+1 {
			t.Errorf("Kick counter = %d, want %d", *h.kicks, before+1)
		}
	})

	t.Run("Doctor decodes into *doctor.Report", func(t *testing.T) {
		rep, err := h.client.Doctor(ctx)
		if err != nil {
			t.Fatalf("Doctor: %v", err)
		}
		if rep.Host != "devbox" {
			t.Errorf("report Host = %q, want devbox", rep.Host)
		}
		want := cannedReport()
		if len(rep.Checks) != len(want.Checks) {
			t.Fatalf("report has %d checks, want %d", len(rep.Checks), len(want.Checks))
		}
		for i, c := range rep.Checks {
			// Tag() reads the decoded uint8 Status: proves UnmarshalJSON mapped the
			// wire tag back to the right enum value end-to-end over the socket.
			if c.Status.Tag() != want.Checks[i].Status.Tag() {
				t.Errorf("check %d %q status Tag = %q, want %q", i, c.Name, c.Status.Tag(), want.Checks[i].Status.Tag())
			}
			if c.Name != want.Checks[i].Name {
				t.Errorf("check %d name = %q, want %q", i, c.Name, want.Checks[i].Name)
			}
		}
	})
}

// TestPortsNotConnected proves the ports 503 boundary maps to the sentinel: with
// the fake Snapshot reporting ok=false, Ports returns ErrNotConnected.
func TestPortsNotConnected(t *testing.T) {
	h := newHarness(t, &fakeAgent{ok: false})
	_, err := h.client.Ports(context.Background())
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Ports = %v, want ErrNotConnected", err)
	}
}

// TestEvents exercises the snapshot-first line, a state delta after a Coalesced
// Publish, and the terminal delivered on server shutdown — the BaseContext fixup.
func TestEvents(t *testing.T) {
	h := newHarness(t, &fakeAgent{ok: true, ports: []uint16{5000}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, errc, err := h.client.Events(ctx)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}

	// First line is always a populated snapshot.
	first := recvEvent(t, events, 2*time.Second)
	if first.Type != "snapshot" {
		t.Fatalf("first event type = %q, want snapshot", first.Type)
	}
	if first.Status == nil || first.Status.Version.Version != "9.9" {
		t.Fatalf("snapshot status = %+v, want populated Status", first.Status)
	}

	// A Coalesced Publish yields a full-Status state event.
	h.hub.Publish(hub.Event{Class: hub.Coalesced})
	state := waitEventType(t, events, "state", 2*time.Second)
	if state.Status == nil || state.Status.Version.Version != "9.9" {
		t.Fatalf("state status = %+v, want populated Status", state.Status)
	}

	// Shut the server down (cancel its Serve ctx). The BaseContext fixup cancels
	// the events handler's request ctx, so net/http finalizes the chunked body and
	// the client goroutine sees a terminal: errc yields (nil clean EOF or a benign
	// closed-conn read error) AND events is closed within the deadline.
	h.stopSrv()

	select {
	case err := <-errc:
		// nil (clean EOF) or a benign read error are both acceptable terminals.
		_ = err
	case <-time.After(3 * time.Second):
		t.Fatal("errc did not yield a terminal after server shutdown")
	}
	// events must be closed after the terminal.
	select {
	case _, ok := <-events:
		if ok {
			// Drain any buffered lines, then require closure.
			deadline := time.After(2 * time.Second)
			for {
				select {
				case _, ok := <-events:
					if !ok {
						return
					}
				case <-deadline:
					t.Fatal("events channel not closed after shutdown")
				}
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("events channel not closed after shutdown")
	}
}

// recvEvent returns the next event or fails on timeout.
func recvEvent(t *testing.T, events <-chan api.Event, timeout time.Duration) api.Event {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("events channel closed before an event arrived")
		}
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for an event")
	}
	return api.Event{}
}

// waitEventType returns the next event whose Type == typ, skipping others (e.g.
// ticks), or fails on timeout.
func waitEventType(t *testing.T, events <-chan api.Event, typ string, timeout time.Duration) api.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("events channel closed before a %q event", typ)
			}
			if ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %q event", typ)
		}
	}
}

// TestDaemonDown proves EC2 at the transport layer: three flavors of "no live
// daemon" each make every method error and Available() false.
func TestDaemonDown(t *testing.T) {
	t.Run("no socket", func(t *testing.T) {
		dir := shortTempDir(t)
		c := New(filepath.Join(dir, "nonexistent.sock"))
		assertAllMethodsError(t, c, context.Background(), 2*time.Second)
	})

	t.Run("dead socket (plain file at the path)", func(t *testing.T) {
		dir := shortTempDir(t)
		path := filepath.Join(dir, "api.sock")
		if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
			t.Fatalf("write dead socket file: %v", err)
		}
		c := New(path)
		assertAllMethodsError(t, c, context.Background(), 2*time.Second)
	})

	t.Run("hung server times out", func(t *testing.T) {
		c := newHungServer(t)
		// A genuinely unbounded base ctx: assertAllMethodsError gives each method
		// its own 200ms budget, so each dials the hung listener and times out on
		// its OWN deadline (proving no method ignores its context) rather than
		// riding a single pre-expired context that only the first call spends.
		assertAllMethodsError(t, c, context.Background(), 200*time.Millisecond)
	})
}

// newHungServer returns a Client dialing a listener that accepts connections but
// never writes a response, so every request blocks until its ctx deadline.
func newHungServer(t *testing.T) *Client {
	t.Helper()
	dir := shortTempDir(t)
	path := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
		}
	}()
	return New(path)
}

// TestHungServerPerCallTimeout proves the per-call StatusTimeout/ProbeTimeout in
// newReq are the ONLY thing that ends a request against a wedged daemon when the
// caller passes an UNBOUNDED context — which is exactly what the CLI does (cobra's
// root.Execute runs on context.Background()). Every request here rides
// context.Background(); only shrinkTimeouts lowers the package defaults. Dropping
// newReq's context.WithTimeout would make these calls hang forever, so this test
// would hang and fail — the regression the other cases (which inject their own
// bounded context) cannot catch.
//
// Doctor is deliberately excluded: it has NO per-call cap (it is long-running and
// rides the caller's ctx, §4.5), so under an unbounded ctx it MUST hang. Its
// context-honoring contract is covered by TestDoctorHonorsContextDeadline (and by
// the hung-server case in assertAllMethodsError, which passes a bounded ctx).
func TestHungServerPerCallTimeout(t *testing.T) {
	c := newHungServer(t)
	restore := shrinkTimeouts(200 * time.Millisecond)
	defer restore()

	ctx := context.Background()
	if c.Available(ctx) {
		t.Error("Available = true against a hung server, want false")
	}
	if _, err := c.Status(ctx); err == nil {
		t.Error("Status err = nil, want a per-call StatusTimeout error")
	}
	if _, err := c.Ports(ctx); err == nil {
		t.Error("Ports err = nil, want a per-call StatusTimeout error")
	}
	if _, err := c.Allow(ctx, 40085); err == nil {
		t.Error("Allow err = nil, want a per-call StatusTimeout error")
	}
	// SetFeature is the PUT-with-body method; it must honor the per-call
	// StatusTimeout under an unbounded ctx exactly like its GET/DELETE siblings.
	// Before it was routed through newReq this case would hang forever if that
	// inline timeout were dropped.
	if _, err := c.SetFeature(ctx, config.FeatureNotify, false); err == nil {
		t.Error("SetFeature err = nil, want a per-call StatusTimeout error")
	}
}

// TestDoctorHonorsContextDeadline locks the long-running contract: Doctor imposes
// no artificial per-call cap and instead ends exactly when the CALLER's context
// does. Against a hung server (never writes a response) a bounded ctx must make
// Doctor return its context error — not hang, and not return early on some fixed
// budget of its own. This is the regression guard for the removed DoctorTimeout:
// if a small fixed cap were reintroduced, Doctor would stop ignoring the caller's
// deadline and this contract (a slow-but-healthy daemon run is the caller's to
// bound or cancel) would silently break.
func TestDoctorHonorsContextDeadline(t *testing.T) {
	c := newHungServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Doctor(ctx); err == nil {
		t.Fatal("Doctor err = nil against a hung server, want the ctx deadline error")
	}
	// It must have waited for roughly the caller's budget, not returned on a
	// smaller cap of its own.
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("Doctor returned after %v, want it to honor the ~200ms ctx deadline", elapsed)
	}
}

// shrinkTimeouts lowers the package default timeouts (for the hung-server case)
// and returns a restore func. Serialized because the vars are package-global; the
// daemon-down subtests do not run in parallel. Doctor has no per-call cap to
// shrink — it rides the caller's ctx — so it is not touched here.
func shrinkTimeouts(d time.Duration) func() {
	os, ps := StatusTimeout, ProbeTimeout
	StatusTimeout, ProbeTimeout = d, d
	return func() { StatusTimeout, ProbeTimeout = os, ps }
}

// assertAllMethodsError calls every non-streaming method and Available and
// requires each to fail (no partial success) against a down/hung daemon. Each
// method gets its OWN fresh WithTimeout(base, per) context rather than a single
// shared one: against a hung listener a shared context is consumed by the first
// call, so later calls would return on the already-cancelled context WITHOUT
// ever dialing — falsely "passing" a method that ignored its context. A fresh
// per-method budget makes every method genuinely exercise the hung path. `per`
// only bites the hung case; for a down socket the dial fails immediately.
func assertAllMethodsError(t *testing.T, c *Client, base context.Context, per time.Duration) {
	t.Helper()
	fresh := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(base, per)
	}
	{
		ctx, cancel := fresh()
		if c.Available(ctx) {
			t.Error("Available = true against a down daemon, want false")
		}
		cancel()
	}
	call := func(name string, fn func(ctx context.Context) error) {
		ctx, cancel := fresh()
		defer cancel()
		if err := fn(ctx); err == nil {
			t.Errorf("%s err = nil, want an error against a down daemon", name)
		}
	}
	call("Status", func(ctx context.Context) error { _, err := c.Status(ctx); return err })
	call("Ports", func(ctx context.Context) error { _, err := c.Ports(ctx); return err })
	call("Allow", func(ctx context.Context) error { _, err := c.Allow(ctx, 40085); return err })
	call("Unallow", func(ctx context.Context) error { _, err := c.Unallow(ctx, 40085); return err })
	call("Features", func(ctx context.Context) error { _, err := c.Features(ctx); return err })
	call("SetFeature", func(ctx context.Context) error {
		_, err := c.SetFeature(ctx, config.FeatureNotify, true)
		return err
	})
	call("Reconcile", func(ctx context.Context) error { return c.Reconcile(ctx) })
	call("Doctor", func(ctx context.Context) error { _, err := c.Doctor(ctx); return err })
	call("Events", func(ctx context.Context) error { _, _, err := c.Events(ctx); return err })
}

// containsInt reports whether s contains v.
func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
