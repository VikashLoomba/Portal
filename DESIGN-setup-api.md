# Portal Setup API — remote bootstrap over the local core API (Stage 7)

**Status:** Approved (decision locked 2026-07-13; v1's restart contract replaced by in-process
hot-swap per maintainer direction). Sections 2–8 are the implementation contract.
**Amendment 2026-07-13 (plan review):** S6 swap ordering refined to construct → stop-old → swap →
start-new — starting the new stack under a live old-host master on the shared `Paths.Sock` would
route new-stack ssh work to the old box; §3 `done` grammar clarified; §4 gains the u2↔u3↔u4 seam
contract and the adapter capability-preservation rule (PtyStreamer).
**Audience:** repo maintainer + implementation agents.
**Related:** `DESIGN-local-core-api.md` (D1–D10, Option A architecture),
`DESIGN-split-daemon.md` (bootstrap manager, agent upload),
`DESIGN-clipboard-read-interception.md` (shim deploy, §9.6 loud-failure posture).

---

## 1. Decision & rationale

`internal/localapi/state.go` promises that *"every frontend — the CLI today, desktop shells
later — is a thin client"* of the local API. Stage 6 delivered the TS reference client, and it
holds that promise for everything **except first-run setup**: `portal install <host>` is the one
operation a desktop shell cannot drive over the socket. It conflates two different concerns in one
CLI command (`cmd/portal/install.go`):

1. **Local machine concerns** — copy the binary to `~/.local/bin`, write the launchd plist, load
   the service, print PATH advice. An embedding desktop app supervises the daemon itself and
   ships the binary inside its bundle; none of this applies.
2. **Remote bootstrap concerns** — validate SSH reachability, persist the host, deploy the
   xdg-open wrapper + BROWSER env snippet, deploy the clip shims, force the portald symlink, run
   the doctor self-test. This is pure product logic that any frontend needs.

**Decision: split them. Remote bootstrap becomes `POST /v1/setup` on the local API, implemented
once in a new `internal/setup` package consumed by both the CLI and the API handler. The daemon
gains an *unconfigured idle mode* (it always starts and serves the API; the host-bound machinery
is a swappable inner "stack", nil until a host exists) and setup activates the new host by
hot-swapping that stack in-process. The daemon never restarts as part of setup.**

The hot-swap lifecycle was chosen over a v1-draft restart contract (exit 0 after host change,
supervisor relaunches) because the end-app experience is strictly better:

- **Onboarding continuity.** With a restart, the API socket vanishes and the events stream EOFs
  at the exact moment setup *succeeds*; every integrator must mask a spawn-poll-reconnect dance
  or their first-run ends on a disconnect flicker. With hot-swap the app holds one long-lived
  socket, watches the setup stream, and sees the events stream emit the configured-state
  transition live.
- **Truthful state throughout.** `GET /v1/status` and `GET /v1/events` stay answerable during the
  whole flow; a restart window is a blackout precisely when the user is watching.
- **No launchd throttle edge.** A short-uptime daemon that exits post-setup can hit launchd's
  respawn throttle (~10s dead gap) in the CLI-installed case.
- **Failed setup never degrades existing connectivity.** Activation swaps construct-new-first;
  a bad new host with `force:false` leaves the old stack — and the user's live forwards and
  sessions — untouched. The restart model cannot offer this.
- **Host switching is the same endpoint.** An in-app "switch dev box" picker is just another
  `POST /v1/setup`; no `PUT /v1/host` needed, no process choreography.

The `portal host` restart-on-switch precedent (`cmd/portal/lifecycle.go:136-142`) was considered
and rejected as the model here: it is a CLI-era implementation convenience, not a product
decision, and "transports are bound at startup" is a refactorable property of our own wiring.

Why now: the first embedded consumer (a TS desktop app bundling `portal` as a sidecar) needs
exactly this to onboard a user without a pre-installed portal. Everything else it needs already
exists (unix-socket API, TS client, self-contained binary).

Non-decision reaffirmed: no FFI, no ssh2 rewrite — Option A (DESIGN-local-core-api §1) stands.

---

## 2. Locked decisions

| # | Decision | Detail |
|---|---|---|
| S1 | **Endpoint = `POST /v1/setup`, streamed ndjson response** | Request `{"host": string, "force": bool}`. Success is `200` + `application/x-ndjson`, one `api.SetupEvent` per line, connection-scoped like doctor/exec: client disconnect cancels the run via `r.Context()`. Additive to `/v1/` (D6: no version bump). |
| S2 | **Failures after the first byte are in-band** | HTTP status is committed before the outcome is known, so step failures are events (`"status":"fail"` + D9 error detail), and the stream always terminates with a `done` event carrying the verdict. Non-2xx D9 envelopes are reserved for pre-stream rejects: malformed body, no host resolvable, setup already running. |
| S3 | **Steps = install's remote half + activation** | `validate` → `configure` → `xdg-open` → `clip-shims` → `agent-symlink` → `activate` → `doctor` → `done`. Binary copy, launchd install, and PATH advice are NOT steps — they stay CLI-only. `activate` is the daemon-side hot-swap (S6); on a same-host re-deploy it is an explicit no-op `ok`. |
| S4 | **`force` replaces the interactive "install anyway? [y/N]"** | `validate` failure with `force:false` fails the run (remaining steps skipped, `done.status=fail`, nothing written — validate precedes configure). With `force:true` it degrades to a warn and continues — byte-for-byte the semantics of answering `y` today. ssh's live stderr (e.g. Tailscale "To authenticate, visit: …") is relayed as `line` events so a GUI can surface it in real time, exactly as install.go:44 preserves it on the terminal. |
| S5 | **Unconfigured idle mode = nil stack** | `portal run` with no host no longer exits with an error (`cmd/portal/run.go:36-38`). The daemon always starts, always serves the API; the host-bound machinery (transport, agentclient, engine, clip/cred/notify/openurl handlers) lives in a swappable **stack** that is nil until a host is configured. Host-dependent endpoints answer `503 not_configured` (new D9 code) while the stack is nil. `GET /v1/status` degrades as today (`host:""`, master down, nil agent — `buildStatus` already tolerates this). This also fixes the current launchd fresh-boot error-relaunch loop. |
| S6 | **Activation = in-process stack hot-swap: construct → stop-old → swap → start-new** | `activate` CONSTRUCTS the new stack against the requested host without starting it (construction failure → step `fail`, old stack untouched and still serving), then fully drains the old stack (agent Shutdown/Bye, ctx cancel, master Close + `Paths.Sock` removal, bounded wait), then swaps the daemon's current-stack ref, then starts the new stack's goroutines. The old stack must be GONE before any new-stack work begins: both system-transport stacks bind the same `Paths.Sock`, and `ssh -S` routes through whichever master owns the socket without re-verifying the destination — starting the new stack under a live old-host master would bootstrap portald / run exec **on the old box** (the S7 hazard, recreated at the activation layer). The cost is a brief (~2s bounded) window during `activate` where host-bound API calls transiently degrade; the setup stream is live precisely then, and other clients see an honest transient (documented in the endpoint description). The daemon process and API socket are never cycled. After the swap, `Activate` publishes a coalesced hub event so `GET /v1/events` subscribers receive the configured-state transition even if the new agent never connects (sharing the hub alone does not guarantee an emission). `Status`/`Deps.Host` report the **active stack's** host; the host file is persisted intent (transient divergence during a run is documented, §4). |
| S7 | **One implementation: `internal/setup`, on an isolated ControlPath** | The remote-bootstrap steps move out of `cmd/portal/install.go` into `internal/setup`, exposed as composable phases (`Validate`, `Configure`, `DeployRemote`, `Verify`) emitting typed events into a sink. The CLI renders events to stdout (output text preserved as closely as practical); the API handler renders them to ndjson; `activate` is daemon-side, not a setup phase. Setup's transports are built fresh for the requested host **with a dedicated ControlPath** (`<ConfigDir>/setup-cm.sock`, best-effort Close + unlink when the run ends). This is mandatory, not hygiene: `sshctl.Exec` routes `ssh -S <sock> <host> …` through whatever master owns the socket without re-verifying the destination (`pkg/transport/sshctl/transport.go:203-205`), so sharing `Paths.Sock` during a host switch would silently execute deploy steps **on the old box**. (`Validate`/`HasSS` already dial direct — transport.go:326-343 — and the native transport dials per-session; the hazard is system-transport mux reuse only. The CLI flow only dodges it today because `Service.Install` bounces the daemon — and its master — before the deploy steps.) |
| S8 | **Single-flight** | One setup at a time, guarded in the handler; a concurrent request gets `409 setup_in_progress`. Idempotent re-run is the recovery story for every partial failure (same as re-running `portal install`). |
| S9 | **Audited** | `audit.Log` gains setup entries: requested host, forced or not, per-step outcome summary, activation (old host → new host), final verdict. Same posture as OpenURL/ClipServed. |
| S10 | **Spec-first lockstep** | `openapi.yaml` gains `POST /v1/setup` in the same commit as the route registration; the conformance test enforces both directions (D2). `pkg/api` gains `SetupRequest`/`SetupEvent`; the doctor report stays an opaque JSON object in the event, matching the `POST /v1/doctor` precedent. |

---

## 3. Endpoint contract

### Request

```
POST /v1/setup
Content-Type: application/json

{"host": "user@devbox", "force": false}
```

- `host` — optional when a host is already configured (re-deploy against the current box);
  required when unconfigured. Whitespace-stripped like `resolveInstallHost`. Both absent →
  `400 invalid_request`.
- `force` — optional, default false (S4).

Pre-stream rejects (D9 envelope): `400 invalid_request`, `409 setup_in_progress`.

### Response stream

`200` + `application/x-ndjson`. Event shape (`pkg/api`):

```go
// SetupEvent is one line of the POST /v1/setup ndjson stream.
type SetupEvent struct {
    Step   string          `json:"step"`             // validate|configure|xdg-open|clip-shims|agent-symlink|activate|doctor|done
    Status string          `json:"status"`           // running|ok|warn|fail
    Line   string          `json:"line,omitempty"`   // relayed ssh stderr / human progress detail
    Error  *ErrorDetail    `json:"error,omitempty"`  // populated when status=fail (and warn where useful)
    Report json.RawMessage `json:"report,omitempty"` // doctor step only: the doctor.Report, opaque like POST /v1/doctor
}
```

Stream grammar: each step emits `running` once, then zero or more `line` events, then exactly one
terminal `ok|warn|fail` — EXCEPT `done`, which is a single event (no `running`, no `line`s) and is
always the last line, `status` `ok` (all steps ok/warn) or `fail`. All steps always appear
(skipped steps after a hard fail are omitted, not faked). The 400/409 pre-stream rejects are the
ONLY non-2xx responses this endpoint has — there is no 500 in the spec; every post-first-byte
failure is in-band (S2) — and the request body is `required` in the OpenAPI spec (an absent body
is a malformed request). The socket and the connection's lifecycle are ordinary — after `done`
the stream ends and the same daemon keeps serving; there is no post-setup reconnect choreography.
During the `activate` step, host-bound calls from OTHER clients may transiently degrade for the
bounded drain window (S6) — an honest transient, not an error state.

### Steps (remote work over fresh, ControlPath-isolated transports — S7)

| Step | Does | Failure semantics |
|---|---|---|
| `validate` | `sshctl.Validate` (key-based reachability; stderr → `line` events), then `HasSS` check | Validate fail: `fail` + stop unless `force` (→ `warn` + continue). Missing `ss`: `warn` (matches install.go:47-49). |
| `configure` | Pre-create config/bin/log dirs; `Cfg.WriteHost` | `fail` + stop. Old stack untouched — teardown belongs to `activate`. |
| `xdg-open` | Wrapper + BROWSER env snippet + rc sourcing (install.go `installXdgOpenWrapper`) | `warn` + continue (matches install.go:107-111). |
| `clip-shims` | `clipshim.Ensure` | `warn` + continue, `Error` populated — LOUD per DESIGN-clipboard §9.6; remediation is re-running setup. (The agentclient also re-ensures shims on every verified connect — `pkg/agentclient/client.go:595-601` — so a warn here self-heals once the new stack connects.) |
| `agent-symlink` | Force the `~/.cache/portal/portald` symlink from the embedded SHA (install.go:304-308) | `warn` + continue. |
| `activate` | S6 hot-swap: construct stack for the host (unstarted), drain old fully, swap ref, start new (§4 ordering). No-op `ok` when the active stack already targets this host. | `fail` + stop (construction error, e.g. native-transport resolution); old stack stays active — existing connectivity preserved. |
| `doctor` | `runDoctor` against the (now-active) host; report embedded in the event | Report speaks for itself; step is `ok` even when the report contains FAILs (matches install.go:131-136 — install never aborts on doctor). |
| `done` | Verdict | — |

New D9 code inventory: `not_configured` (S5), `setup_in_progress` (S8), reuse `invalid_request`.

---

## 4. Daemon changes: the stack model

### What a stack is

A `stack` owns everything bound to one host: the transport pair (`transport.Transport` +
`PortForwarder` from `app.NewTransport`), the `agentclient.Client` + bootstrap manager, the
forward engine (+ its `adaptAgentEvents` pump), and the run.go handler goroutines
(clip/cred/notify/openurl). It has its own child context; teardown = agent
`Shutdown` (best-effort Bye), cancel, master `Close`, CM socket removal, bounded drain.

Host-independent and shared across stacks: `Paths`, `config.Store`, `audit.Log`, the **hub**
(already constructed once in `NewProd` and passed into agentclient — this is what makes
events-stream continuity across a swap free), and the localapi server itself.

### Wiring

- `internal/app` gains a stack factory (`app.NewStack(paths, cfg, hub, host, …)`) extracted from
  the existing `NewProd` wiring. `NewProd` keeps building an eager initial stack-equivalent for
  every other CLI command — **no behavior change outside the daemon**.
- `run.go` becomes a supervisor owning `atomic.Pointer[stack]`: nil when unconfigured at boot,
  else the initial stack. It exposes three closures to the API layer: `Activate(host) error`
  (S6 swap, serialized), `RequestKick`, `PushAllow` — the latter two delegating to the current
  stack.
- `localapi.Deps`' host-bound fields (`Agent`, `Master`, `Ports`, `ExecStream`, `Doctor`,
  `Kick`, `PushAllow`, `Host`) are satisfied by thin adapters that read the current stack ref;
  a nil stack yields the `not_configured` sentinel (the `errTransport` pattern,
  `internal/app/app.go:203`, with a stable detail string). Handlers need no changes beyond
  exec/doctor mapping that sentinel to `503 not_configured`. Adapters must preserve the
  transport's OPTIONAL capabilities, not just the interface they wrap: `handleExec`
  type-asserts `transport.PtyStreamer` on the ExecStream dep, so an ExecStreamer-only adapter
  would silently regress every PTY request to `409 pty_unsupported`.
- `Deps.Host` reports the **active stack's** host (`""` for nil) rather than re-reading the host
  file, so status is always truthful about what the daemon is actually connected to. During a
  setup run there is a transient window (configure written, activate pending) where the file and
  the active stack diverge; documented in the endpoint description, resolved by `activate`.

### Swap ordering (S6)

1. Construct the new stack WITHOUT starting it; on construction error (e.g. native-transport
   resolution), report `fail` and keep the old stack serving — existing connectivity survives a
   failed activation.
2. Drain the old stack fully (Shutdown → cancel → master Close → `Paths.Sock` removal), bounded
   (~2s); in-flight exec sessions to the old host die with it — correct, they target the old box.
   API connections and the events stream are untouched.
3. Swap the ref — API calls now see the new stack.
4. Start the new stack's goroutines; its master is established fresh against the new host on the
   now-free `Paths.Sock`. Publish a coalesced hub event so events subscribers see the transition.

Step 2 preceding steps 3–4 is load-bearing: a live old-host master on the shared `Paths.Sock`
would silently serve any new-stack ssh work (see S6/S7 — `ssh -S` never re-verifies the socket
owner's destination). The bounded degradation window between 2 and 4 is the accepted cost.

### Seam contract (u2 ↔ u3 ↔ u4)

Pinned here so the units compose without renegotiation — this is the exported surface, by name:

```go
package setup // internal/setup (u2)

type Sink func(api.SetupEvent)      // phases own ALL running/line/terminal emissions; callers add none
func NormalizeHost(raw string) string             // resolveInstallHost's all-whitespace strip, moved here
func New(paths app.Paths, cfg *config.Store, sink Sink) *Runner
func (r *Runner) Validate(ctx context.Context, host string, force bool) (proceed bool)
func (r *Runner) Configure(ctx context.Context, host string) error
func (r *Runner) DeployRemote(ctx context.Context, host string)   // xdg-open → clip-shims → agent-symlink
func (r *Runner) Verify(ctx context.Context, host string) *doctor.Report
func (r *Runner) Close(ctx context.Context)       // best-effort setup-transport close + setup-cm.sock unlink
```

One `Runner` per setup run, bound to one requested host's dedicated setup transport (S7); callers
create it fresh per request and defer `Close` — reusing a host-bound Runner across requests could
deploy to a previous host and leak the setup CM socket. Every phase honors ctx cancellation
between remote operations, and the agent-symlink deploy OBSERVES its result before emitting the
step's terminal event (install.go's current `2>/dev/null || true` swallow is not carried over).
Exact unexported internals are u2's to shape; the exported names, one-Runner-per-request
lifecycle, and phase-owned emission are contract.

Activation is daemon-side (u3): run.go exposes `Activate(ctx context.Context, host string) error`
where ctx bounds ONLY construction and the old-stack drain — the new stack starts under the
SUPERVISOR's daemon-lifetime context, never the request's (a client disconnect after a successful
activation must not undo the live stack). A freshly built stack seeds its AgentClient with the
current allowlist (`Subscribe(DenyPorts, allow, true)`, preserving today's run.go:49-52 ordering)
BEFORE `Run`, so a new connect never applies an empty filter. u4 wires the handler: fresh
`setup.New` per request with deferred `Close`, `NormalizeHost` on the host param, and the
`Activate` closure.

---

## 5. CLI refactor (`portal install` stays, shrinks)

`cmd/portal/install.go` becomes composition of local steps + `internal/setup` phases:

1. resolve host (interactive prompt stays CLI-side)
2. `setup.Validate` (stdout renderer; the [y/N] prompt maps to `force` on retry — the CLI keeps
   its interactive prompt by running Validate, prompting on failure, then continuing with force)
3. `setup.Configure`
4. binary copy → `~/.local/bin` (CLI-only)
5. `Service.Install` + start (CLI-only; the daemon boots straight into the configured host, so
   the CLI path never needs `activate`)
6. `setup.DeployRemote` (xdg-open, clip-shims, agent-symlink)
7. PATH advice (CLI-only)
8. `setup.Verify` (doctor render)

`portal host` (lifecycle.go) is unchanged in this stage — still restart-based, which remains
correct for the launchd-managed CLI install. A natural later cleanup is reimplementing it as a
`POST /v1/setup` client when the daemon is up (out of scope, §10). Current install output text is
preserved as closely as practical — it is user-facing documentation (README quotes it).

---

## 6. SDK additions

### TS (`clients/ts`)

- `dto.ts`: `SetupRequest`, `SetupEvent` (report typed `unknown`, never `any`).
- `setup.ts`: `export async function* setup(socketPath, req, options): AsyncGenerator<SetupEvent>`
  — POST with body, then the events.ts ndjson line-reader pattern (shared helper extracted rather
  than duplicated).
- `ready.ts`: `waitReady(socketPath, {timeoutMs, signal})` — poll `GET /v1/version` until it
  answers. Spawn-time helper only (the sidecar needs a beat to bind the socket); setup itself
  never drops the connection.
- Tests: happy path, in-band fail, force-warn path, activate-fail (old-stack-preserved semantics
  visible as `done.status=fail` with no state flip), 409, disconnect-cancel — against a stub
  unix-socket server like the existing suite.

### Go (`pkg/client`)

- `Setup(ctx, SetupRequest) (iter.Seq2[api.SetupEvent, error], error)` mirroring its events
  reader; `WaitReady(ctx, timeout)` helper. Same tests.

### Embedding recipe (docs)

One short doc section (README or `docs/embedding.md`): spawn the bundled binary with
`PORTAL_CONFIG_DIR` + `PORTAL_API_SOCK` pointed at app-scoped paths, `waitReady`, check
`status.host`, run `setup` when empty while rendering the step stream, watch `/v1/events` flip to
configured — one socket, no restarts. The `examples/shell-electron` example gains this flow
behind a "first-run" branch so the reference embedding demonstrates it end-to-end.

---

## 7. Failure modes

| Failure | Detection | Response |
|---|---|---|
| Client disconnects mid-setup | `r.Context()` done | Steps abort at the next ctx check; partial remote state is fine — every step is idempotent; recovery = re-run setup. If the disconnect lands after `activate`, the swap stands (host written + active — a coherent end state). Single-flight lock released; setup CM socket cleaned up. |
| Validate fails, no force | step event `fail` | Remaining steps skipped; `done.status=fail`; nothing written, old stack untouched. |
| WriteHost fails | `configure` fail | Run stops; old stack untouched; no swap. |
| New-stack construction fails | `activate` fail | Old stack stays active — existing forwards/sessions keep working; host file already points at the new host (re-run setup or set it back); status truthfully shows the active (old) host per S6. |
| Daemon killed mid-setup | supervisor relaunch | Relaunch binds whatever the host file says (old or new per whether configure landed); agentclient re-ensures shims on connect (client.go:595); user re-runs setup for the wrapper. |
| Host switch with live exec sessions | S6 drain | Sessions to the old host die at activate — necessarily (they target the old box). API connections and the events stream survive; document in the endpoint description. |
| Mux hijack during host switch | prevented by design | S7's dedicated `setup-cm.sock` — deploy can never ride an old-host master on `Paths.Sock`. |
| Two clients race setup | S8 lock | Second gets 409; first wins. `Activate` is additionally serialized in run.go as defense in depth. |
| Events subscribers during swap | — | Hub is shared (S6); subscribers see disconnect/connect state events, no stream break. |

---

## 8. Implementation units

Each unit compiles and tests green independently; the conformance test gates u4.

| Unit | Scope | Tests |
|---|---|---|
| u1 | `pkg/api`: SetupRequest/SetupEvent + error-code docs; `openapi.yaml` `POST /v1/setup` (spec+types land together for D2 lockstep; conformance skip-listed until u4 registers the route, mirroring how earlier stages staged routes) | type marshal/shape tests |
| u2 | `internal/setup`: extract Validate/Configure/DeployRemote/Verify from install.go; dedicated-ControlPath transport construction + cleanup; rewire `cmd/portal/install.go` | fake-transport step tests (exec-call recording, ordering, idempotence, isolated sock path); CLI output regression test |
| u3 | Stack model: `app.NewStack` factory; run.go supervisor with `atomic.Pointer[stack]` + Activate/teardown; Deps adapters + nil-stack `not_configured` sentinels; unconfigured boot | daemon_test: API answers with no host; exec/doctor 503; status degrades; swap preserves hub subscribers; construct-fail keeps old stack; teardown drains bounded |
| u4 | `internal/localapi` handleSetup: ndjson streaming, force, single-flight 409, activate wiring, audit, route + conformance | handler tests with a fake setup runner + fake activator: stream grammar, in-band fail, activate no-op/fail paths, 409, disconnect-cancel |
| u5 | `pkg/client` Setup + WaitReady | stub-server tests |
| u6 | TS SDK setup()/waitReady() + dto + shared ndjson reader extraction | node:test suite additions |
| u7 | `examples/shell-electron` first-run flow + embedding doc | manual checklist (§9) |

u3 is the load-bearing unit and lands before u4 so the handler composes against real seams.

---

## 9. Manual verification checklist (live box)

1. Fresh config dir, `portal run` → daemon idles, `GET /v1/status` shows `host:""`, exec/doctor
   answer `503 not_configured`.
2. With `curl --unix-socket … GET /v1/events` held open in another terminal:
   `POST /v1/setup {"host":"<box>"}` → full step stream incl. `activate` ok; the events stream
   **never disconnects** and emits the configured-state transition; forwards converge; doctor
   report PASS — all on the same daemon PID.
3. Re-run same-host setup → steps ok, `activate` no-op, daemon PID unchanged.
4. Bad host, no force → validate fail, `done.status=fail`, host file unchanged, old connectivity
   (if any) intact.
5. Live host switch (box A → box B) with a `portal exec` session open to A: deploy goes to B
   (verify shim mtimes on A unchanged — S7 isolation), session to A dies at activate, events
   stream survives, forwards converge on B.
6. `portal install <box>` from scratch → output text unchanged vs v0.4.1 (modulo shared-runner
   phrasing), launchd loaded, doctor PASS.
7. Electron example first-run flow end-to-end on a fresh `PORTAL_CONFIG_DIR`.

---

## 10. Non-goals

- **Installing/managing launchd via the API.** Persistence-after-quit belongs to the embedding
  app's native login-item mechanism (`SMAppService`, Electron `openAtLogin`): the user gets an
  OS-surfaced "Open at Login" toggle attributed to the app, and uninstalling the app cleans it
  up. A portal-written plist pointing into an app bundle would orphan on app deletion and break
  on bundle-path changes — worse UX at the wrong layer. Standalone installs keep the CLI's
  launchd path.
- **`PUT /v1/host`.** Setup *is* the host switch; a bare host-flip without deploy+verify is a
  footgun, not a feature.
- Binary self-copy to `~/.local/bin` via the API (meaningless for a bundled sidecar).
- Uninstall/remote-cleanup endpoint (CLI `uninstall` unchanged; candidate for a later stage).
- Windows support, TCP listeners, auth tokens (D1/D4 unchanged).
