package agent

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VikashLoomba/Portal/pkg/agent/watcher"
	"github.com/VikashLoomba/Portal/pkg/protocol"
)

type clipWriteResponder func(protocol.ClipWriteRequest) []protocol.ClipWriteResponse

type clipWriteHarnessConfig struct {
	clientServices map[string]uint32
	subscribe      bool
	heartbeat      time.Duration
	configure      func(*Server)
	respond        clipWriteResponder
}

type clipWriteHarness struct {
	srv       *Server
	sockPath  string
	ack       *protocol.HelloAck
	enc       *protocol.Encoder
	requests  chan protocol.ClipWriteRequest
	frames    chan *protocol.Envelope
	clientErr chan error
	cancel    context.CancelFunc
	wg        *sync.WaitGroup
	clientWG  *sync.WaitGroup
	conn      *connPair
}

type clipWriteSocketResult struct {
	reply string
	err   error
}

func sendClipWriteResp(enc *protocol.Encoder, resp protocol.ClipWriteResponse) error {
	payload, err := protocol.MarshalPayload(resp)
	if err != nil {
		return err
	}
	return enc.Write(&protocol.Envelope{Msg: &protocol.Msg{
		Service: "clipwrite", Kind: "resp", Payload: payload,
	}})
}

func newClipWriteHarness(t *testing.T, respond clipWriteResponder) *clipWriteHarness {
	t.Helper()
	return newClipWriteHarnessWithConfig(t, clipWriteHarnessConfig{
		clientServices: map[string]uint32{"clipwrite": 1, "openurl": 1},
		subscribe:      true,
		heartbeat:      time.Hour,
		respond:        respond,
	})
}

func newClipWriteHarnessWithConfig(t *testing.T, cfg clipWriteHarnessConfig) *clipWriteHarness {
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

	h := &clipWriteHarness{
		srv: srv, sockPath: sockPath, cancel: cancel, conn: conn,
		requests:  make(chan protocol.ClipWriteRequest, 64),
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
		if _, err := dec.Read(); err != nil {
			t.Fatal(err)
		}
		if _, err := dec.Read(); err != nil {
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
			if env.Msg != nil && env.Msg.Service == "clipwrite" && env.Msg.Kind == "req" {
				req, err := protocol.UnmarshalPayload[protocol.ClipWriteRequest](env.Msg.Payload)
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
						if err := sendClipWriteResp(enc, resp); err != nil {
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
			t.Fatal("clipwrite cmd socket did not come up")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return h
}

func (h *clipWriteHarness) close() {
	h.cancel()
	_ = h.conn.c2aW.Close()
	h.wg.Wait()
	h.clientWG.Wait()
	h.conn.close()
}

func (h *clipWriteHarness) ask(line string) (string, error) {
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

func (h *clipWriteHarness) mustAsk(t *testing.T, line string) string {
	t.Helper()
	reply, err := h.ask(line)
	if err != nil {
		t.Fatalf("clipwrite socket request failed: %v", err)
	}
	return reply
}

func (h *clipWriteHarness) nextRequest(t *testing.T) protocol.ClipWriteRequest {
	t.Helper()
	select {
	case req := <-h.requests:
		return req
	case err := <-h.clientErr:
		t.Fatalf("clipwrite client loop: %v", err)
		return protocol.ClipWriteRequest{}
	case <-time.After(2 * time.Second):
		t.Fatal("no ClipWriteRequest arrived")
		return protocol.ClipWriteRequest{}
	}
}

func (h *clipWriteHarness) inflight() int {
	h.srv.reg.waiterMu.Lock()
	defer h.srv.reg.waiterMu.Unlock()
	return h.srv.reg.inflight["clipwrite"]
}

func (h *clipWriteHarness) waitInflight(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := h.inflight(); got == want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("clipwrite inflight = %d, want %d", got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestClipWrite_ServiceContractAndRegistration(t *testing.T) {
	h := newClipWriteHarness(t, nil)
	defer h.close()

	if h.srv.clipWrite == nil {
		t.Fatal("Server did not retain the registered clipWriteService")
	}
	if got := h.ack.Services["clipwrite"]; got != 1 {
		t.Fatalf("HelloAck.Services[clipwrite] = %d, want 1", got)
	}
	c := h.srv.clipWrite
	if c.Name() != "clipwrite" || c.Version() != 1 || c.OutboxCap() != 8 || c.MaxPayload() != 4096 {
		t.Fatalf("clipwrite service contract = name %q version %d outbox %d payload %d",
			c.Name(), c.Version(), c.OutboxCap(), c.MaxPayload())
	}
	if c.clipWriteTimeout != 9*time.Second || c.clipWriteSockDeadline != 11*time.Second || c.maxInflight != 4 {
		t.Fatalf("clipwrite defaults = timeout %v socket %v inflight %d",
			c.clipWriteTimeout, c.clipWriteSockDeadline, c.maxInflight)
	}
	verbs := c.Verbs()
	if len(verbs) != 1 || verbs[0].Name != "copy" || verbs[0].Deadline != c.clipWriteSockDeadline {
		t.Fatalf("clipwrite verbs = %+v", verbs)
	}
}

func TestClipWrite_VerbGrammarRoundTrip(t *testing.T) {
	h := newClipWriteHarness(t, func(req protocol.ClipWriteRequest) []protocol.ClipWriteResponse {
		return []protocol.ClipWriteResponse{{
			Nonce: req.Nonce, Epoch: req.Epoch, OK: true,
		}}
	})
	defer h.close()

	const (
		textSHA  = "0123456789abcdef0123456789abcdef"
		imageSHA = "fedcba9876543210fedcba9876543210"
	)
	tests := []struct {
		name string
		line string
		want protocol.ClipWriteRequest
	}{
		{
			name: "text",
			line: "copy\ttext\t" + textSHA + "\t42\n",
			want: protocol.ClipWriteRequest{Kind: "text", SHA: textSHA, Size: 42},
		},
		{
			name: "image",
			line: "copy\timage\tpng\t" + imageSHA + "\t118000\n",
			want: protocol.ClipWriteRequest{
				Kind: "image", Format: "png", SHA: imageSHA, Size: 118000,
			},
		},
		{
			name: "clear",
			line: "copy\tclear\n",
			want: protocol.ClipWriteRequest{Kind: "clear"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.mustAsk(t, tt.line); got != "ok\n" {
				t.Fatalf("reply = %q, want ok\\n", got)
			}
			req := h.nextRequest(t)
			if req.Kind != tt.want.Kind || req.Format != tt.want.Format || req.SHA != tt.want.SHA || req.Size != tt.want.Size {
				t.Fatalf("ClipWriteRequest = %+v, want fields %+v", req, tt.want)
			}
			if req.Nonce == 0 || req.Epoch != h.srv.reg.epoch() {
				t.Fatalf("ClipWriteRequest correlation = nonce %d epoch %d, want nonzero/%d",
					req.Nonce, req.Epoch, h.srv.reg.epoch())
			}
		})
	}
}

func TestClipWrite_RejectedShapes(t *testing.T) {
	h := newClipWriteHarness(t, nil)
	defer h.close()

	const sha = "0123456789abcdef0123456789abcdef"
	tests := []struct {
		name string
		line string
	}{
		{name: "bare copy", line: "copy\n"},
		{name: "text no sha", line: "copy\ttext\n"},
		{name: "text no size", line: "copy\ttext\t" + sha + "\n"},
		{name: "zero size", line: "copy\ttext\t" + sha + "\t0\n"},
		{name: "negative size", line: "copy\ttext\t" + sha + "\t-1\n"},
		{name: "oversized", line: "copy\ttext\t" + sha + "\t8388609\n"},
		{name: "invalid size", line: "copy\ttext\t" + sha + "\tnope\n"},
		{name: "plus size", line: "copy\ttext\t" + sha + "\t+10\n"},
		{name: "leading zero size", line: "copy\ttext\t" + sha + "\t010\n"},
		{name: "space size", line: "copy\ttext\t" + sha + "\t 10\n"},
		{name: "uppercase sha", line: "copy\ttext\t0123456789ABCDEF0123456789ABCDEF\t10\n"},
		{name: "short sha", line: "copy\ttext\t0123456789abcdef0123456789abcde\t10\n"},
		{name: "long sha", line: "copy\ttext\t0123456789abcdef0123456789abcdef0\t10\n"},
		{name: "nonhex sha", line: "copy\ttext\t0123456789abcdef0123456789abcdeg\t10\n"},
		{name: "image no format", line: "copy\timage\t" + sha + "\t10\n"},
		{name: "image jpeg", line: "copy\timage\tjpeg\t" + sha + "\t10\n"},
		{name: "image no size", line: "copy\timage\tpng\t" + sha + "\n"},
		{name: "clear trailing token", line: "copy\tclear\textra\n"},
		{name: "unknown kind", line: "copy\tbogus\t" + sha + "\t10\n"},
		{name: "text trailing token", line: "copy\ttext\t" + sha + "\t10\textra\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.mustAsk(t, tt.line); got != "rejected\n" {
				t.Fatalf("reply = %q, want rejected\\n", got)
			}
			select {
			case req := <-h.requests:
				t.Fatalf("rejected request emitted ClipWriteRequest: %+v", req)
			case <-time.After(25 * time.Millisecond):
			}
		})
	}
}

func TestClipWrite_NoClientImmediateNone(t *testing.T) {
	const line = "copy\tclear\n"
	tests := []struct {
		name string
		cfg  clipWriteHarnessConfig
	}{
		{
			name: "not subscribed",
			cfg: clipWriteHarnessConfig{
				clientServices: map[string]uint32{"clipwrite": 1}, heartbeat: time.Hour,
			},
		},
		{
			name: "clipwrite not advertised",
			cfg: clipWriteHarnessConfig{
				clientServices: map[string]uint32{"openurl": 1}, subscribe: true, heartbeat: time.Hour,
			},
		},
		{
			name: "clipwrite version mismatch",
			cfg: clipWriteHarnessConfig{
				clientServices: map[string]uint32{"clipwrite": 2}, subscribe: true, heartbeat: time.Hour,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newClipWriteHarnessWithConfig(t, tt.cfg)
			defer h.close()
			start := time.Now()
			if got := h.mustAsk(t, line); got != "none\n" {
				t.Fatalf("reply = %q, want none\\n", got)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("no-client reply took %v, expected immediate", elapsed)
			}
			select {
			case req := <-h.requests:
				t.Fatalf("no-client path emitted ClipWriteRequest: %+v", req)
			case <-time.After(25 * time.Millisecond):
			}
		})
	}
}

func TestClipWrite_ResponseNotOKMapsNone(t *testing.T) {
	h := newClipWriteHarness(t, func(req protocol.ClipWriteRequest) []protocol.ClipWriteResponse {
		return []protocol.ClipWriteResponse{{
			Nonce: req.Nonce, Epoch: req.Epoch, Err: "disabled",
		}}
	})
	defer h.close()

	if got := h.mustAsk(t, "copy\tclear\n"); got != "none\n" {
		t.Fatalf("reply = %q, want none\\n without response error leakage", got)
	}
	_ = h.nextRequest(t)
}

func TestClipWrite_StaleEpochResponseDropped(t *testing.T) {
	h := newClipWriteHarness(t, func(req protocol.ClipWriteRequest) []protocol.ClipWriteResponse {
		return []protocol.ClipWriteResponse{
			{Nonce: req.Nonce, Epoch: req.Epoch ^ 1, OK: true},
			{Nonce: req.Nonce, Epoch: req.Epoch, OK: true},
		}
	})
	defer h.close()

	if got := h.mustAsk(t, "copy\tclear\n"); got != "ok\n" {
		t.Fatalf("reply = %q, want correct-epoch ok\\n", got)
	}
	_ = h.nextRequest(t)
}

func TestClipWrite_MaxInflight(t *testing.T) {
	h := newClipWriteHarness(t, nil)
	closed := false
	var parkedWG sync.WaitGroup
	defer func() {
		if !closed {
			h.close()
		}
		parkedWG.Wait()
	}()

	results := make(chan clipWriteSocketResult, defaultMaxInflightClipWrite)
	for i := 0; i < defaultMaxInflightClipWrite; i++ {
		parkedWG.Add(1)
		go func() {
			defer parkedWG.Done()
			reply, err := h.ask("copy\tclear\n")
			results <- clipWriteSocketResult{reply: reply, err: err}
		}()
	}
	h.waitInflight(t, defaultMaxInflightClipWrite)

	start := time.Now()
	if got := h.mustAsk(t, "copy\tclear\n"); got != "none\n" {
		t.Fatalf("over-cap reply = %q, want none\\n", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("over-cap reply took %v, expected immediate", elapsed)
	}

	h.close()
	closed = true
	for i := 0; i < defaultMaxInflightClipWrite; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("parked request after context cancel: %v", result.err)
		}
		if result.reply != "none\n" {
			t.Fatalf("parked reply = %q, want none\\n", result.reply)
		}
	}
}

func TestClipWrite_OutboxFullMapsNone(t *testing.T) {
	r := newRegistry(nil)
	r.bindHasClient(func() bool { return true })
	host := &serviceHost{r: r}
	c := newClipWriteService(host, r.log)
	r.register(c)
	r.setClientServices(map[string]uint32{"clipwrite": 1})

	for i := 0; i < c.OutboxCap(); i++ {
		if !r.emit("clipwrite", "occupied", nil) {
			t.Fatalf("could not fill clipwrite outbox at slot %d", i)
		}
	}
	const sha = "0123456789abcdef0123456789abcdef"
	agentConn, callerConn := net.Pipe()
	defer callerConn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer agentConn.Close()
		c.handleCopy(context.Background(), agentConn, "text\t"+sha+"\t10")
	}()
	_ = callerConn.SetDeadline(time.Now().Add(time.Second))
	reply, err := bufio.NewReader(callerConn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if reply != "none\n" {
		t.Fatalf("outbox-full reply = %q, want none\\n", reply)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("outbox-full clipwrite handler did not return immediately")
	}
}

func TestClipWrite_TimeoutBudget(t *testing.T) {
	if defaultClipWriteTimeout >= defaultClipWriteSockDeadline {
		t.Fatalf("default budget ordering violated: clipWriteTimeout %v >= clipWriteSockDeadline %v",
			defaultClipWriteTimeout, defaultClipWriteSockDeadline)
	}
	const shortTimeout = 50 * time.Millisecond
	const shortSockDeadline = 250 * time.Millisecond
	h := newClipWriteHarnessWithConfig(t, clipWriteHarnessConfig{
		clientServices: map[string]uint32{"clipwrite": 1},
		subscribe:      true,
		heartbeat:      time.Hour,
		configure: func(s *Server) {
			s.clipWrite.clipWriteTimeout = shortTimeout
			s.clipWrite.clipWriteSockDeadline = shortSockDeadline
		},
	})
	defer h.close()

	start := time.Now()
	got := h.mustAsk(t, "copy\tclear\n")
	elapsed := time.Since(start)
	if got != "none\n" {
		t.Fatalf("reply = %q, want none\\n", got)
	}
	if elapsed >= shortSockDeadline {
		t.Fatalf("timeout reply took %v, want < socket deadline %v", elapsed, shortSockDeadline)
	}
	_ = h.nextRequest(t)
}

func TestClipWrite_ContextCancelMapsNone(t *testing.T) {
	h := newClipWriteHarness(t, nil)
	closed := false
	defer func() {
		if !closed {
			h.close()
		}
	}()

	resultCh := make(chan clipWriteSocketResult, 1)
	go func() {
		reply, err := h.ask("copy\tclear\n")
		resultCh <- clipWriteSocketResult{reply: reply, err: err}
	}()
	_ = h.nextRequest(t)
	h.waitInflight(t, 1)

	h.close()
	closed = true
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("request after context cancel: %v", result.err)
		}
		if result.reply != "none\n" {
			t.Fatalf("context-cancel reply = %q, want none\\n", result.reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending clipwrite request did not exit after context cancel")
	}
}

func TestParseCopyRequestCanonicalSize(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef"
	kind, format, gotSHA, size, ok := parseCopyRequest("image\tpng\t" + sha + "\t8388608")
	if !ok || kind != "image" || format != "png" || gotSHA != sha || size != maxClipWriteBytes {
		t.Fatalf("at-cap parse = (%q, %q, %q, %d, %t)", kind, format, gotSHA, size, ok)
	}
	for _, value := range []string{"+10", "010", " 10", "10 "} {
		if _, _, _, _, ok := parseCopyRequest("text\t" + sha + "\t" + value); ok {
			t.Errorf("non-canonical size %q accepted", value)
		}
	}
}

func TestParseCopyRequestClearHasNoPayloadFields(t *testing.T) {
	kind, format, sha, size, ok := parseCopyRequest("clear")
	if !ok || kind != "clear" || format != "" || sha != "" || size != 0 {
		t.Fatalf("clear parse = (%q, %q, %q, %d, %t)", kind, format, sha, size, ok)
	}
}

func TestClipWrite_UnknownResponseKindIgnored(t *testing.T) {
	h := newClipWriteHarness(t, nil)
	defer h.close()

	payload, err := protocol.MarshalPayload(protocol.ClipWriteResponse{
		Nonce: 1, Epoch: h.srv.reg.epoch(), OK: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv.clipWrite.HandleMsg("req", payload)
	if got := h.inflight(); got != 0 {
		t.Fatalf("unexpected inflight count after ignored response kind: %d", got)
	}
}

func TestClipWrite_ResponseDecodeFailureDropped(t *testing.T) {
	h := newClipWriteHarness(t, nil)
	defer h.close()

	h.srv.clipWrite.HandleMsg("resp", []byte{0xff})
	if got := h.inflight(); got != 0 {
		t.Fatalf("unexpected inflight count after malformed response: %d", got)
	}
}

func TestClipWriteParserRejectsEmbeddedSeparators(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef"
	for _, rest := range []string{
		"text\t" + sha + "\t10\textra",
		"image\tpng\t" + sha + "\t10\textra",
		"clear\textra",
		strings.Join([]string{"text", sha, "10", ""}, "\t"),
	} {
		if _, _, _, _, ok := parseCopyRequest(rest); ok {
			t.Errorf("parseCopyRequest(%q) accepted", rest)
		}
	}
}
