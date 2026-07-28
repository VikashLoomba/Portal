package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/audit"
	"github.com/VikashLoomba/Portal/internal/bootstrap"
	"github.com/VikashLoomba/Portal/internal/clip"
	"github.com/VikashLoomba/Portal/internal/clipupload"
	"github.com/VikashLoomba/Portal/internal/config"
	"github.com/VikashLoomba/Portal/internal/localapi"
	"github.com/VikashLoomba/Portal/internal/setup"
	"github.com/VikashLoomba/Portal/pkg/agentclient"
	"github.com/VikashLoomba/Portal/pkg/api"
	"github.com/VikashLoomba/Portal/pkg/client"
	"github.com/VikashLoomba/Portal/pkg/protocol"
	"github.com/VikashLoomba/Portal/pkg/transport"
)

func newRunCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the forwarding loop in the foreground (used by launchd)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			s := newSupervisor(ctx, a, os.Stderr)
			return runDaemon(ctx, cancel, a, s)
		},
	}
}

func runDaemon(ctx context.Context, cancel context.CancelFunc, a *app.App, s *supervisor) error {
	host, _ := a.Cfg.ReadHost()
	if host != "" {
		if err := s.startInitial(ctx, host); err != nil {
			a.Log.Logf("daemon stack for %s unavailable; serving unconfigured API: %v", host, err)
		}
	}

	ln, err := localapi.Listen(a.Paths.APISock)
	if err != nil {
		cancel()
		s.waitCurrent()
		return err
	}
	deps := localapi.Deps{
		Version: api.VersionInfo{
			Version:      version,
			GitSHA:       bootstrap.EmbeddedSHA(),
			ProtoVersion: protocol.ProtoVersion,
		},
		Host:         s.host,
		Agent:        supervisorAgent{s},
		Master:       supervisorMaster{s},
		Ports:        supervisorPorts{s},
		Service:      a.Service,
		Config:       a.Cfg,
		Hub:          a.Hub,
		ExecStream:   supervisorExec{s},
		Audit:        a.Audit,
		PushAllow:    s.pushAllow,
		Kick:         s.kick,
		ReconcileGen: s.reconciles,
		Doctor:       s.doctor,
		PinStack: func(ctx context.Context) (localapi.StackView, func()) {
			pinned, release := s.pin(ctx)
			if pinned == nil {
				return localapi.StackView{HostKnown: true}, release
			}
			return localapi.StackView{
				Host:         pinned.host(),
				HostKnown:    true,
				Lifetime:     pinned.ctx,
				Agent:        pinned,
				Master:       pinned,
				Ports:        pinned,
				ExecStream:   pinned,
				ReconcileGen: pinned.reconciles,
				Doctor:       pinned.doctor,
			}, release
		},
		NewSetup: func(sink func(api.SetupEvent)) localapi.SetupRunner {
			return setup.New(a.Paths, a.Cfg, setup.Sink(sink))
		},
		Activate:      s.Activate,
		NormalizeHost: setup.NormalizeHost,
	}
	err = localapi.New(deps).Serve(ctx, ln)
	cancel()
	s.waitCurrent()
	return err
}

func newOnceCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "once",
		Short: "Do a single reconcile pass, then print status",
		RunE: func(cmd *cobra.Command, args []string) error {
			host, _ := a.Cfg.ReadHost()
			if host == "" {
				return fmt.Errorf("no dev box configured — run: %s install <ssh-host>", app.Tool)
			}
			// Daemon-up path: the running daemon already owns an AgentClient
			// against this box. Trigger ITS reconcile over the socket rather than
			// spinning a SECOND AgentClient against the same box — that duplicate
			// is exactly what POST /v1/reconcile exists to avoid (DESIGN §5.2).
			// Status is then rendered from GET /v1/status, so the agent line comes
			// from the live daemon handshake. a.AgentClient.Run/Shutdown are NOT
			// invoked on this branch.
			lc := client.New(a.Paths.APISock)
			if lc.Available(cmd.Context()) {
				// Snapshot the reconcile counter BEFORE the kick so the poll below
				// waits for a pass that ran AFTER our request, not one already in
				// flight (POST /v1/reconcile is async+debounced).
				gen0 := reconcileGen(cmd.Context(), lc)
				if err := lc.Reconcile(cmd.Context()); err == nil {
					pollOnceReconciled(cmd.Context(), lc, gen0, onceConvergeBudget)
					return runStatusTo(cmd.Context(), cmd.OutOrStdout(), a)
				}
			}

			// Daemon-down fallback: spin the agent up briefly to populate
			// Snapshot, then reconcile once. We use a child context so we can
			// cancel Run() directly after Shutdown — avoiding a hang if
			// Shutdown's Bye is lost.
			runCtx, runCancel := context.WithCancel(cmd.Context())
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = a.AgentClient.Subscribe(toU16(app.DenyPorts), toU16(allowOrEmpty(a)), true)
				_ = a.AgentClient.Run(runCtx)
			}()
			defer func() {
				_ = a.AgentClient.Shutdown(cmd.Context(), "once")
				runCancel() // ensure Run exits even if Shutdown Bye is lost
				<-done
			}()

			// Wait briefly for the Subscribe→Snapshot round-trip.
			if err := waitForSnapshot(runCtx, a, snapshotWaitMS); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
			}
			if err := a.Engine().Reconcile(runCtx); err != nil {
				_ = err
			}
			// Render via runStatusTo(cmd.OutOrStdout()), NOT runStatus (which
			// writes os.Stdout): routing BOTH branches through the command's out
			// writer keeps production stdout byte-identical (OutOrStdout defaults
			// to os.Stdout) while making the fallback status capturable in tests
			// exactly like the up-path (reviewer fix).
			return runStatusTo(cmd.Context(), cmd.OutOrStdout(), a)
		},
	}
}

// onceConvergeBudget bounds how long `once` waits for the daemon's async,
// debounced reconcile to complete before it renders status.
const onceConvergeBudget = 1 * time.Second

// reconcileGen reads the daemon's completed-reconcile-pass counter, or 0 if the
// status probe fails. A missed baseline only makes pollOnceReconciled wait for
// the first pass it observes — still correct, and still bounded by the budget.
func reconcileGen(ctx context.Context, lc *client.Client) uint64 {
	if st, err := lc.Status(ctx); err == nil {
		return st.Health.ReconcileCount
	}
	return 0
}

// pollOnceReconciled is a bounded, best-effort poll that gives the daemon's
// async, debounced Kick time to run a full reconcile pass before `once` renders
// status. POST /v1/reconcile only SCHEDULES a pass (202, ~50ms debounce), so
// keying off Master.Up is useless on the daemon-up branch — the running daemon
// already owns the ControlMaster, so Master.Up is true on the first poll and the
// render races ahead of the reconcile, printing the OLD forward set. Instead we
// poll Status.Health.ReconcileCount and return once it advances past the pre-kick
// baseline gen0, i.e. at least one full pass has completed since the kick was
// queued (every pass re-derives ground truth, so any pass after gen0 reflects the
// new listener/allow). It calls lc.Status up to `budget` in 50ms steps and never
// errors — `once` still returns promptly if the daemon never reports progress.
func pollOnceReconciled(ctx context.Context, lc *client.Client, gen0 uint64, budget time.Duration) {
	deadline := time.Now().Add(budget)
	for {
		if st, err := lc.Status(ctx); err == nil && st.Health.ReconcileCount > gen0 {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

const snapshotWaitMS = 5000

// runOpenURLHandler receives URLs from the agent's xdg-open interception
// and opens them on macOS. If the URL targets a localhost port that isn't
// currently forwarded (e.g. an ephemeral port used by `aws sso login`), it
// establishes a temporary forward first — the existing watcher will cancel
// it automatically once the remote process stops listening.
func runOpenURLHandler(ctx context.Context, ch <-chan string, a *app.App) {
	for {
		select {
		case <-ctx.Done():
			return
		case rawURL, ok := <-ch:
			if !ok {
				return
			}
			if rawURL == "" {
				continue
			}
			// Validate scheme before acting on it. macOS 'open' honours
			// any registered scheme including file:// and app:// handlers
			// — restricting to http/https prevents the remote box from
			// opening local Mac files or triggering unintended app actions.
			if !isSafeURL(rawURL) {
				a.Log.Logf("rejected non-http(s) URL from agent: %s", rawURL)
				continue
			}
			ensureForwardedForURL(ctx, rawURL, a)
			a.Log.Logf("opening URL from %s: %s", a.Transport.Describe().Host, rawURL)
			a.Audit.OpenURL(a.Transport.Describe().Host, rawURL)
			// Use "--" so a URL starting with "-" is never mistaken for
			// a flag, and restrict to http/https schemes.
			cmd := exec.CommandContext(ctx, "open", "--", rawURL)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				a.Log.Logf("open %q failed: %v", rawURL, err)
			}
		}
	}
}

// clipProbeTTL is how long a `targets` probe's eager read+upload result stays
// reusable for a follow-up `image`/`text` fetch (DESIGN §5). It collapses the
// TARGETS→image TOCTOU window: the agent emits targets then image back-to-back,
// and serving the second from cache avoids a second osascript coercion.
const clipProbeTTL = 10 * time.Second

// clipCoerceTimeout caps clip.ImagePNG/Text + clipupload.Upload on the paste
// path. It must stay under the agent's clipTimeout (9s) so the Mac always
// answers before the agent gives up (DESIGN §4.5) — no orphaned uploads for a
// waiter that already timed out. clip.ImagePNG/Text honour this deadline and
// additionally cap the osascript coercion at 5s, leaving ~3s for the upload
// within this 8s slot.
const clipCoerceTimeout = 8 * time.Second

const (
	// clipWriteApplyTimeout covers the remote pull and local pasteboard set.
	// It stays below the agent's 9s clipWriteTimeout so a response is available
	// before the agent gives up (WRITE DESIGN §4.4).
	clipWriteApplyTimeout = 8 * time.Second
	clipWriteMaxBytes     = clipupload.MaxUploadBytes
	clipWriteBannerWindow = 5 * time.Second
)

var clipWriteSHARE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// clipEntry caches the result of an eager `targets` read so a back-to-back
// `image`/`text` fetch reuses it instead of re-coercing the clipboard.
type clipEntry struct {
	sha      string
	deadline time.Time
}

// runClipHandler services KindClipRequest events from the agent: a remote shim
// asked the Mac to read its clipboard. It runs on its OWN goroutine fed by a
// DEDICATED channel (not the shared, drop-on-full events channel) so a burst of
// port events can't evict a pending paste (DESIGN §5). A worker semaphore of 1
// serializes the actual clipboard reads so two rapid pastes can't fork two
// osascript calls; while a read is in flight, additional requests answer
// OK=false immediately (the agent maps that to "none" and the shim falls
// through — better than queueing behind a slow coercion and blowing the budget).
//
// Ordering invariant (DESIGN §2): for image/text the bytes are uploaded over
// the side channel and exit-0-confirmed BEFORE the OK=true ClipResponse is
// sent, so by the time the agent answers the shim the file is guaranteed
// present. Bytes NEVER touch the CBOR frame — the response carries only a SHA.
type goroutineTracker interface {
	Add(int)
	Done()
}

func runClipHandler(ctx context.Context, ch <-chan agentclient.EngineEvent, a *app.App, wg goroutineTracker) {
	cb := clip.New()
	// probe caches the most recent eager `targets` read per kind so the
	// follow-up `image`/`text` fetch can reuse it. Single-Mac-per-host, so a
	// per-kind entry is sufficient; the worker semaphore serializes access.
	probe := map[string]clipEntry{}
	var probeMu sync.Mutex
	// sem bounds in-flight reads to 1.
	sem := make(chan struct{}, 1)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Clip == nil {
				continue
			}
			req := ev.Clip
			select {
			case sem <- struct{}{}:
			default:
				// A read is already in flight — don't queue (would blow the
				// budget). Answer not-available so the shim falls through.
				a.AgentClient.SendClipResponse(&protocol.ClipResponse{
					Nonce: req.Nonce, Epoch: req.Epoch, OK: false,
				})
				continue
			}
			// Handle on a worker goroutine so the demux of the next event isn't
			// blocked by the coercion+upload; the semaphore (cap 1) still
			// serializes the reads themselves.
			if wg != nil {
				wg.Add(1)
			}
			go func(req *agentclient.ClipEvent) {
				if wg != nil {
					defer wg.Done()
				}
				defer func() { <-sem }()
				resp := serveClipRequest(ctx, a, cb, probe, &probeMu, req)
				if err := a.AgentClient.SendClipResponse(resp); err != nil {
					a.Log.Logf("clip: send response failed (nonce=%d): %v", req.Nonce, err)
				}
			}(req)
		}
	}
}

// clipWriteDeps is the injectable boundary for the remote pull, pasteboard
// mutation, policy gate, audit, and post-response security banner.
type clipWriteDeps struct {
	Writer         clip.Writer
	Transport      transport.Transport
	FeatureEnabled func(string) bool
	Audit          *audit.Log
	Host           string
	Banner         *clipWriteBanner
	Log            func(string)
}

type clipWriteResponseSender func(*protocol.ClipWriteResponse) error

// runClipWriteHandler drains the dedicated clipboard-write channel and
// delegates to the injectable handler loop. The writer and banner live for the
// lifetime of this stack.
func runClipWriteHandler(ctx context.Context, ch <-chan agentclient.EngineEvent, a *app.App, wg goroutineTracker) {
	logLine := func(line string) {
		if a.Log != nil {
			a.Log.Logf("%s", line)
		}
	}
	host := a.Transport.Describe().Host
	deps := clipWriteDeps{
		Writer:         clip.NewWriter(),
		Transport:      a.Transport,
		FeatureEnabled: a.Cfg.FeatureEnabled,
		Audit:          a.Audit,
		Host:           host,
		Banner:         newClipWriteBanner(host),
		Log:            logLine,
	}
	runClipWriteHandlerWithDeps(ctx, ch, deps, a.AgentClient.SendClipWriteResponse, wg)
}

// runClipWriteHandlerWithDeps serializes pasteboard writers with a cap-1
// semaphore. A request arriving while one is active is refused immediately;
// queueing it could exceed the agent's response budget.
func runClipWriteHandlerWithDeps(ctx context.Context, ch <-chan agentclient.EngineEvent,
	deps clipWriteDeps, send clipWriteResponseSender, wg goroutineTracker) {

	if deps.Banner != nil {
		defer deps.Banner.close()
	}
	sem := make(chan struct{}, 1)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.ClipWrite == nil {
				continue
			}
			req := ev.ClipWrite
			select {
			case sem <- struct{}{}:
			default:
				kind := clipWriteAuditKind(req.Kind)
				deps.Audit.ClipWriteDenied(deps.Host, kind, "inflight")
				resp := &protocol.ClipWriteResponse{
					Nonce: req.Nonce, Epoch: req.Epoch, OK: false, Err: "inflight",
				}
				if err := send(resp); err != nil {
					logClipWriteLine(deps, fmt.Sprintf(
						"clip-write: send response failed (nonce=%d): %v", req.Nonce, err))
				}
				continue
			}
			if wg != nil {
				wg.Add(1)
			}
			go func(req *agentclient.ClipWriteEvent) {
				if wg != nil {
					defer wg.Done()
				}
				defer func() { <-sem }()

				resp := serveClipWriteRequest(ctx, deps, req)
				if err := send(resp); err != nil {
					logClipWriteLine(deps, fmt.Sprintf(
						"clip-write: send response failed (nonce=%d): %v", req.Nonce, err))
				}
				if !resp.OK {
					return
				}

				kind := clipWriteAuditKind(req.Kind)
				detail := fmt.Sprintf("sha=%s size=%d", req.SHA, req.Size)
				if req.Kind == "clear" {
					detail = "cleared"
				}
				// The synchronous audit is ground truth. The banner is dispatched
				// only after the response, outside the 8s apply budget.
				deps.Audit.ClipWritten(deps.Host, kind, detail)
				if deps.Banner != nil {
					deps.Banner.note(req.Kind, req.Format, req.Size)
				}
			}(req)
		}
	}
}

// serveClipWriteRequest re-reads policy, validates the exact wire tuple, pulls
// and verifies bytes, and mutates the pasteboard. It NEVER returns nil and
// echoes Nonce/Epoch on every path. Successful auditing and notification occur
// after the caller sends this response.
func serveClipWriteRequest(ctx context.Context, deps clipWriteDeps,
	req *agentclient.ClipWriteEvent) *protocol.ClipWriteResponse {

	if req == nil {
		req = &agentclient.ClipWriteEvent{}
	}
	resp := &protocol.ClipWriteResponse{Nonce: req.Nonce, Epoch: req.Epoch}
	kind := clipWriteAuditKind(req.Kind)

	if deps.FeatureEnabled == nil || !deps.FeatureEnabled(config.FeatureClipWrite) {
		return denyClipWriteResponse(deps, resp, kind, "disabled")
	}

	ext, pull, reason := clipWriteShape(req.Kind, req.Format, req.SHA, req.Size)
	if reason != "" {
		return denyClipWriteResponse(deps, resp, kind, reason)
	}

	cctx, cancel := context.WithTimeout(ctx, clipWriteApplyTimeout)
	defer cancel()

	var data []byte
	if pull {
		var err error
		data, err = pullClipWrite(cctx, deps.Transport, req.SHA, ext, req.Size)
		if err != nil {
			logClipWriteLine(deps, fmt.Sprintf(
				"clip-write: pull failed (nonce=%d kind=%s): %v", req.Nonce, kind, err))
			return resp
		}
		if int64(len(data)) != req.Size || clipupload.ShortSHA(data) != req.SHA {
			return denyClipWriteResponse(deps, resp, kind, "shamismatch")
		}
	}

	var err error
	switch req.Kind {
	case "text":
		err = deps.Writer.SetText(cctx, data)
	case "image":
		err = deps.Writer.SetImagePNG(cctx, data)
	case "clear":
		err = deps.Writer.Clear(cctx)
	}
	if err != nil {
		logClipWriteLine(deps, fmt.Sprintf(
			"clip-write: pasteboard set failed (nonce=%d kind=%s): %v", req.Nonce, kind, err))
		return resp
	}

	resp.OK = true
	return resp
}

// clipWriteShape enforces WRITE DESIGN §4.1's exact per-kind field semantics
// and returns the extension for byte-carrying requests.
//
// NO NEW VOCABULARY: §7.1 fixes exactly {disabled, oversize, badsha,
// shamismatch, inflight}. Every field-shape violation folds into badsha
// because the content address used to reconstruct the pull path is the
// (kind, format, sha) tuple. oversize is reserved for an in-shape size that is
// out of range on a byte-carrying kind. The agent grammar rejects these shapes
// first; this check is the Mac-side defense against a forged or drifted frame.
//
// Precedence is fixed: kind/format shape, then SHA, then size. A frame violating
// more than one rule therefore produces one deterministic reason.
func clipWriteShape(kind, format, sha string, size int64) (ext string, pull bool, reason string) {
	switch kind {
	case "clear":
		if format != "" || sha != "" || size != 0 {
			return "", false, "badsha"
		}
		return "", false, ""
	case "text":
		if format != "" {
			return "", false, "badsha"
		}
		ext = ".txt"
	case "image":
		if format != "png" {
			return "", false, "badsha"
		}
		ext = ".png"
	default:
		return "", false, "badsha"
	}
	if !clipWriteSHARE.MatchString(sha) {
		return "", false, "badsha"
	}
	if size <= 0 || size > clipWriteMaxBytes {
		return "", false, "oversize"
	}
	return ext, true, ""
}

// pullClipWrite reconstructs the only permitted remote path from validated
// fields and bounds stdout remotely. Transport.Exec buffers stdout, so a bare
// cat would let a same-uid file swap inflate daemon memory before Go could
// enforce the cap. The symlink check is the remote counterpart to portald's
// O_NOFOLLOW open; --noprofile/--norc keeps rc noise out of binary stdout.
func pullClipWrite(ctx context.Context, t transport.Transport, sha, ext string, size int64) ([]byte, error) {
	script := fmt.Sprintf(
		`f="$HOME/.cache/portal/clip/copy-%s%s"; if [ -L "$f" ] || [ ! -f "$f" ]; then exit 1; fi; exec head -c %d < "$f"`,
		sha, ext, size,
	)
	stdout, stderr, err := t.Exec(ctx, nil,
		"bash", "--noprofile", "--norc", "-c", clipWriteShellQuote(script))
	if err != nil {
		if stderr = strings.TrimSpace(stderr); stderr != "" {
			return nil, fmt.Errorf("pull clipboard bytes: %w: %s", err, stderr)
		}
		return nil, fmt.Errorf("pull clipboard bytes: %w", err)
	}
	return []byte(stdout), nil
}

// clipWriteAuditKind closes the audit field over the protocol's three valid
// kinds even before shape validation runs (notably on the inflight path).
func clipWriteAuditKind(kind string) string {
	switch kind {
	case "text", "image", "clear":
		return kind
	default:
		return "unknown"
	}
}

func denyClipWriteResponse(deps clipWriteDeps, resp *protocol.ClipWriteResponse,
	kind, reason string) *protocol.ClipWriteResponse {

	resp.OK = false
	resp.Err = reason
	deps.Audit.ClipWriteDenied(deps.Host, kind, reason)
	return resp
}

func logClipWriteLine(deps clipWriteDeps, line string) {
	if deps.Log != nil {
		deps.Log(line)
	}
}

// clipWriteShellQuote preserves one script argv element across Transport's
// remote-shell join contract.
func clipWriteShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Once the pasteboard has been mutated, its banner is owed. Notification
// dispatch is deliberately independent of the stack context and bounded only
// by raiseNotification's own timeout.
type clipWriteBanner struct {
	host       string
	window     time.Duration
	raise      func(title, subtitle string)
	dispatch   func(func())
	after      func(time.Duration, func()) func() bool
	mu         sync.Mutex
	open       bool
	closed     bool
	suppressed int
	stop       func() bool
}

func newClipWriteBanner(host string) *clipWriteBanner {
	return &clipWriteBanner{
		host:   host,
		window: clipWriteBannerWindow,
		raise: func(title, subtitle string) {
			// This security banner never consults feature.notify. The only
			// supported way to silence remote-write banners is to disable
			// feature.clip-write itself (WRITE DESIGN §5.1/§8.5).
			raiseNotification(context.Background(), title, "", subtitle, "")
		},
		// These raises and the pending timer are intentionally detached from the
		// lifecycle group. Both can live for 5s while stackDrainTimeout is 2s;
		// tracking them would create a false old-stack drain failure and would
		// still risk discarding an owed banner. At process exit, the synchronous
		// audit remains the ground truth if a detached child has not yet spawned.
		dispatch: func(fn func()) { go fn() },
		after: func(d time.Duration, fn func()) func() bool {
			timer := time.AfterFunc(d, fn)
			return timer.Stop
		},
	}
}

func (b *clipWriteBanner) note(kind, format string, size int64) {
	title := "Clipboard set from " + b.host
	subtitle := clipWriteSubtitle(kind, format, size)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.dispatchRaise(title, subtitle)
		return
	}
	if b.open {
		b.suppressed++
		b.mu.Unlock()
		return
	}
	b.open = true
	b.suppressed = 0
	b.stop = b.after(b.window, b.flushWindow)
	b.mu.Unlock()
	b.dispatchRaise(title, subtitle)
}

func (b *clipWriteBanner) flushWindow() {
	b.mu.Lock()
	n := b.suppressed
	b.suppressed = 0
	b.open = false
	b.stop = nil
	b.mu.Unlock()

	if n > 0 {
		b.dispatchRaise(fmt.Sprintf("%d more clipboard writes from %s", n, b.host), "")
	}
}

// close flushes rather than discards an owed trailing summary. The callback
// and close zero suppressed under the same lock, so a lost Stop race cannot
// raise the summary twice.
func (b *clipWriteBanner) close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	if b.stop != nil {
		b.stop()
		b.stop = nil
	}
	n := b.suppressed
	b.suppressed = 0
	b.open = false
	b.mu.Unlock()

	if n > 0 {
		b.dispatchRaise(fmt.Sprintf("%d more clipboard writes from %s", n, b.host), "")
	}
}

func (b *clipWriteBanner) dispatchRaise(title, subtitle string) {
	b.dispatch(func() {
		b.raise(title, subtitle)
	})
}

func clipWriteSubtitle(kind, format string, size int64) string {
	if kind == "clear" {
		return "cleared"
	}
	label := kind
	if format != "" {
		label += "/" + format
	}
	if size < 1024 {
		return fmt.Sprintf("%s, %d bytes", label, size)
	}
	return fmt.Sprintf("%s, %d KB", label, (size+1023)/1024)
}

// serveClipRequest reads/uploads the clipboard for a single ClipRequest and
// returns the ClipResponse to send. It NEVER returns nil. On any error it
// returns OK=false (which the agent maps to "none\n" → shim falls through).
func serveClipRequest(ctx context.Context, a *app.App, cb clip.Clipboard,
	probe map[string]clipEntry, probeMu *sync.Mutex, req *agentclient.ClipEvent) *protocol.ClipResponse {

	resp := &protocol.ClipResponse{Nonce: req.Nonce, Epoch: req.Epoch}

	cctx, cancel := context.WithTimeout(ctx, clipCoerceTimeout)
	defer cancel()

	switch req.Kind {
	case "targets":
		// Report what's available, and TELL the agent which kind so its grep
		// sees the target lines actually on the clipboard. Image wins over text
		// when both are present (HasImage first, else HasText) — matches what a
		// Ctrl+V paste of a screenshot vs selected text expects. For image we
		// eagerly read+upload NOW and cache the sha so the back-to-back `image`
		// fetch is served from cache — collapsing the TARGETS→image TOCTOU
		// window to a single read (§5).
		if cb.HasImage() {
			// Only short-circuit to "serve image" when the image feature is
			// ENABLED and the upload succeeds. When the image feature is
			// DISABLED we must NOT return here: an image being present must not
			// hide text that the user has enabled (e.g. image feature off, text
			// feature on, screenshot + selected text both on the clipboard).
			// In that case we fall through to the text check below. Likewise an
			// enabled-but-failed image coercion/upload falls through.
			if clipFeatureAllowed(a, "image") {
				if sha, err := uploadClipImage(cctx, a, cb); err == nil {
					cacheClip(probe, probeMu, "image", sha)
					a.Audit.ClipServed(a.Transport.Describe().Host, "image", "sha="+sha)
					resp.OK = true
					resp.Has = true
					resp.Kind = "image"
					resp.SHA = sha
					return resp
				}
				// Coerce/upload failed — fall through to checking text / no-content.
			} else {
				// Image present but feature disabled: audit the denial, then fall
				// through to the text check rather than returning Has=false (which
				// would hide servable text from the remote).
				a.Audit.ClipDenied(a.Transport.Describe().Host, "image", "disabled")
			}
		}
		if cb.HasText() {
			if !clipTextServeAllowed(a, cb) {
				reason := "disabled"
				if clipFeatureAllowed(a, "text") {
					reason = "concealed"
				}
				a.Audit.ClipDenied(a.Transport.Describe().Host, "text", reason)
				resp.OK = true
				resp.Has = false
				return resp
			}
			// Eagerly read+upload the text too so the back-to-back `text` fetch
			// is served from cache, applying the source-side length cap first.
			if data, err := cb.Text(cctx); err == nil && len(data) <= clipupload.MaxUploadBytes {
				if _, sha, uerr := clipupload.UploadText(cctx, a.Transport, data); uerr == nil {
					cacheClip(probe, probeMu, "text", sha)
					a.Audit.ClipServed(a.Transport.Describe().Host, "text", fmt.Sprintf("len=%d", len(data)))
					resp.OK = true
					resp.Has = true
					resp.Kind = "text"
					resp.SHA = sha
					return resp
				}
			}
			// Coerce/upload failed or oversized — fall through to no-content.
		}
		// Nothing servable on the clipboard.
		resp.OK = true
		resp.Has = false
		return resp

	case "image":
		if req.Format != "png" {
			return resp // OK=false: portal only serves PNG
		}
		if !clipFeatureAllowed(a, "image") {
			a.Audit.ClipDenied(a.Transport.Describe().Host, "image", "disabled")
			return resp
		}
		if sha, ok := lookupClip(probe, probeMu, "image"); ok {
			// Served from the targets-probe cache (already audited there).
			resp.OK = true
			resp.SHA = sha
			return resp
		}
		if !cb.HasImage() {
			return resp
		}
		sha, err := uploadClipImage(cctx, a, cb)
		if err != nil {
			a.Log.Logf("clip: image read/upload failed: %v", err)
			return resp
		}
		a.Audit.ClipServed(a.Transport.Describe().Host, "image", "sha="+sha)
		resp.OK = true
		resp.SHA = sha
		return resp

	case "text":
		// Gate text reads behind the capability + concealed-clipboard skip
		// BEFORE the cache lookup — the gate is RE-READ EACH PASS (config.go:54)
		// so a disable (echo off > feature.clip-text) or a freshly-concealed
		// clipboard (password manager) must take effect immediately, even within
		// the 10s probe-cache TTL. Gating after the cache hit would let a SHA
		// cached by an earlier `clip targets` probe leak text after the user
		// disabled the feature / copied a secret. When disabled/concealed the
		// response is OK=false → "none" → the shim falls through to the real
		// binary. (Mirrors the image path, which gates before its cache lookup.)
		if !clipTextServeAllowed(a, cb) {
			reason := "disabled"
			if clipFeatureAllowed(a, "text") {
				reason = "concealed"
			}
			a.Audit.ClipDenied(a.Transport.Describe().Host, "text", reason)
			return resp
		}
		if sha, ok := lookupClip(probe, probeMu, "text"); ok {
			// Served from the targets-probe cache (already audited there).
			resp.OK = true
			resp.SHA = sha
			return resp
		}
		if !cb.HasText() {
			return resp
		}
		data, err := cb.Text(cctx)
		if err != nil {
			a.Log.Logf("clip: text read failed: %v", err)
			return resp
		}
		// Source-side length cap (SPEC E): reuse the 8 MiB upload cap so an
		// oversized paste fails fast here rather than stalling the upload.
		if len(data) > clipupload.MaxUploadBytes {
			a.Log.Logf("clip: text too large: %d bytes (max %d)", len(data), clipupload.MaxUploadBytes)
			return resp
		}
		_, sha, err := clipupload.UploadText(cctx, a.Transport, data)
		if err != nil {
			a.Log.Logf("clip: text upload failed: %v", err)
			return resp
		}
		cacheClip(probe, probeMu, "text", sha)
		a.Audit.ClipServed(a.Transport.Describe().Host, "text", fmt.Sprintf("len=%d", len(data)))
		resp.OK = true
		resp.SHA = sha
		return resp

	default:
		return resp // unknown kind: OK=false
	}
}

// clipFeatureAllowed reports whether serving the given clipboard feature
// ("image" or "text") is enabled for this Mac. It is the capability-gate CHECK
// SITE (SPEC C), backed by internal/config feature toggles — a user disables a
// feature by writing "off" into ~/.config/portal/feature.clip-image /
// feature.clip-text (re-read every serve, no daemon restart). The cc-clip
// posture is the default: both image and text are ON when no toggle exists
// (text additionally honours the concealed-clipboard skip in
// clipTextServeAllowed). When this returns false serveClipRequest short-circuits
// to OK=true/Has=false (targets) or OK=false (image/text) — the agent maps both
// to "none" so the remote shim falls through to the real binary.
func clipFeatureAllowed(a *app.App, feature string) bool {
	switch feature {
	case "image":
		return a.Cfg.FeatureEnabled(config.FeatureClipImage)
	case "text":
		return a.Cfg.FeatureEnabled(config.FeatureClipText)
	default:
		// Unknown feature: default-deny (no known caller hits this, but a
		// typo should fail closed rather than silently serve).
		return false
	}
}

// clipTextServeAllowed reports whether the current TEXT clipboard may be
// served: it must be both capability-enabled AND not concealed/transient. It is
// the combined CHECK SITE for the text capability gate + the concealed-clipboard
// skip (SPEC C/E): a password manager that copied a credential marks the
// pasteboard org.nspasteboard.ConcealedType, and the standing text-pull endpoint
// must never auto-exfiltrate that. When it returns false the text serve replies
// "none" and the shim falls through to the real binary.
func clipTextServeAllowed(a *app.App, cb clip.Clipboard) bool {
	if !clipFeatureAllowed(a, "text") {
		return false
	}
	if cb.IsConcealed() {
		// Secret/transient clipboard (password manager). Skip serving — the
		// caller logs this as a "concealed" denial.
		return false
	}
	return true
}

// uploadClipImage coerces the clipboard image to PNG and uploads it, returning
// the short SHA. Only returns success after Upload confirms exit 0 with a
// validated remote path (DESIGN §2 ordering invariant).
func uploadClipImage(ctx context.Context, a *app.App, cb clip.Clipboard) (string, error) {
	png, err := cb.ImagePNG(ctx)
	if err != nil {
		return "", err
	}
	_, sha, err := clipupload.UploadImage(ctx, a.Transport, png)
	if err != nil {
		return "", err
	}
	return sha, nil
}

func cacheClip(probe map[string]clipEntry, mu *sync.Mutex, kind, sha string) {
	mu.Lock()
	probe[kind] = clipEntry{sha: sha, deadline: time.Now().Add(clipProbeTTL)}
	mu.Unlock()
}

func lookupClip(probe map[string]clipEntry, mu *sync.Mutex, kind string) (string, bool) {
	mu.Lock()
	defer mu.Unlock()
	e, ok := probe[kind]
	if !ok || time.Now().After(e.deadline) {
		delete(probe, kind)
		return "", false
	}
	return e.sha, true
}

// ensureForwardedForURL ensures any localhost ports referenced in rawURL
// are forwarded before the browser opens it. It checks both the URL's own
// host:port AND any localhost ports embedded in query parameter values
// (e.g. redirect_uri=http://127.0.0.1:39041/... in AWS SSO URLs).
func ensureForwardedForURL(ctx context.Context, rawURL string, a *app.App) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return
	}

	// Collect every localhost port we need — the direct URL port first,
	// then any localhost port found in query parameter values.
	ports := collectLoopbackPorts(u)
	if len(ports) == 0 {
		return
	}

	h, _ := a.Transport.Health(ctx)
	if !h.Up {
		return
	}
	current, _ := a.PF.ListForwards(ctx)
	forwarded := make(map[int]bool, len(current))
	for _, p := range current {
		forwarded[p] = true
	}

	for _, port := range ports {
		if forwarded[port] {
			continue
		}
		if err := a.PF.Forward(ctx, port, port); err != nil {
			a.Log.Logf("auto-forward port %d: %v", port, err)
			continue
		}
		a.Log.Logf("auto-forwarded localhost:%d -> %s:%d", port, a.Transport.Describe().Host, port)
	}
}

// collectLoopbackPorts extracts every unique localhost port from a URL —
// including ports embedded in query parameter values (e.g. redirect_uri).
func collectLoopbackPorts(u *url.URL) []int {
	seen := map[int]bool{}
	var result []int

	add := func(raw string) {
		parsed, err := url.Parse(raw)
		if err != nil {
			return
		}
		h := parsed.Hostname()
		if h != "localhost" && h != "127.0.0.1" && h != "::1" {
			return
		}
		portStr := parsed.Port()
		if portStr == "" {
			return
		}
		p, err := strconv.Atoi(portStr)
		if err != nil || p <= 0 || p > 65535 {
			return
		}
		if !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}

	// Check the URL itself.
	add(u.String())

	// Check every query parameter value — handles redirect_uri, callback_url, etc.
	for _, v := range u.Query() {
		for _, s := range v {
			add(s)
		}
	}

	return result
}

// isSafeURL returns true only for http and https schemes.
func isSafeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// runStatusCtx is a tiny helper so once.go and the root status default share.
func runStatusCtx(ctx context.Context, a *app.App) error { return runStatus(ctx, a) }

// toU16 narrows []int → []uint16, dropping out-of-range values.
func toU16(in []int) []uint16 {
	out := make([]uint16, 0, len(in))
	for _, v := range in {
		if v <= 0 || v > 65535 {
			continue
		}
		out = append(out, uint16(v))
	}
	return out
}

func allowOrEmpty(a *app.App) []int {
	ps, err := a.Cfg.AllowedPorts()
	if err != nil {
		return nil
	}
	return ps
}
