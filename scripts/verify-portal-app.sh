#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APP="${1:-$ROOT/target/aarch64-apple-darwin/release/Portal.app}"
EXECUTABLE="$APP/Contents/MacOS/Portal"
LAUNCHER="$APP/Contents/Resources/bin/portal"

[ -x "$EXECUTABLE" ] || { echo "Portal verification: missing executable" >&2; exit 1; }
[ -x "$LAUNCHER" ] || { echo "Portal verification: missing CLI launcher" >&2; exit 1; }
[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleExecutable' "$APP/Contents/Info.plist")" = "Portal" ]
/bin/bash -n "$LAUNCHER"

MACHOS="$(find "$APP/Contents" -type f -exec sh -c 'file "$1" | grep -q "Mach-O" && printf "%s\n" "$1"' sh {} \;)"
[ "$(printf '%s\n' "$MACHOS" | grep -c .)" = 1 ] || {
  echo "Portal verification: expected one Mach-O, found:" >&2
  printf '%s\n' "$MACHOS" >&2
  exit 1
}
[ "$MACHOS" = "$EXECUTABLE" ] || {
  echo "Portal verification: unexpected compiled executable $MACHOS" >&2
  exit 1
}

MIN_OS="$(xcrun vtool -show-build "$EXECUTABLE" | awk '/minos/{print $2; exit}')"
python3 - "$MIN_OS" <<'PY'
import sys
parts = tuple(int(value) for value in sys.argv[1].split('.'))
if parts > (13, 0):
    raise SystemExit(f"Portal executable requires macOS {sys.argv[1]}, expected <= 13.0")
PY

DIRECT="$($EXECUTABLE --cli --version)"
VIA_LAUNCHER="$($LAUNCHER --version)"
[ "$DIRECT" = "$VIA_LAUNCHER" ] || {
  echo "Portal verification: direct and launcher versions differ" >&2
  exit 1
}
PROMPT_FALLBACK="$(printf 'not-json' | "$EXECUTABLE" _prompt)"
printf '%s' "$PROMPT_FALLBACK" | grep -F '"outcome":"unavailable"' >/dev/null || {
  echo "Portal verification: prompt process mode did not fail closed" >&2
  exit 1
}

echo "==> Portal.app verified (one Mach-O, macOS $MIN_OS, $DIRECT)"
