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

	"gitlab.i.extrahop.com/vikashl/devportal/internal/bootstrap"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/clipshim"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/hub"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/protocol"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/sshctl"
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
	// Hub is an OPTIONAL read-only fan-out tee for local API observers. nil
	// (the default) means no tee. When set, publish/publishNotify tee an
	// EXPLICIT kind→hub.Event enumeration into it — never a pass-through. The
	// hub can never become a second event-ordering authority: the engine, clip
	// handler, and notify handler keep their dedicated channels (DESIGN §3/§10).
	Hub *hub.Hub
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

	// clipEvents is a DEDICATED channel for KindClipRequest, separate from the
	// shared (drop-on-full) events channel so a burst of port events can never
	// evict a pending paste (DESIGN §5). runClipHandler on the Mac drains it.
	// Buffered modestly: maxInflightClip on the agent already bounds how many
	// requests can be outstanding, and runClipHandler's worker semaphore is 1.
	clipEvents chan EngineEvent

	// notifyEvents is a DEDICATED channel for KindNotify, separate from the
	// shared events channel for the same reason as clipEvents: a port-event
	// burst must not evict a pending notification. runNotifyHandler drains it.
	// Never closed by Run (the handler exits on ctx, not channel close).
	notifyEvents chan EngineEvent

	// stream is the current send-side encoder; nil between connections.
	streamMu sync.Mutex
	enc      *protocol.Encoder
	stdin    io.WriteCloser

	// lastDiscErr holds the string form of the most recent KindDisconnected
	// error ("" when the disconnect carried no error). It is the only
	// Status.Health source that needs a client-side accessor; the hub tee
	// carries no error payload. Always holds a string once set.
	lastDiscErr atomic.Value
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
		cfg:          cfg,
		events:       make(chan EngineEvent, 64),
		clipEvents:   make(chan EngineEvent, 8),
		notifyEvents: make(chan EngineEvent, 16),
	}
}

// Events returns the demuxed event channel.
func (c *Client) Events() <-chan EngineEvent { return c.events }

// ClipEvents returns the dedicated KindClipRequest channel. runClipHandler in
// cmd/portal/run.go drains it on its own goroutine (DESIGN §5). It is NEVER
// closed by Run (only Events() is) — the handler exits on ctx cancellation,
// not on channel close — so a late ClipRequest racing shutdown cannot panic on
// a send to a closed channel.
func (c *Client) ClipEvents() <-chan EngineEvent { return c.clipEvents }

// NotifyEvents returns the dedicated KindNotify channel. runNotifyHandler in
// cmd/portal/run.go drains it on its own goroutine. Like ClipEvents() it is
// NEVER closed by Run — the handler exits on ctx cancellation, not channel
// close — so a late Notify racing shutdown cannot panic on a send to a closed
// channel.
func (c *Client) NotifyEvents() <-chan EngineEvent { return c.notifyEvents }

// SendClipResponse writes a ClipResponse up the current pipe, correlating the
// agent's outstanding waiter by (Nonce,Epoch). It is the Mac-side answer to a
// KindClipRequest. Returns an error if no connection is up — the caller treats
// that as "the paste is abandoned; the agent will time out and answer none".
func (c *Client) SendClipResponse(resp *protocol.ClipResponse) error {
	c.streamMu.Lock()
	enc := c.enc
	c.streamMu.Unlock()
	if enc == nil {
		return errors.New("agentclient: not connected")
	}
	return enc.Write(&protocol.Envelope{ClipResponse: resp})
}

// Snapshot returns the cached desired-set as the wire reports it.
//
//	ok = false until the first SubscribeAck+Snapshot pair has landed.
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
	// Track the last disconnect error for Status.Health regardless of the hub.
	if ev.Kind == KindDisconnected {
		if ev.Err != nil {
			c.lastDiscErr.Store(ev.Err.Error())
		} else {
			c.lastDiscErr.Store("")
		}
	}
	// Tee state-change SIGNALs into the hub as an EXPLICIT enumeration — never
	// a pass-through. KindOpenURL is deliberately NOT mapped (the URL relay
	// stays daemon-internal in v1); clip is not representable in hub.Event.
	if c.cfg.Hub != nil {
		switch ev.Kind {
		case KindConnected, KindDisconnected, KindSnapshotReplaced, KindDelta:
			c.cfg.Hub.Publish(hub.Event{Class: hub.Coalesced})
		}
	}
}

// LastDisconnectErr returns the string form of the most recent
// KindDisconnected error, or "" when unset or the last disconnect carried no
// error. Feeds Status.Health.
func (c *Client) LastDisconnectErr() string {
	if s, ok := c.lastDiscErr.Load().(string); ok {
		return s
	}
	return ""
}

// publishClip sends to the dedicated clip channel. Non-blocking (so the demux
// loop never stalls) but to its OWN channel so a port-event burst on c.events
// can't drop the paste. clipEvents is never closed, so no recover is needed.
func (c *Client) publishClip(ev EngineEvent) {
	select {
	case c.clipEvents <- ev:
	default:
	}
}

// publishNotify sends to the dedicated notify channel. Non-blocking (so the
// demux loop never stalls) and to its OWN channel so a port-event burst on
// c.events can't drop the notification. notifyEvents is never closed, so no
// recover is needed.
func (c *Client) publishNotify(ev EngineEvent) {
	select {
	case c.notifyEvents <- ev:
	default:
	}
	// Tee the notification into the hub's Queued class. Explicit field copy —
	// hub.Notify is duplicated from NotifyEvent so the hub imports nothing from
	// agentclient. Hub.Publish is non-blocking by contract.
	if c.cfg.Hub != nil && ev.Notify != nil {
		c.cfg.Hub.Publish(hub.Event{Class: hub.Queued, Notify: &hub.Notify{
			Title:    ev.Notify.Title,
			Body:     ev.Notify.Body,
			Subtitle: ev.Notify.Subtitle,
			Urgency:  ev.Notify.Urgency,
			Verified: ev.Notify.Verified,
			Source:   ev.Notify.Source,
			Sound:    ev.Notify.Sound,
			Seq:      ev.Notify.Seq,
		}})
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
		remotePath, fmt.Sprintf("--proto-version=%d", protocol.ProtoVersion))
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
		ProtoVersion:  protocol.ProtoVersion,
		ClientGitSHA:  bootstrap.EmbeddedSHA(),
		ClientPID:     os.Getpid(),
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
		// SHA mismatch: the running agent is stale (e.g. upload was
		// interrupted or the binary at that path is corrupted). Force-delete
		// the remote file so EnsureUploaded re-uploads on the next connect.
		c.cfg.Log.Error("agent SHA mismatch — forcing re-upload",
			"agent", first.HelloAck.AgentGitSHA, "embedded", bootstrap.EmbeddedSHA())
		rmPath := fmt.Sprintf("~/.cache/portal/agent-%s", first.HelloAck.AgentGitSHA)
		_, _ = c.cfg.Transport.Exec(context.Background(), "", "bash", "-c",
			"rm -f "+rmPath)
		return fmt.Errorf("agent SHA mismatch: agent=%s embedded=%s",
			first.HelloAck.AgentGitSHA, bootstrap.EmbeddedSHA())
	}

	c.snapMu.Lock()
	c.helloAck = first.HelloAck
	c.snapMu.Unlock()

	// 4b. Deploy/refresh the clipboard read shims now that the agent upload
	// succeeded AND the HelloAck SHA matches (so the portald symlink points at
	// THIS agent). This is what makes shim deploy DAEMON-DRIVEN (DESIGN §9.1):
	// a Mac-binary upgrade that changes the embedded shim text re-converges on
	// reconnect without a manual `portal install`. clipshim.Ensure is a cheap
	// grep in the steady state (the content marker already matches). Failure is
	// non-fatal to the session — port forwarding must not be held hostage to a
	// shim write — but it is logged loudly so the headline feature's breakage
	// is visible (DESIGN §9.6).
	if err := clipshim.Ensure(ctx, c.cfg.Transport); err != nil {
		c.cfg.Log.Error("clip shim deploy failed — clipboard paste will not work until this succeeds", "err", err)
	}

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
				c.snapMu.Lock()
				// Drop stale events from a previous agent process session.
				// After a Snapshot at seq S0, all valid events have Seq > S0.
				if env.PortAdded.Seq <= c.snapSeq {
					c.snapMu.Unlock()
					continue
				}
				p := env.PortAdded.Port
				c.snapSeq = env.PortAdded.Seq
				c.snapPorts = append(c.snapPorts, p)
				c.snapMu.Unlock()
				pendAdded = append(pendAdded, p.Port)
				resetTimer()
			case env.PortRemoved != nil:
				c.snapMu.Lock()
				if env.PortRemoved.Seq <= c.snapSeq {
					c.snapMu.Unlock()
					continue
				}
				port := env.PortRemoved.Port
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
			case env.ClipRequest != nil:
				// Route to the DEDICATED clip channel, not the shared events
				// channel — a port-event burst must not evict a pending paste
				// (DESIGN §5). Non-blocking so the demux loop (which also runs
				// the heartbeat watchdog) never stalls behind a slow handler;
				// the channel is cap-8 > maxInflightClip(4) so a drop here means
				// the handler is genuinely wedged, in which case the agent's
				// clipTimeout answers "none" anyway.
				cr := env.ClipRequest
				c.publishClip(EngineEvent{Kind: KindClipRequest, Clip: &ClipEvent{
					Nonce: cr.Nonce, Epoch: cr.Epoch, Kind: cr.Kind, Format: cr.Format,
				}})
			case env.Notify != nil:
				// Route to the DEDICATED notify channel (same rationale as
				// ClipRequest). Fire-and-forget: no response frame. Non-blocking
				// so a slow notification handler never stalls the demux loop /
				// heartbeat watchdog.
				nf := env.Notify
				c.publishNotify(EngineEvent{Kind: KindNotify, Notify: &NotifyEvent{
					Title: nf.Title, Body: nf.Body, Subtitle: nf.Subtitle,
					Urgency: nf.Urgency, Verified: nf.Verified, Source: nf.Source,
					Sound: nf.Sound, Seq: nf.Seq,
				}})
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
