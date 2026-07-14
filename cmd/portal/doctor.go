package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/doctorprobe"
	"github.com/VikashLoomba/Portal/pkg/client"
	"github.com/VikashLoomba/Portal/pkg/doctor"
	"github.com/VikashLoomba/Portal/pkg/transport"
	"github.com/VikashLoomba/Portal/pkg/transport/sshnative"
)

// newDoctorCmd self-tests the clipboard + notification path end to end over ssh
// (SPEC D). cc-clip verifies its setup at install time; we match that
// confidence with a standalone `portal doctor` that the install flow also runs.
// The single make-or-break is the PATH-winner check — that `command -v xclip` /
// `wl-paste` in a representative shell resolve to OUR shim and not a real
// /usr/bin binary later on PATH. We also confirm the master is up + the agent
// supports the clip+notify verbs, report the shim Version vs the embedded one,
// and run an end-to-end `portald clip targets` smoke (plus a real image/text
// fetch when something is on the Mac clipboard).
func newDoctorCmd(a *app.App) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Self-test the clipboard + notification path over ssh (PATH winner, shim version, agent verbs, smoke)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctorCmd(cmd.Context(), cmd.OutOrStdout(), a)
		},
	}
}

// runDoctorCmd is newDoctorCmd.RunE's body, extracted so tests can drive it with
// a buffer and an App. Production output is byte-identical (cmd.OutOrStdout()
// defaults to os.Stdout). When the daemon is up it POSTs /v1/doctor so the
// self-test runs against the daemon's LIVE ControlMaster (better ground truth
// than a fresh CLI-side probe); when the daemon is down it falls back to today's
// in-process run. Both paths need a host.
//
// nativeOpts is a T5 hermeticity seam: production passes NONE (native resolves
// the real ~/.ssh defaults), while the daemon-down fallback test injects temp-dir
// known_hosts/identity fixtures so the native construction never touches the
// runner's real ~/.ssh. It applies only to the native branch of the factory.
func runDoctorCmd(ctx context.Context, w io.Writer, a *app.App, nativeOpts ...sshnative.Option) error {
	host, _ := a.Cfg.ReadHost()
	if host == "" {
		return fmt.Errorf("no dev box configured — run: %s install <ssh-host>", app.Tool)
	}
	// Prefer the daemon: it renders from its live transport. We decide up/down with
	// the same fast Available probe every other command uses (allow.go, inspect.go,
	// run.go) so a dial "daemon is down" is cleanly distinguished from a /v1/doctor
	// call that runs long: POST /v1/doctor is long-running (§4.5), so once the
	// daemon is confirmed up we let its result stand. A slow or errored daemon run
	// is REPORTED, never silently re-run in-process — a silent local fallback here
	// would double the work (30s+), discard the daemon's live-transport ground
	// truth, and hide from the user that the daemon path was abandoned.
	lc := client.New(a.Paths.APISock)
	if lc.Available(ctx) {
		rep, err := lc.Doctor(ctx)
		if err != nil {
			return fmt.Errorf("daemon doctor failed (daemon is up; not falling back to a local run): %w", err)
		}
		renderDoctor(w, rep)
		if !rep.OK() {
			return errSilent
		}
		return nil
	}
	// Fallback (daemon down): the in-process run over a FRESH transport built by
	// the selection-aware factory so a `native` selection is honored and surfaced
	// even with the daemon down. The nil ssh-stderr sink preserves the current
	// no-leak behavior (routing doctor probes through a.Transport, or a sink-wired
	// transport, would tee ssh stderr into the report). The only test seam that
	// intercepts this path is a.Runner.
	tr, _, err := app.NewTransport(a.Paths, host, a.Runner, a.Cfg, nil, nativeOpts...)
	if err != nil {
		return err
	}
	rep := runDoctor(ctx, host, tr)
	// This branch is only reached with the daemon confirmed DOWN
	// (lc.Available==false above). Force the daemon-down verdict to be honest
	// under a non-system transport — see markDaemonDown.
	markDaemonDown(rep, tr.Describe().Impl)
	renderDoctor(w, rep)
	if !rep.OK() {
		return errSilent
	}
	return nil
}

// markDaemonDown makes the daemon-down fallback report honest under a NON-system
// transport. The fallback in runDoctorCmd is only reached with the relay daemon
// confirmed down, and for native runDoctor built a FRESH client and actively
// Ensure'd it (a native dial is a cheap in-process connect that the install/
// self-test path legitimately needs) — a throwaway connection that shares nothing
// with the dead daemon. That dial succeeds against a reachable box, so the master
// check renders "ssh master: UP (pid=0)" and every downstream probe passes, which
// would render a FALSE "RESULT: PASS": the relay daemon is down, so
// clip/notify/forwards are dead (native forwards have no ControlPersist analogue,
// per T10). Seed a FAIL so the verdict matches reality.
//
// SYSTEM is a deliberate no-op (byte-identical per T9): its fresh transport shares
// the persistent ControlMaster and doctor probes it PASSIVELY (never Ensures), so
// a daemon-down system run already FAILs the master check on its own — adding a
// line here would break the goldens.
func markDaemonDown(rep *doctor.Report, impl transport.Impl) {
	if impl == transport.ImplSystemSSH {
		return
	}
	rep.Add("daemon", doctor.Fail,
		"daemon not running — clip/notify/forwards are down; start it: "+app.Tool+" start")
}

// doctorTransport picks the transport the daemon-up /v1/doctor probe runs over.
//
// For NATIVE it returns the daemon's LIVE transport (a.Transport). A native
// connection is in-process state: a freshly-constructed client shares nothing
// with the daemon's live connection, so its Health reports DOWN without dialing
// (New never dials) — the false "ssh master DOWN" a fresh factory client would
// yield on a healthy native daemon. Ensure is idempotent: a no-op when the live
// client is up, a same-client re-dial (no leaked connection) if the keepalive
// marked it dead. Unlike system-ssh — whose fresh transport shares the persistent
// ControlMaster socket and can probe it via `ssh -O check` — native must be
// observed on the very connection the daemon holds.
//
// For SYSTEM it returns a FRESH nil-sink transport: routing doctor probes through
// a.Transport (StderrSink=os.Stderr) would tee ssh stderr into the launchd log,
// so the fresh transport keeps the report clean while still probing the shared
// ControlMaster socket.
func doctorTransport(ctx context.Context, a *app.App, host string) (transport.Transport, error) {
	sel, err := a.Cfg.Transport()
	if err != nil {
		return nil, err
	}
	if sel == "native" && a.Transport != nil {
		// Idempotent: no-op if already up, same-client re-dial if keepalive-dead.
		_, _ = a.Transport.Ensure(ctx)
		return a.Transport, nil
	}
	tr, _, err := app.NewTransport(a.Paths, host, a.Runner, a.Cfg, nil)
	if err != nil {
		return nil, err
	}
	return tr, nil
}

// renderDoctor writes the human-readable report to w. It is a free function in
// package main (presentation stays here; the data types live in internal/doctor
// so the local API can return them as JSON). Output is byte-for-byte compatible
// with the historical (*doctorReport).write so scripts parsing doctor output do
// not break.
func renderDoctor(w io.Writer, rep *doctor.Report) {
	fmt.Fprintf(w, "portal doctor — %s\n", rep.Host)
	for _, c := range rep.Checks {
		if c.Detail == "" {
			fmt.Fprintf(w, "  [%s] %s\n", c.Status.Tag(), c.Name)
		} else {
			fmt.Fprintf(w, "  [%s] %s: %s\n", c.Status.Tag(), c.Name, c.Detail)
		}
	}
	if rep.OK() {
		fmt.Fprintln(w, "\nRESULT: PASS — clipboard paste should work over plain ssh.")
	} else {
		fmt.Fprintln(w, "\nRESULT: FAIL — clipboard paste will NOT work. Fix the FAIL lines above")
		fmt.Fprintf(w, "        (often: re-run `%s install %s`), then re-run `%s doctor`.\n", app.Tool, rep.Host, app.Tool)
	}
}

func runDoctor(ctx context.Context, host string, tr transport.Transport) *doctor.Report {
	return doctorprobe.Run(ctx, host, tr)
}
