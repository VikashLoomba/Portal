#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APP="${1:-$ROOT/target/aarch64-apple-darwin/release/Portal.app}"
APP="$(cd "$(dirname "$APP")" && pwd)/$(basename "$APP")"
EXECUTABLE="$APP/Contents/MacOS/Portal"

[ -x "$EXECUTABLE" ] || { echo "native e2e: executable not found: $EXECUTABLE" >&2; exit 1; }

PORTAL_E2E_EXECUTABLE="$EXECUTABLE" \
  swift test \
    --package-path "$ROOT/native" \
    --scratch-path "$ROOT/target/swift" \
    --filter PortalLiveIntegrationTests

echo "==> native app E2E verified (daemon, ownerless stream, mutations, restart, cancellation)"
