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

	"github.com/spf13/cobra"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
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

			// Push the initial Subscribe so the agent has the latest filter
			// even before its first connect — Subscribe is buffered until
			// the encoder lands, then replayed.
			allow, _ := a.Cfg.AllowedPorts()
			_ = a.AgentClient.Subscribe(toU16(app.DenyPorts), toU16(allow), true)

			engine, openURLCh := a.NewEngineWithOpenURL()

			// Run agent supervisor, reconcile engine, and URL opener in
			// parallel; any returning ends the daemon (launchd will relaunch).
			var wg sync.WaitGroup
			wg.Add(3)
			errCh := make(chan error, 3)

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
			// Spin the agent up briefly to populate Snapshot, then reconcile
			// once. We use a child context so we can cancel Run() directly
			// after Shutdown — avoiding a hang if Shutdown's Bye is lost.
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
			return runStatus(cmd.Context(), a)
		},
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
			a.Log.Logf("opening URL from %s: %s", a.Transport.Host(), rawURL)
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

	masterPID, _ := a.Transport.MasterPID(ctx)
	if masterPID == 0 {
		return
	}
	current, _ := a.Ports.MasterForwards(ctx, masterPID)
	forwarded := make(map[int]bool, len(current))
	for _, p := range current {
		forwarded[p] = true
	}

	for _, port := range ports {
		if forwarded[port] {
			continue
		}
		if err := a.Transport.Forward(ctx, port, port); err != nil {
			a.Log.Logf("auto-forward port %d: %v", port, err)
			continue
		}
		a.Log.Logf("auto-forwarded localhost:%d -> %s:%d", port, a.Transport.Host(), port)
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
