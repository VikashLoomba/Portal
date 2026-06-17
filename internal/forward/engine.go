// Package forward is the reconcile engine. It is intentionally stateless:
// the forwarded-ports set is derived each pass from the live ssh master's
// LISTEN sockets via the PortLister, never cached in-process. A daemon
// restart, a master rebuild, an `unallow` from another invocation — all
// self-correct because the very next pass observes ground truth.
package forward

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/vikashl/portal/internal/clock"
	"github.com/vikashl/portal/internal/config"
	"github.com/vikashl/portal/internal/discover"
	"github.com/vikashl/portal/internal/proc"
	"github.com/vikashl/portal/internal/sshctl"
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

	conflicts *conflictSet
}

// New constructs an Engine with a fresh in-memory conflict set.
func New(t sshctl.Transport, pl proc.PortLister, rd discover.RemoteDiscoverer,
	cfg *config.Store, clk clock.Clock, log Logger,
	interval time.Duration, deny, skipLocal []int) *Engine {
	return &Engine{
		T: t, PL: pl, RD: rd, Cfg: cfg, Clk: clk, Log: log,
		Interval: interval, Deny: deny, SkipLocal: skipLocal,
		conflicts: newConflictSet(),
	}
}

// Run executes Reconcile every Interval until ctx is cancelled. Returns nil
// on ctx.Done() WITHOUT tearing down the master — matches the bash trap
// `'... exit 0' TERM INT` so the master persists across daemon restarts.
// Only `stop`/`uninstall`/`host`-switch tear the master down explicitly.
func (e *Engine) Run(ctx context.Context) error {
	allow, _ := e.Cfg.AllowedPorts()
	host, _ := e.Cfg.ReadHost()
	e.Log.Logf("portal autoforward starting: host=%s interval=%s sock=%s",
		host, e.Interval, e.T.Sock())
	e.Log.Logf("deny=[%s] skip-local=[%s] allow=[%s]",
		formatPorts(e.Deny), formatPortsOrNone(e.SkipLocal), formatPorts(allow))

	// First pass immediately, then on tick — matches bash `while true; reconcile; sleep`.
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
		e.Log.Logf("WARN: port discovery failed; keeping current forwards")
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
