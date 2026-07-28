// Package clipshim deploys (and removes) portal's transparent dev-box shims:
// xdg-open; clipboard readers and writers xclip/wl-paste/wl-copy/pbcopy/
// pbpaste/xsel; and the credential-facing portal, portal-askpass, and sudo
// wrappers. Shell rc blocks put ~/.local/bin first on PATH and select
// portal-askpass only while it is executable and the user has not configured
// another SUDO_ASKPASS. Clipboard shims relay reads from and writes to the Mac
// via `portald clip` over the existing portal connection (DESIGN §6).
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
// v8 adds conservative clipboard-write argv parsing and the wl-copy, pbcopy,
// pbpaste, and xsel shims without changing the read-side degrade.
const Version = "8"

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

const clipWriteFailMsg = "portal: clipboard write failed (no Mac client connected)"

// clipWriteRelay preserves stdin for a real-binary fallback. Clipboard bytes
// touch disk only when such a fallback exists, and the 0600 file is opened
// before unlink so the exec'd binary receives the original byte stream.
const clipWriteRelay = `_relay_stdin() {
    [ -x "$_portald" ] || return 1
    if [ -z "$_real" ]; then
        _copy 2>/dev/null && exit 0
        return 1
    fi
    _tmp=$(mktemp "${TMPDIR:-/tmp}/portal-clip.XXXXXX" 2>/dev/null) || return 1
    trap 'rm -f "$_tmp"' EXIT HUP INT TERM
    if ! cat > "$_tmp"; then
        rm -f "$_tmp"
        printf '%s\n' 'portal: clipboard write failed (cannot buffer input)' >&2
        exit 1
    fi
    if _copy < "$_tmp" 2>/dev/null; then
        rm -f "$_tmp"
        exit 0
    fi
    exec 0< "$_tmp"
    rm -f "$_tmp"
    return 1
}
_relay_argv() {
    [ -x "$_portald" ] || return 1
    printf '%s' "$_text" | _copy 2>/dev/null && exit 0
    return 1
}
_relay_noinput() {
    [ -x "$_portald" ] || return 1
    _copy < /dev/null 2>/dev/null && exit 0
    return 1
}
`

const clipWriteTail = `if [ -n "$_real" ]; then exec "$_real" "$@"; fi
if [ "$_mode" = write ]; then
    printf '%s\n' '` + clipWriteFailMsg + `' >&2
    exit 1
fi
exit 0
`

// xclipShim parses only enumerated legal prefixes of xclip's canonical
// options. Unknown or ambiguous tokens permanently disable interception, while
// later mode flags still select the correct read/write failure behavior.
const xclipShim = `#!/bin/sh
# ` + Marker + `. Intercepts clipboard reads and writes for coding agents
# and relays them to the Mac via portald; unrecognized forms fall through.
_portald="${HOME}/.cache/portal/portald"
_mode=write
_sel=primary
_target=""
_trim=0
_ok=1
_want=""
for _a in "$@"; do
    if [ -n "$_want" ]; then
        case "$_want" in
          sel) _sel=$_a ;;
          target) _target=$_a ;;
          skip) : ;;
        esac
        _want=""
        continue
    fi
    case "$_a" in
      -*=*) _ok=0 ;;
      -o|-ou|-out) _mode=read ;;
      -i|-in) _mode=write ;;
      -se|-sel|-sele|-selec|-select|-selecti|-selectio|-selection) _want=sel ;;
      -t|-ta|-tar|-targ|-targe|-target) _want=target ;;
      -r|-rm|-rml|-rmla|-rmlas|-rmlast|-rmlastn|-rmlastnl) _trim=1 ;;
      -d|-di|-dis|-disp|-displ|-displa|-display) _want=skip ;;
      -l|-lo|-loo|-loop|-loops) _want=skip ;;
      -n|-no|-nou|-nout|-noutf|-noutf8) : ;;
      -q|-qu|-qui|-quie|-quiet) : ;;
      -si|-sil|-sile|-silen|-silent) : ;;
      -verb|-verbo|-verbos|-verbose) : ;;
      -h|-he|-hel|-help|-vers|-versi|-versio|-version) _mode=info; _ok=0 ;;
      *) _ok=0 ;;
    esac
done
[ -z "$_want" ] || _ok=0
if [ "$_ok" = 1 ]; then
    _sl=$(printf '%s' "$_sel" | tr 'ABCDEFGHIJKLMNOPQRSTUVWXYZ' 'abcdefghijklmnopqrstuvwxyz' 2>/dev/null)
    case "$_sl" in
      p|pr|pri|prim|prima|primar|primary) : ;;
      s|se|sec|seco|secon|second|seconda|secondar|secondary) : ;;
      c|cl|cli|clip|clipb|clipbo|clipboa|clipboar|clipboard) : ;;
      *) _ok=0 ;;
    esac
fi
_wrapper_dir=$(cd "$(dirname "$0")" && pwd)
_real=""
_oifs=$IFS; IFS=:
for _d in $PATH; do
    [ "$_d" = "$_wrapper_dir" ] && continue
    [ -n "$_d" ] || continue
    if [ -x "$_d/xclip" ]; then _real="$_d/xclip"; break; fi
done
IFS=$_oifs
` + clipWriteRelay + `if [ "$_ok" = 1 ]; then
    case "$_mode:$_target" in
      read:TARGETS)
        [ -x "$_portald" ] && "$_portald" clip targets xclip 2>/dev/null && exit 0 ;;
      read:image/png)
        [ -x "$_portald" ] && "$_portald" clip image png 2>/dev/null && exit 0 ;;
      read:image/*) : ;;
      read:|read:UTF8_STRING|read:TEXT|read:STRING|read:text/*)
        [ -x "$_portald" ] && "$_portald" clip text 2>/dev/null && exit 0 ;;
      write:image/png)
        _copy() { "$_portald" clip copy image png; }
        _relay_stdin ;;
      write:image/*) : ;;
      write:|write:UTF8_STRING|write:TEXT|write:STRING|write:text/*)
        _copy() {
            if [ "$_trim" = 1 ]; then
                "$_portald" clip copy text --trim
            else
                "$_portald" clip copy text
            fi
        }
        _relay_stdin ;;
    esac
fi
` + clipWriteTail

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

// wlCopyShim implements wl-copy's write-only surface. Positional presence is
// tracked separately from content so empty argv elements remain part of the
// space-joined payload instead of being mistaken for absent argv.
const wlCopyShim = `#!/bin/sh
# ` + Marker + `. Intercepts wl-copy clipboard writes and relays them to the Mac
# via portald; unrecognized forms fall through to the real wl-copy.
_portald="${HOME}/.cache/portal/portald"
_mode=write
_type=""
_trim=0
_clear=0
_ok=1
_want=""
_endopts=0
_text=""
_has_text=0
for _a in "$@"; do
    if [ -n "$_want" ]; then
        case "$_want" in
          type) _type=$_a ;;
          skip) : ;;
        esac
        _want=""
        continue
    fi
    if [ "$_endopts" = 1 ]; then
        if [ "$_has_text" = 1 ]; then _text="$_text $_a"; else _text=$_a; fi
        _has_text=1
        continue
    fi
    case "$_a" in
      --) _endopts=1 ;;
      --type=*) _type=${_a#--type=} ;;
      -t|--type) _want=type ;;
      -s|--seat) _want=skip ;;
      --seat=*) : ;;
      -n|--trim-newline) _trim=1 ;;
      -c|--clear) _clear=1 ;;
      -p|--primary|-o|--paste-once|-f|--foreground) : ;;
      -h|--help|-v|--version) _mode=info; _ok=0 ;;
      --*) _ok=0 ;;
      -) _ok=0 ;;
      -?*)
        case "${_a#-}" in *[!pnocf]*) _ok=0 ;; esac
        case "${_a#-}" in *n*) _trim=1 ;; esac
        case "${_a#-}" in *c*) _clear=1 ;; esac ;;
      *)
        if [ "$_has_text" = 1 ]; then _text="$_text $_a"; else _text=$_a; fi
        _has_text=1 ;;
    esac
done
[ -z "$_want" ] || _ok=0
_wrapper_dir=$(cd "$(dirname "$0")" && pwd)
_real=""
_oifs=$IFS; IFS=:
for _d in $PATH; do
    [ "$_d" = "$_wrapper_dir" ] && continue
    [ -n "$_d" ] || continue
    if [ -x "$_d/wl-copy" ]; then _real="$_d/wl-copy"; break; fi
done
IFS=$_oifs
` + clipWriteRelay + `if [ "$_ok" = 1 ]; then
    if [ "$_clear" = 1 ]; then
        _copy() { "$_portald" clip copy clear; }
        _relay_noinput
    else
        case "$_type" in
          ""|UTF8_STRING|TEXT|STRING|text/*)
            _copy() {
                if [ "$_trim" = 1 ]; then
                    "$_portald" clip copy text --trim
                else
                    "$_portald" clip copy text
                fi
            }
            if [ "$_has_text" = 1 ]; then _relay_argv; else _relay_stdin; fi ;;
          image/png)
            _copy() { "$_portald" clip copy image png; }
            if [ "$_has_text" = 1 ]; then _relay_argv; else _relay_stdin; fi ;;
        esac
    fi
fi
` + clipWriteTail

// pbCopyShim buffers once to distinguish empty stdin, which maps to clear.
// pbcopy has no real-binary fallback on the target Linux dev boxes.
const pbCopyShim = `#!/bin/sh
# ` + Marker + `. Intercepts pbcopy clipboard writes and relays them to the Mac.
_portald="${HOME}/.cache/portal/portald"
_fail() {
    printf '%s\n' '` + clipWriteFailMsg + `' >&2
    exit 1
}
[ -x "$_portald" ] || _fail
_tmp=$(mktemp "${TMPDIR:-/tmp}/portal-pbcopy.XXXXXX" 2>/dev/null)
if [ -z "$_tmp" ]; then
    "$_portald" clip copy text 2>/dev/null && exit 0
    _fail
fi
trap 'rm -f "$_tmp"' EXIT HUP INT TERM
cat > "$_tmp" || _fail
if [ -s "$_tmp" ]; then
    "$_portald" clip copy text < "$_tmp" 2>/dev/null && exit 0
else
    "$_portald" clip copy clear < /dev/null 2>/dev/null && exit 0
fi
_fail
`

// pbPasteShim keeps read-side failure semantics: no clipboard content is an
// empty stdout stream with a successful exit.
const pbPasteShim = `#!/bin/sh
# ` + Marker + `. Intercepts pbpaste clipboard reads and relays them from the Mac.
_portald="${HOME}/.cache/portal/portald"
[ -x "$_portald" ] && "$_portald" clip text 2>/dev/null && exit 0
exit 0
`

// xselShim supports the conservative input/output/clear surface, including
// xsel's stdin-tty default and bundled selection/mode flags.
const xselShim = `#!/bin/sh
# ` + Marker + `. Intercepts xsel clipboard reads and writes and relays them
# to the Mac via portald; unrecognized forms fall through to the real xsel.
_portald="${HOME}/.cache/portal/portald"
_has_i=0
_has_o=0
_clear=0
_info=0
_ok=1
for _a in "$@"; do
    case "$_a" in
      --input) _has_i=1 ;;
      --output) _has_o=1 ;;
      --clear) _clear=1 ;;
      --clipboard|--primary|--secondary|--nodetach) : ;;
      --help|--version) _info=1; _ok=0 ;;
      --*) _ok=0 ;;
      -) _ok=0 ;;
      -?*)
        case "${_a#-}" in *[!iobpscn]*) _ok=0 ;; esac
        case "${_a#-}" in *i*) _has_i=1 ;; esac
        case "${_a#-}" in *o*) _has_o=1 ;; esac
        case "${_a#-}" in *c*) _clear=1 ;; esac ;;
      *) _ok=0 ;;
    esac
done
if [ "$_info" = 1 ]; then
    _mode=info
elif [ "$_has_i" = 1 ] && [ "$_has_o" = 1 ]; then
    _mode=write
    _ok=0
elif [ "$_clear" = 1 ] && [ "$_has_i" = 1 ]; then
    _mode=write
    _ok=0
elif [ "$_clear" = 1 ]; then
    _mode=write
elif [ "$_has_i" = 1 ]; then
    _mode=write
elif [ "$_has_o" = 1 ]; then
    _mode=read
elif [ -t 0 ]; then
    _mode=read
else
    _mode=write
fi
_wrapper_dir=$(cd "$(dirname "$0")" && pwd)
_real=""
_oifs=$IFS; IFS=:
for _d in $PATH; do
    [ "$_d" = "$_wrapper_dir" ] && continue
    [ -n "$_d" ] || continue
    if [ -x "$_d/xsel" ]; then _real="$_d/xsel"; break; fi
done
IFS=$_oifs
` + clipWriteRelay + `if [ "$_ok" = 1 ]; then
    if [ "$_clear" = 1 ]; then
        _copy() { "$_portald" clip copy clear; }
        _relay_noinput
    elif [ "$_mode" = write ]; then
        _copy() { "$_portald" clip copy text; }
        _relay_stdin
    elif [ "$_mode" = read ]; then
        [ -x "$_portald" ] && "$_portald" clip text 2>/dev/null && exit 0
    fi
fi
` + clipWriteTail

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
	{"wl-copy", wlCopyShim},
	{"pbcopy", pbCopyShim},
	{"pbpaste", pbPasteShim},
	{"xsel", xselShim},
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
# Ensures portal's shims (~/.local/bin/xdg-open, xclip, wl-paste, wl-copy,
# pbcopy, pbpaste, xsel, portal, portal-askpass, sudo) win on PATH.
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

// shimNames returns the deployment table's basenames for uninstall.
func shimNames() string {
	names := make([]string, 0, len(shims))
	for _, sh := range shims {
		names = append(names, sh.name)
	}
	return strings.Join(names, " ")
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
// shell rc files: every entry in the shims deployment table; the portald
// symlink; the env snippet; and all three shell marker blocks. It restores any
// pre-existing binaries backed up at install (preserving type via `mv`, which
// keeps a backed-up symlink a symlink) and never touches /usr/bin (DESIGN
// §9.3/§9.4).
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
for bin in %[8]s; do
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
done`, EarlyPathMarkerStart, EarlyPathMarkerEnd, PathMarkerStart, PathMarkerEnd, AskpassMarkerStart, AskpassMarkerEnd, ownershipMarker, shimNames())
	_, _, _ = tr.Exec(ctx, nil, "bash", "-c", shellQuote(script))
}

// shellQuote wraps a shell script in single quotes for safe remote execution
// via ssh (which joins argv with spaces and runs the result through sh -c).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
