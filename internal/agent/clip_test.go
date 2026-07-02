package agent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/agent/watcher"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/protocol"
)

// tempSockPath returns a Unix-socket path under a freshly-created short temp
// dir. We deliberately avoid t.TempDir() here: it embeds the (long) test name
// in the path, and macOS caps sun_path at 104 bytes — a long test name pushes
// the socket path over the limit and net.Listen fails with "invalid argument".
func tempSockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ptlsock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "cmd.sock")
}

// clipHarness drives a Server with a real cmd Unix socket and a connected
// (subscribed) Mac client. The client side is a goroutine that the test
// programs via a responder func: it decodes agent→client frames, and for every
// ClipRequest it calls respond() and (if a ClipResponse is returned) writes it
// back up the pipe. Heartbeats/snapshots are ignored.
type clipHarness struct {
	srv      *Server
	sockPath string
	cancel   context.CancelFunc
	wg       *sync.WaitGroup
	clientWG *sync.WaitGroup
	conn     *connPair
	enc      *protocol.Encoder
}

// newClipHarness builds the server, completes the handshake+subscribe so
// hasClient is true, and starts a client goroutine driving respond on each
// ClipRequest. respond may return nil to swallow the request (simulate a Mac
// that never answers), letting clipTimeout fire.
func newClipHarness(t *testing.T, respond func(req *protocol.ClipRequest) *protocol.ClipResponse) *clipHarness {
	t.Helper()
	sockPath := tempSockPath(t)
	w := watcher.NewFake()
	w.SetSnapshot(nil)
	conn := newConnPair()

	ctx, cancel := context.WithCancel(context.Background())
	srv := New(Config{
		In:                conn.c2aR,
		Out:               conn.a2cW,
		Watcher:           w,
		AgentSHA:          "testsha",
		HeartbeatInterval: time.Hour, // disable heartbeat
		CmdSockPath:       sockPath,
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.Serve(ctx)
		conn.a2cW.Close()
	}()

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)

	// Handshake. Advertise openurl@1 so TestClip_OpenStillWorks's "open" verb
	// passes the agent's hasClient()&&clientHas("openurl") gate (clip itself
	// stays on the legacy ClipRequest/ClipResponse path this unit).
	if err := enc.Write(&protocol.Envelope{Hello: &protocol.Hello{
		ProtoVersion: protocol.ProtoVersion,
		Services:     map[string]uint32{"openurl": 1},
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

	// Client goroutine: drain agent→client frames, answer ClipRequests.
	var clientWG sync.WaitGroup
	clientWG.Add(1)
	go func() {
		defer clientWG.Done()
		for {
			env, err := dec.Read()
			if err != nil {
				return
			}
			if env.ClipRequest != nil && respond != nil {
				if resp := respond(env.ClipRequest); resp != nil {
					_ = enc.Write(&protocol.Envelope{ClipResponse: resp})
				}
			}
		}
	}()

	// Wait for the cmd socket to come up before returning.
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

	return &clipHarness{
		srv: srv, sockPath: sockPath, cancel: cancel,
		wg: &wg, clientWG: &clientWG, conn: conn, enc: enc,
	}
}

func (h *clipHarness) close() {
	h.cancel()
	h.conn.c2aW.Close()
	h.wg.Wait()
	h.clientWG.Wait()
	h.conn.close()
}

// ask dials the cmd socket, writes a verb line, and returns the trimmed reply.
func (h *clipHarness) ask(t *testing.T, line string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", h.sockPath, time.Second)
	if err != nil {
		t.Fatalf("dial cmd sock: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := conn.Write([]byte(line)); err != nil {
		t.Fatalf("write verb: %v", err)
	}
	buf := make([]byte, 256)
	n, _ := conn.Read(buf)
	return string(buf[:n])
}

// TestClip_WaiterCorrelation drives a full image-png request end to end: the
// agent emits a ClipRequest, the client answers OK with a SHA, and the socket
// reply is the byte-exact "ok\t<sha>\n".
func TestClip_WaiterCorrelation(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef"
	var seen *protocol.ClipRequest
	h := newClipHarness(t, func(req *protocol.ClipRequest) *protocol.ClipResponse {
		seen = req
		return &protocol.ClipResponse{Nonce: req.Nonce, Epoch: req.Epoch, OK: true, SHA: sha}
	})
	defer h.close()

	reply := h.ask(t, "clip\timage\tpng\n")
	if reply != "ok\t"+sha+"\n" {
		t.Fatalf("reply = %q, want ok\\t<sha>\\n", reply)
	}
	if seen == nil || seen.Kind != "image" || seen.Format != "png" {
		t.Fatalf("ClipRequest = %+v, want kind=image fmt=png", seen)
	}
	if seen.Epoch != h.srv.epoch {
		t.Fatalf("ClipRequest.Epoch = %d, want server epoch %d", seen.Epoch, h.srv.epoch)
	}
}

// TestClip_Targets exercises the targets verb byte-exact reply. The agent now
// relays the CANONICAL kind ("image"/"text") the Mac decided; portald maps it
// to tool-specific target lines. A Mac that reports Has=true with no Kind
// defaults to image (older-Mac compatibility).
func TestClip_Targets(t *testing.T) {
	h := newClipHarness(t, func(req *protocol.ClipRequest) *protocol.ClipResponse {
		return &protocol.ClipResponse{Nonce: req.Nonce, Epoch: req.Epoch, OK: true, Has: true}
	})
	defer h.close()

	reply := h.ask(t, "clip\ttargets\n")
	if reply != "ok\timage\n" {
		t.Fatalf("targets reply = %q, want %q", reply, "ok\timage\n")
	}
}

// TestClip_TargetsText: when the Mac reports Kind="text", the agent relays the
// text kind so portald can advertise the text target names.
func TestClip_TargetsText(t *testing.T) {
	h := newClipHarness(t, func(req *protocol.ClipRequest) *protocol.ClipResponse {
		return &protocol.ClipResponse{Nonce: req.Nonce, Epoch: req.Epoch, OK: true, Has: true, Kind: "text"}
	})
	defer h.close()

	reply := h.ask(t, "clip\ttargets\n")
	if reply != "ok\ttext\n" {
		t.Fatalf("targets reply = %q, want %q", reply, "ok\ttext\n")
	}
}

// TestClip_TargetsNoImage: client reports Has=false → "none\n".
func TestClip_TargetsNoImage(t *testing.T) {
	h := newClipHarness(t, func(req *protocol.ClipRequest) *protocol.ClipResponse {
		return &protocol.ClipResponse{Nonce: req.Nonce, Epoch: req.Epoch, OK: true, Has: false}
	})
	defer h.close()

	if reply := h.ask(t, "clip\ttargets\n"); reply != "none\n" {
		t.Fatalf("reply = %q, want none\\n", reply)
	}
}

// TestClip_EpochMismatchDrop: a ClipResponse carrying the wrong epoch must be
// dropped, so the waiter never receives it and clipTimeout (shortened via a
// custom server here is impractical) eventually fires. To keep the test fast we
// assert the response is dropped by observing that a SECOND, correct response
// is what actually satisfies the waiter — i.e. the wrong-epoch one did nothing.
func TestClip_EpochMismatchDrop(t *testing.T) {
	const sha = "ffffffffffffffffffffffffffffffff"
	var h *clipHarness
	h = newClipHarness(t, func(req *protocol.ClipRequest) *protocol.ClipResponse {
		// First send a wrong-epoch response directly (it must be ignored),
		// then return the correct one which the harness writes for us.
		_ = h.enc.Write(&protocol.Envelope{ClipResponse: &protocol.ClipResponse{
			Nonce: req.Nonce, Epoch: req.Epoch ^ 0xdead, OK: true, SHA: "deadbeefdeadbeefdeadbeefdeadbeef",
		}})
		return &protocol.ClipResponse{Nonce: req.Nonce, Epoch: req.Epoch, OK: true, SHA: sha}
	})
	defer h.close()

	reply := h.ask(t, "clip\timage\tpng\n")
	if reply != "ok\t"+sha+"\n" {
		t.Fatalf("reply = %q, want the correct-epoch sha (wrong-epoch must be dropped)", reply)
	}
}

// TestClip_NoClientImmediateNone: with no subscribed client the agent answers
// "none\n" immediately rather than making the shim eat the full timeout.
func TestClip_NoClientImmediateNone(t *testing.T) {
	// Build a server WITHOUT completing Subscribe → hasClient stays false.
	sockPath := tempSockPath(t)
	w := watcher.NewFake()
	w.SetSnapshot(nil)
	conn := newConnPair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(Config{
		In: conn.c2aR, Out: conn.a2cW, Watcher: w, AgentSHA: "testsha",
		HeartbeatInterval: time.Hour, CmdSockPath: sockPath,
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(ctx); conn.a2cW.Close() }()
	defer func() { cancel(); conn.c2aW.Close(); wg.Wait(); conn.close() }()

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	enc.Write(&protocol.Envelope{Hello: &protocol.Hello{ProtoVersion: protocol.ProtoVersion}})
	if _, err := dec.Read(); err != nil { // HelloAck only; no Subscribe → hasClient stays false
		t.Fatal(err)
	}

	// Keep draining agent→client frames so the Serve loop's teardown Bye write
	// doesn't block on the io.Pipe and hang wg.Wait().
	go func() {
		for {
			if _, err := dec.Read(); err != nil {
				return
			}
		}
	}()

	// Wait for the socket.
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
	start := time.Now()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte("clip\timage\tpng\n"))
	buf := make([]byte, 64)
	n, _ := c.Read(buf)
	if got := string(buf[:n]); got != "none\n" {
		t.Fatalf("reply = %q, want none\\n", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("no-client reply took %v, expected immediate", elapsed)
	}
}

// TestClip_DefaultDeny: unknown verbs and malformed clip shapes are rejected.
func TestClip_DefaultDeny(t *testing.T) {
	h := newClipHarness(t, func(req *protocol.ClipRequest) *protocol.ClipResponse {
		t.Errorf("unexpected ClipRequest for default-deny case: %+v", req)
		return nil
	})
	defer h.close()

	cases := []struct {
		line string
		want string
	}{
		{"bogus\tverb\n", "rejected\n"},
		{"clip\n", "rejected\n"},                 // no kind
		{"clip\tbmp\n", "rejected\n"},            // unknown kind
		{"clip\timage\tbmp\n", "rejected\n"},     // wrong image format
		{"clip\timage\n", "rejected\n"},          // image with no format
		{"clip\ttargets\textra\n", "rejected\n"}, // trailing junk
	}
	for _, tc := range cases {
		if got := h.ask(t, tc.line); got != tc.want {
			t.Errorf("ask(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}

// TestClip_OpenStillWorks: the open verb behavior is unchanged by the new
// tab-framed dispatcher, including the http(s)-only default-deny.
func TestClip_OpenStillWorks(t *testing.T) {
	h := newClipHarness(t, nil)
	defer h.close()

	// Drain the OpenURL the agent will emit (so the client goroutine doesn't
	// block the pipe). The harness client goroutine already drains frames, but
	// here respond==nil so OpenURL is just dropped — fine.
	if got := h.ask(t, "open\thttp://example.com\n"); got != "ok\n" {
		t.Errorf("open http reply = %q, want ok\\n", got)
	}
	if got := h.ask(t, "open\tfile:///etc/passwd\n"); got != "rejected\n" {
		t.Errorf("open non-http reply = %q, want rejected\\n", got)
	}
}

// TestClip_MaxInflight bounds concurrent waiters: once maxInflightClip
// requests are parked (the client never answers them), the next request is
// answered "none\n" immediately rather than registering another waiter.
func TestClip_MaxInflight(t *testing.T) {
	// respond==nil: the client drains ClipRequests but never answers, so each
	// dial parks in handleClipReq until clipTimeout. We don't wait that long —
	// we observe the registered-waiter count and the over-cap rejection, then
	// tear the server down (which unblocks the parked diallers via ctx.Done()).
	h := newClipHarness(t, nil)
	var parkedWG sync.WaitGroup
	// Tear the server down first (cancels ctx → parked diallers each get
	// "none\n" and return), THEN wait for those goroutines so none touch t
	// after the test returns.
	defer func() { h.close(); parkedWG.Wait() }()

	for i := 0; i < maxInflightClip; i++ {
		parkedWG.Add(1)
		go func() {
			defer parkedWG.Done()
			// Parks until the server answers (it won't, until ctx cancel on
			// close → "none\n"). The reply is irrelevant here.
			h.ask(t, "clip\timage\tpng\n")
		}()
	}

	// Wait until all maxInflightClip waiters are registered in the server.
	deadline := time.Now().Add(3 * time.Second)
	for {
		h.srv.mu.Lock()
		nWaiters := len(h.srv.clipWaiters)
		h.srv.mu.Unlock()
		if nWaiters >= maxInflightClip {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d waiters registered", nWaiters, maxInflightClip)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The (maxInflightClip+1)-th request must be rejected "none\n" immediately —
	// the cap is hit, so no new waiter is registered and the responder is never
	// consulted.
	start := time.Now()
	if got := h.ask(t, "clip\timage\tpng\n"); got != "none\n" {
		t.Fatalf("over-cap reply = %q, want none\\n", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("over-cap reply took %v, expected immediate", elapsed)
	}
	// Parked diallers unblock when the deferred h.close() cancels ctx.
}

// TestClip_DoesNotAdvanceSeq asserts that emitting a ClipRequest (and handling
// its ClipResponse) never advances s.seq — the port-event staleness counter
// the client compares against. We snapshot seq, run a clip round trip, then
// trigger a real port event and confirm its Seq is exactly prevSeq+1.
func TestClip_DoesNotAdvanceSeq(t *testing.T) {
	const sha = "00000000000000000000000000000000"
	w := watcher.NewFake()
	w.SetSnapshot(nil)
	conn := newConnPair()
	sockPath := tempSockPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := New(Config{
		In: conn.c2aR, Out: conn.a2cW, Watcher: w, AgentSHA: "testsha",
		HeartbeatInterval: time.Hour, CmdSockPath: sockPath,
	})
	var wg sync.WaitGroup
	var clientWG sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(ctx); conn.a2cW.Close() }()
	// Single ordered teardown: cancel → close write side → wait for Serve
	// (which closes a2cW, unblocking the client read) → wait for the client →
	// close the pipes. A naive `defer clientWG.Wait()` would run BEFORE the
	// cancel/close defer (LIFO) and deadlock waiting on a client that can't see
	// EOF until a2cW closes.
	defer func() {
		cancel()
		conn.c2aW.Close()
		wg.Wait()
		clientWG.Wait()
		conn.close()
	}()

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	enc.Write(&protocol.Envelope{Hello: &protocol.Hello{ProtoVersion: protocol.ProtoVersion}})
	dec.Read() // HelloAck
	enc.Write(&protocol.Envelope{Subscribe: &protocol.Subscribe{ResubscribeID: 1}})
	dec.Read()            // SubscribeAck
	snap, _ := dec.Read() // Snapshot (Seq advanced by emitSnapshot)
	if snap.Snapshot == nil {
		t.Fatal("expected initial snapshot")
	}
	prevSeq := snap.Snapshot.Seq

	// Client goroutine answers ClipRequests; for other frames, deliver them on
	// a channel so the main test body can read the PortAdded later.
	frames := make(chan *protocol.Envelope, 8)
	clientWG.Add(1)
	go func() {
		defer clientWG.Done()
		for {
			env, err := dec.Read()
			if err != nil {
				return
			}
			if env.ClipRequest != nil {
				_ = enc.Write(&protocol.Envelope{ClipResponse: &protocol.ClipResponse{
					Nonce: env.ClipRequest.Nonce, Epoch: env.ClipRequest.Epoch, OK: true, SHA: sha,
				}})
				continue
			}
			frames <- env
		}
	}()

	// Wait for the socket, then run a clip round trip.
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
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("clip\timage\tpng\n"))
	rb := make([]byte, 64)
	rn, _ := c.Read(rb)
	c.Close()
	if !strings.HasPrefix(string(rb[:rn]), "ok\t") {
		t.Fatalf("clip reply = %q, want ok", string(rb[:rn]))
	}

	// Now trigger a genuine port event. Its Seq MUST be prevSeq+1 — proving the
	// clip round trip did not consume a port-event sequence number.
	w.Emit(watcher.Event{Kind: watcher.KindAdd, At: time.Now(),
		Listen: watcher.Listen{Port: 8081, Family: 4, Addr: "127.0.0.1"}})
	added := <-frames
	if added.PortAdded == nil {
		t.Fatalf("expected PortAdded, got %+v", added)
	}
	if added.PortAdded.Seq != prevSeq+1 {
		t.Fatalf("PortAdded.Seq = %d, want %d (clip must not advance s.seq)", added.PortAdded.Seq, prevSeq+1)
	}
}
