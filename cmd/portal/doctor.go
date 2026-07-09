package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/clipshim"
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

// runDoctor performs every probe over tr and returns the assembled report. It
// is split out (taking a Transport) so a test can drive it with a fake
// transport that scripts canned ssh-exec replies — no live dev box needed.
func runDoctor(ctx context.Context, host string, tr transport.Transport) *doctor.Report {
	rep := &doctor.Report{Host: host}

	// 0. Transport surfacing (T8) + failure-mode honesty (T10), gated so the
	// default (system) doctor report stays byte-identical (T9). renderDoctor is
	// UNCHANGED — it just renders whatever Checks exist here. The daemon's
	// /v1/doctor closure and the daemon-down fallback both call runDoctor over a
	// factory-built transport, so Impl reflects the config selection on either
	// path. Warn is tolerated by Report.OK, so the note keeps RESULT at PASS.
	if d := tr.Describe(); d.Impl != transport.ImplSystemSSH {
		rep.Add("transport", doctor.Pass, string(d.Impl))
		if d.Impl == transport.ImplNativeSSH {
			rep.Add("forward lifetime", doctor.Warn,
				"native forwards die with the daemon (no ControlPersist analogue); a daemon restart re-establishes them")
		}
	}

	// 1. Master connectivity. Without the ControlMaster the daemon can't relay a
	// clip request at all; everything downstream is moot.
	//
	// Ensure BEFORE the Health gate, but ONLY for native. The two direct callers
	// (install self-test, daemon-down fallback) build a FRESH transport that has
	// never been brought up. For native that transport has not dialed, so Health
	// reports DOWN until Ensure connects — without this a healthy native box
	// would wrongly fail the master check, and a native dial is a cheap
	// in-process connect.
	//
	// For SYSTEM we must NOT Ensure here: sshctl.Ensure is not a passive
	// dial-check — it removes the stale socket and spawns a persistent
	// `ssh -fN -M -o ControlPersist=yes` ControlMaster whenever one isn't
	// running. Calling it from `portal doctor` when the daemon is down (fresh
	// boot, or right after `portal stop`) would flip the master check false-green,
	// hide that the relay daemon is not running, and leave an orphaned master the
	// user never asked for. The system path stays check-only so a daemon-down
	// doctor run still reports `DOWN — start the daemon`, preserving the
	// byte-compat diagnostic. When the daemon IS up the shared ControlMaster is
	// already running, so Health sees it without any build.
	if tr.Describe().Impl == transport.ImplNativeSSH {
		// A dial error is not fatal here; the Health gate below still renders
		// the DOWN line.
		_, _ = tr.Ensure(ctx)
	}
	if h, err := tr.Health(ctx); err != nil || !h.Up {
		rep.Add("ssh master", doctor.Fail, "DOWN — start the daemon: "+app.Tool+" start")
		// Without a master we cannot run any remote probe; bail with what we have.
		return rep
	} else {
		rep.Add("ssh master", doctor.Pass, fmt.Sprintf("UP (pid=%d)", h.Pid))
	}

	// 2. PATH-winner check — THE make-or-break. For each shim, resolve the tool
	// in a representative LOGIN+INTERACTIVE shell (so rc-file PATH edits are in
	// effect) and confirm the resolved path is OUR shim (carries the Marker).
	for _, tool := range []string{"xclip", "wl-paste"} {
		path, isShim := resolveShimWinner(ctx, tr, tool)
		switch {
		case path == "":
			// Neither a shim NOR a real binary resolves. The shim is missing from
			// ~/.local/bin (or ~/.local/bin isn't on PATH). For the headline image
			// path this is a FAIL; agents fall back to "no clipboard".
			rep.Add("PATH winner: "+tool, doctor.Fail,
				"no "+tool+" resolves — shim not installed or ~/.local/bin not on PATH")
		case isShim:
			rep.Add("PATH winner: "+tool, doctor.Pass, path+" (portal shim)")
		default:
			// A REAL binary wins PATH ahead of our shim — the whole feature is
			// silently dead for this tool. Loud FAIL with the cause.
			rep.Add("PATH winner: "+tool, doctor.Fail,
				path+" (real binary wins ahead of the shim) — re-run install to fix PATH order")
		}
	}

	// 3. Shim version vs embedded. A drifted shim still works but may lack
	// newer behavior (e.g. the notify hook); WARN rather than FAIL.
	if onDisk, ok := deployedShimVersion(ctx, tr); ok {
		if onDisk == clipshim.Version {
			rep.Add("shim version", doctor.Pass, "v"+onDisk+" (current)")
		} else {
			rep.Add("shim version", doctor.Warn,
				fmt.Sprintf("deployed=v%s embedded=v%s — re-run install to converge", onDisk, clipshim.Version))
		}
	} else {
		rep.Add("shim version", doctor.Warn, "could not read deployed shim version")
	}

	// 4. portald present + agent verb support. The shim relays through
	// ~/.cache/portal/portald; if it's missing the dangling-symlink window is in
	// effect and pastes fall through. We also confirm portald accepts the clip
	// and notify subcommands (verb support) by running the usage probe.
	portaldOK, verbs := probePortaldVerbs(ctx, tr)
	if portaldOK {
		rep.Add("portald binary", doctor.Pass, "~/.cache/portal/portald present + executable")
	} else {
		rep.Add("portald binary", doctor.Fail,
			"~/.cache/portal/portald missing — agent not uploaded yet (dangling-symlink window)")
	}
	if verbs.clip {
		rep.Add("agent verb: clip", doctor.Pass, "")
	} else if portaldOK {
		rep.Add("agent verb: clip", doctor.Warn, "portald does not advertise the clip subcommand (old agent?)")
	}
	if verbs.notify {
		rep.Add("agent verb: notify", doctor.Pass, "")
	} else if portaldOK {
		rep.Add("agent verb: notify", doctor.Warn, "portald does not advertise the notify subcommand (old agent?)")
	}

	// 5. End-to-end clip targets smoke. Only meaningful when the master is up and
	// portald is present; runs the SAME `portald clip targets` the shim runs.
	if portaldOK {
		out, code := smokeClipTargets(ctx, tr)
		switch {
		case code == 0 && out != "":
			// Something is on the Mac clipboard and was served — best case.
			rep.Add("smoke: clip targets", doctor.Pass, "Mac clipboard served ("+strings.TrimSpace(out)+")")
		case code != 0:
			// Exit 1 is the CORRECT, expected answer when nothing is on the Mac
			// clipboard (or the daemon's gate is off). The round trip itself
			// worked — the shim would cleanly fall through. WARN so the user can
			// copy something and re-run for a green line, but don't FAIL.
			rep.Add("smoke: clip targets", doctor.Warn,
				"round trip OK; nothing currently on the Mac clipboard to serve (copy an image/text and re-run)")
		default:
			rep.Add("smoke: clip targets", doctor.Warn, "unexpected empty success — copy something and re-run")
		}
	}

	return rep
}

// resolveShimWinner runs `command -v <tool>` in a representative login +
// interactive shell on the remote (so rc-file PATH edits are applied) and
// reports the resolved absolute path plus whether that file is OUR shim
// (recognized by the clipshim Marker). An empty path means nothing resolves.
//
// We force a login+interactive bash so the PATH-prepend block (written into
// ~/.bashrc / ~/.zshenv / ~/.profile by clipshim.ensurePathPrepend) is in
// effect — matching the environment a coding agent inherits — rather than the
// bare non-interactive ssh PATH which would not source those files.
func resolveShimWinner(ctx context.Context, tr transport.Transport, tool string) (path string, isShim bool) {
	// `bash -lic` = login + interactive so rc files (and the PATH block) load.
	// command -v prints the resolved path; we then grep the file for the Marker.
	script := fmt.Sprintf(
		`p=$(bash -lic 'command -v %s' 2>/dev/null | tail -1); `+
			`[ -n "$p" ] || { echo "NONE"; exit 0; }; `+
			`if grep -qF %q "$p" 2>/dev/null; then echo "SHIM $p"; else echo "REAL $p"; fi`,
		tool, clipshim.Marker,
	)
	out, _, err := tr.Exec(ctx, nil, "bash", "-c", doctorShellQuote(script))
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(out)
	switch {
	case line == "NONE" || line == "":
		return "", false
	case strings.HasPrefix(line, "SHIM "):
		return strings.TrimSpace(strings.TrimPrefix(line, "SHIM ")), true
	case strings.HasPrefix(line, "REAL "):
		return strings.TrimSpace(strings.TrimPrefix(line, "REAL ")), false
	default:
		return "", false
	}
}

// deployedShimVersion extracts the version number from the Marker line in the
// deployed ~/.local/bin/xclip shim. Returns ok=false if the shim is absent or
// carries no recognizable marker.
func deployedShimVersion(ctx context.Context, tr transport.Transport) (version string, ok bool) {
	// The Marker is "Installed by portal clip-shim v<N>"; grep it out of the
	// shim and print the trailing version token.
	const prefix = "Installed by portal clip-shim v"
	// Grep the marker line out of the deployed shim and strip everything up to
	// and including the prefix, leaving the version token (plus trailing text we
	// trim below).
	script := fmt.Sprintf(
		`line=$(grep -F %q ~/.local/bin/xclip 2>/dev/null | head -1); `+
			`[ -n "$line" ] || exit 0; `+
			`echo "${line##*%s}"`,
		prefix, prefix,
	)
	out, _, err := tr.Exec(ctx, nil, "bash", "-c", doctorShellQuote(script))
	if err != nil {
		return "", false
	}
	// The version is the leading token of the remaining suffix (e.g. "3. Inter…").
	v := strings.TrimSpace(out)
	if v == "" {
		return "", false
	}
	// Take the first whitespace/dot-delimited token.
	v = strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == '.' || r == '\t' })[0]
	if v == "" {
		return "", false
	}
	return v, true
}

// agentVerbs records which portald subcommands the deployed agent advertises.
type agentVerbs struct {
	clip   bool
	notify bool
}

// probePortaldVerbs confirms ~/.cache/portal/portald exists and is executable,
// and probes whether it advertises the clip and notify subcommands. We probe by
// invoking each subcommand with no/invalid args and grepping the usage line it
// prints (a present subcommand prints its own usage to stderr; an absent one
// falls through to the agent's flag parser). This avoids actually triggering a
// clip/notify relay.
func probePortaldVerbs(ctx context.Context, tr transport.Transport) (present bool, verbs agentVerbs) {
	script := `
pd="$HOME/.cache/portal/portald"
[ -x "$pd" ] || { echo "NO_PORTALD"; exit 0; }
echo "PORTALD_OK"
# ` + "`clip`" + ` with no subcommand prints its usage to stderr; capture it.
cu=$("$pd" clip 2>&1 1>/dev/null; true)
case "$cu" in *"usage: portald clip"*) echo "CLIP_OK";; esac
nu=$("$pd" notify 2>&1 1>/dev/null; true)
case "$nu" in *"usage: portald notify"*) echo "NOTIFY_OK";; esac
`
	out, _, err := tr.Exec(ctx, nil, "bash", "-c", doctorShellQuote(script))
	if err != nil {
		return false, verbs
	}
	if !strings.Contains(out, "PORTALD_OK") {
		return false, verbs
	}
	verbs.clip = strings.Contains(out, "CLIP_OK")
	verbs.notify = strings.Contains(out, "NOTIFY_OK")
	return true, verbs
}

// smokeClipTargets runs the exact `portald clip targets xclip` the shim runs and
// returns its stdout + exit code. Exit 0 with output == the Mac served a kind;
// exit 1 == nothing servable (the expected/clean fall-through). We run it via a
// wrapper that echoes the exit code so a non-zero exit (which Exec surfaces as
// an error) is captured as data rather than swallowed.
func smokeClipTargets(ctx context.Context, tr transport.Transport) (out string, code int) {
	script := `"$HOME/.cache/portal/portald" clip targets xclip; echo "EXIT=$?"`
	raw, _, _ := tr.Exec(ctx, nil, "bash", "-c", doctorShellQuote(script))
	// Split the captured EXIT marker off the tail.
	idx := strings.LastIndex(raw, "EXIT=")
	if idx < 0 {
		return strings.TrimSpace(raw), 1
	}
	body := raw[:idx]
	codeStr := strings.TrimSpace(strings.TrimPrefix(raw[idx:], "EXIT="))
	c := 1
	if codeStr == "0" {
		c = 0
	}
	return strings.TrimSpace(body), c
}

// doctorShellQuote wraps a script in single quotes for safe remote execution
// (mirrors install.go's shellQuoteRemote — kept local to avoid coupling).
func doctorShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
