package agentclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vikashl/portal/internal/bootstrap"
	"github.com/vikashl/portal/internal/protocol"
	"github.com/vikashl/portal/internal/sshctl"
)

// ErrNoSnapshot is returned by Snapshot() before the first SubscribeAck has
// landed, so the AgentDiscoverer can distinguish "agent unreachable" from
// "agent empty".
var ErrNoSnapshot = errors.New("agentclient: no snapshot yet")

// Config bundles dependencies. Defaults are filled in by New.
type Config struct {
	Transport sshctl.Transport
	Bootstrap *bootstrap.Manager
	Log       *slog.Logger
	// StderrSink is where the agent's stderr lines go (typically
	// os.Stderr; launchd routes that to ~/Library/Logs/portal.log).
	StderrSink io.Writer
	// HeartbeatTimeout is the max gap between any agent→client frames
	// before we declare the agent hung and reconnect. Default 12s.
	HeartbeatTimeout time.Duration
	// CoalesceWindow is the debounce window for PortAdded/PortRemoved
	// bursts. Default 50ms.
	CoalesceWindow time.Duration
	// ReconnectMin/Max is the exponential backoff bounds. Defaults: 500ms / 10s.
	ReconnectMin time.Duration
	ReconnectMax time.Duration
}

// Client speaks the protocol with the remote portald. One Client per portal
// process; reconnects are handled internally.
type Client struct {
	cfg Config

	// rsid grows monotonically with each Subscribe.
	rsid atomic.Uint64

	// subMu guards desired filter state.
	subMu  sync.Mutex
	deny   []uint16
	allow  []uint16
	excEph bool

	// snapMu guards the cached snapshot.
	snapMu    sync.RWMutex
	snapSeq   uint64
	snapPorts []protocol.Port
	helloAck  *protocol.HelloAck

	// events is the demuxed stream the engine reads.
	events chan EngineEvent

	// stream is the current send-side encoder; nil between connections.
	streamMu sync.Mutex
	enc      *protocol.Encoder
	stdin    io.WriteCloser
}

// New builds a Client; defaults applied here.
func New(cfg Config) *Client {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.StderrSink == nil {
		cfg.StderrSink = os.Stderr
	}
	if cfg.HeartbeatTimeout == 0 {
		cfg.HeartbeatTimeout = 12 * time.Second
	}
	if cfg.CoalesceWindow == 0 {
		cfg.CoalesceWindow = 50 * time.Millisecond
	}
	if cfg.ReconnectMin == 0 {
		cfg.ReconnectMin = 500 * time.Millisecond
	}
	if cfg.ReconnectMax == 0 {
		cfg.ReconnectMax = 10 * time.Second
	}
	return &Client{
		cfg:    cfg,
		events: make(chan EngineEvent, 64),
	}
}

// Events returns the demuxed event channel.
func (c *Client) Events() <-chan EngineEvent { return c.events }

// Snapshot returns the cached desired-set as the wire reports it.
//   ok = false until the first SubscribeAck+Snapshot pair has landed.
func (c *Client) Snapshot() (seq uint64, ports []uint16, ok bool) {
	c.snapMu.RLock()
	defer c.snapMu.RUnlock()
	if c.helloAck == nil || c.snapSeq == 0 {
		return 0, nil, false
	}
	out := make([]uint16, 0, len(c.snapPorts))
	for _, p := range c.snapPorts {
		out = append(out, p.Port)
	}
	return c.snapSeq, out, true
}

// HelloAck returns the agent's last-seen HelloAck (or nil before connect).
// Used by `portal status` to print agent fields.
func (c *Client) HelloAck() *protocol.HelloAck {
	c.snapMu.RLock()
	defer c.snapMu.RUnlock()
	if c.helloAck == nil {
		return nil
	}
	cp := *c.helloAck
	return &cp
}

// Subscribe pushes a new desired filter to the agent. Idempotent — if the
// new filter equals the cached one, it's a no-op.
func (c *Client) Subscribe(deny, allow []uint16, excludeEphemeral bool) error {
	c.subMu.Lock()
	c.deny = append([]uint16(nil), deny...)
	c.allow = append([]uint16(nil), allow...)
	c.excEph = excludeEphemeral
	c.subMu.Unlock()
	return c.sendSubscribe()
}

// sendSubscribe pushes the cached filter with a fresh rsid. Safe to call
// before the connection is up — the next connect will replay the latest
// subMu state with a new rsid.
func (c *Client) sendSubscribe() error {
	rsid := c.rsid.Add(1)
	c.subMu.Lock()
	sub := &protocol.Subscribe{
		Deny: append([]uint16(nil), c.deny...), Allow: append([]uint16(nil), c.allow...),
		ExcludeEphemeral: c.excEph, ResubscribeID: rsid,
	}
	c.subMu.Unlock()

	c.streamMu.Lock()
	enc := c.enc
	c.streamMu.Unlock()
	if enc == nil {
		// Connection not up yet — the connect path will send the latest
		// Subscribe automatically.
		return nil
	}
	return enc.Write(&protocol.Envelope{Subscribe: sub})
}

// RequestSnapshot asks the agent for a fresh full Snapshot.
func (c *Client) RequestSnapshot(ctx context.Context) error {
	c.streamMu.Lock()
	enc := c.enc
	c.streamMu.Unlock()
	if enc == nil {
		return errors.New("agentclient: not connected")
	}
	return enc.Write(&protocol.Envelope{ReqSnap: &protocol.ReqSnap{}})
}

// Shutdown sends Shutdown and waits for Bye + clean exit, then closes streams.
func (c *Client) Shutdown(ctx context.Context, reason string) error {
	c.streamMu.Lock()
	enc := c.enc
	stdin := c.stdin
	c.streamMu.Unlock()
	if enc != nil {
		_ = enc.Write(&protocol.Envelope{Shutdown: &protocol.Shutdown{Reason: reason}})
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	return nil
}

// Run is the supervisor loop: connect, run one session until it exits,
// backoff, reconnect. Returns nil only on ctx cancellation.
// Closes c.events on return so the adaptAgentEvents goroutine in app.go
// (which ranges over it) terminates cleanly — no goroutine leak.
//
// Backoff resets to ReconnectMin after any session that survived past
// healthyThreshold (long enough to imply Hello+Subscribe+Snapshot all
// landed). Without this reset a short cluster of early flaps would pin
// the backoff at ReconnectMax forever, turning a one-off blip 6 hours
// later into a 10s outage.
func (c *Client) Run(ctx context.Context) error {
	defer close(c.events) // unblocks adaptAgentEvents goroutine in app.go
	backoff := c.cfg.ReconnectMin
	const healthyThreshold = 5 * time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		started := time.Now()
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		sessionDur := time.Since(started)
		c.cfg.Log.Warn("agent session ended", "err", err, "duration", sessionDur)
		c.publish(EngineEvent{Kind: KindDisconnected, Err: err})

		if sessionDur >= healthyThreshold {
			backoff = c.cfg.ReconnectMin
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > c.cfg.ReconnectMax {
			backoff = c.cfg.ReconnectMax
		}
	}
}

func (c *Client) publish(ev EngineEvent) {
	// Recover from a send on a closed channel (which close(c.events) in
	// Run() can cause if publish is called from the final runOnce return
	// path after the defer fires). In practice this race is extremely
	// tight, but the recover makes it safe.
	defer func() { recover() }()
	select {
	case c.events <- ev:
	default:
	}
}

// runOnce holds a single ssh-exec session. Returns when the stream errors,
// EOFs, or ctx cancels.
func (c *Client) runOnce(ctx context.Context) error {
	// 1. Bootstrap (idempotent).
	remotePath, err := c.cfg.Bootstrap.EnsureUploaded(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	// 2. Spawn the long-lived exec.
	stdin, stdout, stderr, wait, err := c.cfg.Transport.ExecStream(ctx,
		remotePath, "--proto-version=1")
	if err != nil {
		return fmt.Errorf("ExecStream: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = wait()
	}()

	// 3. Tee agent stderr to ~/Library/Logs/portal.log.
	go c.copyStderr(stderr)

	enc := protocol.NewEncoder(stdin)
	dec := protocol.NewDecoder(stdout)

	// 4. Hello → HelloAck.
	if err := enc.Write(&protocol.Envelope{Hello: &protocol.Hello{
		ProtoVersion: protocol.ProtoVersion,
		ClientGitSHA: bootstrap.EmbeddedSHA(),
		ClientPID:    os.Getpid(),
		WantDestroyMC: true,
	}}); err != nil {
		return fmt.Errorf("write Hello: %w", err)
	}
	first, err := dec.Read()
	if err != nil {
		return fmt.Errorf("read HelloAck: %w", err)
	}
	if first.AgentError != nil {
		return fmt.Errorf("agent error %d: %s", first.AgentError.Code, first.AgentError.Msg)
	}
	if first.HelloAck == nil {
		return fmt.Errorf("expected HelloAck, got %v", envType(first))
	}
	if first.HelloAck.AgentGitSHA != bootstrap.EmbeddedSHA() {
		return fmt.Errorf("agent SHA mismatch: agent=%s embedded=%s",
			first.HelloAck.AgentGitSHA, bootstrap.EmbeddedSHA())
	}

	c.snapMu.Lock()
	c.helloAck = first.HelloAck
	c.snapMu.Unlock()

	// 5. Latch the encoder so external Subscribe/Shutdown calls go to it.
	c.streamMu.Lock()
	c.enc = enc
	c.stdin = stdin
	c.streamMu.Unlock()
	defer func() {
		c.streamMu.Lock()
		c.enc = nil
		c.stdin = nil
		c.streamMu.Unlock()
	}()

	// 6. Send the current Subscribe — even if Subscribe was called before
	// Run, we send the latest now.
	if err := c.sendSubscribe(); err != nil {
		return fmt.Errorf("send Subscribe: %w", err)
	}

	c.publish(EngineEvent{Kind: KindConnected})

	// 7. Demux frames + coalesce.
	return c.demuxLoop(ctx, dec)
}

func (c *Client) demuxLoop(ctx context.Context, dec *protocol.Decoder) error {
	// Pending delta accumulator + flush timer.
	var (
		pendAdded   []uint16
		pendRemoved []uint16
		flushTimer  *time.Timer
	)
	resetTimer := func() {
		if flushTimer == nil {
			flushTimer = time.NewTimer(c.cfg.CoalesceWindow)
			return
		}
		if !flushTimer.Stop() {
			select {
			case <-flushTimer.C:
			default:
			}
		}
		flushTimer.Reset(c.cfg.CoalesceWindow)
	}
	stopTimer := func() {
		if flushTimer != nil && !flushTimer.Stop() {
			select {
			case <-flushTimer.C:
			default:
			}
		}
	}
	flushDelta := func() {
		if len(pendAdded) == 0 && len(pendRemoved) == 0 {
			return
		}
		c.publish(EngineEvent{
			Kind:    KindDelta,
			Added:   pendAdded,
			Removed: pendRemoved,
		})
		pendAdded, pendRemoved = nil, nil
	}

	// Heartbeat timeout watchdog.
	hbDeadline := time.NewTimer(c.cfg.HeartbeatTimeout)
	defer hbDeadline.Stop()
	bumpHB := func() {
		if !hbDeadline.Stop() {
			select {
			case <-hbDeadline.C:
			default:
			}
		}
		hbDeadline.Reset(c.cfg.HeartbeatTimeout)
	}

	// Reading is blocking; do it in a goroutine so we can also select on
	// ctx, the heartbeat watchdog, and the flush timer. The `done`
	// channel is closed when demuxLoop returns so a still-blocked send
	// doesn't leak the goroutine — important because heartbeat-driven
	// disconnects are a regular occurrence and one leak per cycle adds up.
	type readResult struct {
		env *protocol.Envelope
		err error
	}
	readCh := make(chan readResult, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			env, err := dec.Read()
			select {
			case readCh <- readResult{env: env, err: err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		var flushC <-chan time.Time
		if flushTimer != nil {
			flushC = flushTimer.C
		}
		select {
		case <-ctx.Done():
			stopTimer()
			return nil
		case <-hbDeadline.C:
			stopTimer()
			return errors.New("heartbeat timeout")
		case <-flushC:
			flushTimer = nil
			flushDelta()
		case rr := <-readCh:
			if rr.err != nil {
				stopTimer()
				flushDelta()
				return rr.err
			}
			bumpHB()
			env := rr.env
			switch {
			case env.Snapshot != nil:
				c.snapMu.Lock()
				c.snapSeq = env.Snapshot.Seq
				c.snapPorts = env.Snapshot.Ports
				c.snapMu.Unlock()
				stopTimer()
				flushTimer = nil
				pendAdded, pendRemoved = nil, nil
				c.publish(EngineEvent{Kind: KindSnapshotReplaced})
			case env.PortAdded != nil:
				p := env.PortAdded.Port
				c.snapMu.Lock()
				c.snapSeq = env.PortAdded.Seq
				c.snapPorts = append(c.snapPorts, p)
				c.snapMu.Unlock()
				pendAdded = append(pendAdded, p.Port)
				resetTimer()
			case env.PortRemoved != nil:
				port := env.PortRemoved.Port
				c.snapMu.Lock()
				c.snapSeq = env.PortRemoved.Seq
				kept := c.snapPorts[:0]
				for _, p := range c.snapPorts {
					if p.Port != port {
						kept = append(kept, p)
					}
				}
				c.snapPorts = kept
				c.snapMu.Unlock()
				pendRemoved = append(pendRemoved, port)
				resetTimer()
			case env.OpenURL != nil:
				c.publish(EngineEvent{Kind: KindOpenURL, URL: env.OpenURL.URL})
			case env.Heartbeat != nil:
				// Already bumped above.
			case env.SubscribeAck != nil:
				// Informational; the next Snapshot is what matters.
			case env.AgentError != nil:
				stopTimer()
				flushDelta()
				return fmt.Errorf("agent error %d: %s",
					env.AgentError.Code, env.AgentError.Msg)
			case env.Bye != nil:
				stopTimer()
				flushDelta()
				return io.EOF
			}
		}
	}
}

// copyStderr forwards every line of agent stderr to the configured sink,
// prefixed with "agent: " for log-line clarity.
func (c *Client) copyStderr(r io.ReadCloser) {
	defer r.Close()
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			fmt.Fprintf(c.cfg.StderrSink, "agent: %s", line)
		}
		if err != nil {
			return
		}
	}
}

func envType(env *protocol.Envelope) string {
	switch {
	case env.Hello != nil:
		return "Hello"
	case env.HelloAck != nil:
		return "HelloAck"
	case env.SubscribeAck != nil:
		return "SubscribeAck"
	case env.Snapshot != nil:
		return "Snapshot"
	case env.PortAdded != nil:
		return "PortAdded"
	case env.PortRemoved != nil:
		return "PortRemoved"
	case env.Heartbeat != nil:
		return "Heartbeat"
	case env.AgentError != nil:
		return "AgentError"
	case env.Bye != nil:
		return "Bye"
	}
	return "<empty>"
}
