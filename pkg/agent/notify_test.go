package agent

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/pkg/agent/watcher"
	"github.com/VikashLoomba/Portal/pkg/protocol"
)

// notifyHarness drives a Server with a real cmd socket and a connected client
// that captures every Notify frame the agent relays up the pipe. It mirrors
// clipHarness but for the fire-and-forget notify path (no response frame).
type notifyHarness struct {
	srv      *Server
	sockPath string
	cancel   context.CancelFunc
	wg       *sync.WaitGroup
	clientWG *sync.WaitGroup
	conn     *connPair

	mu       sync.Mutex
	notifies []relayedNotify
}

// relayedNotify mirrors exactly what the production client surfaces: the payload
// decoded on its own (payload-only, as agentclient/registry.dispatch does) PLUS
// the registry-stamped Msg.Seq handed to the handler out-of-band (DESIGN S3).
// Keeping seq SEPARATE from the payload is what makes the Seq assertion catch a
// regression — threading Msg.Seq back into the payload struct (as the old
// harness did) masked that svc_notify never sets a payload Seq.
type relayedNotify struct {
	n   protocol.Notify
	seq uint64
}

func newNotifyHarness(t *testing.T, subscribe bool) *notifyHarness {
	t.Helper()
	sockPath := tempSockPath(t)
	w := watcher.NewFake()
	w.SetSnapshot(nil)
	conn := newConnPair()
	ctx, cancel := context.WithCancel(context.Background())
	srv := New(Config{
		In: conn.c2aR, Out: conn.a2cW, Watcher: w, AgentSHA: "testsha",
		HeartbeatInterval: time.Hour, CmdSockPath: sockPath,
	})

	h := &notifyHarness{srv: srv, sockPath: sockPath, cancel: cancel, conn: conn}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = srv.Serve(ctx); conn.a2cW.Close() }()
	h.wg = &wg

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	// Advertise the client's registered handlers so the agent's notify service
	// gates open (clientHas("notify")). openurl is advertised too, mirroring the
	// real Client which registers both handlers (DESIGN S4 symmetric advertise).
	if err := enc.Write(&protocol.Envelope{Hello: &protocol.Hello{
		ProtoVersion: protocol.ProtoVersion,
		Services:     map[string]uint32{"openurl": 1, "notify": 1},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Read(); err != nil { // HelloAck
		t.Fatal(err)
	}
	if subscribe {
		if err := enc.Write(&protocol.Envelope{Subscribe: &protocol.Subscribe{ResubscribeID: 1}}); err != nil {
			t.Fatal(err)
		}
		if _, err := dec.Read(); err != nil { // SubscribeAck
			t.Fatal(err)
		}
		if _, err := dec.Read(); err != nil { // initial Snapshot
			t.Fatal(err)
		}
	}

	var clientWG sync.WaitGroup
	clientWG.Add(1)
	go func() {
		defer clientWG.Done()
		for {
			env, err := dec.Read()
			if err != nil {
				return
			}
			// Notify now rides a Msg{svc:notify,kind:event} frame (v4). Decode the
			// payload back into a protocol.Notify EXACTLY as the production client
			// does — payload-only (agentclient/registry.dispatch), which never sees
			// a payload Seq because svc_notify does not set one. The correlation Seq
			// is the registry-stamped Msg.Seq (DESIGN S3), captured SEPARATELY so
			// the Seq assertion exercises the real production surface.
			if env.Msg != nil && env.Msg.Service == "notify" && env.Msg.Kind == "event" {
				n, err := protocol.UnmarshalPayload[protocol.Notify](env.Msg.Payload)
				if err != nil {
					continue
				}
				h.mu.Lock()
				h.notifies = append(h.notifies, relayedNotify{n: n, seq: env.Msg.Seq})
				h.mu.Unlock()
			}
		}
	}()
	h.clientWG = &clientWG

	if subscribe {
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
	}
	return h
}

func (h *notifyHarness) close() {
	h.cancel()
	h.conn.c2aW.Close()
	h.wg.Wait()
	h.clientWG.Wait()
	h.conn.close()
}

func (h *notifyHarness) ask(t *testing.T, line string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", h.sockPath, time.Second)
	if err != nil {
		t.Fatalf("dial cmd sock: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(line)); err != nil {
		t.Fatalf("write verb: %v", err)
	}
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	return string(buf[:n])
}

func (h *notifyHarness) lastNotify() *relayedNotify {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.notifies) == 0 {
		return nil
	}
	rn := h.notifies[len(h.notifies)-1]
	return &rn
}

// notifyCount returns how many notify frames have been relayed so far.
func (h *notifyHarness) notifyCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.notifies)
}

// seqs returns the registry-stamped Msg.Seq of every relayed notify, in order.
func (h *notifyHarness) seqs() []uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]uint64, len(h.notifies))
	for i, rn := range h.notifies {
		out[i] = rn.seq
	}
	return out
}

// TestNotify_Relay drives a verified hook notification end to end: the agent
// parses the JSON body, relays a Notify frame up the pipe, and answers "ok\n".
func TestNotify_Relay(t *testing.T) {
	h := newNotifyHarness(t, true)
	defer h.close()

	body := `{"title":"Claude finished","body":"done","urgency":0,"verified":true,"source":"claude_hook"}`
	if got := h.ask(t, "notify\t"+body+"\n"); got != "ok\n" {
		t.Fatalf("notify reply = %q, want ok\\n", got)
	}

	// Poll briefly for the relayed frame (Serve-loop write is asynchronous).
	deadline := time.Now().Add(2 * time.Second)
	for h.lastNotify() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	rn := h.lastNotify()
	if rn == nil {
		t.Fatal("no Notify frame relayed")
	}
	if rn.n.Title != "Claude finished" || rn.n.Body != "done" || !rn.n.Verified || rn.n.Source != "claude_hook" {
		t.Fatalf("relayed Notify = %+v, want verified hook fields", rn.n)
	}
	// The correlation Seq the production client surfaces (NotifyEvent.Seq) is the
	// registry-stamped Msg.Seq, not a payload field. It must be non-zero.
	if rn.seq == 0 {
		t.Errorf("relayed Msg.Seq should be non-zero (stamped by the registry)")
	}
	// The payload itself must NOT carry a duplicate seq — svc_notify never sets
	// one, and the payload struct no longer has the field.
}

// TestNotify_SeqMonotonic fires several notifications and asserts the
// registry-stamped Msg.Seq (the value the production client surfaces as
// NotifyEvent.Seq → /v1/events "seq") increments 1,2,3,... — the per-
// notification correlation the field exists for. This is the end-to-end guard
// for the v4 regression where the payload Seq was always 0.
func TestNotify_SeqMonotonic(t *testing.T) {
	h := newNotifyHarness(t, true)
	defer h.close()

	const n = 5
	for i := 0; i < n; i++ {
		if got := h.ask(t, "notify\t{\"title\":\"n\"}\n"); got != "ok\n" {
			t.Fatalf("notify #%d = %q, want ok\\n", i, got)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.notifyCount() < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := h.seqs()
	if len(got) != n {
		t.Fatalf("relayed %d notifies, want %d", len(got), n)
	}
	for i, s := range got {
		if s != uint64(i+1) {
			t.Fatalf("relayed seqs = %v, want 1..%d (monotonic per-notification)", got, n)
		}
	}
}

// TestNotify_NoClient: without a subscribed client the agent answers
// "no-client\n" rather than relaying.
func TestNotify_NoClient(t *testing.T) {
	h := newNotifyHarness(t, false)
	// Drain agent->client frames so teardown's Bye write doesn't block.
	defer h.close()

	// Bring the socket up: it only serves while configured, regardless of
	// hasClient (the verb itself checks hasClient). Wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := net.DialTimeout("unix", h.sockPath, 100*time.Millisecond); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cmd socket did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	body := `{"title":"hi","verified":false}`
	if got := h.ask(t, "notify\t"+body+"\n"); got != "no-client\n" {
		t.Fatalf("notify reply = %q, want no-client\\n", got)
	}
}

// TestNotify_DefaultDeny: malformed JSON, an empty body, a missing title, and an
// oversized body are all rejected (default-deny / bounded).
func TestNotify_DefaultDeny(t *testing.T) {
	h := newNotifyHarness(t, true)
	defer h.close()

	big := `{"title":"` + string(make([]byte, notifyBodyMax)) + `"}`
	cases := []struct {
		name string
		line string
		want string
	}{
		{"malformed json", "notify\t{not json}\n", "rejected\n"},
		{"empty body", "notify\t\n", "rejected\n"},
		{"missing title", "notify\t{\"body\":\"x\"}\n", "rejected\n"},
		{"blank title", "notify\t{\"title\":\"   \"}\n", "rejected\n"},
		{"oversized body", "notify\t" + big + "\n", "rejected\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.ask(t, tc.line); got != tc.want {
				t.Errorf("ask = %q, want %q", got, tc.want)
			}
		})
	}
}
