package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/agent/watcher"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/protocol"
)

// connPair holds two unidirectional pipes: client→agent and agent→client.
type connPair struct {
	c2aR *io.PipeReader
	c2aW *io.PipeWriter
	a2cR *io.PipeReader
	a2cW *io.PipeWriter
}

func newConnPair() *connPair {
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	return &connPair{c2aR: c2aR, c2aW: c2aW, a2cR: a2cR, a2cW: a2cW}
}

func (p *connPair) close() {
	p.c2aW.Close()
	p.c2aR.Close()
	p.a2cW.Close()
	p.a2cR.Close()
}

// runServer launches the Server in a goroutine, returns the wait func.
func runServer(t *testing.T, w watcher.Watcher, conn *connPair) (context.CancelFunc, *sync.WaitGroup, *errCapture) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := New(Config{
		In:                conn.c2aR,
		Out:               conn.a2cW,
		Watcher:           w,
		AgentSHA:          "testsha",
		Kernel:            "linux-test",
		BootID:            "00000000-0000-0000-0000-000000000000",
		EphemMin:          32768,
		EphemMax:          60999,
		HeartbeatInterval: time.Hour, // disable heartbeat in tests
	})
	cap := &errCapture{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := srv.Serve(ctx)
		cap.set(err)
		conn.a2cW.Close()
	}()
	return cancel, &wg, cap
}

type errCapture struct {
	mu sync.Mutex
	v  error
}

func (e *errCapture) set(err error) { e.mu.Lock(); e.v = err; e.mu.Unlock() }
func (e *errCapture) get() error    { e.mu.Lock(); defer e.mu.Unlock(); return e.v }

func TestServer_HandshakeAndSnapshot(t *testing.T) {
	w := watcher.NewFake()
	w.SetSnapshot([]watcher.Listen{
		{Port: 8081, Family: 4, Addr: "127.0.0.1"},
		{Port: 9111, Family: 4, Addr: "127.0.0.1"},
	})
	conn := newConnPair()
	defer conn.close()

	cancel, wg, _ := runServer(t, w, conn)
	defer cancel()

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)

	// Hello → HelloAck
	if err := enc.Write(&protocol.Envelope{Hello: &protocol.Hello{
		ProtoVersion: protocol.ProtoVersion, ClientGitSHA: "client-sha",
	}}); err != nil {
		t.Fatal(err)
	}
	ack, err := dec.Read()
	if err != nil {
		t.Fatal(err)
	}
	if ack.HelloAck == nil || ack.HelloAck.AgentGitSHA != "testsha" {
		t.Fatalf("HelloAck mismatch: %+v", ack)
	}

	// Subscribe → SubscribeAck → Snapshot
	if err := enc.Write(&protocol.Envelope{Subscribe: &protocol.Subscribe{
		Deny: []uint16{22, 25}, Allow: []uint16{40085}, ExcludeEphemeral: true,
		ResubscribeID: 1,
	}}); err != nil {
		t.Fatal(err)
	}

	subAck, err := dec.Read()
	if err != nil {
		t.Fatal(err)
	}
	if subAck.SubscribeAck == nil || subAck.SubscribeAck.ResubscribeID != 1 {
		t.Fatalf("SubscribeAck mismatch: %+v", subAck)
	}
	snap, err := dec.Read()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Snapshot == nil {
		t.Fatalf("expected Snapshot, got %+v", snap)
	}
	if len(snap.Snapshot.Ports) != 2 {
		t.Errorf("expected 2 ports, got %d (%v)", len(snap.Snapshot.Ports), snap.Snapshot.Ports)
	}

	// Clean shutdown
	if err := enc.Write(&protocol.Envelope{Shutdown: &protocol.Shutdown{Reason: "test-done"}}); err != nil {
		t.Fatal(err)
	}
	bye, err := dec.Read()
	if err != nil {
		t.Fatal(err)
	}
	if bye.Bye == nil {
		t.Errorf("expected Bye, got %+v", bye)
	}
	conn.c2aW.Close()
	wg.Wait()
}

func TestServer_ProtoVersionMismatch(t *testing.T) {
	// pv==3 is the REAL 3-vs-4 boundary: v4 deleted the feature-frame fields, so
	// a v3 agent/client pairing must fatal loudly (SHA-keyed re-upload heals it).
	// pv==99 keeps the far-future case covered.
	for _, pv := range []uint32{3, 99} {
		pv := pv
		t.Run(fmt.Sprintf("pv=%d", pv), func(t *testing.T) {
			w := watcher.NewFake()
			conn := newConnPair()
			defer conn.close()
			_, wg, cap := runServer(t, w, conn)

			enc := protocol.NewEncoder(conn.c2aW)
			dec := protocol.NewDecoder(conn.a2cR)

			enc.Write(&protocol.Envelope{Hello: &protocol.Hello{ProtoVersion: pv}})
			env, err := dec.Read()
			if err != nil {
				t.Fatal(err)
			}
			if env.AgentError == nil || env.AgentError.Code != protocol.CodeProtocolMismatch || !env.AgentError.Fatal {
				t.Errorf("expected fatal AgentError CodeProtocolMismatch, got %+v", env)
			}
			conn.c2aW.Close()
			wg.Wait()
			if cap.get() == nil {
				t.Errorf("expected non-nil server error on version mismatch")
			}
		})
	}
}

// warnCounter is a minimal slog.Handler that counts Warn+ records, used to
// assert "exactly one warning" on service-version mismatch (EC10).
type warnCounter struct {
	mu sync.Mutex
	n  int
}

func (w *warnCounter) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		w.mu.Lock()
		w.n++
		w.mu.Unlock()
	}
	return nil
}
func (w *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnCounter) WithGroup(string) slog.Handler      { return w }
func (w *warnCounter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

// newOpenHarness drives a Server with a real cmd socket and a subscribed client
// that advertises clientSvcs. Its client goroutine drains agent→client frames,
// forwarding every Msg frame onto the returned channel so a test can assert the
// emitted service frames. log (nil ⇒ discard) captures the agent's slog output.
func newOpenHarness(t *testing.T, clientSvcs map[string]uint32, log *slog.Logger) (sockPath string, msgs chan *protocol.Envelope, cleanup func()) {
	t.Helper()
	sockPath = tempSockPath(t)
	w := watcher.NewFake()
	w.SetSnapshot(nil)
	conn := newConnPair()
	ctx, cancel := context.WithCancel(context.Background())
	srv := New(Config{
		In: conn.c2aR, Out: conn.a2cW, Watcher: w, AgentSHA: "testsha",
		HeartbeatInterval: time.Hour, CmdSockPath: sockPath, Log: log,
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(ctx); conn.a2cW.Close() }()

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	if err := enc.Write(&protocol.Envelope{Hello: &protocol.Hello{
		ProtoVersion: protocol.ProtoVersion, Services: clientSvcs,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Read(); err != nil { // HelloAck
		t.Fatal(err)
	}
	if err := enc.Write(&protocol.Envelope{Subscribe: &protocol.Subscribe{ResubscribeID: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Read(); err != nil { // SubscribeAck
		t.Fatal(err)
	}
	if _, err := dec.Read(); err != nil { // initial Snapshot
		t.Fatal(err)
	}

	msgs = make(chan *protocol.Envelope, 64)
	var clientWG sync.WaitGroup
	clientWG.Add(1)
	go func() {
		defer clientWG.Done()
		for {
			env, err := dec.Read()
			if err != nil {
				return
			}
			if env.Msg != nil {
				msgs <- env
			}
		}
	}()

	// Wait for the cmd socket.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := net.DialTimeout("unix", sockPath, 100*time.Millisecond); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cmd socket did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cleanup = func() {
		cancel()
		conn.c2aW.Close()
		wg.Wait()
		clientWG.Wait()
		conn.close()
	}
	return sockPath, msgs, cleanup
}

// askSock dials sockPath, writes line, and returns the raw reply.
func askSock(t *testing.T, sockPath, line string) string {
	t.Helper()
	c, err := net.DialTimeout("unix", sockPath, time.Second)
	if err != nil {
		t.Fatalf("dial cmd sock: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte(line)); err != nil {
		t.Fatalf("write verb: %v", err)
	}
	buf := make([]byte, 256)
	n, _ := c.Read(buf)
	return string(buf[:n])
}

// TestServer_OpenURLService_E2E drives the "open" cmd verb end-to-end through
// the migrated openurl service (EC2/EC3): an accepted http URL surfaces as a
// single Envelope{Msg} with Service=="openurl", Kind=="open" and the URL in the
// OpenURL payload; a non-http URL is byte-identically "rejected\n".
func TestServer_OpenURLService_E2E(t *testing.T) {
	sock, msgs, cleanup := newOpenHarness(t, map[string]uint32{"openurl": 1}, nil)
	defer cleanup()

	if got := askSock(t, sock, "open\thttp://example.com/x\n"); got != "ok\n" {
		t.Fatalf("open http reply = %q, want %q", got, "ok\n")
	}
	env := <-msgs
	if env.Msg == nil || env.Msg.Service != "openurl" || env.Msg.Kind != "open" {
		t.Fatalf("frame = %+v, want Msg{svc=openurl,kind=open}", env)
	}
	ou, err := protocol.UnmarshalPayload[protocol.OpenURL](env.Msg.Payload)
	if err != nil {
		t.Fatalf("decode OpenURL payload: %v", err)
	}
	if ou.URL != "http://example.com/x" {
		t.Fatalf("payload URL = %q, want %q", ou.URL, "http://example.com/x")
	}

	// Non-http/https → "rejected\n" (checked before the client gate).
	if got := askSock(t, sock, "open\tfile:///etc/passwd\n"); got != "rejected\n" {
		t.Fatalf("open file:// reply = %q, want %q", got, "rejected\n")
	}
}

// TestServer_OpenURLService_NoClientWhenUnadvertised: a subscribed client that
// did NOT advertise openurl gates the open verb to "no-client\n" (EC2 slice) —
// the agent answers exactly as it would with no client at all.
func TestServer_OpenURLService_NoClientWhenUnadvertised(t *testing.T) {
	sock, _, cleanup := newOpenHarness(t, nil, nil) // client advertises no services
	defer cleanup()

	if got := askSock(t, sock, "open\thttp://example.com\n"); got != "no-client\n" {
		t.Fatalf("open reply = %q, want %q", got, "no-client\n")
	}
}

// TestServer_OpenURLService_BudgetRecycles is the release-guard proof at the full
// stack: with the client actively reading frames, driving (outbox cap + several)
// "open" verbs must EACH answer "ok\n" and EACH URL must arrive as a distinct
// Msg. Omitting the Serve drain arm's reg.release would wedge the openurl
// admission budget after the first cap emits and every later open would drop.
func TestServer_OpenURLService_BudgetRecycles(t *testing.T) {
	sock, msgs, cleanup := newOpenHarness(t, map[string]uint32{"openurl": 1}, nil)
	defer cleanup()

	const n = 8 + 5 // openurl OutboxCap is 8; exceed it to force budget recycle
	for i := 0; i < n; i++ {
		url := fmt.Sprintf("http://example.com/%d", i)
		if got := askSock(t, sock, "open\t"+url+"\n"); got != "ok\n" {
			t.Fatalf("open #%d reply = %q, want %q", i, got, "ok\n")
		}
		env := <-msgs
		ou, err := protocol.UnmarshalPayload[protocol.OpenURL](env.Msg.Payload)
		if err != nil {
			t.Fatalf("open #%d decode: %v", i, err)
		}
		if ou.URL != url {
			t.Fatalf("open #%d URL = %q, want %q", i, ou.URL, url)
		}
	}
}

// TestServer_OpenURLService_VersionMismatch: the client advertises openurl@2
// while the agent registered openurl@1 (EC10 mechanism). Exact-equality means
// the agent treats the service as absent, so the open verb answers "no-client\n"
// and setClientServices logs exactly one warning.
func TestServer_OpenURLService_VersionMismatch(t *testing.T) {
	wc := &warnCounter{}
	sock, _, cleanup := newOpenHarness(t, map[string]uint32{"openurl": 2}, slog.New(wc))
	defer cleanup()

	if got := askSock(t, sock, "open\thttp://example.com\n"); got != "no-client\n" {
		t.Fatalf("open reply = %q, want %q (version mismatch means absent)", got, "no-client\n")
	}
	if got := wc.count(); got != 1 {
		t.Fatalf("agent warnings = %d, want exactly 1", got)
	}
}

func TestServer_PortAddedAndRemoved(t *testing.T) {
	w := watcher.NewFake()
	w.SetSnapshot(nil) // start empty
	conn := newConnPair()
	defer conn.close()
	cancel, wg, _ := runServer(t, w, conn)
	defer cancel()

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)

	// Handshake
	enc.Write(&protocol.Envelope{Hello: &protocol.Hello{ProtoVersion: protocol.ProtoVersion}})
	if _, err := dec.Read(); err != nil { // HelloAck
		t.Fatal(err)
	}
	enc.Write(&protocol.Envelope{Subscribe: &protocol.Subscribe{ResubscribeID: 1, ExcludeEphemeral: true}})
	dec.Read() // SubscribeAck
	snap, _ := dec.Read()
	if snap.Snapshot == nil || len(snap.Snapshot.Ports) != 0 {
		t.Fatalf("initial snapshot should be empty: %+v", snap)
	}

	// Emit Add → expect PortAdded
	w.Emit(watcher.Event{
		Kind: watcher.KindAdd, At: time.Now(),
		Listen: watcher.Listen{Port: 8081, Family: 4, Addr: "127.0.0.1"},
	})
	added, err := dec.Read()
	if err != nil {
		t.Fatal(err)
	}
	if added.PortAdded == nil || added.PortAdded.Port.Port != 8081 {
		t.Fatalf("expected PortAdded(8081), got %+v", added)
	}

	// Emit Remove → expect PortRemoved
	w.Emit(watcher.Event{
		Kind: watcher.KindRemove, At: time.Now(),
		Listen: watcher.Listen{Port: 8081, Family: 4, Addr: "127.0.0.1"},
		Source: protocol.SourceDestroyMulti,
	})
	rem, err := dec.Read()
	if err != nil {
		t.Fatal(err)
	}
	if rem.PortRemoved == nil || rem.PortRemoved.Port != 8081 || rem.PortRemoved.Source != protocol.SourceDestroyMulti {
		t.Fatalf("expected PortRemoved(8081, src=2), got %+v", rem)
	}

	// Sequence numbers should be monotonic.
	if snap.Snapshot.Seq >= added.PortAdded.Seq {
		t.Errorf("seq should be monotonic: snap=%d added=%d", snap.Snapshot.Seq, added.PortAdded.Seq)
	}
	if added.PortAdded.Seq >= rem.PortRemoved.Seq {
		t.Errorf("seq should be monotonic: added=%d removed=%d", added.PortAdded.Seq, rem.PortRemoved.Seq)
	}

	conn.c2aW.Close()
	wg.Wait()
}

func TestServer_DedupAddedAndRemovedNoOps(t *testing.T) {
	// If the watcher emits a duplicate Add for an already-emitted port, the
	// agent must NOT push a second PortAdded — server keeps lastEmitted set.
	w := watcher.NewFake()
	w.SetSnapshot([]watcher.Listen{{Port: 8081, Family: 4, Addr: "127.0.0.1"}})
	conn := newConnPair()
	defer conn.close()
	cancel, wg, _ := runServer(t, w, conn)
	defer cancel()

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	enc.Write(&protocol.Envelope{Hello: &protocol.Hello{ProtoVersion: protocol.ProtoVersion}})
	dec.Read()
	enc.Write(&protocol.Envelope{Subscribe: &protocol.Subscribe{ResubscribeID: 1}})
	dec.Read()
	dec.Read() // initial Snapshot — already contains 8081

	// Duplicate Add → no frame.
	w.Emit(watcher.Event{Kind: watcher.KindAdd, At: time.Now(),
		Listen: watcher.Listen{Port: 8081, Family: 4, Addr: "127.0.0.1"}})

	// Send a Ping; expect Heartbeat back. If a duplicate Added had been
	// emitted we'd get that first instead.
	enc.Write(&protocol.Envelope{Ping: &protocol.Ping{Nonce: 1}})
	resp, err := dec.Read()
	if err != nil {
		t.Fatal(err)
	}
	if resp.Heartbeat == nil {
		t.Errorf("expected Heartbeat, got %+v (likely a duplicate PortAdded)", resp)
	}

	conn.c2aW.Close()
	wg.Wait()
}

// ---------------------------------------------------------------------------
// u6: hardening + full v4 exit-criteria suite (agent side).
// ---------------------------------------------------------------------------

// u6Harness drives a real Server with a live cmd socket and a subscribed client
// that advertises clientSvcs. Its client goroutine drains agent→client frames:
// clip request Msgs are answered from a programmable template (setClip); other
// service Msgs (openurl/notify) land on msgs; port/snapshot frames land on
// ports. Sends onto msgs/ports are NON-BLOCKING so the client reader never
// stalls the Serve loop's io.Pipe writes (clean teardown regardless of drains).
type u6Harness struct {
	srv      *Server
	w        *watcher.Fake
	sockPath string
	snapSeq  uint64
	cancel   context.CancelFunc
	wg       *sync.WaitGroup
	clientWG *sync.WaitGroup
	conn     *connPair
	enc      *protocol.Encoder

	mu          sync.Mutex
	clipRespond bool
	clipTmpl    protocol.ClipResponse

	msgs  chan *protocol.Envelope
	ports chan *protocol.Envelope
}

func newU6Harness(t *testing.T, clientSvcs map[string]uint32, log *slog.Logger) *u6Harness {
	t.Helper()
	sockPath := tempSockPath(t)
	w := watcher.NewFake()
	w.SetSnapshot(nil)
	conn := newConnPair()
	ctx, cancel := context.WithCancel(context.Background())
	srv := New(Config{
		In: conn.c2aR, Out: conn.a2cW, Watcher: w, AgentSHA: "testsha",
		HeartbeatInterval: time.Hour, CmdSockPath: sockPath, Log: log,
	})
	h := &u6Harness{
		srv: srv, w: w, sockPath: sockPath, cancel: cancel, conn: conn,
		msgs:  make(chan *protocol.Envelope, 128),
		ports: make(chan *protocol.Envelope, 64),
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(ctx); conn.a2cW.Close() }()
	h.wg = &wg

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	h.enc = enc
	if err := enc.Write(&protocol.Envelope{Hello: &protocol.Hello{
		ProtoVersion: protocol.ProtoVersion, Services: clientSvcs,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Read(); err != nil { // HelloAck
		t.Fatal(err)
	}
	if err := enc.Write(&protocol.Envelope{Subscribe: &protocol.Subscribe{ResubscribeID: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Read(); err != nil { // SubscribeAck
		t.Fatal(err)
	}
	snap, err := dec.Read() // initial Snapshot
	if err != nil {
		t.Fatal(err)
	}
	if snap.Snapshot == nil {
		t.Fatalf("expected initial Snapshot, got %+v", snap)
	}
	h.snapSeq = snap.Snapshot.Seq

	var clientWG sync.WaitGroup
	clientWG.Add(1)
	go func() {
		defer clientWG.Done()
		for {
			env, err := dec.Read()
			if err != nil {
				return
			}
			switch {
			case env.Msg != nil && env.Msg.Service == "clip" && env.Msg.Kind == "req":
				h.mu.Lock()
				respond, tmpl := h.clipRespond, h.clipTmpl
				h.mu.Unlock()
				if !respond {
					continue
				}
				req, err := protocol.UnmarshalPayload[protocol.ClipRequest](env.Msg.Payload)
				if err != nil {
					continue
				}
				resp := tmpl
				resp.Nonce, resp.Epoch = req.Nonce, req.Epoch
				_ = sendClipResp(enc, &resp)
			case env.Msg != nil:
				select {
				case h.msgs <- env:
				default:
				}
			case env.PortAdded != nil || env.PortRemoved != nil || env.Snapshot != nil:
				select {
				case h.ports <- env:
				default:
				}
			}
		}
	}()
	h.clientWG = &clientWG

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := net.DialTimeout("unix", sockPath, 100*time.Millisecond); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cmd socket did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return h
}

func (h *u6Harness) setClip(tmpl protocol.ClipResponse) {
	h.mu.Lock()
	h.clipRespond = true
	h.clipTmpl = tmpl
	h.mu.Unlock()
}

func (h *u6Harness) nextMsg(t *testing.T) *protocol.Envelope {
	t.Helper()
	select {
	case e := <-h.msgs:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no service Msg arrived")
		return nil
	}
}

func (h *u6Harness) nextPort(t *testing.T) *protocol.Envelope {
	t.Helper()
	select {
	case e := <-h.ports:
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no port frame arrived")
		return nil
	}
}

func (h *u6Harness) close() {
	h.cancel()
	h.conn.c2aW.Close()
	h.wg.Wait()
	h.clientWG.Wait()
	h.conn.close()
}

// TestServer_RecordsClientServices (EC2, forward direction) asserts the agent
// records the client's advertised Services from Hello — the in-package direct
// read that complements the e2e's HelloAck.Services (reverse direction) and
// behavioral clientHas() gating checks.
func TestServer_RecordsClientServices(t *testing.T) {
	h := newU6Harness(t, map[string]uint32{"openurl": 1, "notify": 1, "clip": 1}, nil)
	defer h.close()

	h.srv.reg.mu.Lock()
	got := make(map[string]uint32, len(h.srv.reg.clientSvcs))
	for k, v := range h.srv.reg.clientSvcs {
		got[k] = v
	}
	h.srv.reg.mu.Unlock()

	for _, svc := range []string{"openurl", "notify", "clip"} {
		if got[svc] != 1 {
			t.Fatalf("recorded clientSvcs[%q] = %d, want 1 (got %v)", svc, got[svc], got)
		}
	}
}

// TestServer_CmdSocketGrammarGolden (EC3) pins the cmd-socket byte grammar to
// HARD-CODED expected strings, identical to v3, for every verb — proving the
// deployed shims keep working with ZERO redeployment after the Msg migration.
func TestServer_CmdSocketGrammarGolden(t *testing.T) {
	h := newU6Harness(t, map[string]uint32{"openurl": 1, "notify": 1, "clip": 1}, nil)
	defer h.close()

	const sha = "0123456789abcdef0123456789abcdef"

	golden := func(line, want string) {
		t.Helper()
		if got := askSock(t, h.sockPath, line); got != want {
			t.Fatalf("ask(%q) = %q, want %q", line, got, want)
		}
	}

	// open
	golden("open\thttp://example.com/x\n", "ok\n")
	golden("open\tfile:///etc/passwd\n", "rejected\n")
	// notify
	golden("notify\t{\"title\":\"hi\"}\n", "ok\n")
	// clip targets → canonical kind
	h.setClip(protocol.ClipResponse{OK: true, Has: true})
	golden("clip\ttargets\n", "ok\timage\n")
	h.setClip(protocol.ClipResponse{OK: true, Has: true, Kind: "text"})
	golden("clip\ttargets\n", "ok\ttext\n")
	h.setClip(protocol.ClipResponse{OK: true, Has: false})
	golden("clip\ttargets\n", "none\n")
	// clip image/text → sha
	h.setClip(protocol.ClipResponse{OK: true, SHA: sha})
	golden("clip\timage\tpng\n", "ok\t"+sha+"\n")
	golden("clip\ttext\n", "ok\t"+sha+"\n")
	// default-deny
	golden("bogus\tverb\n", "rejected\n")
}

// TestServer_SeqIsolationAcrossServices (EC4, generalized) bursts openurl+notify+
// clip traffic, then emits a real port event and asserts its Seq is exactly
// prevSnapshotSeq+1 — service traffic never advances the port-event counter s.seq.
func TestServer_SeqIsolationAcrossServices(t *testing.T) {
	h := newU6Harness(t, map[string]uint32{"openurl": 1, "notify": 1, "clip": 1}, nil)
	defer h.close()
	h.setClip(protocol.ClipResponse{OK: true, SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})

	prev := h.snapSeq
	for i := 0; i < 3; i++ {
		if got := askSock(t, h.sockPath, fmt.Sprintf("open\thttp://example.com/%d\n", i)); got != "ok\n" {
			t.Fatalf("open #%d = %q, want ok\\n", i, got)
		}
	}
	for i := 0; i < 3; i++ {
		if got := askSock(t, h.sockPath, "notify\t{\"title\":\"t\"}\n"); got != "ok\n" {
			t.Fatalf("notify #%d = %q, want ok\\n", i, got)
		}
	}
	for i := 0; i < 3; i++ {
		if got := askSock(t, h.sockPath, "clip\timage\tpng\n"); got != "ok\taaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" {
			t.Fatalf("clip #%d = %q, want ok\\t<sha>\\n", i, got)
		}
	}

	h.w.Emit(watcher.Event{Kind: watcher.KindAdd, At: time.Now(),
		Listen: watcher.Listen{Port: 8081, Family: 4, Addr: "127.0.0.1"}})
	got := h.nextPort(t)
	if got.PortAdded == nil {
		t.Fatalf("expected PortAdded, got %+v", got)
	}
	if got.PortAdded.Seq != prev+1 {
		t.Fatalf("PortAdded.Seq = %d, want %d (service traffic must not advance s.seq)", got.PortAdded.Seq, prev+1)
	}
}

// TestServer_OutboxBudgetRecycleAllServices (release guard, backs EC2/EC8) drives
// > outbox-cap emits for EACH of open/notify/clip with the client actively
// draining, and asserts none silently stops relaying after `cap`. A regressed or
// per-service-omitted reg.release in the Serve drain arm would wedge that
// service's admission budget after the first cap emits — caught HERE at scale.
func TestServer_OutboxBudgetRecycleAllServices(t *testing.T) {
	h := newU6Harness(t, map[string]uint32{"openurl": 1, "notify": 1, "clip": 1}, nil)
	defer h.close()

	const n = 8 + 5 // every service's OutboxCap is 8; exceed it to force recycle.

	for i := 0; i < n; i++ {
		url := fmt.Sprintf("http://example.com/o/%d", i)
		if got := askSock(t, h.sockPath, "open\t"+url+"\n"); got != "ok\n" {
			t.Fatalf("open #%d = %q, want ok\\n (budget did not recycle)", i, got)
		}
		env := h.nextMsg(t)
		ou, err := protocol.UnmarshalPayload[protocol.OpenURL](env.Msg.Payload)
		if err != nil || ou.URL != url {
			t.Fatalf("open #%d relayed %q (err %v), want %q", i, ou.URL, err, url)
		}
	}

	for i := 0; i < n; i++ {
		if got := askSock(t, h.sockPath, "notify\t{\"title\":\"n\"}\n"); got != "ok\n" {
			t.Fatalf("notify #%d = %q, want ok\\n (budget did not recycle)", i, got)
		}
		env := h.nextMsg(t)
		if env.Msg.Service != "notify" || env.Msg.Kind != "event" {
			t.Fatalf("notify #%d relayed %+v", i, env.Msg)
		}
	}

	const sha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	h.setClip(protocol.ClipResponse{OK: true, SHA: sha})
	for i := 0; i < n; i++ {
		// A full round-trip reply proves emit succeeded AND the budget recycled;
		// clip is sequential here so only one waiter is ever in flight.
		if got := askSock(t, h.sockPath, "clip\timage\tpng\n"); got != "ok\t"+sha+"\n" {
			t.Fatalf("clip #%d = %q, want ok\\t<sha>\\n (budget did not recycle)", i, got)
		}
	}
}

// TestServer_PayloadCapDropSessionLives (EC5) delivers an inbound Msg whose
// Payload exceeds the clip service's MaxPayload: it is dropped + logged and the
// session stays alive (a subsequent port event still flows).
func TestServer_PayloadCapDropSessionLives(t *testing.T) {
	wc := &warnCounter{}
	h := newU6Harness(t, map[string]uint32{"openurl": 1, "notify": 1, "clip": 1}, slog.New(wc))
	defer h.close()

	// A single valid CBOR item larger than clip's MaxPayload (4096): the size
	// check drops it before decode, so its contents are irrelevant.
	big, err := protocol.MarshalPayload(make([]byte, 5000))
	if err != nil {
		t.Fatal(err)
	}
	if len(big) <= 4096 {
		t.Fatalf("test payload %d bytes is not oversized", len(big))
	}
	if err := h.enc.Write(&protocol.Envelope{Msg: &protocol.Msg{
		Service: "clip", Kind: "resp", Payload: big,
	}}); err != nil {
		t.Fatal(err)
	}

	// Session lives: a genuine port event still round-trips after the drop.
	h.w.Emit(watcher.Event{Kind: watcher.KindAdd, At: time.Now(),
		Listen: watcher.Listen{Port: 8082, Family: 4, Addr: "127.0.0.1"}})
	got := h.nextPort(t)
	if got.PortAdded == nil || got.PortAdded.Port.Port != 8082 {
		t.Fatalf("expected PortAdded(8082) after oversized drop, got %+v", got)
	}

	// The oversized payload was logged as a drop.
	deadline := time.Now().Add(2 * time.Second)
	for wc.count() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("oversized payload drop was not logged")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestServer_OversizeFrameFatal (EC5) asserts codec behavior is UNCHANGED: a
// genuinely oversized FRAME (length prefix > MaxFrameBytes) is still fatal — the
// decoder rejects it before allocation and the Serve loop dies.
func TestServer_OversizeFrameFatal(t *testing.T) {
	w := watcher.NewFake()
	conn := newConnPair()
	defer conn.close()
	_, wg, cap := runServer(t, w, conn)

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	enc.Write(&protocol.Envelope{Hello: &protocol.Hello{ProtoVersion: protocol.ProtoVersion}})
	if _, err := dec.Read(); err != nil { // HelloAck
		t.Fatal(err)
	}

	// Hand-craft a frame header claiming a body larger than MaxFrameBytes.
	hdr := make([]byte, 6)
	hdr[0] = protocol.FrameMagic[0]
	hdr[1] = protocol.FrameMagic[1]
	binary.BigEndian.PutUint32(hdr[2:6], uint32(protocol.MaxFrameBytes+1))
	if _, err := conn.c2aW.Write(hdr); err != nil {
		t.Fatal(err)
	}

	wg.Wait()
	if err := cap.get(); !errors.Is(err, protocol.ErrFrameTooLarge) {
		t.Fatalf("oversized frame server error = %v, want ErrFrameTooLarge", err)
	}
}

// TestServer_PanicIsolation (EC6, agent side) registers a deliberately-panicking
// fake service via an in-package seam, delivers a frame that triggers the panic,
// and asserts the frame is dropped while the Serve loop survives: a subsequent
// port event and a Ping→Heartbeat both flow.
func TestServer_PanicIsolation(t *testing.T) {
	w := watcher.NewFake()
	w.SetSnapshot(nil)
	conn := newConnPair()
	ctx, cancel := context.WithCancel(context.Background())
	srv := New(Config{
		In: conn.c2aR, Out: conn.a2cW, Watcher: w, AgentSHA: "testsha",
		HeartbeatInterval: time.Hour,
	})
	// In-package seam: add one panicking service before Serve. Inbound dispatch
	// keys only on the registered service name, so no cmd-verb gating is needed.
	boom := &fakeService{name: "boom", version: 1, maxPayload: 64, outboxCap: 2, verbName: "boomverb", panicOnMsg: true}
	srv.reg.register(boom)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(ctx); conn.a2cW.Close() }()

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	enc.Write(&protocol.Envelope{Hello: &protocol.Hello{ProtoVersion: protocol.ProtoVersion}})
	dec.Read() // HelloAck
	enc.Write(&protocol.Envelope{Subscribe: &protocol.Subscribe{ResubscribeID: 1}})
	dec.Read()            // SubscribeAck
	snap, _ := dec.Read() // Snapshot
	prevSeq := snap.Snapshot.Seq

	frames := make(chan *protocol.Envelope, 8)
	var cwg sync.WaitGroup
	cwg.Add(1)
	go func() {
		defer cwg.Done()
		for {
			env, err := dec.Read()
			if err != nil {
				return
			}
			if env.PortAdded != nil || env.Heartbeat != nil {
				frames <- env
			}
		}
	}()
	defer func() { cancel(); conn.c2aW.Close(); wg.Wait(); cwg.Wait(); conn.close() }()

	// Deliver the panic-triggering frame.
	pl, _ := protocol.MarshalPayload(protocol.OpenURL{URL: "x"})
	enc.Write(&protocol.Envelope{Msg: &protocol.Msg{Service: "boom", Kind: "x", Payload: pl}})

	// Session lives: a real port event arrives with the expected Seq.
	w.Emit(watcher.Event{Kind: watcher.KindAdd, At: time.Now(),
		Listen: watcher.Listen{Port: 8081, Family: 4, Addr: "127.0.0.1"}})
	var added *protocol.Envelope
	for added == nil {
		select {
		case f := <-frames:
			if f.PortAdded != nil {
				added = f
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no PortAdded after panic (session died)")
		}
	}
	if added.PortAdded.Seq != prevSeq+1 {
		t.Fatalf("PortAdded.Seq = %d, want %d", added.PortAdded.Seq, prevSeq+1)
	}

	// Heartbeats keep flowing: a Ping is answered.
	enc.Write(&protocol.Envelope{Ping: &protocol.Ping{Nonce: 7}})
	for {
		select {
		case f := <-frames:
			if f.Heartbeat != nil && f.Heartbeat.Nonce == 7 {
				goto alive
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no Heartbeat after panic (session died)")
		}
	}
alive:
	if kinds := boom.handledKinds(); len(kinds) != 0 {
		t.Fatalf("panicking handler recorded %v; frame should have been dropped", kinds)
	}
}

// TestServer_MixedVersionFatal (EC7) is the wire-level 3-vs-4 boundary: a
// Hello{pv:3} against this v4 agent yields a fatal AgentError{CodeProtocolMismatch}
// and the session dies with no further frames written.
func TestServer_MixedVersionFatal(t *testing.T) {
	w := watcher.NewFake()
	conn := newConnPair()
	defer conn.close()
	_, wg, cap := runServer(t, w, conn)

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	enc.Write(&protocol.Envelope{Hello: &protocol.Hello{ProtoVersion: 3}})

	env, err := dec.Read()
	if err != nil {
		t.Fatal(err)
	}
	if env.AgentError == nil || env.AgentError.Code != protocol.CodeProtocolMismatch || !env.AgentError.Fatal {
		t.Fatalf("expected fatal AgentError CodeProtocolMismatch, got %+v", env)
	}

	// Session dies: no further frames, the stream EOFs.
	if _, err := dec.Read(); err == nil {
		t.Fatal("expected EOF after fatal mismatch, got a frame")
	}
	conn.c2aW.Close()
	wg.Wait()
	if cap.get() == nil {
		t.Fatal("expected non-nil server error on version mismatch")
	}
}

// TestServer_ZeroServiceGating (EC8) exercises the hasClient()&&clientHas(svc)
// gate when hasClient is TRUE (a client IS subscribed) but clientHas is FALSE (it
// advertised NO services): open⇒no-client, notify⇒no-client, clip⇒none, while the
// session layer (ports/snapshots) keeps flowing.
func TestServer_ZeroServiceGating(t *testing.T) {
	h := newU6Harness(t, nil, nil) // subscribed client, zero advertised services
	defer h.close()

	if got := askSock(t, h.sockPath, "open\thttp://example.com\n"); got != "no-client\n" {
		t.Fatalf("open = %q, want no-client\\n", got)
	}
	if got := askSock(t, h.sockPath, "notify\t{\"title\":\"hi\"}\n"); got != "no-client\n" {
		t.Fatalf("notify = %q, want no-client\\n", got)
	}
	if got := askSock(t, h.sockPath, "clip\timage\tpng\n"); got != "none\n" {
		t.Fatalf("clip = %q, want none\\n", got)
	}

	h.w.Emit(watcher.Event{Kind: watcher.KindAdd, At: time.Now(),
		Listen: watcher.Listen{Port: 8081, Family: 4, Addr: "127.0.0.1"}})
	got := h.nextPort(t)
	if got.PortAdded == nil || got.PortAdded.Port.Port != 8081 {
		t.Fatalf("expected PortAdded(8081) for zero-service consumer, got %+v", got)
	}
}

// TestServer_ClipTimeoutBudget (EC9 cross-check, full stack) re-asserts the clip
// timeout budget via the clipService field-shortening seam: shorten BOTH
// clipTimeout and clipSockDeadline (ordering preserved) and assert "none\n"
// arrives before the shortened socket deadline. The structural ordering of the
// two agent-side defaults is asserted too; the shim's outer 13s bound lives in
// package main and is not importable here.
func TestServer_ClipTimeoutBudget(t *testing.T) {
	if defaultClipTimeout >= defaultClipSockDeadline {
		t.Fatalf("default budget ordering violated: clipTimeout %v >= clipSockDeadline %v",
			defaultClipTimeout, defaultClipSockDeadline)
	}

	sockPath := tempSockPath(t)
	w := watcher.NewFake()
	w.SetSnapshot(nil)
	conn := newConnPair()
	ctx, cancel := context.WithCancel(context.Background())
	srv := New(Config{
		In: conn.c2aR, Out: conn.a2cW, Watcher: w, AgentSHA: "testsha",
		HeartbeatInterval: time.Hour, CmdSockPath: sockPath,
	})
	const shortTimeout = 50 * time.Millisecond
	const shortSockDeadline = 200 * time.Millisecond
	// Written before `go Serve` establishes happens-before with the goroutines
	// Serve spawns (the cmd handler reads these fields live). Ordering preserved.
	srv.clip.clipTimeout = shortTimeout
	srv.clip.clipSockDeadline = shortSockDeadline

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(ctx); conn.a2cW.Close() }()
	defer func() { cancel(); conn.c2aW.Close(); wg.Wait(); conn.close() }()

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	enc.Write(&protocol.Envelope{Hello: &protocol.Hello{
		ProtoVersion: protocol.ProtoVersion, Services: map[string]uint32{"clip": 1},
	}})
	if _, err := dec.Read(); err != nil { // HelloAck
		t.Fatal(err)
	}
	enc.Write(&protocol.Envelope{Subscribe: &protocol.Subscribe{ResubscribeID: 1}})
	if _, err := dec.Read(); err != nil { // SubscribeAck
		t.Fatal(err)
	}
	if _, err := dec.Read(); err != nil { // Snapshot
		t.Fatal(err)
	}
	// Client drains but NEVER answers the clip request → the waiter times out.
	go func() {
		for {
			if _, err := dec.Read(); err != nil {
				return
			}
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := net.DialTimeout("unix", sockPath, 100*time.Millisecond); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cmd socket did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}

	c, err := net.DialTimeout("unix", sockPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	start := time.Now()
	c.Write([]byte("clip\timage\tpng\n"))
	buf := make([]byte, 64)
	n, _ := c.Read(buf)
	elapsed := time.Since(start)
	if got := string(buf[:n]); got != "none\n" {
		t.Fatalf("reply = %q, want none\\n", got)
	}
	if elapsed >= shortSockDeadline {
		t.Fatalf("none\\n took %v, want < clipSockDeadline %v", elapsed, shortSockDeadline)
	}
}

// countingWriter wraps an io.Writer and counts bytes written through it.
type countingWriter struct {
	mu sync.Mutex
	w  io.Writer
	n  int
}

func (c *countingWriter) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.n += len(b)
	c.mu.Unlock()
	return c.w.Write(b)
}

func (c *countingWriter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestServer_SoleWriterAfterExit (risk mitigation) asserts that no service can
// write agent→client frames after Serve returns: the registry holds no encoder,
// so a post-exit emit() only buffers onto the (undrained) outbox channel and
// produces ZERO bytes on the wire.
func TestServer_SoleWriterAfterExit(t *testing.T) {
	w := watcher.NewFake()
	w.SetSnapshot(nil)
	conn := newConnPair()
	defer conn.close()
	cw := &countingWriter{w: conn.a2cW}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(Config{
		In: conn.c2aR, Out: cw, Watcher: w, AgentSHA: "testsha",
		HeartbeatInterval: time.Hour,
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(ctx); conn.a2cW.Close() }()

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	enc.Write(&protocol.Envelope{Hello: &protocol.Hello{ProtoVersion: protocol.ProtoVersion}})
	dec.Read() // HelloAck
	enc.Write(&protocol.Envelope{Subscribe: &protocol.Subscribe{ResubscribeID: 1}})
	dec.Read() // SubscribeAck
	dec.Read() // Snapshot

	// Shut the session down cleanly; read the Bye so the pipe write completes.
	enc.Write(&protocol.Envelope{Shutdown: &protocol.Shutdown{Reason: "done"}})
	if bye, err := dec.Read(); err != nil || bye.Bye == nil {
		t.Fatalf("expected Bye, got %+v (err %v)", bye, err)
	}
	wg.Wait()
	before := cw.count()

	// Post-exit emit: only buffers onto the outbox; nothing drains it, so no bytes
	// reach the wire.
	pl, _ := protocol.MarshalPayload(protocol.OpenURL{URL: "http://after-exit"})
	srv.reg.emit("openurl", "open", pl)
	time.Sleep(50 * time.Millisecond)
	if after := cw.count(); after != before {
		t.Fatalf("bytes written after Serve exit: before=%d after=%d (service became a second writer)", before, after)
	}
}

func TestServer_StdinEOFCleanExit(t *testing.T) {
	w := watcher.NewFake()
	conn := newConnPair()
	defer conn.close()
	_, wg, cap := runServer(t, w, conn)

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	enc.Write(&protocol.Envelope{Hello: &protocol.Hello{ProtoVersion: protocol.ProtoVersion}})
	dec.Read() // HelloAck
	conn.c2aW.Close()
	wg.Wait()
	if err := cap.get(); err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("expected nil or EOF, got %v", err)
	}
}
