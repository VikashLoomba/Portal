#!/bin/bash
# BoltFFI 0.30.1 Swift strict-concurrency compatibility patch.
#
# withTaskCancellationHandler requires its onCancel closure to be @Sendable,
# but the generated boltffiAsyncCall parameters captured by that closure are
# emitted as ordinary escaping function values. Swift 6 also requires the
# generic continuation result to be Sendable. Keep these exact and fail closed:
# a generator upgrade must be reviewed instead of accepting a partial patch.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FILE="${1:-$ROOT/native/Sources/PortalFFIGenerated/BoltFFI/PortalFfiBoltFFI.swift}"

[ -f "$FILE" ] || {
  echo "BoltFFI Swift patch: generated source not found: $FILE" >&2
  exit 1
}

python3 - "$FILE" <<'PY'
from pathlib import Path
import sys

path = Path(sys.argv[1])
source = path.read_text()
replacements = {
    "func boltffiAsyncCall<T>(\n":
        "func boltffiAsyncCall<T: Sendable>(\n",
    "    cancel: @escaping (RustFutureHandle?) -> Void,\n":
        "    cancel: @escaping @Sendable (RustFutureHandle?) -> Void,\n",
    "    free: @escaping (RustFutureHandle?) -> Void,\n":
        "    free: @escaping @Sendable (RustFutureHandle?) -> Void,\n",
}

for old, new in replacements.items():
    old_count = source.count(old)
    new_count = source.count(new)
    if old_count == 1 and new_count == 0:
        source = source.replace(old, new)
    elif old_count == 0 and new_count == 1:
        # Idempotent invocation.
        pass
    else:
        raise SystemExit(
            f"BoltFFI Swift patch: expected one generated shape for {old.strip()!r}; "
            f"found old={old_count}, patched={new_count}"
        )

path.write_text(source)
PY

echo "==> patched BoltFFI Swift 6 Sendable annotations"
