#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APP="${1:-$ROOT/target/aarch64-apple-darwin/release/Portal.app}"
APP="$(cd "$(dirname "$APP")" && pwd)/$(basename "$APP")"
EXECUTABLE="$APP/Contents/MacOS/Portal"
TMP="/tmp/portal-gui-e2e.$$"
mkdir -p "$TMP"
CONFIG="$TMP/config"
SOCKET="$TMP/api.sock"
DAEMON_PID=""
GUI_PID=""
mkdir -p "$CONFIG"
cat >"$CONFIG/config.toml" <<'EOF'
[[boxes]]
name = "e2e-box"
host = "127.0.0.1"
index = 1
allow = [3000]
deny = []
enabled = false
EOF

cleanup() {
  [ -z "$GUI_PID" ] || kill "$GUI_PID" 2>/dev/null || true
  [ -z "$DAEMON_PID" ] || kill "$DAEMON_PID" 2>/dev/null || true
  wait "$GUI_PID" 2>/dev/null || true
  wait "$DAEMON_PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

[ -x "$EXECUTABLE" ] || { echo "GUI lifecycle: executable not found: $EXECUTABLE" >&2; exit 1; }

PORTAL_CONFIG_DIR="$CONFIG" PORTAL_API_SOCK="$SOCKET" \
  "$EXECUTABLE" --daemon >"$TMP/daemon.log" 2>&1 &
DAEMON_PID=$!
for _ in $(seq 1 200); do
  [ -S "$SOCKET" ] && break
  kill -0 "$DAEMON_PID" 2>/dev/null || { cat "$TMP/daemon.log" >&2; exit 1; }
  sleep 0.05
done
[ -S "$SOCKET" ] || { echo "GUI lifecycle: daemon socket not ready" >&2; exit 1; }

PORTAL_CONFIG_DIR="$CONFIG" PORTAL_API_SOCK="$SOCKET" PORTAL_SKIP_APP_BOOTSTRAP=1 \
  "$EXECUTABLE" >"$TMP/gui.log" 2>&1 &
GUI_PID=$!

window_ready=0
for _ in $(seq 1 200); do
  if osascript -e "tell application \"System Events\" to tell first process whose unix id is $GUI_PID to get count of windows" 2>/dev/null | grep -q '^1$'; then
    window_ready=1
    break
  fi
  kill -0 "$GUI_PID" 2>/dev/null || { cat "$TMP/gui.log" >&2; exit 1; }
  sleep 0.05
done
[ "$window_ready" = 1 ] || { echo "GUI lifecycle: management window not ready" >&2; exit 1; }

xcrun swift "$ROOT/scripts/audit-native-accessibility.swift" "$GUI_PID" >"$TMP/accessibility.txt"
for expected in \
  'AXWindow|AXStandardWindow|Portal' \
  'AXRadioButton|AXSegment||Overview' \
  'AXRadioButton|AXSegment||Logs' \
  'AXMenuItem||Overview' \
  'AXMenuItem||Logs' \
  'Check for Updates…' \
  'Add Box…' \
  'e2e-box enabled' \
  'Image clipboard' \
  'Text clipboard' \
  'AXMenuBarItem|AXMenuExtra|Portal — Disabled'; do
  grep -F "$expected" "$TMP/accessibility.txt" >/dev/null || {
    echo "GUI lifecycle: accessibility tree is missing: $expected" >&2
    cat "$TMP/accessibility.txt" >&2
    exit 1
  }
done

osascript <<APPLESCRIPT
  tell application "System Events"
    tell first process whose unix id is $GUI_PID to set frontmost to true
    keystroke "2" using command down
  end tell
APPLESCRIPT
xcrun swift "$ROOT/scripts/audit-native-accessibility.swift" "$GUI_PID" >"$TMP/accessibility-logs.txt"
grep -F 'AXRadioButton|AXSegment||Logs||1' "$TMP/accessibility-logs.txt" >/dev/null || {
  echo "GUI lifecycle: Command-2 did not select Logs" >&2
  exit 1
}
osascript -e "tell application \"System Events\" to keystroke \"1\" using command down"

osascript -e "tell application \"System Events\" to tell first process whose unix id is $GUI_PID to click (first button of window 1 whose subrole is \"AXCloseButton\")" >/dev/null
kill -0 "$GUI_PID"
kill -0 "$DAEMON_PID"
PORTAL_CONFIG_DIR="$CONFIG" PORTAL_API_SOCK="$SOCKET" "$EXECUTABLE" --cli status >/dev/null

osascript <<APPLESCRIPT
  tell application "System Events"
    tell first process whose unix id is $GUI_PID to set frontmost to true
    keystroke "q" using command down
  end tell
APPLESCRIPT
for _ in $(seq 1 200); do
  kill -0 "$GUI_PID" 2>/dev/null || break
  sleep 0.05
done
if kill -0 "$GUI_PID" 2>/dev/null; then
  echo "GUI lifecycle: Quit Portal did not terminate the GUI" >&2
  exit 1
fi
GUI_PID=""

# Closing and quitting the GUI must not own or stop forwarding state.
kill -0 "$DAEMON_PID"
PORTAL_CONFIG_DIR="$CONFIG" PORTAL_API_SOCK="$SOCKET" "$EXECUTABLE" --cli status >/dev/null

kill "$DAEMON_PID"
wait "$DAEMON_PID"
DAEMON_PID=""
[ ! -e "$SOCKET" ] || { echo "GUI lifecycle: daemon left its socket behind" >&2; exit 1; }
if PORTAL_CONFIG_DIR="$CONFIG" PORTAL_API_SOCK="$SOCKET" "$EXECUTABLE" --cli status >/dev/null 2>&1; then
  echo "GUI lifecycle: status succeeded after daemon shutdown" >&2
  exit 1
fi

echo "==> native GUI lifecycle verified (window close/quit independent; daemon stop authoritative)"
