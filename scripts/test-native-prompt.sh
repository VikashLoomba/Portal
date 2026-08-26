#!/bin/bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APP="${1:-$ROOT/target/aarch64-apple-darwin/release/Portal.app}"
APP="$(cd "$(dirname "$APP")" && pwd)/$(basename "$APP")"
EXECUTABLE="$APP/Contents/MacOS/Portal"
TMP="$(mktemp -d -t portal-prompt-e2e)"
PROMPT_PID=""
trap 'if [ -n "$PROMPT_PID" ]; then kill "$PROMPT_PID" 2>/dev/null || true; fi; rm -rf "$TMP"' EXIT

[ -x "$EXECUTABLE" ] || { echo "prompt e2e: executable not found: $EXECUTABLE" >&2; exit 1; }

run_prompt() {
  local case_name="$1" button="$2" secret="$3" remembered="$4" touch_id="$5" expected="$6"
  local timeout_secs="${7:-10}"
  local request="$TMP/$case_name.request" output="$TMP/$case_name.output"
  python3 - "$request" "$remembered" "$touch_id" "$timeout_secs" <<'PY'
import json, pathlib, sys
pathlib.Path(sys.argv[1]).write_text(json.dumps({
    "label": "portal-e2e",
    "requester": "native prompt verification",
    "host": "local-test",
    "mode": "askpass",
    "target": "Password:",
    "remembered": sys.argv[2] == "true",
    "touch_id_enroll": sys.argv[3] == "true",
    "timeout_secs": int(sys.argv[4]),
}) + "\n")
PY

  "$EXECUTABLE" --prompt <"$request" >"$output" 2>"$TMP/$case_name.stderr" &
  PROMPT_PID=$!
  local ready=0
  for _ in $(seq 1 200); do
    if osascript -e "tell application \"System Events\" to tell first process whose unix id is $PROMPT_PID to get count of windows" 2>/dev/null | grep -q '^1$'; then
      ready=1
      break
    fi
    kill -0 "$PROMPT_PID" 2>/dev/null || break
    sleep 0.05
  done
  [ "$ready" = 1 ] || { echo "prompt e2e: $case_name alert not ready" >&2; cat "$TMP/$case_name.stderr" >&2; exit 1; }

  if [ "$remembered" = false ]; then
    field_label="$(osascript -e "tell application \"System Events\" to tell first process whose unix id is $PROMPT_PID to get description of text field 1 of window 1")"
    [ "$field_label" = "Credential password" ] || {
      echo "prompt e2e: secure field accessibility label missing" >&2
      exit 1
    }
  fi
  if [ -n "$secret" ]; then
    osascript -e "tell application \"System Events\" to tell first process whose unix id is $PROMPT_PID to set value of text field 1 of window 1 to \"$secret\"" >/dev/null
  fi
  if [ "$button" = __escape__ ]; then
    osascript -e "tell application \"System Events\" to tell first process whose unix id is $PROMPT_PID to key code 53" >/dev/null
  elif [ "$button" = __timeout__ ]; then
    :
  else
    osascript -e "tell application \"System Events\" to tell first process whose unix id is $PROMPT_PID to click button \"$button\" of window 1" >/dev/null
  fi

  local exited=0
  for _ in $(seq 1 200); do
    if ! kill -0 "$PROMPT_PID" 2>/dev/null; then exited=1; break; fi
    sleep 0.05
  done
  [ "$exited" = 1 ] || { echo "prompt e2e: $case_name did not exit" >&2; exit 1; }
  wait "$PROMPT_PID"
  PROMPT_PID=""

  python3 - "$output" "$expected" "$secret" <<'PY'
import json, pathlib, sys
value = json.loads(pathlib.Path(sys.argv[1]).read_text())
expected, entered = sys.argv[2], sys.argv[3]
assert value["outcome"] == expected, value
if expected in ("allow-once", "allow-remember") and entered:
    assert value.get("secret") == entered, "approved secret was not returned"
else:
    assert not value.get("secret"), "secret escaped a non-secret outcome"
PY
}

run_prompt allow-once "Allow Once" "portal-e2e-secret" false false allow-once
run_prompt empty-secret "Allow Once" "" false false deny
run_prompt deny "Deny" "not-returned" false false deny
run_prompt cancel __escape__ "not-returned" false false deny
run_prompt timeout __timeout__ "not-returned" false false timeout 1
run_prompt remembered "Approve" "" true true allow-remember
run_prompt forget "Forget" "" true true forget

echo "==> native credential prompt verified (allow, empty-deny, deny, cancel, timeout, remembered, forget)"
