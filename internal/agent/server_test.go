package agent

import (
	"context"
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
