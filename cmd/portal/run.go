package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/agentclient"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/bootstrap"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/clip"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/clipupload"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/config"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/doctor"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/localapi"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/localclient"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/protocol"
)

func newRunCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the forwarding loop in the foreground (used by launchd)",
		RunE: func(cmd *cobra.Command, args []string) error {
			host, _ := a.Cfg.ReadHost()
			if host == "" {
				return fmt.Errorf("no dev box configured — run: %s install <ssh-host>", app.Tool)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			// A cancelable child of the signal ctx so a FATAL API bind/serve
			// failure (D10) can bring the other goroutines down: cancelling it
			// makes wg.Wait return so the daemon exits non-zero and launchd
			// relaunches loudly.
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			// Push the initial Subscribe so the agent has the latest filter
			// even before its first connect — Subscribe is buffered until
			// the encoder lands, then replayed.
			allow, _ := a.Cfg.AllowedPorts()
			_ = a.AgentClient.Subscribe(toU16(app.DenyPorts), toU16(allow), true)

			engine, openURLCh := a.NewEngineWithOpenURL()

			// Run agent supervisor, reconcile engine, URL opener, the
			// clipboard-read handler, the notification handler, and the local
			// API server in parallel; any returning ends the daemon (launchd
			// will relaunch).
			var wg sync.WaitGroup
			wg.Add(6)
			errCh := make(chan error, 6)

			go func() {
				defer wg.Done()
				errCh <- a.AgentClient.Run(ctx)
			}()
			go func() {
				defer wg.Done()
				errCh <- engine.Run(ctx)
			}()
			go func() {
				defer wg.Done()
				runOpenURLHandler(ctx, openURLCh, a)
			}()
			go func() {
				defer wg.Done()
				runClipHandler(ctx, a.AgentClient.ClipEvents(), a)
			}()
			go func() {
				defer wg.Done()
				runNotifyHandler(ctx, a.AgentClient.NotifyEvents(), a)
			}()
			go func() {
				defer wg.Done()
				// D10: API bind failure is FATAL. On a bind failure we cancel
				// the shared ctx so the other five goroutines return, wg.Wait
				// unblocks, and the daemon exits non-zero for launchd to relaunch
				// loudly. A serve failure (Serve returns non-nil) is fatal for
				// the same reason; a clean ctx-cancel shutdown returns nil.
				ln, err := localapi.Listen(a.Paths.APISock)
				if err != nil {
					errCh <- err
					cancel()
					return
				}
				deps := localapi.Deps{
					Version: localapi.VersionInfo{
						Version:      version,
						GitSHA:       bootstrap.EmbeddedSHA(),
						ProtoVersion: protocol.ProtoVersion,
					},
					Host:    a.Cfg.ReadHost,
					Agent:   a.AgentClient,
					Master:  a.Transport,
					Ports:   a.PF,
					Service: a.Service,
					Config:  a.Cfg,
					Hub:     a.Hub,
					PushAllow: func(allow []int) error {
						return a.AgentClient.Subscribe(toU16(app.DenyPorts), toU16(allow), true)
					},
					Kick:         engine.Kick,
					ReconcileGen: engine.Reconciles,
					Doctor: func(c context.Context) *doctor.Report {
						// nil ssh-stderr sink: routing doctor probes through a
						// sink-wired transport would leak ssh stderr into the
						// launchd log on the daemon-up path. Config was validated at
						// startup (NewProd), so ignoring the factory error is safe.
						tr, _, _ := app.NewTransport(a.Paths, host, a.Runner, a.Cfg, nil)
						return runDoctor(c, host, tr)
					},
				}
				srv := localapi.New(deps)
				if err := srv.Serve(ctx, ln); err != nil {
					errCh <- err
					cancel()
				}
			}()

			wg.Wait()
			close(errCh)
			for err := range errCh {
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
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
			lc := localclient.New(a.Paths.APISock)
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
func reconcileGen(ctx context.Context, lc *localclient.Client) uint64 {
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
func pollOnceReconciled(ctx context.Context, lc *localclient.Client, gen0 uint64, budget time.Duration) {
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
func runClipHandler(ctx context.Context, ch <-chan agentclient.EngineEvent, a *app.App) {
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
			go func(req *agentclient.ClipEvent) {
				defer func() { <-sem }()
				resp := serveClipRequest(ctx, a, cb, probe, &probeMu, req)
				if err := a.AgentClient.SendClipResponse(resp); err != nil {
					a.Log.Logf("clip: send response failed (nonce=%d): %v", req.Nonce, err)
				}
			}(req)
		}
	}
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
