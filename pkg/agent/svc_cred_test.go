package agent

import (
	"bufio"
	"context"
	"encoding/base64"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/pkg/agent/watcher"
	"github.com/VikashLoomba/Portal/pkg/protocol"
)

type credResponder func(protocol.CredRequest) []protocol.CredResponse

type credHarnessConfig struct {
	clientServices map[string]uint32
	subscribe      bool
	heartbeat      time.Duration
	configure      func(*Server)
	respond        credResponder
}

type credHarness struct {
	srv       *Server
	sockPath  string
	ack       *protocol.HelloAck
	enc       *protocol.Encoder
	requests  chan protocol.CredRequest
	frames    chan *protocol.Envelope
	clientErr chan error
	cancel    context.CancelFunc
	wg        *sync.WaitGroup
	clientWG  *sync.WaitGroup
	conn      *connPair
}

type credSocketResult struct {
	reply string
	err   error
}

func sendCredResp(enc *protocol.Encoder, resp protocol.CredResponse) error {
	payload, err := protocol.MarshalPayload(resp)
	if err != nil {
		return err
	}
	return enc.Write(&protocol.Envelope{Msg: &protocol.Msg{
		Service: "cred", Kind: "resp", Payload: payload,
	}})
}

func newCredHarness(t *testing.T, respond credResponder) *credHarness {
	t.Helper()
	return newCredHarnessWithConfig(t, credHarnessConfig{
		clientServices: map[string]uint32{"cred": 1, "openurl": 1},
		subscribe:      true,
		heartbeat:      time.Hour,
		respond:        respond,
	})
}

func newCredHarnessWithConfig(t *testing.T, cfg credHarnessConfig) *credHarness {
	t.Helper()
	if cfg.heartbeat == 0 {
		cfg.heartbeat = time.Hour
	}
	sockPath := tempSockPath(t)
	w := watcher.NewFake()
	w.SetSnapshot(nil)
	conn := newConnPair()
	ctx, cancel := context.WithCancel(context.Background())
	srv := New(Config{
		In: conn.c2aR, Out: conn.a2cW, Watcher: w, AgentSHA: "testsha",
		HeartbeatInterval: cfg.heartbeat, CmdSockPath: sockPath,
	})
	if cfg.configure != nil {
		cfg.configure(srv)
	}

	h := &credHarness{
		srv: srv, sockPath: sockPath, cancel: cancel, conn: conn,
		requests:  make(chan protocol.CredRequest, 64),
		frames:    make(chan *protocol.Envelope, 64),
		clientErr: make(chan error, 1),
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.Serve(ctx)
		_ = conn.a2cW.Close()
	}()
	h.wg = &wg

	enc := protocol.NewEncoder(conn.c2aW)
	dec := protocol.NewDecoder(conn.a2cR)
	h.enc = enc
	if err := enc.Write(&protocol.Envelope{Hello: &protocol.Hello{
		ProtoVersion: protocol.ProtoVersion, Services: cfg.clientServices,
	}}); err != nil {
		t.Fatal(err)
	}
	ackEnv, err := dec.Read()
	if err != nil {
		t.Fatal(err)
	}
	if ackEnv.HelloAck == nil {
		t.Fatalf("expected HelloAck, got %+v", ackEnv)
	}
	h.ack = ackEnv.HelloAck
	if cfg.subscribe {
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
			if env.Msg != nil && env.Msg.Service == "cred" && env.Msg.Kind == "req" {
				req, err := protocol.UnmarshalPayload[protocol.CredRequest](env.Msg.Payload)
				if err != nil {
					select {
					case h.clientErr <- err:
					default:
					}
					return
				}
				h.requests <- req
				if cfg.respond != nil {
					for _, resp := range cfg.respond(req) {
						if err := sendCredResp(enc, resp); err != nil {
							select {
							case h.clientErr <- err:
							default:
							}
							return
						}
					}
				}
				continue
			}
			select {
			case h.frames <- env:
			default:
			}
		}
	}()
	h.clientWG = &clientWG

	deadline := time.Now().Add(2 * time.Second)
	for {
		probe, err := net.DialTimeout("unix", sockPath, 100*time.Millisecond)
		if err == nil {
			_ = probe.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cred cmd socket did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return h
}

func (h *credHarness) close() {
	h.cancel()
	_ = h.conn.c2aW.Close()
	h.wg.Wait()
	h.clientWG.Wait()
	h.conn.close()
}

func (h *credHarness) ask(line string) (string, error) {
	conn, err := net.DialTimeout("unix", h.sockPath, time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return "", err
	}
	if _, err := conn.Write([]byte(line)); err != nil {
		return "", err
	}
	return bufio.NewReader(conn).ReadString('\n')
}

func (h *credHarness) mustAsk(t *testing.T, line string) string {
	t.Helper()
	reply, err := h.ask(line)
	if err != nil {
		t.Fatalf("cred socket request failed: %v", err)
	}
	return reply
}

func (h *credHarness) nextRequest(t *testing.T) protocol.CredRequest {
	t.Helper()
	select {
	case req := <-h.requests:
		return req
	case err := <-h.clientErr:
		t.Fatalf("cred client loop: %v", err)
		return protocol.CredRequest{}
	case <-time.After(2 * time.Second):
		t.Fatal("no CredRequest arrived")
		return protocol.CredRequest{}
	}
}

func (h *credHarness) inflight() int {
	h.srv.reg.waiterMu.Lock()
	defer h.srv.reg.waiterMu.Unlock()
	return h.srv.reg.inflight["cred"]
}

func (h *credHarness) waitInflight(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := h.inflight(); got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("cred inflight = %d, want %d", got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *credHarness) waitHeartbeat(t *testing.T, nonce uint64) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case env := <-h.frames:
			if env.Heartbeat != nil && env.Heartbeat.Nonce == nonce {
				return
			}
		case err := <-h.clientErr:
			t.Fatalf("cred client loop: %v", err)
		case <-deadline:
			t.Fatalf("no Heartbeat with nonce %d", nonce)
		}
	}
}

func (h *credHarness) waitOpenURL(t *testing.T, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case env := <-h.frames:
			if env.Msg == nil || env.Msg.Service != "openurl" {
				continue
			}
			got, err := protocol.UnmarshalPayload[protocol.OpenURL](env.Msg.Payload)
			if err != nil {
				t.Fatalf("decode OpenURL: %v", err)
			}
			if got.URL != want {
				t.Fatalf("OpenURL = %q, want %q", got.URL, want)
			}
			return
		case err := <-h.clientErr:
			t.Fatalf("cred client loop: %v", err)
		case <-deadline:
			t.Fatal("no openurl Msg arrived")
		}
	}
}

func credRequestLine(t *testing.T, req CredShimReq) string {
	t.Helper()
	raw, err := protocol.MarshalPayload(req)
	if err != nil {
		t.Fatalf("marshal CredShimReq: %v", err)
	}
	return "cred\t" + base64.StdEncoding.EncodeToString(raw) + "\n"
}

func TestCred_ServiceContractAndRegistration(t *testing.T) {
	h := newCredHarness(t, nil)
	defer h.close()

	if h.srv.cred == nil {
		t.Fatal("Server did not retain the registered credService")
	}
	if got := h.ack.Services["cred"]; got != 1 {
		t.Fatalf("HelloAck.Services[cred] = %d, want 1", got)
	}
	c := h.srv.cred
	if c.Name() != "cred" || c.Version() != 1 || c.OutboxCap() != 2 || c.MaxPayload() != 8192 {
		t.Fatalf("cred service contract = name %q version %d outbox %d payload %d",
			c.Name(), c.Version(), c.OutboxCap(), c.MaxPayload())
	}
	if c.credTimeout != 130*time.Second || c.credSockDeadline != 135*time.Second || c.maxInflight != 1 {
		t.Fatalf("cred defaults = timeout %v socket %v inflight %d",
			c.credTimeout, c.credSockDeadline, c.maxInflight)
	}
	verbs := c.Verbs()
	if len(verbs) != 1 || verbs[0].Name != "cred" || verbs[0].Deadline != c.credSockDeadline {
		t.Fatalf("cred verbs = %+v", verbs)
	}
}

func TestCred_FramingRoundTrip(t *testing.T) {
	secret := []byte{0x00, 0x01, 0xfe, 0xff, '\n'}
	h := newCredHarness(t, func(req protocol.CredRequest) []protocol.CredResponse {
		resp := protocol.CredResponse{Nonce: req.Nonce, Epoch: req.Epoch}
		switch req.Label {
		case "allow":
			resp.OK = true
			resp.Secret = secret
		case "empty-reason":
		default:
			resp.Err = req.Label
		}
		return []protocol.CredResponse{resp}
	})
	defer h.close()

	full := CredShimReq{
		Label: "allow", Mode: "env", Target: "PW",
		Requester: "pid 4242: sh -c curl",
	}
	wantOK := "ok\t" + base64.StdEncoding.EncodeToString(secret) + "\n"
	if got := h.mustAsk(t, credRequestLine(t, full)); got != wantOK {
		t.Fatalf("allow reply = %q, want %q", got, wantOK)
	}
	request := h.nextRequest(t)
	if request.Label != full.Label || request.Mode != full.Mode || request.Target != full.Target || request.Requester != full.Requester {
		t.Fatalf("CredRequest = %+v, want local fields %+v", request, full)
	}
	if request.Nonce == 0 || request.Epoch != h.srv.reg.epoch() {
		t.Fatalf("CredRequest correlation = nonce %d epoch %d, want nonzero/%d",
			request.Nonce, request.Epoch, h.srv.reg.epoch())
	}

	for _, reason := range []string{
		"denied", "timeout", "disabled", "cooldown", "gui-unavailable", "label-invalid",
	} {
		got := h.mustAsk(t, credRequestLine(t, CredShimReq{Label: reason, Mode: "stdin"}))
		if want := "deny\t" + reason + "\n"; got != want {
			t.Errorf("response reason %q = %q, want %q", reason, got, want)
		}
		_ = h.nextRequest(t)
	}

	if got := h.mustAsk(t, credRequestLine(t, CredShimReq{Label: "empty-reason", Mode: "askpass"})); got != "deny\tdenied\n" {
		t.Fatalf("empty response reason = %q, want deny\\tdenied\\n", got)
	}
	_ = h.nextRequest(t)
}

func TestCred_NoClientImmediate(t *testing.T) {
	line := credRequestLine(t, CredShimReq{Label: "sudo", Mode: "askpass"})
	tests := []struct {
		name string
		cfg  credHarnessConfig
	}{
		{
			name: "not subscribed",
			cfg: credHarnessConfig{
				clientServices: map[string]uint32{"cred": 1}, heartbeat: time.Hour,
			},
		},
		{
			name: "cred not advertised",
			cfg: credHarnessConfig{
				clientServices: map[string]uint32{"openurl": 1}, subscribe: true, heartbeat: time.Hour,
			},
		},
		{
			name: "cred version mismatch",
			cfg: credHarnessConfig{
				clientServices: map[string]uint32{"cred": 2}, subscribe: true, heartbeat: time.Hour,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCredHarnessWithConfig(t, tt.cfg)
			defer h.close()
			start := time.Now()
			if got := h.mustAsk(t, line); got != "deny\tno-client\n" {
				t.Fatalf("reply = %q, want deny\\tno-client\\n", got)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("no-client reply took %v, expected immediate", elapsed)
			}
			select {
			case req := <-h.requests:
				t.Fatalf("no-client path emitted CredRequest: %+v", req)
			case <-time.After(25 * time.Millisecond):
			}
		})
	}
}

func TestCred_BusyAndContextCancel(t *testing.T) {
	h := newCredHarness(t, nil)
	closed := false
	defer func() {
		if !closed {
			h.close()
		}
	}()

	firstLine := credRequestLine(t, CredShimReq{Label: "first", Mode: "stdin"})
	firstResult := make(chan credSocketResult, 1)
	go func() {
		reply, err := h.ask(firstLine)
		firstResult <- credSocketResult{reply: reply, err: err}
	}()
	_ = h.nextRequest(t)
	h.waitInflight(t, 1)

	start := time.Now()
	secondLine := credRequestLine(t, CredShimReq{Label: "second", Mode: "stdin"})
	if got := h.mustAsk(t, secondLine); got != "deny\tbusy\n" {
		t.Fatalf("second reply = %q, want deny\\tbusy\\n", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("busy reply took %v, expected immediate", elapsed)
	}
	if got := h.inflight(); got != 1 {
		t.Fatalf("cred inflight after busy denial = %d, want 1", got)
	}

	h.close()
	closed = true
	select {
	case result := <-firstResult:
		if result.err != nil {
			t.Fatalf("first request after context cancel: %v", result.err)
		}
		if result.reply != "deny\ttimeout\n" {
			t.Fatalf("context-cancel reply = %q, want deny\\ttimeout\\n", result.reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending cred request did not exit after context cancel")
	}
}

func TestCred_TimeoutBudget(t *testing.T) {
	if defaultCredTimeout >= defaultCredSockDeadline {
		t.Fatalf("default budget ordering violated: credTimeout %v >= credSockDeadline %v",
			defaultCredTimeout, defaultCredSockDeadline)
	}
	const shortTimeout = 50 * time.Millisecond
	const shortSockDeadline = 250 * time.Millisecond
	h := newCredHarnessWithConfig(t, credHarnessConfig{
		clientServices: map[string]uint32{"cred": 1},
		subscribe:      true,
		heartbeat:      time.Hour,
		configure: func(s *Server) {
			s.cred.credTimeout = shortTimeout
			s.cred.credSockDeadline = shortSockDeadline
		},
	})
	defer h.close()

	start := time.Now()
	got := h.mustAsk(t, credRequestLine(t, CredShimReq{Label: "timeout", Mode: "stdin"}))
	elapsed := time.Since(start)
	if got != "deny\ttimeout\n" {
		t.Fatalf("reply = %q, want deny\\ttimeout\\n", got)
	}
	if elapsed >= shortSockDeadline {
		t.Fatalf("timeout denial took %v, want < socket deadline %v", elapsed, shortSockDeadline)
	}
	_ = h.nextRequest(t)
}

func TestCred_OutboxFullMapsTimeout(t *testing.T) {
	r := newRegistry(nil)
	r.bindHasClient(func() bool { return true })
	host := &serviceHost{r: r}
	c := newCredService(host, r.log)
	r.register(c)
	r.setClientServices(map[string]uint32{"cred": 1})

	// Occupy both cred admission slots without draining the merged outbox. The
	// request's Call cannot emit, so the registry returns ErrCallTimeout
	// immediately rather than waiting the full human timeout.
	if !r.emit("cred", "occupied", nil) || !r.emit("cred", "occupied", nil) {
		t.Fatal("could not fill cred outbox")
	}
	line := credRequestLine(t, CredShimReq{Label: "outbox", Mode: "stdin"})
	rest := strings.TrimSuffix(strings.TrimPrefix(line, "cred\t"), "\n")
	agentConn, callerConn := net.Pipe()
	defer callerConn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer agentConn.Close()
		c.handleCred(context.Background(), agentConn, rest)
	}()
	_ = callerConn.SetDeadline(time.Now().Add(time.Second))
	reply, err := bufio.NewReader(callerConn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if reply != "deny\ttimeout\n" {
		t.Fatalf("outbox-full reply = %q, want deny\\ttimeout\\n", reply)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("outbox-full cred handler did not return immediately")
	}
}

func TestCred_StaleEpochResponseDropped(t *testing.T) {
	wantSecret := []byte("correct-secret")
	h := newCredHarness(t, func(req protocol.CredRequest) []protocol.CredResponse {
		return []protocol.CredResponse{
			{Nonce: req.Nonce, Epoch: req.Epoch ^ 1, OK: true, Secret: []byte("stale-secret")},
			{Nonce: req.Nonce, Epoch: req.Epoch, OK: true, Secret: wantSecret},
		}
	})
	defer h.close()

	want := "ok\t" + base64.StdEncoding.EncodeToString(wantSecret) + "\n"
	if got := h.mustAsk(t, credRequestLine(t, CredShimReq{Label: "epoch", Mode: "env"})); got != want {
		t.Fatalf("reply = %q, want correct-epoch response %q", got, want)
	}
	_ = h.nextRequest(t)
}

func TestCred_MalformedAndInvalidRequest(t *testing.T) {
	h := newCredHarness(t, nil)
	defer h.close()

	invalidCBOR := base64.StdEncoding.EncodeToString([]byte{0xa1, 0x61, 'l'})
	scalarCBOR := base64.StdEncoding.EncodeToString([]byte{0x01})
	tests := []struct {
		name string
		line string
	}{
		{name: "malformed base64", line: "cred\t%%%\n"},
		{name: "truncated CBOR", line: "cred\t" + invalidCBOR + "\n"},
		{name: "non-map CBOR", line: "cred\t" + scalarCBOR + "\n"},
		{name: "empty label", line: credRequestLine(t, CredShimReq{Mode: "stdin"})},
		{name: "invalid mode", line: credRequestLine(t, CredShimReq{Label: "label", Mode: "file"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.mustAsk(t, tt.line); got != "deny\tlabel-invalid\n" {
				t.Fatalf("reply = %q, want deny\\tlabel-invalid\\n", got)
			}
			select {
			case req := <-h.requests:
				t.Fatalf("invalid request emitted CredRequest: %+v", req)
			default:
			}
		})
	}
}

func TestCred_RequestByteCaps(t *testing.T) {
	h := newCredHarness(t, func(req protocol.CredRequest) []protocol.CredResponse {
		return []protocol.CredResponse{{Nonce: req.Nonce, Epoch: req.Epoch, Err: "denied"}}
	})
	defer h.close()

	boundary := CredShimReq{
		Label: strings.Repeat("l", maxCredLabelBytes), Mode: "env",
		Requester: strings.Repeat("r", maxCredContextBytes),
		Target:    strings.Repeat("t", maxCredContextBytes),
	}
	if got := h.mustAsk(t, credRequestLine(t, boundary)); got != "deny\tdenied\n" {
		t.Fatalf("at-cap request = %q, want forwarded denial", got)
	}
	gotBoundary := h.nextRequest(t)
	if len(gotBoundary.Label) != maxCredLabelBytes || len(gotBoundary.Requester) != maxCredContextBytes || len(gotBoundary.Target) != maxCredContextBytes {
		t.Fatalf("at-cap CredRequest lengths = label %d requester %d target %d",
			len(gotBoundary.Label), len(gotBoundary.Requester), len(gotBoundary.Target))
	}

	tests := []struct {
		name string
		req  CredShimReq
	}{
		{
			name: "label over cap",
			req:  CredShimReq{Label: strings.Repeat("l", maxCredLabelBytes+1), Mode: "env"},
		},
		{
			name: "multibyte label over byte cap",
			req:  CredShimReq{Label: strings.Repeat("é", maxCredLabelBytes/2+1), Mode: "env"},
		},
		{
			name: "requester over cap",
			req: CredShimReq{
				Label: "label", Mode: "stdin", Requester: strings.Repeat("r", maxCredContextBytes+1),
			},
		},
		{
			name: "target over cap",
			req: CredShimReq{
				Label: "label", Mode: "askpass", Target: strings.Repeat("t", maxCredContextBytes+1),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.mustAsk(t, credRequestLine(t, tt.req)); got != "deny\tlabel-invalid\n" {
				t.Fatalf("reply = %q, want deny\\tlabel-invalid\\n", got)
			}
			select {
			case req := <-h.requests:
				t.Fatalf("over-cap request emitted CredRequest: %+v", req)
			default:
			}
		})
	}
}

func TestCred_PendingCallDoesNotBlockHeartbeatOrOtherService(t *testing.T) {
	h := newCredHarness(t, nil)
	closed := false
	defer func() {
		if !closed {
			h.close()
		}
	}()

	line := credRequestLine(t, CredShimReq{Label: "pending", Mode: "stdin"})
	resultCh := make(chan credSocketResult, 1)
	go func() {
		reply, err := h.ask(line)
		resultCh <- credSocketResult{reply: reply, err: err}
	}()
	_ = h.nextRequest(t)
	h.waitInflight(t, 1)

	const pingNonce = 0x1122334455667788
	if err := h.enc.Write(&protocol.Envelope{Ping: &protocol.Ping{Nonce: pingNonce}}); err != nil {
		t.Fatal(err)
	}
	h.waitHeartbeat(t, pingNonce)

	const url = "http://example.com/while-cred-pends"
	if got := h.mustAsk(t, "open\t"+url+"\n"); got != "ok\n" {
		t.Fatalf("open while cred pending = %q, want ok\\n", got)
	}
	h.waitOpenURL(t, url)
	if got := h.inflight(); got != 1 {
		t.Fatalf("pending cred inflight after heartbeat/open = %d, want 1", got)
	}

	h.close()
	closed = true
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("pending cred after close: %v", result.err)
		}
		if result.reply != "deny\ttimeout\n" {
			t.Fatalf("pending reply = %q, want deny\\ttimeout\\n", result.reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending cred call did not unblock on shutdown")
	}
}
