# Portal Credential Touch ID Approval — the v2 the Cred Contract Promised

**Status:** Proposed. Companion/amendment to `DESIGN-cred.md` (CRED), whose §1 deferred exactly
this: *"Touch ID approval … v2, with the identical consent flow (the click becomes a
fingerprint)."*
**Audience:** repo maintainer + implementation agents.
**Sequencing:** branches off main AFTER the clipboard-write feature merges (both touch the
features table; this lands `cred-touchid` as the 7th gate).

---

## 1. Problem & decision

CRED v1 shipped the remember flow: the first sudo askpass pops Dialog A (type the password once,
"Allow & Remember"), and every later request for that label is Dialog B — a click on "Allow".
The click is deliberately low-friction, but it is also the weakest consent gesture we could ask
for: it is one Return keystroke, it is what every prompt-fatigue attack trains users to emit, and
it carries no proof that the person at the keyboard is the Mac's owner.

**Decision:** when the Mac has usable biometrics, the remembered-item click becomes a **Touch ID
(or Apple Watch) authentication**, and the sudo/askpass first-type flow **enrolls by default**:

1. **Enrollment happens the first time you type the password for a sudo request.** Dialog A in
   askpass mode (biometry available) makes **"Allow & Remember" the default button** and states
   plainly: typing the password once and pressing Return stores it in the Keychain and turns
   every subsequent approval for that credential into a fingerprint. "Allow Once" remains one
   click away for users who refuse storage; env/stdin modes keep v1's "Allow Once" default.
2. **Subsequent sudo requests authenticate with Touch ID.** The remembered-item path evaluates
   `LAPolicyDeviceOwnerAuthenticationWithBiometricsOrWatch` (falling back to plain biometrics)
   with a reason carrying the label + host; success releases the Keychain secret down the
   existing pipe. Cancel is a denial (with CRED's cooldown). Biometry unavailable, lockout, or
   evaluation failure **falls back to Dialog B exactly as v1** — the click path never gets worse.

**The v1 constraint is dissolved, not waived.** CRED §1 deferred Touch ID because
LocalAuthentication seemed to require a signed native helper, and the release pipeline is pure-Go
`CGO_ENABLED=0` cross-compiled on ubuntu with no macOS signing lane. The 2026-07-27 spike (§6)
proves no helper is needed: **unsigned `/usr/bin/osascript -l JavaScript` drives `LAContext`
through the JXA ObjC bridge** — the same cgo-free shell-out pattern `internal/prompt`,
`internal/clip`, and the notifier already use. No cgo, no signing, no new dependency.

**What this feature is NOT (named constraints, CRED-style honesty):**

- **It does not re-bind the Keychain item to biometrics.** The item stays a legacy
  `security`-created generic password readable by the same flow that wrote it (CRED §6). Binding
  the *item itself* to `kSecAccessControl` biometry requires the data-protection keychain and an
  entitled, signed app — the signing-lane constraint stands for that half. Touch ID here gates
  **portal's release decision** (the consent), hardening the prompt-fatigue surface; it is not a
  same-UID-attacker defense, which CRED §1's threat model already excludes.
- **The system sheet is attributed to "osascript"**, not "portal" — cosmetics of the unsigned
  path. The localized reason carries the portal context (label + host, sanitized). Accepted, and
  consistent with every other osascript surface portal owns.
- **No SSH_ASKPASS / non-sudo scope change.** The flow triggers wherever CRED triggers; nothing
  new is intercepted.

---

## 2. Locked decisions

| # | Decision | Detail |
|---|---|---|
| T1 | **Zero wire change** | No new frames, verbs, reasons, or ProtoVersion/service bumps. `CredRequest`/`CredResponse` and the C1 `Err` set are untouched; Touch ID cancel maps to `denied`, deadline overrun to `timeout`. The feature is entirely Mac-side. Box shims untouched — no clipshim `Version` bump. |
| T2 | **`internal/prompt` gains a `Biometry` seam** | Same package (Touch ID is a consent surface): `type Biometry interface { Available(ctx) bool; Approve(ctx, reason string, deadline time.Time) (BiometryOutcome, error) }` with `BiometryOutcome ∈ {BiometryApproved, BiometryCanceled, BiometryTimeout, BiometryFallback}`. `New()`-style constructor `NewBiometry()` returns the platform impl; non-darwin stub answers `Available()=false`. A concurrency-safe `BiometryFake` mirrors `prompt.Fake` for handler tests. |
| T3 | **darwin impl: JXA over the existing `scriptRunner` seam** | Two scripts run via `osascript -l JavaScript` (runner seam extended to carry the `-l JavaScript` flag; the AppleScript dialogs keep their current invocation): (a) **probe** — `canEvaluatePolicyError(4)` then `(1)`, printing `available:4` / `available:1` / `unavailable`; (b) **approve** — `LAContext` with `localizedCancelTitle="Deny"` and `localizedFallbackTitle=""` (hides "Enter Password…" — portal's fallback is its OWN Dialog B, never the login password), `evaluatePolicyLocalizedReasonReply` wrapped in try/catch (§6: invalid input raises a catchable exception), reply block + `NSRunLoop` pump (§6: proven to fire), an in-script `NSDate` deadline printing `touchid:timeout`, and stdout tokens `touchid:approved` / `touchid:canceled` / `touchid:fallback:<laerror>` / `touchid:timeout`. The reason string is embedded via `strconv.Quote` after the existing control-strip + truncate sanitization (valid JS string literal; no new escaping machinery). Policy pick: 4 (biometrics **or Apple Watch** — clamshell-friendly) when available, else 1. |
| T4 | **LAError → outcome mapping** | reply(true) → `Approved`. `-2 userCancel` → `Canceled`. In-script deadline → `Timeout`. EVERYTHING else → `Fallback` (Dialog B): `-1 authenticationFailed` (dirty finger after the sheet's own retries — the click is the consent equivalent, denying would be wrong), `-3 userFallback` (defensive; the button is hidden), `-4 systemCancel`, `-8 biometryLockout`, `-10 appCancel`, `-1004 invalidContext`, bridge exceptions, malformed output, runner errors. Fail-open INTO the v1 dialog, never past it: no path serves a secret without either a fingerprint/watch approval or a Dialog B click. |
| T5 | **Handler flow (`cmd/portal/run_cred.go`)** | `credServeDeps` gains `Biometry prompt.Biometry` (nil ⇒ unavailable). Remembered path becomes: gate `cred-touchid` on AND `Biometry.Available()` → `Approve(reason, deadline)`: `Approved` → `KC.Get` → serve with **audit source `keychain-touchid`**; `Canceled` → `Cooldown.record` + deny `denied`; `Timeout` → deny `timeout`; `Fallback` → **Dialog B verbatim v1** (source stays `keychain` so audit distinguishes fingerprint from click). Reason text: `portal: approve credential "<label>" for <host>` (both fields already sanitized/truncated). Fresh path: `TouchIDEnroll` (T6) set when `mode=="askpass" && !remembered && gate && Available`. The gate off or biometry absent reproduces v1 byte-for-byte. |
| T6 | **Dialog A enroll variant** | `prompt.Request` gains `TouchIDEnroll bool`. `dialogScript` with it set: default button becomes `"Allow & Remember"` and the message gains the line `Remember stores this in your Mac Keychain; future approvals for this credential use Touch ID.` Button labels and result tokens are UNCHANGED (`Cancel` / `Allow Once` / `Allow & Remember`), so `parseDialogResult` is untouched. Env/stdin modes and non-biometry Macs never set the flag. |
| T7 | **Timeout budget: C10 unchanged** | The Touch ID attempt spends from the same 115s dialog budget: `Approve` receives the running request's deadline; its in-script deadline is the remaining seconds (min 5s rule via the existing `credPromptTimeoutSecs` shape) minus a 1s guard so the script exits cleanly before the Go-side ctx kill. A `Fallback` dialog then receives only what remains — exactly how Forget re-prompts already work. The 130s/135s/140s outer ordering is untouched. |
| T8 | **Gate + audit + surfaces** | `internal/config` gains `FeatureCredTouchID = "cred-touchid"`, default ON; off → v1 click flow (the gate exists for "never biometrics", not for safety — every path still requires explicit consent). `cmd/portal/features.go` featureNames + both "known:" strings gain it (7 gates post-clipboard-write). Audit: `CredServed` source set gains `keychain-touchid` (signature unchanged — source is already a string); no secret ever appears in the JXA script text, argv, stdout, or logs (the approve path carries NO secret — the secret comes from `KC.Get` after approval, as today). `portal keychain list` prints a `touch id: available|unavailable` header line (non-interactive probe). README cred section + threat-model paragraph updated with §1's honesty items; root helpText mentions Touch ID. |

---

## 3. File contract

### 3.1 New files

| Path | Purpose |
|---|---|
| `internal/prompt/biometry.go` (+`_test.go`) | T2: `Biometry` interface, `BiometryOutcome`, `NewBiometry()`, `BiometryFake`. |
| `internal/prompt/biometry_darwin.go` | T3/T4: JXA probe + approve scripts over the runner seam, token/LAError parsing. |
| `internal/prompt/biometry_stub.go` | T2: non-darwin `Available()=false` stub. |

### 3.2 Modified files

| Path | Change |
|---|---|
| `internal/prompt/prompt.go` | T6: `Request.TouchIDEnroll`. |
| `internal/prompt/prompt_osa.go` (+tests) | T6: enroll-variant `dialogScript`; T3: runner carries the osascript language flag. |
| `internal/prompt/prompt_darwin.go` | T3: construct the JS-capable runner; `NewBiometry()` darwin wiring. |
| `cmd/portal/run_cred.go` (+tests) | T5: `deps.Biometry`, remembered-path Touch ID flow, enroll flag, audit source. |
| `internal/config/config.go` (+tests) | T8: `FeatureCredTouchID`. |
| `cmd/portal/features.go` (+tests) | T8: 7th gate in featureNames + "known:" strings. |
| `cmd/portal/keychain.go` (+tests) | T8: `list` Touch ID availability header. |
| `internal/audit` (tests only) | T8: `keychain-touchid` source asserted; no signature change. |
| `README.md`, `cmd/portal/root.go` | T8: Touch ID docs + honesty paragraph. |

---

## 4. Implementation order (green after every unit)

| Unit | Scope |
|---|---|
| t1 | **T2/T3/T4 primitives**: `Biometry` interface + darwin JXA impl + stub + fake; runner-seam extension; exhaustive parse tests over faked runner outputs (approved / canceled / every fallback LAError / timeout token / malformed stdout / runner error / exception text). No handler wiring. |
| t2 | **T5/T6/T7 handler**: `credServeDeps.Biometry`, remembered-path flow, enroll flag, budget threading; `run_cred_test.go` covers every outcome matrix cell (touchid-approved / touchid-canceled+cooldown / touchid-timeout / every-fallback→DialogB / gate-off→v1 / biometry-absent→v1 / enroll-flag set only for askpass+fresh+available); `prompt_osa` enroll-variant script-text assertions. |
| t3 | **T8 surfaces**: config gate, features list, keychain list header, README, helpText; exit-criteria sweep — gofmt, `go vet`, `GOFLAGS=-trimpath make test`, `go test -race ./...`, `make test-ts`, `go.mod` unchanged, secret-never-in-JXA grep. |

---

## 5. Exit criteria

1. Full gate green (`gofmt` / `go vet` / `make agent` / `go test ./...` / `-race` / `make test-ts`); `go.mod` byte-identical.
2. **T1:** zero diffs under `pkg/protocol`, `docs/wire.cddl`, `docs/vectors`, `internal/clipshim`, `cmd/portald`.
3. **T4/T5:** the outcome matrix is fully tested; no code path serves a secret after a `Canceled`/`Timeout`; every `Fallback` lands in Dialog B with the remaining budget; cooldown recorded on Touch ID cancel exactly as on a click Deny.
4. **T6:** enroll variant asserted in script text (default button + message line) and set only for `askpass` + fresh + available + gate-on; `parseDialogResult` untouched.
5. **T8:** `portal features` lists 7 gates; `feature.cred-touchid off` reproduces v1 flows in the handler tests; audit lines distinguish `keychain-touchid` vs `keychain`; a grep proves the JXA scripts never interpolate secret bytes.
6. All-platform builds: darwin files compile cross-compiled from linux CI (`CGO_ENABLED=0`), stub keeps non-darwin green.

---

## 6. JXA LocalAuthentication spike verdict (empirical, 2026-07-27, this Mac)

Question: can unsigned `/usr/bin/osascript -l JavaScript` drive `LAContext` — availability probe,
policy evaluation, and the reply **block** — without cgo, signing, or entitlements?
**Verdict: yes, proven non-interactively:**

1. `canEvaluatePolicyError(1)` and `(4)` both returned `true` via the bridge (probe path works;
   this Mac advertises biometrics AND biometrics-or-watch).
2. `evaluatePolicyLocalizedReasonReply` with an invalid policy raises a **catchable ObjC
   exception** (`Code=-1001`), not a block callback → the impl wraps evaluation in try/catch.
3. On an invalidated context (`c.invalidate` — zero-arg ObjC methods invoke property-style in
   JXA), the **reply block fired through the NSRunLoop pump** with `ok=false errcode=-10`
   (appCancel) — the JS-function-as-block + runloop machinery is sound end to end.

The only unproven leg is the interactive success path (requires a finger on the sensor) — §8.1
covers it. Design consequences locked into T3: try/catch around evaluate; property-style
zero-arg calls; numeric LAError extraction via `$(err.code).js`.

---

## 7. Risks

| Risk | Mitigation |
|---|---|
| JXA bridge drift across macOS versions | T4's catch-everything-→-`Fallback` means any bridge breakage degrades to v1's Dialog B, never to a hang or an unserved secret; t1 parse tests pin every token path. |
| Sheet attribution "osascript" confuses users | Reason text leads with `portal:`; README documents it; identical attribution to portal's existing notification path. |
| Enroll-by-default stores a password the user didn't mean to keep | The default flip is askpass-only, biometry-only; the dialog states the storage + Touch ID consequence in the message body; "Allow Once" remains one click away; `portal keychain forget` and Keychain Access.app remove it; the `cred-touchid` gate disables the whole behavior. |
| Touch ID sheet racing the 115s budget | In-script deadline (remaining − 1s) exits cleanly with `touchid:timeout` before the Go ctx kill; both map to deny `timeout` (C10 ordering untouched). |
| Lockout after failed attempts (`-8`) | `Fallback` → Dialog B click; never a dead end. |
| Clamshell / external display (no sensor reachable) | Policy 4 lets a paired Apple Watch approve; if neither is evaluable, `Available()` is false and the request runs v1 flow with no probe latency added to the sheet path. |
| A same-UID Mac process invokes the same JXA path to mine approvals | It gains nothing: approval releases nothing by itself — the secret flows only through portal's handler, and the Keychain item was already same-UID-readable in v1 (CRED §1/§6 threat model unchanged, documented honestly). |

---

## 8. Manual verification (live Mac + box, post-merge)

1. **Enroll:** on the box (no controlling terminal) `ssh <host> sudo whoami` → Dialog A with
   default **Allow & Remember** and the Touch ID message line; type password, press Return →
   `root`; Keychain Access shows the `portal-cred` item.
2. **Touch ID serve:** repeat the sudo → Touch ID sheet (reason shows label + host); fingerprint
   → `root` with no typing; `audit.log` shows `source=keychain-touchid`.
3. **Watch serve (if paired):** close the lid, repeat → Apple Watch approval prompt works.
4. **Cancel:** repeat, click Deny on the sheet → box exits 111; immediate retry → `cooldown`
   denial with no sheet; after 10s the sheet returns.
5. **Fallback:** fail the fingerprint repeatedly (or trigger lockout) → Dialog B click flow
   appears within the same request; Allow serves with `source=keychain`.
6. **Gate:** `portal features cred-touchid off` → click-only Dialog B returns; `on` restores.
7. **Forget/re-enroll:** `portal keychain forget <label>` → next sudo is Dialog A enroll again.
8. **Timeout:** let the sheet sit → auto-dismiss at the budget; box exits 112.
9. **Audit:** one line per outcome above; no secret bytes anywhere in `audit.log` or portal logs.
