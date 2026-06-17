package agent

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/vikashl/portal/internal/agent/watcher"
	"github.com/vikashl/portal/internal/protocol"
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
	w := watcher.NewFake()
	conn := newConnPair()
	defer conn.close()
	_, wg, cap := runServer(t, w, conn)

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)

	enc.Write(&protocol.Envelope{Hello: &protocol.Hello{ProtoVersion: 99}})
	env, err := dec.Read()
	if err != nil {
		t.Fatal(err)
	}
	if env.AgentError == nil || env.AgentError.Code != protocol.CodeProtocolMismatch {
		t.Errorf("expected AgentError CodeProtocolMismatch, got %+v", env)
	}
	conn.c2aW.Close()
	wg.Wait()
	if cap.get() == nil {
		t.Errorf("expected non-nil server error on version mismatch")
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
