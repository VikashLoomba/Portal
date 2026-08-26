#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GENERATED="$ROOT/native/Sources/PortalFFIGenerated/BoltFFI/PortalFfiBoltFFI.swift"
APP="$ROOT/native/Sources/PortalApp"
WRAPPER="$ROOT/native/Sources/PortalFFI/PortalFFI.swift"

[ -f "$GENERATED" ] || { echo "Swift boundary: generate PortalFFI first" >&2; exit 1; }

if grep -R -n -E 'import PortalFFIGenerated|PortalStateStreamSource' "$APP"; then
  echo "Swift boundary: app code bypasses the ownerless PortalFFI façade" >&2
  exit 1
fi

grep -F 'public func stateUpdates() -> AsyncStream<PortalStateEvent>' "$WRAPPER" >/dev/null
grep -F 'func boltffiAsyncCall<T: Sendable>(' "$GENERATED" >/dev/null
grep -F 'cancel: @escaping @Sendable (RustFutureHandle?) -> Void' "$GENERATED" >/dev/null
grep -F 'free: @escaping @Sendable (RustFutureHandle?) -> Void' "$GENERATED" >/dev/null

echo "==> Swift boundary verified (ownerless façade, strict Sendable runtime)"
