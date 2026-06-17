package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/vikashl/portal/internal/agent/watcher"
	"github.com/vikashl/portal/internal/protocol"
)

// Config bundles the constructor inputs. Everything is injected so the
// server is fully testable with FakeWatcher + bytes.Buffer pipes.
type Config struct {
	In           io.Reader     // stdin (Mac → agent)
	Out          io.Writer     // stdout (agent → Mac)
	Watcher      watcher.Watcher
	AgentSHA     string        // baked at build time via -ldflags
	Kernel       string        // typically "uname -r"; OK to leave empty
	BootID       string        // /proc/sys/kernel/random/boot_id; OK to leave empty
	EphemMin     uint16
	EphemMax     uint16
	HeartbeatInterval time.Duration // default 5s
	Log          *slog.Logger  // stderr handler; nil → discard
	Now          func() time.Time
}

// Server is the agent's RPC top loop. One Server per ssh-exec lifetime.
type Server struct {
	cfg    Config
	filter *Filter
	enc    *protocol.Encoder
	dec    *protocol.Decoder

	mu          sync.Mutex
	seq         uint64
	lastRSID    uint64
	lastEmitted map[uint16]protocol.Port
	startedAt   time.Time
}

// New constructs a Server. Defaults are filled in.
func New(cfg Config) *Server {
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 5 * time.Second
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
	return &Server{
		cfg:         cfg,
		filter:      NewFilter(cfg.EphemMin, cfg.EphemMax),
		enc:         protocol.NewEncoder(cfg.Out),
		dec:         protocol.NewDecoder(cfg.In),
		lastEmitted: map[uint16]protocol.Port{},
	}
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

	// 2. HelloAck.
	if err := s.enc.Write(&protocol.Envelope{HelloAck: &protocol.HelloAck{
		ProtoVersion: protocol.ProtoVersion,
		AgentGitSHA:  s.cfg.AgentSHA,
		AgentPID:     os.Getpid(),
		Kernel:       s.cfg.Kernel,
		BootID:       s.cfg.BootID,
		EphemMin:     s.cfg.EphemMin,
		EphemMax:     s.cfg.EphemMax,
		NowUnixNano:  s.cfg.Now().UnixNano(),
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

	hb := time.NewTicker(s.cfg.HeartbeatInterval)
	defer hb.Stop()

	for {
		select {
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
			Now: s.cfg.Now().UnixNano(),
		}})
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
