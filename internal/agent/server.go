package agent

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/agent/watcher"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/protocol"
)

// Config bundles the constructor inputs. Everything is injected so the
// server is fully testable with FakeWatcher + bytes.Buffer pipes.
type Config struct {
	In                io.Reader // stdin (Mac → agent)
	Out               io.Writer // stdout (agent → Mac)
	Watcher           watcher.Watcher
	AgentSHA          string // baked at build time via -ldflags
	Kernel            string // typically "uname -r"; OK to leave empty
	BootID            string // /proc/sys/kernel/random/boot_id; OK to leave empty
	EphemMin          uint16
	EphemMax          uint16
	HeartbeatInterval time.Duration // default 5s
	// BackpressureKill is the maximum time the watcher→encoder channel may
	// stay full before the agent sends AgentError{Fatal} and exits. This
	// is the design-spec "5-second kill-switch" for a stalled client.
	// Default 5s. Set 0 to disable.
	BackpressureKill time.Duration
	Log              *slog.Logger // stderr handler; nil → discard
	Now              func() time.Time
	// CmdSockPath is the Unix socket path where `portald open <url>`
	// connects to relay URLs back to the Mac client. Empty = disabled.
	// The socket is only active while a Mac client is subscribed.
	CmdSockPath string
}

// Clip request handling tunables. These keep the whole paste round trip well
// under the client's HeartbeatTimeout (12s) so a paste never trips a reconnect
// (see DESIGN §4.5). The clip socket deadline (~11s) is strictly greater than
// clipTimeout (~9s) so the agent always writes "none\n" before the socket read
// deadline fires; both are strictly less than the shim's dial+read deadline so
// the shim never gives up before the agent answers.
const (
	// clipTimeout bounds how long handleClipReq waits on the Mac client for a
	// ClipResponse before answering the shim with "none\n".
	clipTimeout = 9 * time.Second
	// clipSockDeadline is the cmd-socket read/write deadline applied to clip
	// verbs only (open keeps its tighter 5s). > clipTimeout so the agent's
	// own "none\n" write always wins the race against the socket deadline.
	clipSockDeadline = 11 * time.Second
	// maxInflightClip bounds concurrent clip waiters as a DoS guard (DESIGN
	// §7.1): a same-uid process spamming the socket cannot fork unbounded
	// waiters / pending ClipRequest writes on the Serve loop.
	maxInflightClip = 4
)

// Server is the agent's RPC top loop. One Server per ssh-exec lifetime.
type Server struct {
	cfg    Config
	filter *Filter
	enc    *protocol.Encoder
	dec    *protocol.Decoder

	// reg is the service registry. It auto-registers the compiled-in openurl
	// service in New and owns the per-service outbox the Serve loop drains, the
	// inbound Msg dispatch, and (from u4/u5) the notify/clip services. It never
	// holds an *Encoder — the Serve loop stays the sole agent→client writer.
	reg *registry

	mu          sync.Mutex
	seq         uint64
	lastRSID    uint64
	lastEmitted map[uint16]protocol.Port
	startedAt   time.Time
	hasClient   bool // true once SubscribeAck has been sent; gates cmd socket

	// clipWaiters correlates an outstanding ClipRequest (keyed by Nonce) with
	// the handleClipReq goroutine waiting on its ClipResponse. Guarded by s.mu.
	// A ClipResponse with a matching (Nonce,Epoch) is delivered non-blocking to
	// the registered channel; an unmatched or stale-epoch response is dropped.
	clipWaiters map[uint64]chan *protocol.ClipResponse
	// clipSeq is the monotonic nonce source for ClipRequest. It is DELIBERATELY
	// separate from s.seq (the port-event staleness counter the client compares
	// against) — emitting a ClipRequest must never advance s.seq. Bumped via
	// atomic so handleClipReq can mint a nonce without taking s.mu.
	clipSeq uint64
	// epoch is this Server process's clip identity, seeded randomly at New().
	// It is echoed in every ClipRequest and must match in a ClipResponse;
	// because it is random per process, a stale ClipResponse arriving down a
	// NEW pipe after reconnect (where clipSeq reset to 0 on the peer) is dropped
	// on the epoch check rather than mis-delivered to a fresh waiter. Immutable
	// after New(), so it needs no lock.
	epoch uint64

	// clipReqCh carries ClipRequest envelopes from handleClipReq to the Serve
	// loop, which is the SOLE writer of agent→client frames (mirrors openURLCh).
	// handleClipReq never writes the envelope itself — that would race the
	// Serve goroutine's enc.Write. Buffered so a brief Serve stall doesn't block
	// the cmd-socket goroutine; a full channel degrades to "none\n".
	clipReqCh chan *protocol.ClipRequest

	// bpDeadline fires if the openURLCh or the main enc write stays stalled
	// for BackpressureKill. Nil when nothing is queued.
	bpDeadline *time.Timer
	bpMu       sync.Mutex

	// bpKillCh is closed when the backpressure deadline fires.
	bpKillCh chan struct{}
}

// New constructs a Server. Defaults are filled in.
func New(cfg Config) *Server {
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 5 * time.Second
	}
	if cfg.BackpressureKill == 0 {
		cfg.BackpressureKill = 5 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.EphemMin == 0 {
		cfg.EphemMin = 32768
	}
	if cfg.EphemMax == 0 {
		cfg.EphemMax = 60999
	}
	s := &Server{
		cfg:         cfg,
		filter:      NewFilter(cfg.EphemMin, cfg.EphemMax),
		enc:         protocol.NewEncoder(cfg.Out),
		dec:         protocol.NewDecoder(cfg.In),
		lastEmitted: map[uint16]protocol.Port{},
		clipWaiters: map[uint64]chan *protocol.ClipResponse{},
		clipReqCh:   make(chan *protocol.ClipRequest, 8),
		epoch:       randEpoch(),
		bpKillCh:    make(chan struct{}),
	}
	// Build the registry and bind the Server's guarded subscription reader so
	// services can gate on `hasClient() && clientHas(svc)` without ever touching
	// s.mu directly. Then auto-register the compiled-in services (openurl and
	// notify this unit; clip stays legacy until u5).
	s.reg = newRegistry(cfg.Log)
	s.reg.bindHasClient(func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.hasClient })
	s.reg.register(newOpenURLService(s.reg, cfg.Log))
	s.reg.register(newNotifyService(s.reg, cfg.Log))
	return s
}

// randEpoch returns a non-zero random clip epoch. A zero epoch would be
// indistinguishable from an unset field, so on the astronomically unlikely
// all-zero draw we fall back to 1.
func randEpoch() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail; if it does, any fixed non-zero value
		// is still correct (epoch only needs to differ across reconnects, and
		// a new Server is a new process so this branch is effectively dead).
		return 1
	}
	e := binary.LittleEndian.Uint64(b[:])
	if e == 0 {
		return 1
	}
	return e
}

// Serve runs the agent until ctx is cancelled, stdin closes, or the agent
// hits a fatal protocol error. Returns nil on graceful exit; err on fatal.
func (s *Server) Serve(ctx context.Context) error {
	s.startedAt = s.cfg.Now()

	// 1. Wait for Hello.
	hello, err := s.readHello()
	if err != nil {
		return err
	}
	if hello.ProtoVersion != protocol.ProtoVersion {
		_ = s.enc.Write(&protocol.Envelope{AgentError: &protocol.AgentError{
			Code: protocol.CodeProtocolMismatch, Fatal: true,
			Msg: fmt.Sprintf("agent supports pv=%d, got %d", protocol.ProtoVersion, hello.ProtoVersion),
		}})
		return fmt.Errorf("proto version mismatch: %d vs %d", hello.ProtoVersion, protocol.ProtoVersion)
	}
	// Record the client's advertised services (DESIGN S4). A registered service
	// the client advertises at a mismatched version is treated as absent (one
	// warning logged here); a service the client omits gates its cmd-verb callers
	// to "no-client\n" exactly as an unsubscribed client would.
	s.reg.setClientServices(hello.Services)

	// 2. HelloAck — advertises the agent's registered services symmetrically.
	if err := s.enc.Write(&protocol.Envelope{HelloAck: &protocol.HelloAck{
		ProtoVersion: protocol.ProtoVersion,
		AgentGitSHA:  s.cfg.AgentSHA,
		AgentPID:     os.Getpid(),
		Kernel:       s.cfg.Kernel,
		BootID:       s.cfg.BootID,
		EphemMin:     s.cfg.EphemMin,
		EphemMax:     s.cfg.EphemMax,
		NowUnixNano:  s.cfg.Now().UnixNano(),
		Services:     s.reg.services(),
	}}); err != nil {
		return err
	}

	// 3. Start watcher.
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	events, err := s.cfg.Watcher.Start(wctx)
	if err != nil {
		_ = s.enc.Write(&protocol.Envelope{AgentError: &protocol.AgentError{
			Code: protocol.CodeWatcherFailed, Fatal: true, Msg: err.Error(),
		}})
		return err
	}

	// 4. Spawn read-loop goroutine for client commands.
	cmdCh := make(chan *protocol.Envelope, 4)
	readErrCh := make(chan error, 1)
	go s.readLoop(ctx, cmdCh, readErrCh)

	// 5. Start cmd Unix socket if configured. The socket relays cmd-verb
	// requests (open/clip/notify) from `portald <verb>` on the box. It is only
	// live while a client is actively subscribed; the service handlers gate on
	// hasClient, which is set in handleSubscribe.
	if s.cfg.CmdSockPath != "" {
		go s.serveCmdSock(ctx)
	}

	hb := time.NewTicker(s.cfg.HeartbeatInterval)
	defer hb.Stop()

	for {
		select {
		case <-s.bpKillCh:
			_ = s.enc.Write(&protocol.Envelope{AgentError: &protocol.AgentError{
				Code: protocol.CodeInternalPanic, Fatal: true,
				Msg: "backpressure: client stalled for >5s",
			}})
			return errors.New("agent: client stalled (backpressure kill)")

		case <-ctx.Done():
			_ = s.enc.Write(&protocol.Envelope{Bye: &protocol.Bye{Reason: "ctx-cancel"}})
			return nil

		case err := <-readErrCh:
			if errors.Is(err, io.EOF) {
				return nil // clean: client closed stdin
			}
			s.cfg.Log.Warn("read loop error", "err", err)
			return err

		case env, ok := <-cmdCh:
			if !ok {
				return nil
			}
			if err := s.handleCommand(ctx, env); err != nil {
				if errors.Is(err, errFatalShutdown) {
					return nil
				}
				return err
			}

		case ev, ok := <-events:
			if !ok {
				// Watcher closed early — treat as fatal.
				_ = s.enc.Write(&protocol.Envelope{AgentError: &protocol.AgentError{
					Code: protocol.CodeWatcherFailed, Fatal: true, Msg: "watcher channel closed",
				}})
				return errors.New("watcher closed")
			}
			s.handleEvent(ev)

		case env := <-s.reg.outbox():
			// The RELEASED drain arm: the Serve loop is the SOLE writer of
			// agent→client frames, so every registered service's outbox is drained
			// here. Re-check hasClient to drop a frame that raced a disconnect. The
			// per-service admission budget consumed at emit() is returned via
			// reg.release — UNCONDITIONALLY, whether or not the frame was written —
			// so the per-service DropNewest budget recycles across the whole
			// session rather than after the first cap emits. Does NOT touch s.seq:
			// Msg.Seq is registry-stamped, separate from the port-event counter.
			s.mu.Lock()
			active := s.hasClient
			s.mu.Unlock()
			if active {
				_ = s.enc.Write(env)
			}
			s.reg.release(env.Msg.Service)

		case req := <-s.clipReqCh:
			// The Serve loop is the SOLE writer of agent→client frames, so the
			// ClipRequest envelope is written here (interleaved with heartbeats)
			// rather than by handleClipReq. Crucially this does NOT touch s.seq:
			// req.Nonce/req.Epoch are wholly separate counters from the
			// port-event staleness Seq the client compares against. Gate on
			// hasClient so a request that raced a disconnect is dropped (the
			// waiter still times out and answers "none\n").
			s.mu.Lock()
			active := s.hasClient
			s.mu.Unlock()
			if active {
				_ = s.enc.Write(&protocol.Envelope{ClipRequest: req})
			}

		case <-hb.C:
			s.mu.Lock()
			seq := s.seq
			s.mu.Unlock()
			_ = s.enc.Write(&protocol.Envelope{Heartbeat: &protocol.Heartbeat{
				Seq: seq, UptimeNano: s.cfg.Now().Sub(s.startedAt).Nanoseconds(),
				Now: s.cfg.Now().UnixNano(),
			}})
		}
	}
}

func (s *Server) readHello() (*protocol.Hello, error) {
	env, err := s.dec.Read()
	if err != nil {
		return nil, err
	}
	if env.Hello == nil {
		_ = s.enc.Write(&protocol.Envelope{AgentError: &protocol.AgentError{
			Code: protocol.CodeProtocolMismatch, Fatal: true, Msg: "first frame must be Hello",
		}})
		return nil, errors.New("agent: first frame was not Hello")
	}
	return env.Hello, nil
}

func (s *Server) readLoop(ctx context.Context, out chan<- *protocol.Envelope, errCh chan<- error) {
	defer close(out)
	for {
		env, err := s.dec.Read()
		if err != nil {
			select {
			case errCh <- err:
			case <-ctx.Done():
			}
			return
		}
		select {
		case out <- env:
		case <-ctx.Done():
			return
		}
	}
}

var errFatalShutdown = errors.New("client requested shutdown")

func (s *Server) handleCommand(ctx context.Context, env *protocol.Envelope) error {
	switch {
	case env.Subscribe != nil:
		return s.handleSubscribe(ctx, env.Subscribe)
	case env.ReqSnap != nil:
		return s.handleReqSnap(ctx)
	case env.Ping != nil:
		s.mu.Lock()
		seq := s.seq
		s.mu.Unlock()
		return s.enc.Write(&protocol.Envelope{Heartbeat: &protocol.Heartbeat{
			Seq: seq, UptimeNano: s.cfg.Now().Sub(s.startedAt).Nanoseconds(),
			Now: s.cfg.Now().UnixNano(), Nonce: env.Ping.Nonce,
		}})
	case env.ClipResponse != nil:
		s.handleClipResponse(env.ClipResponse)
		return nil
	case env.Msg != nil:
		// Inbound client→agent service frame (DESIGN §4). Only clip "resp" uses
		// this after u5; harmless now (openurl is agent→client only). Dispatch
		// runs under the registry's payload-cap/recover guards — an unknown
		// service or panicking handler drops the frame, the session lives.
		s.reg.dispatch(env.Msg)
		return nil
	case env.Shutdown != nil:
		_ = s.enc.Write(&protocol.Envelope{Bye: &protocol.Bye{Reason: env.Shutdown.Reason}})
		return errFatalShutdown
	case env.Hello != nil:
		// Second Hello after handshake is a protocol violation.
		return s.fatal(protocol.CodeProtocolMismatch, "Hello sent after handshake")
	default:
		// Unknown / empty envelope — ignore. Protocol allows extension.
		s.cfg.Log.Warn("ignoring envelope with no known field set")
		return nil
	}
}

func (s *Server) handleSubscribe(ctx context.Context, sub *protocol.Subscribe) error {
	s.mu.Lock()
	if sub.ResubscribeID <= s.lastRSID && s.lastRSID != 0 {
		s.mu.Unlock()
		return nil // race-safe: drop stale Subscribe
	}
	s.lastRSID = sub.ResubscribeID
	s.mu.Unlock()

	s.filter.SetAllowDeny(sub.Deny, sub.Allow, sub.ExcludeEphemeral)

	s.mu.Lock()
	s.hasClient = true
	s.mu.Unlock()

	if err := s.enc.Write(&protocol.Envelope{SubscribeAck: &protocol.SubscribeAck{
		ResubscribeID: sub.ResubscribeID,
	}}); err != nil {
		return err
	}
	return s.emitSnapshot(ctx)
}

func (s *Server) handleReqSnap(ctx context.Context) error {
	return s.emitSnapshot(ctx)
}

func (s *Server) emitSnapshot(ctx context.Context) error {
	raw, err := s.cfg.Watcher.SnapshotNow(ctx)
	if err != nil {
		return s.fatal(protocol.CodeWatcherFailed, err.Error())
	}
	filtered := s.filter.Apply(raw)
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.lastEmitted = make(map[uint16]protocol.Port, len(filtered))
	ports := make([]protocol.Port, 0, len(filtered))
	for _, l := range filtered {
		p := toWirePort(l)
		s.lastEmitted[p.Port] = p
		ports = append(ports, p)
	}
	s.mu.Unlock()
	return s.enc.Write(&protocol.Envelope{Snapshot: &protocol.Snapshot{
		Seq: seq, GeneratedAt: s.cfg.Now().UnixNano(), Ports: ports,
	}})
}

func (s *Server) handleEvent(ev watcher.Event) {
	if !s.filter.Accept(ev.Listen) {
		return
	}
	switch ev.Kind {
	case watcher.KindAdd:
		s.mu.Lock()
		// A re-bind of the same port (e.g. a server restart) produces
		// Add(new-inode) THEN Remove(old-inode) from the watcher. The
		// dedup key MUST include the inode — keying by port alone would
		// drop the new Add (lastEmitted[port] still points at the old
		// entry), then process the subsequent Remove and report the port
		// as gone even though the kernel is still listening.
		if existing, dup := s.lastEmitted[ev.Listen.Port]; dup && existing.InodeNS == ev.Listen.InodeNS {
			s.mu.Unlock()
			return
		}
		s.seq++
		seq := s.seq
		p := toWirePort(ev.Listen)
		s.lastEmitted[p.Port] = p
		s.mu.Unlock()
		_ = s.enc.Write(&protocol.Envelope{PortAdded: &protocol.PortAdded{
			Seq: seq, Port: p, At: ev.At.UnixNano(),
		}})

	case watcher.KindRemove:
		s.mu.Lock()
		existing, ok := s.lastEmitted[ev.Listen.Port]
		if !ok {
			s.mu.Unlock()
			return
		}
		// Ignore a Remove whose inode no longer matches what we last
		// reported as live: the new-inode Add already replaced the entry,
		// so this Remove refers to a generation we never advertised.
		if ev.Listen.InodeNS != 0 && existing.InodeNS != 0 && existing.InodeNS != ev.Listen.InodeNS {
			s.mu.Unlock()
			return
		}
		s.seq++
		seq := s.seq
		delete(s.lastEmitted, ev.Listen.Port)
		s.mu.Unlock()
		_ = s.enc.Write(&protocol.Envelope{PortRemoved: &protocol.PortRemoved{
			Seq: seq, Port: ev.Listen.Port, Family: ev.Listen.Family,
			At: ev.At.UnixNano(), Source: ev.Source,
		}})
	}
}

// serveCmdSock listens on CmdSockPath for connections from `portald open`.
// Each connection sends one URL line and reads back "ok" or "no-client".
// The socket is removed on context cancellation so stale socks don't block
// the next session startup.
func (s *Server) serveCmdSock(ctx context.Context) {
	path := s.cfg.CmdSockPath
	_ = os.Remove(path) // clean up any previous session's socket
	l, err := net.Listen("unix", path)
	if err != nil {
		s.cfg.Log.Warn("cmd socket listen failed", "err", err)
		return
	}
	// Restrict to owner-only so other users on the dev box cannot inject
	// URLs. Defense-in-depth alongside the parent dir being 0700.
	if err := os.Chmod(path, 0600); err != nil {
		s.cfg.Log.Warn("cmd socket chmod failed", "err", err)
	}
	defer func() {
		l.Close()
		_ = os.Remove(path)
	}()

	// Close the listener when ctx cancels so Accept unblocks.
	// Use a stopCh so this goroutine exits when serveCmdSock returns early
	// for any reason — not just ctx cancellation.
	stopCh := make(chan struct{})
	defer close(stopCh)
	go func() {
		select {
		case <-ctx.Done():
		case <-stopCh:
		}
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			return // ctx cancelled or listener closed
		}
		go s.handleCmdConn(ctx, conn)
	}
}

// handleCmdConn dispatches a single tab-framed verb request on the cmd socket.
// The grammar is default-deny: anything that isn't an exact, recognized verb
// shape replies "rejected\n". Image/text bytes NEVER traverse this socket
// inbound — only tiny control lines — so a single bounded read is sufficient.
//
//	open\t<url>\n        → relay URL to the Mac (openurl service, 5s deadline)
//	clip\ttargets\n      → "ok\timage/png\n" | "none\n"
//	clip\timage\tpng\n   → "ok\t<sha>\n" | "none\n"
//	clip\ttext\n         → "ok\t<sha>\n" | "none\n"
//	notify\t<json>\n     → relay a notification to the Mac; "ok\n"|"no-client\n"|"dropped\n"
//	<anything else>      → "rejected\n"
func (s *Server) handleCmdConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// Tight default deadline for the read + the open path. Clip extends it
	// below (see clipSockDeadline) because the round trip to the Mac is slower
	// than a local URL hand-off. routeVerb re-applies each claimed verb's live
	// per-verb deadline before dispatching.
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		return
	}
	line := strings.TrimRight(string(buf[:n]), "\r\n")
	if line == "" {
		return
	}
	verb, rest, _ := strings.Cut(line, "\t")
	// Claimed verbs route to their registered service first ("open" → openurl,
	// "notify" → notify, each applying its own 5s deadline). clip is still
	// served by the legacy handler this unit (dual-stack until u5); an unknown
	// verb default-denies.
	if s.reg.routeVerb(ctx, conn, verb, rest) {
		return
	}
	switch verb {
	case "clip":
		s.handleClipReq(ctx, conn, rest)
	default:
		// Default-deny: a truly unknown token lands here.
		_, _ = conn.Write([]byte("rejected\n"))
	}
}

// handleClipReq services a `clip <kind> [fmt]` verb. It relays a ClipRequest up
// the pipe (via the Serve loop) and waits for the correlated ClipResponse,
// answering the socket with the byte-exact replies portald clip parses. It
// answers "none\n" — never an error — on every adverse path (no client,
// inflight cap hit, channel full, timeout, ctx cancel) so the shim falls
// through cleanly to the real binary. The image/text bytes themselves cross
// out-of-band (clipupload); this socket only carries the SHA.
func (s *Server) handleClipReq(ctx context.Context, conn net.Conn, rest string) {
	// Parse the kind/format off the tab-framed remainder. Reject unknown shapes
	// to preserve default-deny.
	var kind, format string
	switch rest {
	case "targets":
		kind = "targets"
	case "text":
		kind = "text"
	case "image\tpng":
		kind, format = "image", "png"
	default:
		_, _ = conn.Write([]byte("rejected\n"))
		return
	}

	// Clip's round trip to the Mac is slower than a local URL hand-off; widen
	// the deadline for this path only. It stays < the shim's dial+read deadline
	// and > clipTimeout so the agent's own "none\n" always lands first.
	conn.SetDeadline(time.Now().Add(clipSockDeadline))

	// A disconnected/mid-reconnect client must not make the shim eat the full
	// timeout — answer "none\n" immediately.
	s.mu.Lock()
	if !s.hasClient {
		s.mu.Unlock()
		_, _ = conn.Write([]byte("none\n"))
		return
	}
	// Bound concurrent waiters (DoS guard, DESIGN §7.1).
	if len(s.clipWaiters) >= maxInflightClip {
		s.mu.Unlock()
		_, _ = conn.Write([]byte("none\n"))
		return
	}
	nonce := atomic.AddUint64(&s.clipSeq, 1)
	ch := make(chan *protocol.ClipResponse, 1)
	s.clipWaiters[nonce] = ch
	s.mu.Unlock()

	// Always tear the waiter down on exit so a late/duplicate ClipResponse is
	// dropped (handleClipResponse no-ops on a missing nonce).
	defer func() {
		s.mu.Lock()
		delete(s.clipWaiters, nonce)
		s.mu.Unlock()
	}()

	// Hand the ClipRequest to the Serve loop (the sole frame writer). A full
	// channel means the writer is badly backed up — degrade to "none\n".
	req := &protocol.ClipRequest{Nonce: nonce, Epoch: s.epoch, Kind: kind, Format: format}
	select {
	case s.clipReqCh <- req:
	default:
		_, _ = conn.Write([]byte("none\n"))
		return
	case <-ctx.Done():
		_, _ = conn.Write([]byte("none\n"))
		return
	}

	select {
	case resp := <-ch:
		s.writeClipReply(conn, kind, resp)
	case <-time.After(clipTimeout):
		_, _ = conn.Write([]byte("none\n"))
	case <-ctx.Done():
		_, _ = conn.Write([]byte("none\n"))
	}
}

// writeClipReply maps a ClipResponse to the byte-exact socket reply portald
// clip expects. Anything short of an affirmative answer is "none\n".
func (s *Server) writeClipReply(conn net.Conn, kind string, resp *protocol.ClipResponse) {
	if resp == nil || !resp.OK {
		_, _ = conn.Write([]byte("none\n"))
		return
	}
	switch kind {
	case "targets":
		if resp.Has {
			// Advertise the CANONICAL kind the Mac decided ("image" or "text").
			// portald clip targets maps this to the tool-specific target line(s)
			// its caller (xclip vs wl-paste) greps for — the agent stays
			// tool-agnostic. Default to image if the Mac left Kind empty (an
			// older Mac that only ever reported image availability).
			k := resp.Kind
			if k != "image" && k != "text" {
				k = "image"
			}
			_, _ = conn.Write([]byte("ok\t" + k + "\n"))
		} else {
			_, _ = conn.Write([]byte("none\n"))
		}
	case "image", "text":
		if resp.SHA != "" {
			_, _ = conn.Write([]byte("ok\t" + resp.SHA + "\n"))
		} else {
			_, _ = conn.Write([]byte("none\n"))
		}
	default:
		_, _ = conn.Write([]byte("none\n"))
	}
}

// handleClipResponse delivers a ClipResponse to its waiting handleClipReq
// goroutine. A response whose Epoch does not match this Server's epoch is a
// stale/cross-generation frame (e.g. arriving down a new pipe after reconnect)
// and is dropped. A response whose Nonce has no registered waiter (late or
// duplicate) is also dropped. Delivery is non-blocking — the waiter channel is
// buffered 1, so this never stalls the Serve loop's frame dispatch.
func (s *Server) handleClipResponse(resp *protocol.ClipResponse) {
	if resp.Epoch != s.epoch {
		s.cfg.Log.Warn("dropping clip response with stale epoch",
			"got", resp.Epoch, "want", s.epoch, "nonce", resp.Nonce)
		return
	}
	s.mu.Lock()
	ch, ok := s.clipWaiters[resp.Nonce]
	s.mu.Unlock()
	if !ok {
		return // no waiter (timed out / duplicate) — drop
	}
	select {
	case ch <- resp:
	default:
		// Waiter already satisfied — drop the duplicate.
	}
}

// armBackpressure starts the kill-timer if it isn't already running.
// Called when a watcher event cannot be delivered because the channel is full.
func (s *Server) armBackpressure() {
	if s.cfg.BackpressureKill <= 0 {
		return
	}
	s.bpMu.Lock()
	defer s.bpMu.Unlock()
	if s.bpDeadline == nil {
		bpCh := s.bpKillCh
		s.bpDeadline = time.AfterFunc(s.cfg.BackpressureKill, func() {
			select {
			case <-bpCh: // already closed
			default:
				close(bpCh)
			}
		})
	}
}

// disarmBackpressure cancels the kill-timer on successful delivery.
func (s *Server) disarmBackpressure() {
	s.bpMu.Lock()
	defer s.bpMu.Unlock()
	if s.bpDeadline != nil {
		s.bpDeadline.Stop()
		s.bpDeadline = nil
	}
}

func (s *Server) fatal(code uint16, msg string) error {
	_ = s.enc.Write(&protocol.Envelope{AgentError: &protocol.AgentError{
		Code: code, Fatal: true, Msg: msg,
	}})
	return errors.New("agent: " + msg)
}

func toWirePort(l watcher.Listen) protocol.Port {
	return protocol.Port{
		Port:    l.Port,
		Family:  l.Family,
		Addr:    l.Addr,
		InodeNS: l.InodeNS,
	}
}

// readBootID reads /proc/sys/kernel/random/boot_id (Linux). Empty on error
// or non-Linux.
func readBootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ReadBootID is exported for cmd/portald use.
func ReadBootID() string { return readBootID() }
