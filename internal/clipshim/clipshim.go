// Package clipshim deploys (and removes) portal's transparent dev-box shims:
// xdg-open, clipboard readers xclip/wl-paste, and the credential-facing portal,
// portal-askpass, and sudo wrappers. Shell rc blocks put ~/.local/bin first on
// PATH and select portal-askpass only while it is executable and the user has
// not configured another SUDO_ASKPASS. A coding agent's own Ctrl+V execs
// xclip/wl-paste; those shims relay the read to the Mac via `portald clip`,
// which serves the Mac clipboard over the existing portal connection (DESIGN
// §6).
//
// The deploy is idempotent and DAEMON-DRIVEN (DESIGN §9.1): both `portal
// install` (first run) and the agentclient reconnect loop call Ensure after the
// agent upload + HelloAck SHA match, so a Mac-binary upgrade that changes the
// embedded shim text re-converges WITHOUT a manual reinstall. The Version
// content marker makes the steady-state case a cheap grep.
//
// This logic lives in its own internal package (rather than cmd/portal) so the
// agentclient daemon loop — which cannot import the CLI main package — can call
// it too, sharing exactly one implementation with the CLI.
package clipshim

import (
	"context"
	"fmt"
	"strings"

	"github.com/VikashLoomba/Portal/pkg/transport"
)

// Version is the content-version marker embedded in every shim. Ensure
// re-deploys the shims whenever the marker text on disk differs from this — so
// an upgrade that changes the embedded script text converges on the next daemon
// reconnect without a manual reinstall (DESIGN §9.1). Bump this whenever any
// shim script below changes.
//
// v7 covers bash login and sshd-sourced non-interactive shells, and lets the
// sudo shim select portal-askpass when no startup file exported it.
const Version = "7"

// Marker is the exact string grep -qF searches for to decide whether the
// currently-deployed shim is already at Version (skip re-deploy).
const Marker = "Installed by portal clip-shim v" + Version

// ownershipMarker is the version-INDEPENDENT prefix carried by every shim
// marker ever shipped, including the legacy unversioned xdg-open wrapper.
// Backup and restore decisions key on it so a portal shim of ANY version is
// never mistaken for a user binary: keying them on the versioned Marker would
// make an upgrade copy the outgoing shim into an empty backup slot, and
// uninstall would then "restore" that stale shim instead of deleting it.
const ownershipMarker = "Installed by portal"

// XDGOpenWrapper is installed at ~/.local/bin/xdg-open. It first relays open
// requests through portald, then safely resolves a real xdg-open by treating
// PATH entries as data. It is exported for the fresh-install path; reconnect
// convergence uses the same script through the shims table below.
const XDGOpenWrapper = `#!/bin/sh
# ` + Marker + `. Relays xdg-open calls to the Mac client when a portal session
# is active; otherwise falls through to the real xdg-open.
_portald="${HOME}/.cache/portal/portald"
if [ -x "$_portald" ] && "$_portald" open "$@" 2>/dev/null; then
    exit 0
fi
_wrapper_dir=$(cd "$(dirname "$0")" && pwd)
_real=""
_oifs=$IFS; IFS=:
for _d in $PATH; do
    [ "$_d" = "$_wrapper_dir" ] && continue
    [ -n "$_d" ] || continue
    if [ -x "$_d/xdg-open" ]; then _real="$_d/xdg-open"; break; fi
done
IFS=$_oifs
if [ -z "$_real" ]; then
    exit 0
fi
exec "$_real" "$@"
exit 0
`

// xclipShim is installed at ~/.local/bin/xclip. It intercepts a coding agent's
// clipboard IMAGE and TEXT reads (and TARGETS probes) and relays them to the
// Mac via `portald clip`. Modeled on the xdg-open wrapper (DESIGN §6.2) and on
// cc-clip's xclip shim flag surface.
//
// Scope: TARGETS probes, image/png reads, and text reads (UTF8_STRING / TEXT /
// STRING / text/plain, plus the bare `-selection clipboard -o`). The Mac
// decides image-vs-text on a TARGETS probe and gates text behind its capability
// + concealed-clipboard skip (DESIGN §7.1); a disabled/concealed text read
// answers "none" and falls through here. An -t image/bmp (or any non-png image)
// request falls through rather than receiving PNG bytes mislabeled as another
// format (format honesty).
//
// Every interception is `… && exit 0`: a non-zero `portald clip` (no client,
// rejected, empty clipboard, dial failure — all of it) short-circuits to the
// real-binary fallback, so the agent never sees a spurious error and never
// hangs beyond portald clip's own deadline. Recursion is avoided by resolving
// the real xclip with a quoted IFS loop that treats PATH entries strictly as
// data, skips our own dir and empty entries, and rejects a logical-path alias
// of this wrapper. A headless box with no real xclip degrades to empty stdout =
// "no content", which is the correct answer.
const xclipShim = `#!/bin/sh
# ` + Marker + `. Intercepts clipboard IMAGE and TEXT reads for coding agents
# and relays them to the Mac via portald; falls through to the real xclip on
# clipboard writes, anything unrecognized, or any failure.
_portald="${HOME}/.cache/portal/portald"
_args="$*"
case "$_args" in
  *"-t TARGETS"*"-o"*)
    [ -x "$_portald" ] && "$_portald" clip targets xclip 2>/dev/null && exit 0 ;;
  *"-t image/png"*"-o"*)
    [ -x "$_portald" ] && "$_portald" clip image png 2>/dev/null && exit 0 ;;
  # image/bmp (and any non-png image): portal only serves PNG — do NOT hand
  # PNG bytes mislabeled as another type. Fall through so the agent's png
  # branch wins or it concludes no-image cleanly.
  *"-t UTF8_STRING"*-o*|*"-t TEXT"*-o*|*"-t STRING"*-o*|*"-t text/plain"*-o*|*"-selection clipboard -o"*|*"-o -selection clipboard"*)
    [ -x "$_portald" ] && "$_portald" clip text 2>/dev/null && exit 0 ;;
esac
# Fallback: inspect PATH entries as data, excluding our own dir and empty
# entries. Never feed PATH through a shell parser.
_wrapper_dir=$(cd "$(dirname "$0")" && pwd)
_real=""
_oifs=$IFS; IFS=:
for _d in $PATH; do
    [ "$_d" = "$_wrapper_dir" ] && continue
    [ -n "$_d" ] || continue
    if [ -x "$_d/xclip" ]; then _real="$_d/xclip"; break; fi
done
IFS=$_oifs
if [ -z "$_real" ]; then
    exit 0
fi
exec "$_real" "$@"
exit 0   # headless box, no real xclip: empty stdout = "no image" (correct degrade)
`

// wlPasteShim is installed at ~/.local/bin/wl-paste. wl-paste is opencode's
// PRIMARY image path (it tries `wl-paste -t image/png` BEFORE xclip), so this
// is in scope alongside xclip — same machinery (DESIGN §6.3), and it serves
// TEXT too (matching cc-clip's wl-paste shim). Patterns: `--list-types` →
// `clip targets`; `--type image/png` | `-t image/png` → `clip image png`;
// `--type text/` | `-t text/` | NO ARGS (bare `wl-paste` defaults to the most
// recent text offer) → `clip text`; non-png image still falls through.
//
// EMPTY-ARGS is the bare `wl-paste` form opencode/agents use to read text; we
// detect it by an empty $_args and route it to `clip text`. The Mac gates text
// behind its capability + concealed-clipboard skip, so a disabled/concealed
// read answers "none" and falls through to the real wl-paste here.
const wlPasteShim = `#!/bin/sh
# ` + Marker + `. Intercepts clipboard IMAGE and TEXT reads for coding agents
# and relays them to the Mac via portald; falls through to the real wl-paste on
# clipboard writes (--clear), anything unrecognized, or any failure.
_portald="${HOME}/.cache/portal/portald"
_args="$*"
case "$_args" in
  *"--list-types"*)
    [ -x "$_portald" ] && "$_portald" clip targets wl-paste 2>/dev/null && exit 0 ;;
  *"--type image/png"*|*"-t image/png"*)
    [ -x "$_portald" ] && "$_portald" clip image png 2>/dev/null && exit 0 ;;
  # non-png image: fall through to the real wl-paste.
  *"--type image/"*|*"-t image/"*) : ;;
  *"--type text/"*|*"-t text/"*|"")
    [ -x "$_portald" ] && "$_portald" clip text 2>/dev/null && exit 0 ;;
esac
_wrapper_dir=$(cd "$(dirname "$0")" && pwd)
_real=""
_oifs=$IFS; IFS=:
for _d in $PATH; do
    [ "$_d" = "$_wrapper_dir" ] && continue
    [ -n "$_d" ] || continue
    if [ -x "$_d/wl-paste" ]; then _real="$_d/wl-paste"; break; fi
done
IFS=$_oifs
if [ -z "$_real" ]; then
    exit 0
fi
exec "$_real" "$@"
exit 0   # headless box, no real wl-paste: empty stdout = "no image" (correct degrade)
`

// NotifyHookMarker is the portal-ownership marker on the Claude Code hook
// command line in settings.json AND on the notify-hook script. It mirrors
// cc-clip's CC_CLIP_MANAGED=1 prefix: the settings.json merge strips any hook
// entry carrying this marker before re-adding ours, so user-authored bare hooks
// are preserved and our entry stays idempotent. Stable across versions (the
// strip/install logic keys on it), unlike Version which gates re-deploy.
const NotifyHookMarker = "PORTAL_MANAGED=1"

// notifyHookScript is installed at ~/.local/bin/portal-notify-hook. A Claude
// Code Stop/Notification hook invokes it; it reads the hook JSON on stdin and
// pipes it to `portald notify --hook`, which classifies it and relays it to the
// connected Mac (verified). It falls through silently (exit 0) when no portal
// session is active so it never blocks the coding agent — a hook that exits
// non-zero can surface errors in Claude Code, so we always exit 0.
const notifyHookScript = `#!/bin/sh
# ` + Marker + `. Claude Code Stop/Notification hook. Reads the hook JSON on
# stdin and relays it to the connected Mac via portald notify --hook; exits 0
# regardless so a missing portal session never blocks the coding agent.
_portald="${HOME}/.cache/portal/portald"
if [ -x "$_portald" ]; then
    "$_portald" notify --hook 2>/dev/null || true
fi
exit 0
`

// claudeSettingsPath is the Claude Code user settings file the hook is merged
// into. The merge adds Stop and Notification hook entries (matcher "") whose
// command runs the notify-hook script with the PORTAL_MANAGED marker.
const claudeSettingsPath = "~/.claude/settings.json"

// shims is the table the deploy/verify loop iterates. name is the basename at
// ~/.local/bin; script is the /bin/sh source.
var shims = []struct {
	name   string
	script string
}{
	{"xdg-open", XDGOpenWrapper},
	{"xclip", xclipShim},
	{"wl-paste", wlPasteShim},
	{"portal", portalShim},
	{"portal-askpass", portalAskpassShim},
	{"sudo", sudoShim},
}

// PathMarkerStart/PathMarkerEnd delimit the portal PATH-prepend block written
// at the bottom of shell rc files. The block is removed on uninstall by
// matching these markers, so they MUST stay stable across versions.
const (
	PathMarkerStart = "# >>> portal PATH (clip shims) >>>"
	PathMarkerEnd   = "# <<< portal PATH (clip shims) <<<"
)

// EarlyPathMarkerStart/EarlyPathMarkerEnd delimit the PATH block written at
// the top of ~/.bashrc for sshd-sourced non-interactive bash. These markers
// are shipped state and MUST stay stable across versions.
const (
	EarlyPathMarkerStart = "# >>> portal PATH early (non-interactive) >>>"
	EarlyPathMarkerEnd   = "# <<< portal PATH early (non-interactive) <<<"
)

// pathPrependSnippet is the marker block injected into each shell rc/profile.
// It is a DEDUP prepend (DESIGN §9.2): it removes any existing ~/.local/bin
// occurrence from PATH and re-adds it at the FRONT, so the shim wins even on a
// box that already has /usr/bin/xclip with ~/.local/bin later on PATH. PATH
// ordering is the single make-or-break for the whole feature. We inject into
// ~/.bashrc, ~/.zshrc, ~/.zshenv and ~/.profile (not just one) because tool
// managers (nvm/asdf/mise/conda) re-export PATH later. Existing
// ~/.bash_profile and ~/.bash_login files receive it too so bash login shells
// that select either file do not bypass the shims.
const pathPrependSnippet = PathMarkerStart + `
# Ensures portal's shims (~/.local/bin/xdg-open, xclip, wl-paste, portal,
# portal-askpass, sudo) win on PATH.
PATH="$HOME/.local/bin:$(printf '%s' "$PATH" | tr ':' '\n' | grep -vxF "$HOME/.local/bin" | paste -sd: -)"
export PATH
` + PathMarkerEnd

// earlyPathPrependSnippet carries the same dedup-prepend as the bottom block,
// but is placed before Debian and Ubuntu's interactive-shell guard. The bottom
// block remains necessary so portal re-wins after interactive PATH managers.
const earlyPathPrependSnippet = EarlyPathMarkerStart + `
# Placed above the distro interactive guard so sshd-sourced non-interactive
# bash gets the shims; the bottom portal PATH block re-wins interactively.
PATH="$HOME/.local/bin:$(printf '%s' "$PATH" | tr ':' '\n' | grep -vxF "$HOME/.local/bin" | paste -sd: -)"
export PATH
` + EarlyPathMarkerEnd

// rcFiles is the set of shell startup files we create when missing while
// managing the PATH and SUDO_ASKPASS blocks.
var rcFiles = []string{"~/.bashrc", "~/.zshrc", "~/.zshenv", "~/.profile"}

// conditionalRCFiles receive both bottom blocks only when already present.
// Creating either would make bash ignore ~/.profile in login shells.
var conditionalRCFiles = []string{"~/.bash_profile", "~/.bash_login"}

// Ensure deploys all versioned shims plus the PATH-prepend and SUDO_ASKPASS
// blocks to the dev box over tr, idempotently. It is invoked from `portal
// install` (first run) and from the agentclient reconnect loop after
// EnsureUploaded + a HelloAck SHA match. The Version content marker makes the
// steady-state case a cheap grep (no rewrite when already current).
//
// For each shim it backs up a pre-existing non-shim binary preserving type
// (cp -P), atomically writes the shim 0755, then verifies the marker landed.
// For each rc file it converges each applicable marker block exactly once.
// Returns an error describing the FIRST failure so the caller can surface it
// loudly (DESIGN §9.6).
func Ensure(ctx context.Context, tr transport.Transport) error {
	// Fast path: if every versioned shim carries the current marker, only
	// ensure the environment blocks (cheap, idempotent). Steady state on every
	// reconnect.
	check := currentShimsProbe()
	out, _, _ := tr.Exec(ctx, nil, "bash", "-c", shellQuote(check))
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(out) != "current" {
		for _, sh := range shims {
			if err := deployShim(ctx, tr, sh.name, sh.script); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
		}
	}
	// Deploy the notification hook (script + Claude Code settings.json merge).
	// Best-effort: a failed notify-hook deploy must NOT fail the whole Ensure
	// (which would also block the clip-shim PATH convergence below) — the
	// headline clip feature and port forwarding take priority over notifications.
	// Failure is logged by the caller via the returned error only when it is the
	// FIRST failure; here we swallow it so PATH-prepend still runs.
	_ = ensureNotifyHook(ctx, tr)
	if err := ctx.Err(); err != nil {
		return err
	}

	// Shell marker blocks converge even on the fast path so a user who deleted
	// one receives it again without forcing a shim rewrite.
	if err := ensureEarlyPathPrepend(ctx, tr); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensurePathPrepend(ctx, tr); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ensureAskpassEnv(ctx, tr); err != nil {
		return err
	}
	return ctx.Err()
}

// currentShimsProbe returns the remote marker check for every entry in shims.
// Deriving it from the deployment table keeps the fast path from overlooking a
// newly-added shim and incorrectly treating a partial installation as current.
func currentShimsProbe() string {
	checks := make([]string, 0, len(shims))
	for _, sh := range shims {
		checks = append(checks, fmt.Sprintf(`grep -qF %q ~/.local/bin/%s 2>/dev/null`, Marker, sh.name))
	}
	return strings.Join(checks, " && ") + " && echo current || echo stale"
}

// ensureNotifyHook deploys the notify-hook script to ~/.local/bin and merges
// the PORTAL_MANAGED Stop/Notification hook entries into Claude Code's
// settings.json. The settings.json merge is done with python3 (present on
// essentially every dev box with Claude Code) because robustly editing JSON in
// pure /bin/sh is error-prone; if python3 is absent the script deploy still
// happens and the merge is skipped (the hook simply won't be wired until a
// python3-capable box, a graceful degrade rather than a corrupt settings file).
//
// The merge is idempotent: it strips any existing hook entry whose command
// carries NotifyHookMarker (ours, from a prior deploy) before re-adding exactly
// one entry per event, preserving any user-authored hooks (which lack the
// marker). This mirrors cc-clip's CC_CLIP_MANAGED ownership tracking.
func ensureNotifyHook(ctx context.Context, tr transport.Transport) error {
	// 1. Write the hook script atomically (same pattern as deployShim).
	bin := "~/.local/bin/portal-notify-hook"
	writeScript := fmt.Sprintf(
		`mkdir -p ~/.local/bin && cat > %s.portal.tmp && chmod 0755 %s.portal.tmp && mv %s.portal.tmp %s`,
		bin, bin, bin, bin,
	)
	if _, _, err := tr.Exec(ctx, []byte(notifyHookScript), "bash", "-c", shellQuote(writeScript)); err != nil {
		return fmt.Errorf("write notify hook script: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// 2. Merge the Stop/Notification hook entries into Claude Code settings.json.
	// The python3 program reads the existing settings (if any), drops our prior
	// managed entries, appends one fresh entry per event, and writes it back
	// atomically. The command line carries NotifyHookMarker so the strip step
	// recognizes our own entries; the actual command runs the script above.
	merge := mergeClaudeSettingsProgram()
	if _, _, err := tr.Exec(ctx, nil, "bash", "-c", shellQuote(merge)); err != nil {
		return fmt.Errorf("merge claude settings: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// mergeClaudeSettingsProgram returns the bash command that merges the portal
// notification hooks into Claude Code's settings.json via python3. It is a
// no-op (graceful skip) when python3 is unavailable.
func mergeClaudeSettingsProgram() string {
	// The hook command: run the notify-hook script under the PORTAL_MANAGED env
	// marker so the strip step can recognize and replace our own entries.
	hookCmd := "env " + NotifyHookMarker + " ~/.local/bin/portal-notify-hook"
	py := fmt.Sprintf(`import json,os,sys
p=os.path.expanduser("~/.claude/settings.json")
os.makedirs(os.path.dirname(p),exist_ok=True)
try:
    d=json.load(open(p))
    if not isinstance(d,dict): d={}
except Exception:
    d={}
hooks=d.get("hooks")
if not isinstance(hooks,dict): hooks={}
cmd=%q
marker=%q
for ev in ("Stop","Notification"):
    arr=hooks.get(ev)
    if not isinstance(arr,list): arr=[]
    kept=[]
    for m in arr:
        # Drop any prior portal-managed matcher (recognized by our marker on the
        # command), preserve everything else (user-authored hooks).
        if isinstance(m,dict):
            drop=False
            for h in (m.get("hooks") or []):
                if isinstance(h,dict) and marker in str(h.get("command","")):
                    drop=True
            if drop: continue
        kept.append(m)
    kept.append({"matcher":"","hooks":[{"type":"command","command":cmd}]})
    hooks[ev]=kept
d["hooks"]=hooks
tmp=p+".portal.tmp"
open(tmp,"w").write(json.dumps(d,indent=2))
os.replace(tmp,p)
`, hookCmd, NotifyHookMarker)
	// Run only if python3 exists; otherwise skip silently (graceful degrade).
	// The python program is fed on stdin (not a heredoc) so it survives the
	// single-quote shellQuote wrapping cleanly, and the whole thing is guarded
	// by a python3 presence check.
	return "if command -v python3 >/dev/null 2>&1; then python3 - <<'PORTAL_PY'\n" + py + "PORTAL_PY\nfi"
}

// deployShim backs up a pre-existing non-shim binary at ~/.local/bin/<name>
// (preserving type via cp -P so a symlink stays a symlink — DESIGN §9.3), writes
// our shim atomically at 0755, and verifies the marker landed.
func deployShim(ctx context.Context, tr transport.Transport, name, script string) error {
	bin := "~/.local/bin/" + name
	backup := bin + ".portal-backup"
	// Back up only a pre-existing file that is NOT a portal shim of any
	// version (ownershipMarker), and only if no backup exists yet (so repeated
	// installs and upgrades never clobber the original with our own shim —
	// DESIGN §9.3). cp -P preserves a symlink as a symlink.
	backupScript := fmt.Sprintf(
		`if [ -e %s ] && ! grep -qF %q %s 2>/dev/null && [ ! -e %s ]; then cp -P %s %s; fi`,
		bin, ownershipMarker, bin, backup, bin, backup,
	)
	_, _, _ = tr.Exec(ctx, nil, "bash", "-c", shellQuote(backupScript))
	if err := ctx.Err(); err != nil {
		return err
	}

	// Atomic write: cat to a unique .tmp, chmod 0755, mv into place.
	writeScript := fmt.Sprintf(
		`mkdir -p ~/.local/bin && cat > %s.portal.tmp && chmod 0755 %s.portal.tmp && mv %s.portal.tmp %s`,
		bin, bin, bin, bin,
	)
	if _, _, err := tr.Exec(ctx, []byte(script), "bash", "-c", shellQuote(writeScript)); err != nil {
		return fmt.Errorf("write %s shim: %w", name, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	verifyScript := fmt.Sprintf(`grep -qF %q %s 2>/dev/null && echo ok || echo missing`, Marker, bin)
	out, _, _ := tr.Exec(ctx, nil, "bash", "-c", shellQuote(verifyScript))
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(out) != "ok" {
		return fmt.Errorf("%s shim not found at %s after write — check the upload", name, bin)
	}
	return nil
}

// ensureEarlyPathPrepend puts the non-interactive PATH block at the top of
// ~/.bashrc exactly once. The truncate-write keeps the existing file's inode
// and permissions; ~/.bashrc is created when absent like the bottom blocks.
func ensureEarlyPathPrepend(ctx context.Context, tr transport.Transport) error {
	script := fmt.Sprintf(`block=$(cat)
rc=~/.bashrc
if [ -f "$rc" ] && grep -qF %q "$rc"; then
    exit 0
fi
touch "$rc" || exit 1
tmp=$(mktemp) || exit 1
if printf '%%s\n\n' "$block" > "$tmp" &&
    cat "$rc" >> "$tmp" &&
    cat "$tmp" > "$rc" &&
    rm -f "$tmp"
then
    exit 0
fi
rm -f "$tmp"
exit 1`, EarlyPathMarkerStart)
	if _, _, err := tr.Exec(ctx, []byte(earlyPathPrependSnippet), "bash", "-c", shellQuote(script)); err != nil {
		return fmt.Errorf("write early PATH-prepend block: %w", err)
	}
	return nil
}

// ensurePathPrepend appends the bottom PATH block exactly once. The standard
// rc files are created when missing; bash login alternatives are touched only
// when already present so they never begin shadowing ~/.profile.
func ensurePathPrepend(ctx context.Context, tr transport.Transport) error {
	// The block text is passed on stdin so its characters need no further shell
	// quoting; each loop appends it to files missing the start marker.
	rcList := strings.Join(rcFiles, " ")
	conditionalRCList := strings.Join(conditionalRCFiles, " ")
	script := fmt.Sprintf(`block=$(cat); for rc in %s; do
    if [ -f "$rc" ] && grep -qF %q "$rc"; then continue; fi
    printf '\n%%s\n' "$block" >> "$rc"
done
for rc in %s; do
    [ -f "$rc" ] || continue
    if grep -qF %q "$rc"; then continue; fi
    printf '\n%%s\n' "$block" >> "$rc"
done`, rcList, PathMarkerStart, conditionalRCList, PathMarkerStart)
	if _, _, err := tr.Exec(ctx, []byte(pathPrependSnippet), "bash", "-c", shellQuote(script)); err != nil {
		return fmt.Errorf("write PATH-prepend block: %w", err)
	}
	return nil
}

// Remove deletes everything portal deploys to the dev box's ~/.local/bin and
// shell rc files: the xdg-open wrapper; the xclip, wl-paste, portal,
// portal-askpass, and sudo shims; the portald symlink; the env snippet; and all
// three shell marker blocks. It restores any pre-existing binaries backed up
// at install (preserving type via `mv`, which keeps a backed-up symlink a
// symlink) and never touches /usr/bin (DESIGN §9.3/§9.4).
//
// Each rc-file edit strips the env.sh source line and all marker blocks
// (start..end inclusive) with awk range deletes keyed on the stable markers.
// The truncate-write preserves the rc file's inode and mode. Best-effort:
// errors are ignored (uninstall continues regardless).
func Remove(ctx context.Context, tr transport.Transport) {
	script := fmt.Sprintf(`
# Restore each ~/.local/bin entry from a GENUINE user backup, preserving
# symlink type via mv. A backup carrying the portal ownership marker is our
# own shim (copied there by an older release's versioned backup grep): delete
# it with the shim so uninstall never resurrects a stale portal shim.
for bin in xdg-open xclip wl-paste portal portal-askpass sudo; do
    if [ -e ~/.local/bin/"$bin".portal-backup ] && ! grep -qF %[7]q ~/.local/bin/"$bin".portal-backup 2>/dev/null; then
        mv ~/.local/bin/"$bin".portal-backup ~/.local/bin/"$bin"
    else
        rm -f ~/.local/bin/"$bin".portal-backup ~/.local/bin/"$bin"
    fi
done
rm -f ~/.local/bin/portal-notify-hook
rm -f ~/.cache/portal/portald
rm -f ~/.config/portal/env.sh
# Strip the portal-managed Stop/Notification hook entries from Claude Code's
# settings.json (recognized by the PORTAL_MANAGED marker on the command),
# preserving any user-authored hooks. python3 only; skipped if absent.
if command -v python3 >/dev/null 2>&1; then python3 - <<'PORTAL_PY'
import json,os
p=os.path.expanduser("~/.claude/settings.json")
try:
    d=json.load(open(p))
    if not isinstance(d,dict): raise ValueError
except Exception:
    d=None
if isinstance(d,dict) and isinstance(d.get("hooks"),dict):
    hooks=d["hooks"]
    for ev in ("Stop","Notification"):
        arr=hooks.get(ev)
        if not isinstance(arr,list): continue
        kept=[]
        for m in arr:
            drop=False
            if isinstance(m,dict):
                for h in (m.get("hooks") or []):
                    if isinstance(h,dict) and "PORTAL_MANAGED=1" in str(h.get("command","")):
                        drop=True
            if not drop: kept.append(m)
        if kept: hooks[ev]=kept
        else: hooks.pop(ev,None)
    if not hooks: d.pop("hooks",None)
    tmp=p+".portal.tmp"
    open(tmp,"w").write(json.dumps(d,indent=2))
    os.replace(tmp,p)
PORTAL_PY
fi
# Strip the env.sh source line and all portal marker blocks from each rc.
for rc in ~/.bashrc ~/.zshrc ~/.zshenv ~/.profile ~/.bash_profile ~/.bash_login; do
    [ -f "$rc" ] || continue
    tmp=$(mktemp) || continue
    awk '
        index($0, %[1]q) { early_path_skip=1 }
        early_path_skip && index($0, %[2]q) { early_path_skip=0; next }
        early_path_skip { next }
        index($0, %[3]q) { path_skip=1 }
        path_skip && index($0, %[4]q) { path_skip=0; next }
        path_skip { next }
        index($0, %[5]q) { askpass_skip=1 }
        askpass_skip && index($0, %[6]q) { askpass_skip=0; next }
        askpass_skip { next }
        index($0, "portal/env.sh") { next }
        { print }
    ' "$rc" > "$tmp" && cat "$tmp" > "$rc"
    rm -f "$tmp"
done`, EarlyPathMarkerStart, EarlyPathMarkerEnd, PathMarkerStart, PathMarkerEnd, AskpassMarkerStart, AskpassMarkerEnd, ownershipMarker)
	_, _, _ = tr.Exec(ctx, nil, "bash", "-c", shellQuote(script))
}

// shellQuote wraps a shell script in single quotes for safe remote execution
// via ssh (which joins argv with spaces and runs the result through sh -c).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
