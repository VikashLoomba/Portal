#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d -t portal-cli-launcher)"
trap 'rm -rf "$TMP"' EXIT

APP="$TMP/Portal With Spaces.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources/bin" "$TMP/bin"
cp "$ROOT/native/Resources/bin/portal" "$APP/Contents/Resources/bin/portal"
cat > "$APP/Contents/MacOS/Portal" <<'FAKE'
#!/bin/bash
printf 'args:'
printf ' <%s>' "$@"
printf '\n'
printf 'stdin:'
cat
printf 'stderr-marker\n' >&2
exit 23
FAKE
chmod +x "$APP/Contents/MacOS/Portal" "$APP/Contents/Resources/bin/portal"
ln -s "../Portal With Spaces.app/Contents/Resources/bin/portal" "$TMP/bin/portal"

set +e
printf 'payload\n' | "$TMP/bin/portal" status --flag "two words" \
  >"$TMP/stdout" 2>"$TMP/stderr"
CODE=$?
set -e

[ "$CODE" = 23 ] || { echo "launcher: exit status $CODE, want 23" >&2; exit 1; }
grep -F 'args: <--cli> <status> <--flag> <two words>' "$TMP/stdout" >/dev/null
grep -F 'stdin:payload' "$TMP/stdout" >/dev/null
grep -F 'stderr-marker' "$TMP/stderr" >/dev/null

echo "==> app-owned portal launcher preserves paths, arguments, stdio, and status"
