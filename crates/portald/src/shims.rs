//! The dev-box PATH shims (DESIGN-clipsync §2.2). Interception mechanism is
//! v1's (proven): ~/.local/bin sits first on PATH, agents shell out to
//! xclip/wl-paste, the shim answers. What changed is WHAT the shim does —
//! `portald clip paste` reads the LOCAL store in microseconds. No cmd-socket
//! wait, no timeout tower, no network in the paste path.
//!
//! Failure posture (§2.6): when portald can serve, serve; when the store
//! errors, portald prints the reason to stderr and exits 1 — the shim tees
//! that into ~/.cache/portal/shim.log, THEN falls through to the real binary
//! (non-agent clipboard use keeps working). Diagnosable, not silent.
//!
//! Write-side shims (wl-copy / xclip -i / pbcopy) belong to the clip-WRITE
//! relay (box → Mac), a separate service that still crosses the connection —
//! they land with that phase, not here.
//!
//! The Mac deploys these strings verbatim (bootstrap phase); `VERSION` is the
//! content marker that makes redeploys a cheap grep, exactly like v1.

/// Bump when any shim text changes (drives daemon-driven re-deploys).
pub const VERSION: &str = "13";

/// Marker every shim carries; version-independent prefix owns backup/restore
/// decisions (v1 doctrine: a portal shim of ANY version is never mistaken
/// for a user binary).
pub const OWNERSHIP_MARKER: &str = "Installed by portal clip-shim";

fn marker() -> String {
    format!("{OWNERSHIP_MARKER} v{VERSION}")
}

/// Shared prologue: locate portald, define the logged-fallthrough helper.
fn prologue(marker: &str) -> String {
    format!(
        r#"#!/bin/sh
# {marker}. Reads answer from the LOCAL portal clip store (clipsync);
# on failure the reason is logged and we fall through to the real binary.
_portald="${{HOME}}/.cache/portal/portald"
_log="${{HOME}}/.cache/portal/shim.log"
_try() {{
    [ -x "$_portald" ] || return 1
    "$_portald" "$@" 2>>"$_log"
}}
_real() {{
    _wrapper_dir=$(cd "$(dirname "$0")" && pwd -P)
    _name=$1; shift
    _oifs=$IFS; IFS=:
    for _d in $PATH; do
        [ -n "$_d" ] || continue
        _cand=$(cd "$_d" 2>/dev/null && pwd -P)
        [ -n "$_cand" ] || continue
        [ "$_cand" = "$_wrapper_dir" ] && continue
        if [ -x "$_cand/$_name" ]; then IFS=$_oifs; exec "$_cand/$_name" "$@"; fi
    done
    IFS=$_oifs
    exit 0
}}
"#
    )
}

/// xclip: parse the enumerated legal prefixes of its canonical options
/// (v1's parser, verbatim discipline); READ forms answer from the store,
/// write/unknown forms fall through.
pub fn xclip() -> String {
    let mut s = prologue(&marker());
    s.push_str(
        r#"_mode=write
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
      -f|-fi|-fil|-filt|-filte|-filter) : ;;
      -n|-no|-nou|-nout|-noutf|-noutf8) : ;;
      -q|-qu|-qui|-quie|-quiet) : ;;
      -si|-sil|-sile|-silen|-silent) : ;;
      -verb|-verbo|-verbos|-verbose) : ;;
      -debug) : ;;
      -h|-he|-hel|-help|-vers|-versi|-versio|-version) _ok=0 ;;
      *) _ok=0 ;;
    esac
done
[ -z "$_want" ] || _ok=0
if [ "$_ok" = 1 ] && [ "$_mode" = read ]; then
    case "$_target" in
      TARGETS)
        _try clip targets xclip && exit 0 ;;
      image/png)
        _try clip paste --type image/png && exit 0 ;;
      ""|UTF8_STRING|TEXT|STRING|text/plain|text/plain\;*)
        if [ "$_trim" = 1 ]; then
            _try clip paste --trim && exit 0
        else
            _try clip paste && exit 0
        fi ;;
    esac
fi
if [ "$_ok" = 1 ] && [ "$_mode" = write ]; then
    case "$_target" in
      ""|UTF8_STRING|TEXT|STRING|text/*)
        _flags="--empty-clears"
        [ "$_trim" = 1 ] && _flags="--trim $_flags"
        _try clip copy --type text $_flags && exit 0 ;;
      image/png)
        _try clip copy --type image/png && exit 0 ;;
    esac
fi
_real xclip "$@"
"#,
    );
    s
}

/// wl-paste: pattern surface identical to v1's shim, store-backed answers.
pub fn wl_paste() -> String {
    let mut s = prologue(&marker());
    s.push_str(
        r#"_args="$*"
case "$_args" in
  *"--list-types"*)
    _try clip targets wl-paste && exit 0 ;;
  *"--type image/png"*|*"-t image/png"*)
    _try clip paste --type image/png && exit 0 ;;
  *"--type image/"*|*"-t image/"*) : ;;
  *"--no-newline"*|*"-n"*)
    _try clip paste --trim && exit 0 ;;
  *"--type text/"*|*"-t text/"*|"")
    _try clip paste && exit 0 ;;
esac
_real wl-paste "$@"
"#,
    );
    s
}

/// pbpaste: bare text read.
pub fn pbpaste() -> String {
    let mut s = prologue(&marker());
    s.push_str(
        r#"_try clip paste && exit 0
_real pbpaste "$@"
"#,
    );
    s
}

/// wl-copy: write-only surface. Conservative argv parse (v1 discipline);
/// unrecognized forms fall through to the real wl-copy.
pub fn wl_copy() -> String {
    let mut s = prologue(&marker());
    s.push_str(
        r#"_type=""
_trim=0
_clear=0
_ok=1
_want=""
_text=""
_has_text=0
_endopts=0
for _a in "$@"; do
    if [ -n "$_want" ]; then
        case "$_want" in type) _type=$_a ;; skip) : ;; esac
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
      -h|--help|-v|--version) _ok=0 ;;
      --*) _ok=0 ;;
      -*) _ok=0 ;;
      *)
        if [ "$_has_text" = 1 ]; then _text="$_text $_a"; else _text=$_a; fi
        _has_text=1 ;;
    esac
done
[ -z "$_want" ] || _ok=0
if [ "$_ok" = 1 ]; then
    if [ "$_clear" = 1 ]; then
        printf '' | _try clip copy --type text --empty-clears && exit 0
    else
        case "$_type" in
          ""|UTF8_STRING|TEXT|STRING|text/*)
            _flags="--empty-clears"
            [ "$_trim" = 1 ] && _flags="--trim $_flags"
            if [ "$_has_text" = 1 ]; then
                printf '%s' "$_text" | _try clip copy --type text $_flags && exit 0
            else
                _try clip copy --type text $_flags && exit 0
            fi ;;
          image/png)
            _try clip copy --type image/png && exit 0 ;;
        esac
    fi
fi
_real wl-copy "$@"
"#,
    );
    s
}

/// pbcopy: bare text write (no real binary on Linux boxes: loud failure).
pub fn pbcopy() -> String {
    let mut s = prologue(&marker());
    s.push_str(
        r#"_try clip copy --type text --empty-clears && exit 0
echo "portal: clipboard write failed (no Mac client connected)" >&2
exit 1
"#,
    );
    s
}

/// sudo wrapper (v1 doctrine, verbatim semantics): transparent askpass fires
/// ONLY when the caller has NO controlling terminal (an agent) — in any
/// session where a human could be prompted, exec the real sudo untouched.
/// This deliberately fail-safe check prevents portal from hijacking a human
/// password prompt, including when sudo's stdin has been redirected.
pub fn sudo() -> String {
    format!(
        r#"#!/bin/sh
# {marker}. Transparent sudo askpass for agents (no controlling tty only).
_wrapper_dir=$(cd "$(dirname "$0")" && pwd -P)
_real=""
_oifs=$IFS; IFS=:
for _d in $PATH; do
    [ -n "$_d" ] || continue
    _cand=$(cd "$_d" 2>/dev/null && pwd -P)
    [ -n "$_cand" ] || continue
    [ "$_cand" = "$_wrapper_dir" ] && continue
    if [ -x "$_cand/sudo" ]; then _real="$_cand/sudo"; break; fi
done
IFS=$_oifs
if [ -z "$_real" ]; then
    echo "portal sudo shim: no real sudo on PATH" >&2
    exit 127
fi
# Controlling terminal present? Human session — passthrough, ALWAYS.
if [ -t 0 ] || [ -t 1 ] || [ -t 2 ] || tty -s 2>/dev/null; then
    exec "$_real" "$@"
fi
# Respect a user-configured SUDO_ASKPASS; select ours only when unset.
if [ -z "${{SUDO_ASKPASS:-}}" ] && [ -x "$HOME/.local/bin/portal-askpass" ]; then
    SUDO_ASKPASS="$HOME/.local/bin/portal-askpass" export SUDO_ASKPASS
    exec "$_real" -A "$@"
fi
exec "$_real" "$@"
"#,
        marker = marker()
    )
}

/// Agent-facing `portal` command. The Mac-side CLI is not copied to the box;
/// this stable wrapper dispatches box-side verbs to the current embedded
/// portald, including the secure `portal keychain run` surface.
pub fn portal() -> String {
    format!(
        r#"#!/bin/sh
# {marker}. Agent-facing portal command; dispatches to the current portald.
_portald="${{HOME}}/.cache/portal/portald"
if [ -x "$_portald" ]; then
    exec "$_portald" "$@"
fi
echo "portal: portald is unavailable" >&2
exit 127
"#,
        marker = marker()
    )
}

/// SUDO_ASKPASS helper: sudo invokes it with the prompt as argv; the secret
/// comes back on stdout (portald keychain askpass owns the socket protocol).
pub fn portal_askpass() -> String {
    format!(
        r#"#!/bin/sh
# {marker}. SUDO_ASKPASS helper: asks the Mac for the sudo credential.
_portald="${{HOME}}/.cache/portal/portald"
if [ -x "$_portald" ]; then
    exec "$_portald" keychain askpass "$@"
fi
exit 111
"#,
        marker = marker()
    )
}

/// xdg-open: THE box→Mac URL relay entry point. OAuth CLIs (gh, gcloud,
/// rclone, claude) and Python's webbrowser all shell out to xdg-open on
/// Linux; on a headless box the real one has nothing to open. Relaying to
/// `portald open` sends the URL up the pipe, where the Mac establishes a
/// forward for loopback callback ports and opens the browser (see
/// portal-core::callback). Falls through to the real xdg-open when no
/// portal session is live (desktop boxes keep working).
pub fn xdg_open() -> String {
    format!(
        r#"#!/bin/sh
# {marker}. Relays URL opens to the Mac client when a portal session is
# active; otherwise falls through to the real xdg-open.
_portald="${{HOME}}/.cache/portal/portald"
_log="${{HOME}}/.cache/portal/shim.log"
if [ -x "$_portald" ] && "$_portald" open "$@" 2>>"$_log"; then
    exit 0
fi
_wrapper_dir=$(cd "$(dirname "$0")" && pwd -P)
_oifs=$IFS; IFS=:
for _d in $PATH; do
    [ -n "$_d" ] || continue
    _cand=$(cd "$_d" 2>/dev/null && pwd -P)
    [ -n "$_cand" ] || continue
    [ "$_cand" = "$_wrapper_dir" ] && continue
    if [ -x "$_cand/xdg-open" ]; then IFS=$_oifs; exec "$_cand/xdg-open" "$@"; fi
done
IFS=$_oifs
# No real xdg-open (headless box) and no client: nothing can open it.
exit 0
"#,
        marker = marker()
    )
}

/// (name, script) table the Mac-side deploy iterates.
pub fn all() -> Vec<(&'static str, String)> {
    vec![
        ("xclip", xclip()),
        ("wl-paste", wl_paste()),
        ("pbpaste", pbpaste()),
        ("wl-copy", wl_copy()),
        ("pbcopy", pbcopy()),
        ("portal", portal()),
        ("sudo", sudo()),
        ("portal-askpass", portal_askpass()),
        ("xdg-open", xdg_open()),
    ]
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn every_shim_carries_the_version_marker_and_shape() {
        assert_eq!(VERSION, "13");
        assert!(all().iter().any(|(name, _)| *name == "portal"));
        for (name, script) in all() {
            assert!(script.starts_with("#!/bin/sh\n"), "{name}: shebang");
            assert!(script.contains(&marker()), "{name}: version marker");
            assert!(script.contains(OWNERSHIP_MARKER), "{name}: ownership");
            assert!(!script.contains("cmd-"), "{name}: no cmd-socket relics");
        }
        // Clip shims specifically log their fallthrough reason (cred shims
        // are pure passthrough wrappers — a human sudo must be untouched).
        for name in ["xclip", "wl-paste", "pbpaste"] {
            let script = all().into_iter().find(|(n, _)| *n == name).unwrap().1;
            assert!(script.contains("shim.log"), "{name}: logged fallthrough");
        }
    }

    #[test]
    fn read_paths_hit_the_local_store_verbs() {
        let x = xclip();
        assert!(x.contains("clip targets xclip"));
        assert!(x.contains("clip paste --type image/png"));
        assert!(x.contains("clip paste --trim"));
        let w = wl_paste();
        assert!(w.contains("clip targets wl-paste"));
        assert!(w.contains("clip paste --type image/png"));
    }

    /// The URL relay: portald first (the Mac forwards + opens), real
    /// xdg-open as fallthrough, self-exclusion in PATH resolution, and a
    /// clean exit when neither exists (headless box, no session).
    #[test]
    fn xdg_open_relays_then_falls_through() {
        let s = xdg_open();
        assert!(s.contains(r#""$_portald" open "$@""#), "{s}");
        assert!(
            s.contains("shim.log"),
            "relay failures must be diagnosable: {s}"
        );
        assert!(
            s.contains(r#"[ "$_cand" = "$_wrapper_dir" ] && continue"#),
            "must never exec itself: {s}"
        );
        assert!(s.contains(r#"exec "$_cand/xdg-open""#), "{s}");
    }

    #[test]
    fn sudo_shim_is_fail_safe_around_ttys() {
        let s = sudo();
        // The tty check must gate the askpass path…
        assert!(s.contains("[ -t 0 ] || [ -t 1 ] || [ -t 2 ]"), "{s}");
        // …respect user-configured SUDO_ASKPASS…
        assert!(s.contains(r#"[ -z "${SUDO_ASKPASS:-}" ]"#), "{s}");
        // …and never shadow itself in PATH resolution.
        assert!(s.contains("portal-askpass"), "{s}");
        let p = portal();
        assert!(p.contains(r#"exec "$_portald" "$@""#), "{p}");
        let a = portal_askpass();
        assert!(a.contains("keychain askpass"), "{a}");
    }

    /// The shims must be valid POSIX sh (parse check only).
    #[test]
    fn shims_parse_under_sh() {
        for (name, script) in all() {
            let dir = tempfile::tempdir().unwrap();
            let path = dir.path().join(name);
            std::fs::write(&path, &script).unwrap();
            let ok = std::process::Command::new("sh")
                .arg("-n")
                .arg(&path)
                .status()
                .unwrap()
                .success();
            assert!(ok, "{name}: sh -n rejected the script");
        }
    }
}
