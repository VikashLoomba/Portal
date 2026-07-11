package clipshim

import (
	"context"
	"fmt"
	"strings"

	"github.com/VikashLoomba/Portal/pkg/transport"
)

// portalShim is installed at ~/.local/bin/portal. It gives coding agents the
// same `portal keychain ...` command name as the Mac CLI while dispatching to
// the sha-symlinked box agent.
const portalShim = `#!/bin/sh
# ` + Marker + `. Agent-facing portal command; dispatches to the current portald.
_portald="${HOME}/.cache/portal/portald"
if [ -x "$_portald" ]; then
    exec "$_portald" "$@"
fi
printf '%s\n' 'portal: portald is unavailable' >&2
exit 127
`

// portalAskpassShim is installed at ~/.local/bin/portal-askpass. sudo invokes
// it through SUDO_ASKPASS; a missing-agent path exits non-zero so sudo aborts
// instead of waiting for or accepting an absent password.
const portalAskpassShim = `#!/bin/sh
# ` + Marker + `. SUDO_ASKPASS helper; requests one approved secret from the Mac.
_portald="${HOME}/.cache/portal/portald"
if [ -x "$_portald" ]; then
    exec "$_portald" keychain askpass "$@"
fi
printf '%s\n' 'portal-askpass: portald is unavailable' >&2
exit 1
`

// sudoShim is installed at ~/.local/bin/sudo. Human terminal passthrough stays
// first. A detached invocation defaults an empty SUDO_ASKPASS to portal's
// executable helper, then passes through if no usable helper exists or sudo's
// leading options select an explicit input/non-interactive/edit/help mode.
// Only a fully detached invocation with usable askpass and no conflicting sudo
// flag receives an injected -A; flags after the command belong to that command.
const sudoShim = `#!/bin/sh
# ` + Marker + `. Detached sudo bridge; terminal sessions pass through unchanged.
# Detached shells default an empty SUDO_ASKPASS to executable portal-askpass.
_wrapper_dir=$(cd "$(dirname "$0")" && pwd)
_real=""
_oifs=$IFS; IFS=:
for _d in $PATH; do
    [ "$_d" = "$_wrapper_dir" ] && continue
    [ -n "$_d" ] || continue
    if [ -x "$_d/sudo" ]; then _real="$_d/sudo"; break; fi
done
IFS=$_oifs
if [ -z "$_real" ]; then
    printf '%s\n' 'portal sudo: real sudo not found' >&2
    exit 1
fi

# SAFETY INVARIANT: a human in any interactive session reaches real sudo
# byte-for-byte, even with redirected stdin. Askpass is injected only when no
# controlling terminal exists on which sudo could prompt that human.
# The /dev/tty probe MUST stay a subshell ( ... ), never a { ...; } group:
# ':' is a POSIX special built-in, so a failed redirect on it in a
# non-interactive shell (dash) is FATAL and would abort the whole script; the
# subshell contains that exit so the fallthrough to askpass still runs.
if [ -t 0 ] || [ -t 1 ] || [ -t 2 ] || ( : < /dev/tty ) 2>/dev/null; then
    exec "$_real" "$@"
fi

# A detached shell may reach this wrapper even when no startup file exported
# SUDO_ASKPASS. Select portal's helper only when the user left it empty.
if [ -z "${SUDO_ASKPASS:-}" ] && [ -x "$HOME/.local/bin/portal-askpass" ]; then
    SUDO_ASKPASS="$HOME/.local/bin/portal-askpass"
    export SUDO_ASKPASS
fi

# Without an executable askpass helper, preserve sudo's native behavior.
if [ -z "${SUDO_ASKPASS:-}" ] || [ ! -x "$SUDO_ASKPASS" ]; then
    exec "$_real" "$@"
fi

# Explicit askpass/stdin/non-interactive/edit/help/timestamp modes belong to
# sudo's caller. Scan only sudo's leading options, including bundled short
# flags: -- or the first non-option starts the command, whose own flags must
# not suppress portal askpass.
_passthrough=0
for a in "$@"; do
    case "$a" in
        --askpass|--stdin|--non-interactive|--edit)
            _passthrough=1
            break
            ;;
        --)
            break
            ;;
        --*)
            ;;
        -*)
            case "${a#-}" in
                *[ASnehVKkv]*)
                    _passthrough=1
                    break
                    ;;
            esac
            ;;
        *)
            break
            ;;
    esac
done
if [ "$_passthrough" -eq 1 ]; then
    exec "$_real" "$@"
fi

# The only -A injection branch: no controlling terminal + executable askpass + no conflict.
exec "$_real" -A "$@"
`

// AskpassMarkerStart/AskpassMarkerEnd delimit the independently-convergent
// SUDO_ASKPASS export block. They remain stable across shim versions so Remove
// can delete the complete range from every shell startup file.
const (
	AskpassMarkerStart = "# >>> portal askpass (sudo) >>>"
	AskpassMarkerEnd   = "# <<< portal askpass (sudo) <<<"
)

// askpassEnvSnippet exports the helper only when its executable shim exists and
// the user has not selected another SUDO_ASKPASS. This block stays separate
// from the pre-existing PATH block so upgrading an installed box re-converges
// even when that older PATH marker is already there.
const askpassEnvSnippet = AskpassMarkerStart + `
if [ -z "${SUDO_ASKPASS:-}" ] && [ -x "$HOME/.local/bin/portal-askpass" ]; then
    export SUDO_ASKPASS="$HOME/.local/bin/portal-askpass"
fi
` + AskpassMarkerEnd

// ensureAskpassEnv appends the SUDO_ASKPASS block exactly once. Standard rc
// files are created when missing; bash login alternatives are never created.
func ensureAskpassEnv(ctx context.Context, tr transport.Transport) error {
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
done`, rcList, AskpassMarkerStart, conditionalRCList, AskpassMarkerStart)
	if _, _, err := tr.Exec(ctx, []byte(askpassEnvSnippet), "bash", "-c", shellQuote(script)); err != nil {
		return fmt.Errorf("write SUDO_ASKPASS block: %w", err)
	}
	return nil
}
