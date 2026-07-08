package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/VikashLoomba/Portal/internal/agent/watcher"
	"github.com/VikashLoomba/Portal/pkg/protocol"
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

// Server is the agent's RPC top loop. One Server per ssh-exec lifetime.
type Server struct {
	cfg    Config
	filter *Filter
	enc    *protocol.Encoder
	dec    *protocol.Decoder

	// reg is the service registry. It auto-registers the compiled-in openurl,
	// notify, and clip services in New and owns the per-service outbox the Serve
	// loop drains, the inbound Msg dispatch, and the generalized clip Call/epoch/
	// nonce correlation machinery. It never holds an *Encoder — the Serve loop
	// stays the sole agent→client writer.
	reg *registry
	// clip is the registered clip service. It is retained ONLY as a construction
	// handle and a white-box test accessor (the timeout-budget test shortens its
	// fields) — it is NOT part of the request path (cmd-socket clip verbs route
	// through reg.routeVerb, inbound clip responses through reg.dispatch). It is
	// the SAME instance registered in reg, so a field shortened here is the one
	// routeVerb/reg.call read live.
	clip *clipService

	mu          sync.Mutex
	seq         uint64
	lastRSID    uint64
	lastEmitted map[uint16]protocol.Port
	startedAt   time.Time
	hasClient   bool // true once SubscribeAck has been sent; gates cmd socket

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
		bpKillCh:    make(chan struct{}),
	}
	// Build the registry and bind the Server's guarded subscription reader so
	// services can gate on `hasClient() && clientHas(svc)` without ever touching
	// s.mu directly. Then auto-register all compiled-in services (openurl,
	// notify, clip). The registry owns the clip epoch/nonce/waiter correlation
	// machinery (newRegistry seeds the epoch via newEpoch); server.go no longer
	// carries any clip-specific state.
	s.reg = newRegistry(cfg.Log)
	s.reg.bindHasClient(func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.hasClient })
	s.reg.register(newOpenURLService(s.reg, cfg.Log))
	s.reg.register(newNotifyService(s.reg, cfg.Log))
	s.clip = newClipService(s.reg, cfg.Log)
	s.reg.register(s.clip)
	return s
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
	case env.Msg != nil:
		// Inbound client→agent service frame (DESIGN §4): the clip "resp" flows
		// here → reg.dispatch → clipService.HandleMsg → reg.completeCall. Dispatch
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
	// Claimed verbs route to their registered service ("open" → openurl,
	// "notify" → notify, "clip" → clip, each applying its own live per-verb
	// deadline). An unknown verb is not claimed, so routeVerb returns false and
	// we default-deny.
	if s.reg.routeVerb(ctx, conn, verb, rest) {
		return
	}
	// Default-deny: a truly unknown token lands here.
	_, _ = conn.Write([]byte("rejected\n"))
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
