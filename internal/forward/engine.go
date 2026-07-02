// Package forward is the reconcile engine. It is intentionally stateless:
// the forwarded-ports set is derived each pass from the live ssh master's
// LISTEN sockets via the PortLister, never cached in-process. A daemon
// restart, a master rebuild, an `unallow` from another invocation — all
// self-correct because the very next pass observes ground truth.
package forward

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/clock"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/discover"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/proc"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/sshctl"
)

// Engine wires the dependencies the reconcile loop needs. Build with New().
type Engine struct {
	T         sshctl.Transport
	PL        proc.PortLister
	RD        discover.RemoteDiscoverer
	Cfg       *config.Store
	Clk       clock.Clock
	Log       Logger
	Interval  time.Duration
	Deny      []int
	SkipLocal []int

	// AgentEvents is the event-driven trigger source: every Connected /
	// SnapshotReplaced / Delta nudges Reconcile (with a small debounce).
	// Disconnected logs and KEEPS existing forwards. Nil means "no agent
	// wired in yet" — the engine falls back to plain interval polling.
	AgentEvents <-chan EngineEvent

	// OpenURLSink receives URLs from EvOpenURL events. Wired by the
	// composition root to the goroutine in cmd/portal/run.go that calls
	// `open <url>` on macOS. Non-blocking send — drops if the consumer
	// is slow (best-effort for a URL open is fine).
	OpenURLSink chan<- string

	// SafetyInterval fires Reconcile periodically as a backstop in case
	// the master-side forwards drift (someone runs ssh -O cancel out of
	// band, the master is rebuilt, etc.). Default: 60s when AgentEvents is
	// non-nil, Interval when nil.
	SafetyInterval time.Duration

	// kick is a buffered (cap 1) coalescing trigger for an out-of-band
	// reconcile — POST /v1/reconcile's clean entry point. Kick() does a
	// non-blocking send; runEventDriven selects on it and fires the same
	// debounce path as EvConnected/EvDelta. Created by New().
	kick chan struct{}

	// reconciles is a monotonic count of completed Reconcile passes (every
	// return of Reconcile bumps it, success or failure). It is the ONLY signal
	// a client can use to know an out-of-band Kick has actually run to
	// completion: Kick/POST /v1/reconcile is async+debounced, so Master.Up (the
	// master is already owned by the daemon) tells a caller nothing about
	// convergence. `once` reads Reconciles() before its Kick and polls until it
	// advances (see cmd/portal/run.go).
	reconciles atomic.Uint64

	conflicts *conflictSet
}

// EngineEvent is the local mirror of agentclient.EngineEvent — declared
// here so internal/forward doesn't need to import internal/agentclient
// (which would induce a layering cycle). The composition root adapts.
// OpenURL events are passed through but the engine itself ignores them
// (they are consumed by cmd/portal/run.go's open-url handler goroutine).
type EngineEvent struct {
	Kind    EngineEventKind
	Err     error
	Added   []uint16
	Removed []uint16
	URL     string // populated on EvOpenURL
}

// EngineEventKind mirrors agentclient.EngineEventKind.
type EngineEventKind uint8

const (
	EvConnected EngineEventKind = 1 + iota
	EvDisconnected
	EvSnapshotReplaced
	EvDelta
	EvOpenURL
)

// New constructs an Engine with a fresh in-memory conflict set.
func New(t sshctl.Transport, pl proc.PortLister, rd discover.RemoteDiscoverer,
	cfg *config.Store, clk clock.Clock, log Logger,
	interval time.Duration, deny, skipLocal []int) *Engine {
	return &Engine{
		T: t, PL: pl, RD: rd, Cfg: cfg, Clk: clk, Log: log,
		Interval: interval, Deny: deny, SkipLocal: skipLocal,
		kick:      make(chan struct{}, 1),
		conflicts: newConflictSet(),
	}
}

// Kick requests an out-of-band reconcile. The send is non-blocking and
// coalescing (cap-1 buffer): concurrent kicks collapse into at most one
// pending trigger, and Kick never blocks its caller (POST /v1/reconcile).
// Nil-safe: an Engine built without New() has no kick channel, so this is a
// no-op there — New() always sets it.
func (e *Engine) Kick() {
	if e.kick == nil {
		return
	}
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Reconciles reports the number of Reconcile passes that have completed. It is
// monotonic and safe to call from any goroutine. A caller that Kick()s an
// out-of-band reconcile can read this first, then wait for it to advance past
// that baseline to know a full pass has run since (§5 `once` convergence).
func (e *Engine) Reconciles() uint64 { return e.reconciles.Load() }

// Run is event-driven when AgentEvents is non-nil: it reconciles whenever
// an agent event lands (debounced 50ms to coalesce bursts) and on a 60s
// safety ticker that catches master-side drift. When AgentEvents is nil
// it falls back to the legacy poll-every-Interval loop. Either way, ctx
// cancellation returns nil WITHOUT tearing down the master — matches the
// bash trap `'... exit 0' TERM INT` so the master persists across daemon
// restarts. Only `stop`/`uninstall`/`host`-switch tear the master down.
func (e *Engine) Run(ctx context.Context) error {
	allow, _ := e.Cfg.AllowedPorts()
	host, _ := e.Cfg.ReadHost()
	e.Log.Logf("portal autoforward starting: host=%s interval=%s sock=%s",
		host, e.Interval, e.T.Sock())
	e.Log.Logf("deny=[%s] skip-local=[%s] allow=[%s]",
		formatPorts(e.Deny), formatPortsOrNone(e.SkipLocal), formatPorts(allow))

	if e.AgentEvents != nil {
		return e.runEventDriven(ctx)
	}
	return e.runIntervalLegacy(ctx)
}

// runIntervalLegacy is the poll-every-Interval shape used when no agent is
// wired in (e.g. unit tests with a synthetic discoverer).
func (e *Engine) runIntervalLegacy(ctx context.Context) error {
	if err := e.Reconcile(ctx); err != nil && ctx.Err() == nil {
		// non-fatal: next tick will retry.
	}
	tickCh, stop := e.Clk.NewTicker(e.Interval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			e.Log.Logf("received signal, exiting loop (master left running)")
			return nil
		case <-tickCh:
			_ = e.Reconcile(ctx)
		}
	}
}

// runEventDriven blocks until ctx.Done(), nudging Reconcile on every agent
// event. A 50ms debounce coalesces bursts (e.g. `docker compose up` exposing
// 8 ports in 80ms = one Reconcile, not eight).
func (e *Engine) runEventDriven(ctx context.Context) error {
	// Run one pass immediately so the master is established and any ports
	// already listening get forwarded before the first agent event arrives.
	// Matches the legacy polling loop's behaviour (and the bash original's
	// `while true; do reconcile; sleep $INTERVAL; done` which runs reconcile
	// first). Without this the master stays DOWN until the agent connects.
	if err := e.Reconcile(ctx); err != nil && ctx.Err() != nil {
		return nil
	}

	safety := e.SafetyInterval
	if safety <= 0 {
		safety = 60 * time.Second
	}
	debounce := 50 * time.Millisecond
	var pending bool
	var debounceTimer *time.Timer
	fireSoon := func() {
		if debounceTimer == nil {
			debounceTimer = time.NewTimer(debounce)
			pending = true
			return
		}
		if !pending {
			debounceTimer.Reset(debounce)
			pending = true
		}
	}

	tickCh, stop := e.Clk.NewTicker(safety)
	defer stop()

	for {
		var debounceC <-chan time.Time
		if debounceTimer != nil {
			debounceC = debounceTimer.C
		}
		select {
		case <-ctx.Done():
			e.Log.Logf("received signal, exiting loop (master left running)")
			return nil
		case ev := <-e.AgentEvents:
			switch ev.Kind {
			case EvOpenURL:
				// Not a reconcile trigger; passed through for the open-URL
				// handler in cmd/portal/run.go (see App.OpenURLEvents).
				if e.OpenURLSink != nil {
					select {
					case e.OpenURLSink <- ev.URL:
					default:
					}
				}
			case EvConnected, EvSnapshotReplaced, EvDelta:
				fireSoon()
			case EvDisconnected:
				// KEEP existing forwards — matches bash semantics for transient
				// blips. A reconnect → SnapshotReplaced will reconverge.
				if ev.Err != nil {
					e.Log.Logf("agent disconnected; preserving forwards: %v", ev.Err)
				} else {
					e.Log.Logf("agent disconnected; preserving forwards")
				}
			}
		case <-e.kick:
			// Out-of-band reconcile trigger (POST /v1/reconcile). Same debounce
			// path as EvConnected/EvDelta so bursts coalesce.
			fireSoon()
		case <-debounceC:
			pending = false
			_ = e.Reconcile(ctx)
		case <-tickCh:
			_ = e.Reconcile(ctx)
		}
	}
}

// Reconcile performs ONE stateless pass. Steps:
//
//  1. EnsureMaster — rebuild if the master is down, then re-derive everything.
//  2. AllowedPorts — re-read the file each pass so allow/unallow propagate.
//  3. DesiredPorts — what the remote wants forwarded right now. On error,
//     keep current forwards (do NOT cancel anything).
//  4. MasterForwards — what the live master is ACTUALLY forwarding (lsof).
//     This is the source of truth — never an in-memory cache.
//  5. Add desired − current; cancel current − desired.
func (e *Engine) Reconcile(ctx context.Context) error {
	// Count every completed pass (including the early error returns below) so a
	// waiter observing the counter advance knows the engine has re-derived
	// ground truth once more, even when the master is momentarily unreachable.
	defer e.reconciles.Add(1)
	pid, rebuilt, err := e.T.EnsureMaster(ctx)
	if err != nil {
		e.Log.Logf("WARN: ssh master unreachable: %v", err)
		return err
	}
	if pid == 0 {
		e.Log.Logf("WARN: could not establish master to %s", e.T.Host())
		return errMasterDown
	}
	if rebuilt {
		e.Log.Logf("master established (pid=%d)", pid)
	}

	allow, _ := e.Cfg.AllowedPorts()
	desired, err := e.RD.DesiredPorts(ctx, e.Deny, allow)
	if err != nil {
		if errors.Is(err, discover.ErrAgentNotReady) {
			// Normal during startup while the agent handshake is in flight.
			e.Log.Logf("waiting for agent snapshot")
		} else {
			e.Log.Logf("WARN: port discovery failed; keeping current forwards")
		}
		return err
	}

	current, _ := e.PL.MasterForwards(ctx, pid)

	desiredSet := indexSet(desired)
	currentSet := indexSet(current)
	skipSet := indexSet(e.SkipLocal)

	for _, p := range desired {
		if _, already := currentSet[p]; already {
			continue
		}
		if _, skip := skipSet[p]; skip {
			continue
		}
		holder, _ := e.PL.LocalHolder(ctx, p)
		if holder != 0 && holder != pid {
			// Lazy ProcessName: only consult ps if note() will actually
			// log (i.e. this conflict isn't already deduped). Matches the
			// bash early-return-before-`ps` behavior.
			e.conflicts.note(p, holder, func() string { return e.PL.ProcessName(ctx, holder) }, e.Log)
			continue
		}
		if err := e.T.Forward(ctx, p, p); err != nil {
			if fe, ok := err.(*ForwardErrorAdapter); ok {
				e.Log.Logf("ERROR adding forward %d: %s", p, fe.Stderr)
			} else {
				e.Log.Logf("ERROR adding forward %d: %v", p, err)
			}
			continue
		}
		e.conflicts.clear(p)
		e.Log.Logf("forwarded localhost:%d -> %s:%d", p, e.T.Host(), p)
	}

	for _, p := range current {
		if _, want := desiredSet[p]; !want {
			_ = e.T.Cancel(ctx, p, p)
			e.Log.Logf("removed forward %d (no longer wanted)", p)
		}
	}

	e.conflicts.prune(desired)
	return nil
}

// ForwardErrorAdapter is a re-export so engine.go can type-assert the sshctl
// transport's error without an import cycle. The transport sets the same
// underlying type; we copy the public fields here.
type ForwardErrorAdapter = sshctl.ForwardError

func indexSet(in []int) map[int]struct{} {
	m := make(map[int]struct{}, len(in))
	for _, n := range in {
		m[n] = struct{}{}
	}
	return m
}

// formatPorts joins ports as space-separated decimals (matches `echo $LIST`
// in bash). An empty list renders as an empty string.
func formatPorts(ports []int) string {
	if len(ports) == 0 {
		return ""
	}
	var b strings.Builder
	for i, p := range ports {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strconv.Itoa(p))
	}
	return b.String()
}

// formatPortsOrNone renders an empty list as the literal "none" — matches
// bash's `${SKIP_LOCAL:-none}` substitution.
func formatPortsOrNone(ports []int) string {
	if len(ports) == 0 {
		return "none"
	}
	return formatPorts(ports)
}

type sentinel string

func (e sentinel) Error() string { return string(e) }

const errMasterDown = sentinel("ssh master down")
