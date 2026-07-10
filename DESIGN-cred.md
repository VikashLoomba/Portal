# Portal Credential Sharing + Askpass (`portal keychain`) — Cred Contract

**Status:** Approved direction (first post-v0.3.0 feature; product-value cut ratified 2026-07-09).
**Audience:** repo maintainer + implementation agents (codex GPT-5.6 max implements; Claude drivers verify; Fable principal reviews).
**Related:** `DESIGN-service-registration.md` (the `registry.call` waiter machinery the cred service rides),
`DESIGN-local-core-api.md` §6/§7 (clip-shim + capability/audit conventions this mirrors),
`DESIGN-exec-bootstrap.md` (contract format precedent), `internal/clipshim` (deploy rails being extended).

---

## 1. Problem & decision

A coding agent on the dev box routinely needs a credential it must never see: a webapp login for the
dev server it is testing, or the user's password for a `sudo` command. Today the agent either asks the
user to paste the secret into the conversation (secret enters the transcript/context forever) or the
task dead-ends. portal already owns the perfect delivery path: an authenticated Mac↔box connection, a
shim-deploy mechanism that intercepts well-known binaries, per-service capability negotiation, feature
gates, and an audit log.

**Decision:** ship a `cred` service — "clip-read for secrets" — with two user-visible surfaces on the box:

1. **`portal keychain run`** — the agent wraps any command; the Mac pops a native secure-input dialog
   showing the label + requester; on approval the secret is injected into the child process (env var or
   stdin) and the agent sees only the child's stdio and exit code.
2. **Transparent sudo** — a `sudo` wrapper shim + `SUDO_ASKPASS` helper: when an agent (no TTY) runs
   `sudo <cmd>`, the Mac dialog pops, the password flows down the existing pipe directly into sudo's
   askpass pipe, and the command proceeds. Interactive (TTY) sudo is untouched.

**Keychain-remember is v1** (product rule: never slice by implementation convenience). The first
approval dialog offers "Allow & Remember"; the secret is stored in the **macOS Keychain**
(`security add-generic-password`, service `portal-cred`) and every later request for the same label is
a **click-to-approve confirmation** — no retyping. Retype-every-time is a *worse* security posture (it
trains users to type passwords into popups — the exact prompt-fatigue attack this design defends
against) and betrays the `portal keychain` name. The 2026-07-09 spike (§6) empirically confirmed the
read-back path is silent, so nothing external blocks this.

**Out of scope (v1), each with its real constraint named:**

- **Touch ID approval** — requires a signed native helper (LocalAuthentication); the release pipeline
  is pure-Go `CGO_ENABLED=0` cross-compiled from ubuntu-latest and has no macOS signing lane. v2, with
  the identical consent flow (the click becomes a fingerprint).
- **`SSH_ASKPASS` exported by default** — OpenSSH only consults it without a TTY when `DISPLAY` is set,
  and `SSH_ASKPASS_REQUIRE=prefer` would hijack *interactive* passphrase prompts in `portal exec` PTY
  sessions. That UX needs its own design. The `portal-askpass` helper itself is generic — a user can
  point `SSH_ASKPASS` at it manually today; documented, not defaulted.
- **Control-API surfacing of pending prompts** (so a GUI app can render approval UI) — a different user
  story (third-party desktop apps), not part of the two named scenarios. The built-in dialog is the product here.
- **`git-credential-portal` helper** — natural fast-follow; needs structured (host/protocol/username)
  fields rather than a free label. The `keychain run` primitive already covers git via env injection.
- **Non-Linux dev boxes** — existing platform scope (unchanged).
- **`doctor` integration** — doctor is deliberately non-interactive; a cred self-test would pop a
  dialog. Manual verification (§8) covers the path instead.

**Threat model (document this as plainly as the OSC 52 heads-up):** the guarantee is that the secret
never enters the agent's context window, transcript, argv, logs, or the box's disk — it travels
in-memory: Mac Keychain/dialog → daemon → SSH pipe → portald → consumer process (child env / sudo's
pipe). It is **not** a defense against an actively malicious same-UID process on the box, which can
read a child's `/proc/<pid>/environ` or ptrace it. Same-UID isolation on Linux is impossible without
containers; the design goal is keeping secrets out of LLM context and durable records, and the consent
dialog + audit log are the control points. The wire adds no new exposure: clipboard **text** (equally
sensitive) already rides this channel.

---

## 2. Locked decisions

| # | Decision | Detail |
|---|---|---|
| C1 | **`cred` service, no ProtoVersion bump** | New service `"cred"` version 1 negotiated via the existing symmetric `Services map[string]uint32` (Hello/HelloAck). ProtoVersion stays 4 — a peer that doesn't advertise `cred` leaves the handlers dormant (exactly how a v0.3.0/new mix degrades: `keychain run` answers "no Mac support" cleanly). New payload structs in `pkg/protocol/messages.go` riding `Msg.Payload`: `CredRequest{Nonce, Epoch uint64; Label, Requester, Mode, Target string}` (agent→client; `Mode ∈ {"env","stdin","askpass"}`; `Target` = env-var name, command summary, or askpass prompt) and `CredResponse{Nonce, Epoch uint64; OK bool; Secret []byte; Err string}` (client→agent; `Err ∈ {"denied","timeout","disabled","cooldown","gui-unavailable","label-invalid","no-client","busy"}`). `busy`/`no-client`/`timeout` may be generated as box-local socket denials; the Mac never sends `no-client`, but may send `busy` defensively when a second request races an open dialog. **The secret rides in-band** (unlike clip's out-of-band SHA files): secrets are small and must never touch the box's disk. Caps: `Label ≤ 200` bytes, `Requester/Target ≤ 300` bytes, `Secret ≤ 4096` bytes; `MaxPayload() = 8192`. `docs/wire.cddl` + Go/TS golden vectors extended (C9). |
| C2 | **`pkg/agent/svc_cred.go` mirrors `svc_clip.go`, inflight 1** | New compiled-in service: `Name()="cred"`, `Version()=1`, `OutboxCap()=2`, cmd-socket verb `cred`. Uses `host.Call` with `maxInflight = 1` (one human, one dialog — a second concurrent ask gets an immediate busy-deny, not a queue). Timeout budget (C10) is its own — NOT clip's 9s. Gates on `HasClient() && ClientHas("cred")`; adverse paths (no client, cap hit, timeout, marshal fail) answer a deny line, never hang. Same field-not-const pattern as clipService so tests can shorten timeouts per-instance. |
| C3 | **cmd-socket framing: base64, single-line, binary-safe** | Request line from portald: `cred\t<base64(CBOR CredShimReq{Label, Mode, Target, Requester})>\n` (base64 keeps attacker-influenced labels from corrupting the tab/newline framing). Reply: `ok\t<base64(secret)>\n` or `deny\t<reason>\n` with reason from C1's set, plus portald-generated `multiple-clients` and `invalid-response`; those two reasons are box-local and never sent by the Mac. `busy`/`no-client`/`timeout` may also be generated locally; the Mac never sends `no-client`, but may send `busy` defensively when a second request races an open dialog. The verb's socket deadline is C10's, applied via `Verb.Deadline` (routeVerb) like clip. |
| C4 | **`portald keychain` subcommand family (box side)** | New `keychain` case in portald's dispatch. `portald keychain run --label <L> (--env NAME \| --stdin) -- <argv...>`: dials the cmd socket (same fanout/single-agent refusal semantics as `runClip`), sends the C3 line, and on `ok` **delivers without the calling shell ever seeing the secret**: `--env` validates `NAME` against `[A-Za-z_][A-Za-z0-9_]*` then `syscall.Exec`s the child with `NAME=<secret>` appended to the environment (no lingering parent); `--stdin` runs the child via os/exec with the secret + `\n` as its entire stdin, propagating the child's exit code. Exit codes: child's on success; **111** = denied, **112** = timeout, **2** = usage; stderr messages are agent-legible ("portal keychain: denied by user on the Mac"). Portald emits box-local `multiple-clients` when fanout finds more than one distinct agent and `invalid-response` when the selected agent's reply is malformed; neither reason is sent by the Mac. `portald keychain askpass [prompt...]`: sudo/ssh invoke it with the prompt as argv; it sends `Mode="askpass"`, `Target=<prompt>`, and on `ok` prints the secret + `\n` to stdout (consumed by sudo's pipe — never by the agent) or exits 1 on deny. Requester context: read `/proc/<ppid>/cmdline` (self's parent), format `pid <ppid>: <cmdline>`, truncate to 300 bytes. `--help` for `keychain`/`run`/`askpass` is **agent-first**: written for an LLM that discovers the tool via `--help`, with copy-pasteable examples INCLUDING the quoting subtlety (`portal keychain run --label "staging admin" --env PW -- sh -c 'curl -d "pass=$PW" …'` — single quotes so the *child* shell expands `$PW`). |
| C5 | **Box shims (clipshim `Version` 3→5)** | Three new shims in `internal/clipshim` (new file `credshim.go`, same package — the package is portal's box-shim deployer, name kept to avoid churn): (i) **`~/.local/bin/portal`** — passthrough `exec "$_portald" "$@"` with a clear error if portald is missing, so the agent-facing name matches the Mac (`portal keychain …`); (ii) **`~/.local/bin/portal-askpass`** — `exec "$_portald" keychain askpass "$@"`; (iii) **`~/.local/bin/sudo`** — the seamless wrapper: resolve the real sudo via the proven PATH-exclusion trick (`grep -vxF` on our dir); passthrough verbatim when stdin is a TTY, or when sudo's own leading options contain `-A/--askpass/-S/--stdin/-n/--non-interactive/-e/--edit/-h/-V/-K/-k/-v`, or when `SUDO_ASKPASS` is unset/non-executable, or when no real sudo exists (error); otherwise `exec <real-sudo> -A "$@"`. The option scan stops at `--` or the first command token so command-specific flags do not suppress askpass. All three carry the `Marker`, get the backup/restore dance (`deployShim`), and are removed/restored by `Remove()`. Env: a **separate new marker block** (`# >>> portal askpass (sudo) >>>` / `# <<< portal askpass (sudo) <<<`) appended to the same `rcFiles`, exporting `SUDO_ASKPASS="$HOME/.local/bin/portal-askpass"` guarded by `[ -x ... ]` — a separate block because `ensurePathPrepend` only appends when the start marker is absent, so mutating the existing clip-shims block would never re-converge on existing installs. `Remove()` strips both blocks. |
| C6 | **Mac handler: serialized dialogs, cooldown, explicit delivery copy** | `runCredHandler` in `cmd/portal` mirrors `runClipHandler`: consumes a new dedicated `CredEvents()` channel (cap 2) from `pkg/agentclient` (new `KindCredRequest` decoder registered for service `cred`, advertised in `Hello.Services` automatically via the registry), serializes with a semaphore of 1, answers via new `SendCredResponse`. If a second request races while the dialog semaphore is held, the Mac handler defensively answers `busy`. Serve path: feature gate `cred` off → deny `disabled`; label empty/oversized after sanitization (strip control chars, cap per C1) → `label-invalid`; label in **deny-cooldown** (10s after an explicit Deny/Cancel, keyed by label) → deny `cooldown` with NO dialog (anti-spam); otherwise prompt (C7). Every outcome audited (C8). |
| C7 | **Dialog UX + Keychain remember (v1)** | New `internal/prompt` package behind an interface (`Prompter`) so all handler tests run hermetically with a fake; the darwin impl shells to `osascript` using the proven `appleScriptStr` escaping. **Dialog A (no remembered item):** `display dialog` with hidden answer; text = label + `requested by <requester> on <host>` + explicit delivery ("will be set as env var PW for: sh -c …" / "will be piped to sudo"); buttons `{"Cancel","Allow Once","Allow & Remember"}`, default `Allow Once`, cancel `Cancel`, `giving up after 120`. **Dialog B (remembered item exists):** confirmation only (no text field); buttons `{"Deny","Forget","Allow"}`, default `Allow`, cancel `Deny`, `giving up after 120`; `Forget` deletes the stored item and falls through to Dialog A. New `internal/keychain` package: store/lookup/delete via `/usr/bin/security` with commands fed on **stdin via `security -i`** (the secret must NEVER appear in argv — `ps` exposure); service `portal-cred`, account = label, update with `-U`, **no `-T` flag at all** (§6: creator-trust via the `/usr/bin/security` child is the robust path; `-T ''` semantics are explicitly not built on). A locked keychain or any `security` read failure is treated as "no remembered item" → Dialog A (graceful). A labels-only index at `~/.config/portal/cred-labels` (0600) tracks remembered labels for list/forget (the `security` CLI cannot enumerate by service without a slow `dump-keychain`); list/forget tolerate index/keychain drift. osascript failure (no GUI session) → deny `gui-unavailable`. |
| C8 | **Audit events** | `internal/audit` gains: `CredServed(host, label, mode, source, dur)` with `source ∈ {prompt, prompt-remembered, keychain}` (fresh entry / fresh entry + stored / served from Keychain after click-approve), `CredDenied(host, label, mode, reason)` (reason from C1's `Err` set + `user-denied` split from timeout), `CredForgotten(host, label)`. The secret value appears in NO log line, NO error string, NO argv on either machine (grep-gated in u8). |
| C9 | **Wire spec + vectors** | `docs/wire.cddl` gains `cred-request` / `cred-response` group entries in the msg-payload union; golden vectors added under `docs/vectors/` and asserted from BOTH the Go vector tests and the TS vector tests (`clients/ts`), keeping the any-language-conformance property true. |
| C10 | **Timeout budget (human-scale, unlike clip)** | Dialog `giving up after` **120s** < agent `credTimeout` **130s** (host.Call wait) < cred `Verb.Deadline` **135s** (cmd-socket) < portald keychain socket-read deadline **140s** (outer bound). A pending cred waiter blocks nothing: heartbeats and other services interleave via the Serve loop's merged outbox, so a 2-minute human pause cannot trip the 12s heartbeat reconnect (unlike clip, whose 9s ceiling exists for paste UX, not protocol safety). |
| C11 | **Mac CLI: `portal keychain list\|forget <label>`** | On the Mac, `portal keychain` manages remembered credentials: `list` prints the index labels; `forget <label>` deletes the Keychain item + index entry (tolerating either already gone) and audits `CredForgotten`. Registered in root.go under a new help entry; the box-side `portal keychain run/askpass` asymmetry is stated in both helps. Keychain Access.app remains the system-level escape hatch (its deletions are tolerated via the drift rule in C7). |
| C12 | **Feature gate + surfaces** | `internal/config` gains `FeatureCred = "cred"` (file `feature.cred`, default ON like every gate — nothing is ever served without a per-request human click, so the gate exists for "never even prompt me"). `cmd/portal/features.go` featureNames + both "known:" strings gain `cred`; README capability-gates table goes to 5 rows; README gains a "Credential sharing (`portal keychain`)" section + the §1 threat-model paragraph; root help (`helpText`) Capabilities/Sessions sections updated. |

---

## 3. File contract

### 3.1 New files

| Path | Purpose |
|---|---|
| `pkg/agent/svc_cred.go` (+`_test.go`) | C2/C3: the box-side cred service (verb, Call, deny mapping). |
| `cmd/portald/keychain.go` (+`_test.go`) | C4: `keychain run/askpass` subcommands, socket fanout, env/stdin delivery, agent-first help. |
| `internal/clipshim/credshim.go` (+`_test.go`) | C5: `portal`, `portal-askpass`, `sudo` shim scripts + askpass env marker block. |
| `internal/prompt/prompt.go`, `prompt_darwin.go` (+`_test.go`) | C7: `Prompter` interface, osascript Dialog A/B impl, fake for tests. |
| `internal/keychain/keychain.go` (+`_test.go`) | C7: `security -i` store/lookup/delete + labels index (exec seam faked in tests). |
| `cmd/portal/run_cred.go` (+`_test.go`) | C6: `runCredHandler` — gate, sanitize, cooldown, prompt, keychain, respond, audit. |
| `cmd/portal/keychain.go` (+`_test.go`) | C11: Mac-side `portal keychain list|forget`. |

### 3.2 Modified files

| Path | Change |
|---|---|
| `pkg/protocol/messages.go` (+ vector tests) | C1: `CredRequest`/`CredResponse` payload structs. |
| `docs/wire.cddl`, `docs/vectors/*`, `clients/ts` vector tests | C9: cred entries + golden vectors (Go + TS). |
| `pkg/agentclient/client.go`, `registry.go` (+tests) | C6: `cred` decoder + dedicated channel + `CredEvents()` + `SendCredResponse` (mirrors clip's). |
| `pkg/agent/server_test.go` or service tests | C2: registration/dormancy coverage for the new service. |
| `cmd/portald/main.go` | C4: `keychain` dispatch case + top-level help line. |
| `internal/clipshim/clipshim.go` (+tests) | C5: `Version = "5"`, shims table + Remove() gain the three shims + both env blocks. |
| `internal/config/config.go` (+tests) | C12: `FeatureCred`. |
| `internal/audit/audit.go` (+tests) | C8: `CredServed`/`CredDenied`/`CredForgotten`. |
| `cmd/portal/run.go` | C6: wire `runCredHandler` next to the clip/notify handlers. |
| `cmd/portal/features.go` | C12: add `cred` to featureNames + "known:" strings. |
| `cmd/portal/root.go` | C11/C12: register keychain cmd; helpText additions. |
| `README.md` | C12: gates table, keychain section, threat-model paragraph, usage block. |

---

## 4. Implementation order (green after every unit)

| Unit | Scope |
|---|---|
| u1 | **C1/C9 protocol**: `CredRequest`/`CredResponse`, `wire.cddl`, Go + TS golden vectors. Pure additive. Green (`make test` + `make test-ts`). |
| u2 | **C2/C3 agent service**: `svc_cred.go` registered; verb framing, deny mapping, inflight-1, timeout fields; tests mirror `svc_clip_test` (incl. no-client, cap-hit, timeout, stale-epoch). |
| u3 | **C4 portald keychain**: subcommands, fanout, `--env` validation + `syscall.Exec`, `--stdin` pipe, exit codes 111/112, requester capture, agent-first help. Hermetic tests over a fake cmd socket. |
| u4 | **C7/C8/C12 Mac primitives**: `internal/prompt` (interface + darwin osascript impl), `internal/keychain` (exec-seam tests), `FeatureCred`, audit funcs. No wiring yet. |
| u5 | **C6 Mac wiring**: agentclient cred channel + `SendCredResponse`; `runCredHandler` with fake Prompter/keychain covering every outcome (allow-once / allow-remember / remembered-allow / forget-fallthrough / deny / timeout / cooldown / disabled / gui-unavailable / label-invalid / oversize secret). |
| u6 | **C5 shims**: credshim.go scripts, Version 4, deploy + Remove coverage (marker greps, sudo wrapper passthrough matrix as script-content assertions). |
| u7 | **C11/C12 surface**: Mac `portal keychain list|forget`, features list, root help, portald help polish, README. |
| u8 | **Hardening**: refine sudo's leading-option scan and bump shim `Version` to 5; add direct `internal/osa` escaping tests; run `GOFLAGS=-trimpath make test` (CI parity), `go test -race ./...`, gofmt, and greps — secret never in argv (`security -i` only; no `add-generic-password …-w <arg>` form), secret never logged/formatted, no new deps (`go.mod` unchanged), exit-criteria sweep. |

---

## 5. Exit criteria

1. `make build`, `go vet ./...`, `GOFLAGS=-trimpath make test`, `go test -race ./...`, `make test-ts` all green; changed packages gofmt-clean; `go.mod` unchanged.
2. **C1/C9:** vectors round-trip `CredRequest`/`CredResponse` byte-identically from Go and TS; a `Secret` > 4096 or `Label` > 200 is rejected at the Mac handler (deny `label-invalid`/oversize) — never a frame-cap panic.
3. **C2:** with no client / no `cred` advertisement, the verb answers `deny\tno-client` immediately; inflight-1 proven (second concurrent ask denied busy while first pends); stale-epoch response dropped (mirrors clip test).
4. **C4:** `--env` rejects invalid names; on approval the child sees `NAME=<secret>` and the parent is replaced (`syscall.Exec`); `--stdin` child reads exactly secret+`\n` then EOF; deny → exit 111, timeout → 112, child's own exit code propagates otherwise; askpass prints secret+`\n` on stdout ONLY on approval.
5. **C5:** all three shims deploy with marker + backup, `Remove()` restores a pre-existing sudo/portal binary and strips BOTH rc marker blocks; the sudo wrapper's passthrough matrix (TTY / leading -A / -S / -n / command flags / no-SUDO_ASKPASS / no-real-sudo) is asserted against the script text and behavior; `Version = "5"` re-converges an existing v4 box.
6. **C6/C7:** every runCredHandler outcome audited exactly once with the right event/reason; cooldown suppresses the dialog for 10s after an explicit deny; dialogs serialized; label sanitized before osascript; keychain lookup failure falls back to Dialog A; Forget deletes then falls through.
7. **C8:** a grep proves no code path formats/logs the secret and no `security` invocation carries it in argv.
8. **C12:** `portal features` lists 5 gates; `feature.cred off` → deny `disabled` with no dialog; README/help updated.

---

## 6. Keychain ACL spike verdict (empirical, 2026-07-09, macOS 26.5.1 arm64)

Question: does `security find-generic-password -w` from portal's **ad-hoc-signed** daemon read back its
own item silently, or does macOS interpose a SecurityAgent prompt? **Verdict: silent, in every tested
configuration.** The ACL subject is the `/usr/bin/security` **child** (Apple platform-signed), not the
Go parent — and the item's creator (`/usr/bin/security`) is trusted by default, so reader == creator
and no prompt fires. Confirmed: baseline no-`-T` add → silent read (exit 0); `-T ''` → still silent;
ad-hoc Go binary exec'ing the read → silent; `-U` update-in-place → silent; plaintext absent from
metadata queries; deletes verified (final finds exit 44). Design consequences locked into C7:
**use the default no-`-T` path** (do not build on `-T ''` semantics); a **locked login keychain** (not
tested — GUI-session keychains unlock at login; lock-on-sleep or headless contexts differ) makes the
read fail → treated as "no remembered item" → Dialog A, a graceful degrade, never a hang.

---

## 7. Risks

| Risk | Mitigation |
|---|---|
| Prompt-fatigue / label spoofing (a hostile box process spams or mimics dialogs) | Serialized dialogs (sem 1) + inflight-1 at the agent; 10s deny-cooldown per label; label sanitized + length-capped; requester cmdline + host shown; delivery ("env PW for …" / "piped to sudo") explicit; portal chrome (title) fixed and never attacker-fed; remember-flow means the common case is a click, not a retype. |
| Secret leaks via argv/ps or logs | `security -i` with stdin-fed commands on the Mac; in-band CBOR on the wire (no temp files); syscall.Exec env injection on the box; u8 grep gates. |
| `sudo` wrapper breaks real sudo usage | TTY + leading-sudo-option passthrough matrix (C5) asserted in tests, including command flags after the command token; backup/restore via the proven deployShim dance; manual verification (§8) exercises interactive sudo; worst-case escape hatch: `portal features cred off` + uninstall restores everything. |
| osascript dialog unavailable (no Aqua session) | Deny `gui-unavailable` immediately, agent-legible error at the shim; never hangs the agent. |
| Human-scale timeouts vs protocol liveness | C10 budget ordered outside-in; waiter never blocks the Serve loop; heartbeats interleave (proven by the same machinery clip uses; a test pins a pending cred call not delaying heartbeats). |
| rc-block migration on existing installs | New env export lives in its OWN marker block (C5) so existing boxes converge by append; `Version=5` re-deploys refined shims via the existing marker grep. |
| Requester/label are attacker-controlled display data | Documented (§1 threat model); rendered as data (sanitized, quoted, truncated); the only trusted dialog fields are host + portal chrome. |
| Locked keychain / Keychain Access drift | Read failure ⇒ Dialog A; list/forget tolerate missing items on either side (C7/C11). |

---

## 8. Manual verification (live box, post-merge)

1. **Scenario 1 end-to-end:** on the box, `portal keychain run --label "spike test" --env PW -- sh -c 'echo "len=${#PW}"'` → Mac dialog shows label, requester, "env PW", host; Allow Once → prints `len=<n>`, exit 0; the secret never appears in the box command's output or portal logs.
2. **Deny + cooldown:** repeat, hit Deny → exit 111 with agent-legible stderr; immediately re-run → denied `cooldown` with NO dialog; after 10s a fresh dialog appears.
3. **Scenario 2 sudo:** `ssh vikash-system sudo whoami` (no TTY) → Mac dialog ("piped to sudo") → `root`; interactive `portal exec` then `sudo whoami` → in-terminal password prompt as before (wrapper passthrough).
4. **Remember flow:** Allow & Remember on a label → item visible in Keychain Access under `portal-cred`; re-run → Dialog B (click Allow, no typing) → served; `portal keychain list` shows the label; `portal keychain forget <label>` → next ask is Dialog A again.
5. **Gate:** `portal features cred off` → box-side ask fails fast, agent-legible; audit shows `cred-denied … reason=disabled`; `on` restores.
6. **Timeout:** let a dialog sit 120s → auto-dismiss, box exits 112.
7. **Uninstall:** `portal uninstall` → box `sudo`/`portal` shims removed/restored, both rc marker blocks stripped, `SUDO_ASKPASS` gone from fresh shells.
8. **Audit:** `~/.config/portal/audit.log` contains exactly one line per outcome above, none containing the secret.
