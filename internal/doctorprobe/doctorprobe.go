// Package doctorprobe runs portal's remote clipboard and notification checks.
package doctorprobe

import (
	"context"
	"fmt"
	"strings"

	"github.com/VikashLoomba/Portal/internal/app"
	"github.com/VikashLoomba/Portal/internal/clipshim"
	"github.com/VikashLoomba/Portal/pkg/doctor"
	"github.com/VikashLoomba/Portal/pkg/transport"
)

// Run performs every remote doctor probe over tr and returns the assembled report.
func Run(ctx context.Context, host string, tr transport.Transport) *doctor.Report {
	rep := &doctor.Report{Host: host}
	if ctx.Err() != nil {
		return rep
	}

	if d := tr.Describe(); d.Impl != transport.ImplSystemSSH {
		rep.Add("transport", doctor.Pass, string(d.Impl))
		if d.Impl == transport.ImplNativeSSH {
			rep.Add("forward lifetime", doctor.Warn,
				"native forwards die with the daemon (no ControlPersist analogue); a daemon restart re-establishes them")
		}
	}

	// A fresh native transport has no connection until Ensure dials. System
	// doctor probes stay passive so a stopped daemon cannot spawn an orphaned
	// ControlMaster and turn the health check false-green.
	if tr.Describe().Impl == transport.ImplNativeSSH {
		_, _ = tr.Ensure(ctx)
		if ctx.Err() != nil {
			return rep
		}
	}
	if h, err := tr.Health(ctx); err != nil || !h.Up {
		rep.Add("ssh master", doctor.Fail, "DOWN — start the daemon: "+app.Tool+" start")
		return rep
	} else {
		rep.Add("ssh master", doctor.Pass, fmt.Sprintf("UP (pid=%d)", h.Pid))
	}
	if ctx.Err() != nil {
		return rep
	}

	for _, tool := range []string{"xclip", "wl-paste"} {
		path, isShim := resolveShimWinner(ctx, tr, tool)
		switch {
		case path == "":
			rep.Add("PATH winner: "+tool, doctor.Fail,
				"no "+tool+" resolves — shim not installed or ~/.local/bin not on PATH")
		case isShim:
			rep.Add("PATH winner: "+tool, doctor.Pass, path+" (portal shim)")
		default:
			rep.Add("PATH winner: "+tool, doctor.Fail,
				path+" (real binary wins ahead of the shim) — re-run install to fix PATH order")
		}
		if ctx.Err() != nil {
			return rep
		}
	}

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
	if ctx.Err() != nil {
		return rep
	}

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
	if ctx.Err() != nil {
		return rep
	}

	if portaldOK {
		out, code := smokeClipTargets(ctx, tr)
		switch {
		case code == 0 && out != "":
			rep.Add("smoke: clip targets", doctor.Pass, "Mac clipboard served ("+strings.TrimSpace(out)+")")
		case code != 0:
			rep.Add("smoke: clip targets", doctor.Warn,
				"round trip OK; nothing currently on the Mac clipboard to serve (copy an image/text and re-run)")
		default:
			rep.Add("smoke: clip targets", doctor.Warn, "unexpected empty success — copy something and re-run")
		}
	}

	return rep
}

func resolveShimWinner(ctx context.Context, tr transport.Transport, tool string) (path string, isShim bool) {
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

func deployedShimVersion(ctx context.Context, tr transport.Transport) (version string, ok bool) {
	const prefix = "Installed by portal clip-shim v"
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
	v := strings.TrimSpace(out)
	if v == "" {
		return "", false
	}
	v = strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == '.' || r == '\t' })[0]
	if v == "" {
		return "", false
	}
	return v, true
}

type agentVerbs struct {
	clip   bool
	notify bool
}

func probePortaldVerbs(ctx context.Context, tr transport.Transport) (present bool, verbs agentVerbs) {
	script := `
pd="$HOME/.cache/portal/portald"
[ -x "$pd" ] || { echo "NO_PORTALD"; exit 0; }
echo "PORTALD_OK"
cu=$("$pd" clip 2>&1 1>/dev/null; true)
case "$cu" in *"usage: portald clip"*) echo "CLIP_OK";; esac
nu=$("$pd" notify 2>&1 1>/dev/null; true)
case "$nu" in *"usage: portald notify"*) echo "NOTIFY_OK";; esac
`
	out, _, err := tr.Exec(ctx, nil, "bash", "-c", doctorShellQuote(script))
	if err != nil || !strings.Contains(out, "PORTALD_OK") {
		return false, verbs
	}
	verbs.clip = strings.Contains(out, "CLIP_OK")
	verbs.notify = strings.Contains(out, "NOTIFY_OK")
	return true, verbs
}

func smokeClipTargets(ctx context.Context, tr transport.Transport) (out string, code int) {
	script := `"$HOME/.cache/portal/portald" clip targets xclip; echo "EXIT=$?"`
	raw, _, _ := tr.Exec(ctx, nil, "bash", "-c", doctorShellQuote(script))
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

func doctorShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
