package agent

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/agent/watcher"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/protocol"
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
	notifies []*protocol.Notify
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
	if err := enc.Write(&protocol.Envelope{Hello: &protocol.Hello{ProtoVersion: protocol.ProtoVersion}}); err != nil {
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
			if env.Notify != nil {
				h.mu.Lock()
				h.notifies = append(h.notifies, env.Notify)
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

func (h *notifyHarness) lastNotify() *protocol.Notify {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.notifies) == 0 {
		return nil
	}
	return h.notifies[len(h.notifies)-1]
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
	n := h.lastNotify()
	if n == nil {
		t.Fatal("no Notify frame relayed")
	}
	if n.Title != "Claude finished" || n.Body != "done" || !n.Verified || n.Source != "claude_hook" {
		t.Fatalf("relayed Notify = %+v, want verified hook fields", n)
	}
	if n.Seq == 0 {
		t.Errorf("Notify.Seq should be non-zero (stamped by the agent)")
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
