package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"gitlab.i.extrahop.com/vikashl/devportal/internal/app"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/clipshim"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/doctor"
	"gitlab.i.extrahop.com/vikashl/devportal/internal/sshctl"
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
			host, _ := a.Cfg.ReadHost()
			if host == "" {
				return fmt.Errorf("no dev box configured — run: %s install <ssh-host>", app.Tool)
			}
			tr := sshctl.New(a.Paths.Sock, host, app.SSHOpts, a.Runner)
			rep := runDoctor(cmd.Context(), host, tr)
			renderDoctor(cmd.OutOrStdout(), rep)
			if !rep.OK() {
				return errSilent
			}
			return nil
		},
	}
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
func runDoctor(ctx context.Context, host string, tr sshctl.Transport) *doctor.Report {
	rep := &doctor.Report{Host: host}

	// 1. Master connectivity. Without the ControlMaster the daemon can't relay a
	// clip request at all; everything downstream is moot.
	if pid, err := tr.MasterPID(ctx); err != nil || pid == 0 {
		rep.Add("ssh master", doctor.Fail, "DOWN — start the daemon: "+app.Tool+" start")
		// Without a master we cannot run any remote probe; bail with what we have.
		return rep
	} else {
		rep.Add("ssh master", doctor.Pass, fmt.Sprintf("UP (pid=%d)", pid))
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
func resolveShimWinner(ctx context.Context, tr sshctl.Transport, tool string) (path string, isShim bool) {
	// `bash -lic` = login + interactive so rc files (and the PATH block) load.
	// command -v prints the resolved path; we then grep the file for the Marker.
	script := fmt.Sprintf(
		`p=$(bash -lic 'command -v %s' 2>/dev/null | tail -1); `+
			`[ -n "$p" ] || { echo "NONE"; exit 0; }; `+
			`if grep -qF %q "$p" 2>/dev/null; then echo "SHIM $p"; else echo "REAL $p"; fi`,
		tool, clipshim.Marker,
	)
	out, err := tr.Exec(ctx, "", "bash", "-c", doctorShellQuote(script))
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
func deployedShimVersion(ctx context.Context, tr sshctl.Transport) (version string, ok bool) {
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
	out, err := tr.Exec(ctx, "", "bash", "-c", doctorShellQuote(script))
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
func probePortaldVerbs(ctx context.Context, tr sshctl.Transport) (present bool, verbs agentVerbs) {
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
	out, err := tr.Exec(ctx, "", "bash", "-c", doctorShellQuote(script))
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
func smokeClipTargets(ctx context.Context, tr sshctl.Transport) (out string, code int) {
	script := `"$HOME/.cache/portal/portald" clip targets xclip; echo "EXIT=$?"`
	raw, _ := tr.Exec(ctx, "", "bash", "-c", doctorShellQuote(script))
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
