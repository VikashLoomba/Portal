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

// sudoShim is installed at ~/.local/bin/sudo. It is a pure passthrough for
// interactive use, missing askpass configuration, and every explicit sudo
// input/non-interactive/edit/help flag in sudo's leading option prefix. Only a
// non-TTY agent invocation with a usable SUDO_ASKPASS and no conflicting sudo
// flag receives an injected -A; flags after the command belong to that command.
const sudoShim = `#!/bin/sh
# ` + Marker + `. Non-TTY sudo askpass bridge; interactive sudo is untouched.
_wrapper_dir=$(cd "$(dirname "$0")" && pwd)
_real=$(printf '%s' "$PATH" | tr ':' '\n' | grep -vxF "$_wrapper_dir" | tr '\n' ':' | xargs -I{} sh -c 'PATH={} command -v sudo 2>/dev/null' | head -1)
if [ -z "$_real" ]; then
    printf '%s\n' 'portal sudo: real sudo not found' >&2
    exit 1
fi

# SAFETY INVARIANT: a human with a TTY always reaches real sudo byte-for-byte;
# portal must never replace an interactive terminal password prompt.
if [ -t 0 ]; then
    exec "$_real" "$@"
fi

# Without an executable askpass helper, preserve sudo's native behavior.
if [ -z "${SUDO_ASKPASS:-}" ] || [ ! -x "$SUDO_ASKPASS" ]; then
    exec "$_real" "$@"
fi

# Explicit askpass/stdin/non-interactive/edit/help/timestamp modes belong to
# sudo's caller. Scan only sudo's leading options: -- or the first non-option
# starts the command, whose own flags must not suppress portal askpass.
_passthrough=0
for a in "$@"; do
    case "$a" in
        -A|--askpass|-S|--stdin|-n|--non-interactive|-e|--edit|-h|-V|-K|-k|-v)
            _passthrough=1
            break
            ;;
        --)
            break
            ;;
        -*)
            ;;
        *)
            break
            ;;
    esac
done
if [ "$_passthrough" -eq 1 ]; then
    exec "$_real" "$@"
fi

# The only -A injection branch: non-TTY + executable askpass + no conflict.
exec "$_real" -A "$@"
`

// AskpassMarkerStart/AskpassMarkerEnd delimit the independently-convergent
// SUDO_ASKPASS export block. They remain stable across shim versions so Remove
// can delete the complete range from every shell startup file.
const (
	AskpassMarkerStart = "# >>> portal askpass (sudo) >>>"
	AskpassMarkerEnd   = "# <<< portal askpass (sudo) <<<"
)

// askpassEnvSnippet exports the helper only while its executable shim exists.
// This block stays separate from the pre-existing PATH block so upgrading an
// installed box re-converges even when that older PATH marker is already there.
const askpassEnvSnippet = AskpassMarkerStart + `
[ -x "$HOME/.local/bin/portal-askpass" ] && export SUDO_ASKPASS="$HOME/.local/bin/portal-askpass"
` + AskpassMarkerEnd

// ensureAskpassEnv appends the SUDO_ASKPASS marker block to every shell startup
// file exactly once, creating missing files just like ensurePathPrepend.
func ensureAskpassEnv(ctx context.Context, tr transport.Transport) error {
	rcList := strings.Join(rcFiles, " ")
	script := fmt.Sprintf(`block=$(cat); for rc in %s; do
    if [ -f "$rc" ] && grep -qF %q "$rc"; then continue; fi
    printf '\n%%s\n' "$block" >> "$rc"
done`, rcList, AskpassMarkerStart)
	if _, _, err := tr.Exec(ctx, []byte(askpassEnvSnippet), "bash", "-c", shellQuote(script)); err != nil {
		return fmt.Errorf("write SUDO_ASKPASS block: %w", err)
	}
	return nil
}
