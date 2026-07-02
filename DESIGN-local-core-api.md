# Portal Local Core API — Option A Product Architecture

**Status:** Approved direction (decision locked 2026-07-01). Sections 4–5 (Stages 1–2) are the
implementation contract; sections 6–9 (Stages 3–6) are committed direction at plan resolution and
will get their own contract docs when they start.
**Audience:** repo maintainer + implementation agents.
**Related:** `DESIGN-split-daemon.md` (wire protocol, agent, bootstrap),
`DESIGN-clipboard-read-interception.md` (clip/notify relay, cmd-socket trust model).

---

## 1. Decision & rationale

portal's platform layers (transport, bootstrap, wire protocol, session client, agent runtime) are
the base for "local shell, remote brain" desktop applications. The **product architecture is
Option A: the Go core runs as a local daemon exposing a control API on a Unix socket; every
frontend — the `portal` CLI today, desktop shells (Electron/Tauri/Swift) later — is a thin client
of that API.** The Go core remains the only implementation of the hard parts: reconnect
supervision, bootstrap, heartbeat discipline, request correlation.

Alternatives rejected:

- **cgo `c-shared` + FFI bindings** — the core's essence is long-lived goroutines, channels, and a
  reconnect supervisor: the worst shape to expose across FFI (callback thunks, per-language
  threading rules, packaging per platform, lost crash isolation).
- **Native wire-protocol clients per language** — highest effort, highest drift risk. Kept open as
  an escape hatch via a formal wire spec (Stage 6), not load-bearing.

Precedents: Docker Desktop (CLI/GUI ↔ dockerd socket), Tailscale (GUI ↔ tailscaled LocalAPI), and
portal's own remote cmd socket (`cmd-<pid>.sock`) — this design mirrors that socket on the Mac side.

Two existing warts this dissolves immediately:

1. `portal status` from a separate process cannot see the daemon's agent handshake — the agent
   line is elided (`cmd/portal/inspect.go:54`).
2. `portal allow N` cannot push a Subscribe — its in-process `AgentClient` is never connected, so
   the change converges via the daemon's 60s safety reconcile (`cmd/portal/allow.go:90`), while
   the command prints "takes effect within ~100ms". The API makes that message true. (Note:
   DESIGN-split-daemon §7's "fsnotify (or 1s ticker fallback)" row was never implemented — the
   `pushAllowlist` comment in allow.go is the ground truth; that sibling row is stale.)

---

## 2. Locked decisions (Stage 0)

| # | Decision | Detail |
|---|---|---|
| D1 | **HTTP/1.1 + JSON over a Unix socket** | No gRPC (no toolchain for consumers; surface too small to pay for protobuf), no TCP listener in v1, ever. curl-debuggable at every later stage. |
| D2 | **Spec-first, hand-written handlers** | `internal/localapi/openapi.yaml` is committed, `go:embed`-ed, and served at `GET /v1/openapi.yaml`. Handlers are hand-written `net/http`; a **conformance test** walks the spec and the mux and fails on drift in either direction. Codegen (`oapi-codegen` server stubs, generated TS clients) is deferred until the first external client exists (Stage 6) — zero new dependencies until a consumer needs them. |
| D3 | **Events = snapshot-as-reset ndjson** | `GET /v1/events` streams one JSON object per line: first line is always a full state snapshot, then deltas, plus a periodic tick. No cursors, no resume protocol — a reconnecting client is always coherent. Same discipline as the wire protocol's Snapshot → seq-gated deltas, one layer up. |
| D4 | **Trust model = the remote cmd socket's, mirrored** | Socket dir 0700, socket 0600, peer-uid check (`LOCAL_PEERCRED` on darwin / `SO_PEERCRED` on linux) as defense in depth. Same-uid processes are authorized by reachability; the compensating controls remain the feature gates + audit log. No auth token in v1. If a TCP fallback is ever forced (e.g. a platform without UDS HTTP), that is a new decision: loopback + 0600 token file, Docker-style. |
| D5 | **Socket path derives from ConfigDir** | `<ConfigDir>/api.sock` (default `~/.config/portal/api.sock`), new `Paths.APISock` field, `PORTAL_API_SOCK` env honored as a test seam. Deriving from ConfigDir means the existing `PORTAL_CONFIG_DIR` staging-instance harness (DESIGN-split-daemon §10) isolates the API socket with no extra work. ConfigDir is ensured 0700. |
| D6 | **Versioning** | All paths under `/v1/`. `GET /v1/version` returns portal version + embedded git SHA + wire ProtoVersion. Breaking API changes mean `/v2/`, not silent mutation. |
| D7 | **The socket is the single-instance lock** | On startup the daemon probes an existing socket: a live responder means another daemon owns it → fatal exit (loud, launchd-visible); a dead socket is unlinked and taken over. This also fixes today's unenforced "two daemons" ambiguity. |
| D8 | **Repo shape: `internal/` until Stage 6** | New packages `internal/hub`, `internal/localapi`, `internal/localclient`. Extraction to `pkg/`/separate module happens only after the seams are proven by ≥2 consumers and ≥2 transports. |
| D9 | **Error shape** | Non-2xx responses carry `{"error":{"code":"<machine-key>","message":"<human>"}}`. Codes are stable strings (`not_connected`, `invalid_port`, `feature_unknown`, …). |
| D10 | **API bind failure is fatal** | Under launchd `KeepAlive`, a loud crash beats a daemon that silently lost its control plane. |

---

## 3. Architecture

```
 MAC                                                        │ REMOTE dev box
 ┌──────────────┐  ┌──────────────┐  ┌───────────────────┐  │
 │ portal CLI   │  │ desktop shell│  │ curl / scripts    │  │
 │ (Stage 2)    │  │ (Stage 6)    │  │                   │  │
 └──────┬───────┘  └──────┬───────┘  └─────────┬─────────┘  │
        └────────────┬────┴────────────────────┘            │
                     ▼  HTTP/1.1 + JSON, ndjson events      │
        ~/.config/portal/api.sock  (0600, peer-uid check)   │
 ┌───────────────────┴────────────────────────────────────┐ │
 │ portal run (launchd daemon)                            │ │
 │  ┌────────────┐   ┌──────────┐   ┌──────────────────┐  │ │
 │  │ localapi   │◄──│ hub      │◄──│ agentclient      │──┼─┼── ssh exec pipe ──► portald
 │  │ .Server    │   │ (fan-out │tee│ (demux, reconnect│  │ │   (framed CBOR)
 │  │ (Stage 1)  │   │  QoS)    │   │  supervisor)     │  │ │
 │  └─────┬──────┘   └──────────┘   └──────────────────┘  │ │
 │        │ mutate: allow/features/reconcile/doctor       │ │
 │  ┌─────▼──────┐   ┌──────────────┐                     │ │
 │  │ config     │   │ forward      │── ssh -O forward ───┼─┼── ControlMaster
 │  │ .Store     │   │ .Engine      │                     │ │
 │  └────────────┘   └──────────────┘                     │ │
 └────────────────────────────────────────────────────────┘ │
```

The hub **tees** — it never sits between the agentclient and its existing consumers. The engine,
clip handler, and notify handler keep their current dedicated channels; the hub is an additional,
read-only fan-out for API observers. The hub can never become a second event-ordering authority.

---

## 4. Stage 1 contract — event hub + local API server

### 4.1 New files

| Path | Purpose |
|---|---|
| `internal/hub/hub.go` | `Hub` fan-out: `Subscribe(class) (<-chan Event, cancel)`, `Publish(Event)`. Per-subscriber buffers with per-class drop policy. Typed `Event` struct (tagged union style, like `agentclient.EngineEvent` — no `any`/`interface{}` payloads). |
| `internal/hub/hub_test.go` | Drop-policy semantics per class; slow-subscriber never blocks Publish; unsubscribe races; no goroutine leaks. |
| `internal/localapi/server.go` | `Server` + mux + middleware (peer-cred check, panic recovery, request log at debug). `Serve(ctx, listener)`; socket create/chmod/stale-probe-takeover lives here. |
| `internal/localapi/state.go` | `Status` aggregate + `Deps` (narrow interfaces over agentclient/transport/proc/config/service so tests fake them without an `App`). |
| `internal/localapi/handlers.go` | v1 endpoint handlers. |
| `internal/localapi/events.go` | ndjson streamer: snapshot-first, coalesced state deltas, notify events, 30s tick; client disconnect detection. |
| `internal/localapi/peercred_darwin.go` / `peercred_linux.go` | Peer-uid extraction (`LOCAL_PEERCRED` / `SO_PEERCRED`). |
| `internal/localapi/openapi.yaml` | The committed spec, embedded and served. |
| `internal/localapi/spec_test.go` | Conformance: every spec path+method has a registered handler and vice versa. |
| `internal/localapi/server_test.go` | Integration over a **real** unix socket in `t.TempDir()`: full daemon fakes (fake transport + a real `agent.Server` wired over `io.Pipe` pairs, reusing the `agentclient/client_test.go` harness), real HTTP client. Covers status, events snapshot+delta across a simulated reconnect, allow round-trip, single-instance probe, socket perms, and every endpoint's primary failure path. |

### 4.2 Modified files

| Path | Change |
|---|---|
| `internal/agentclient/client.go` | `Config` gains optional `Hub *hub.Hub`. Tee sites: `publish` (state events), `publishNotify` (notify events), plus snapshot-cache updates publish a state event. **`publishClip` is NOT teed** — pastes are answered by the daemon itself; shells must not see or race them. `KindOpenURL` (which also flows through `publish`) is explicitly **not** mapped into `hub.Event` — the URL relay stays daemon-internal in v1; the tee is an explicit kind→`hub.Event` enumeration, never a pass-through. Tee is non-blocking by construction (hub contract). |
| `internal/app/paths.go` | `Paths.APISock` (+ `PORTAL_API_SOCK` seam) in `DerivePaths`. |
| `internal/app/app.go` | Construct `hub.Hub`, pass into `agentclient.Config`; expose on `App`. |
| `internal/forward/engine.go` | `Kick()` method + buffered kick channel; `runEventDriven` selects on it and fires the existing debounce. Gives `POST /v1/reconcile` a clean trigger. |
| `cmd/portal/run.go` | Sixth goroutine: build `localapi.Server` from `App` deps, serve on `Paths.APISock`. Bind failure → daemon exits non-zero (D10). |
| `cmd/portal/doctor.go` | Move `runDoctor`'s **existing** report types (`doctorCheck`/`doctorReport` — today unexported in `package main` with an int-enum status, hence unserializable) into an importable, JSON-tagged form (e.g. a small `internal/doctor` package) that run.go wires into `localapi.Deps` as a closure; `POST /v1/doctor` returns it as JSON. CLI rendering stays byte-compatible. |
| `cmd/portal/lifecycle.go` | `uninstall`/`stop` paths remove a stale `api.sock` best-effort. |

### 4.3 Hub design

QoS classes, mirroring the wire-layer lesson (a port-event burst must never evict a paste):

| Class | Used for | Buffer | Drop policy |
|---|---|---|---|
| `Coalesced` | state changes (connect/disconnect/snapshot/delta) | 1 | latest-wins: a slow subscriber sees current truth, never a backlog |
| `Queued` | notifications | 16 | drop-oldest, count recorded (surfaced in `Status.Health`) |

Rules: `Publish` never blocks and never errors. Subscribers get independent buffers. `cancel()` is
idempotent and safe during delivery. Clip events are not representable in `hub.Event` at all —
exclusion by type, not by convention.

### 4.4 State aggregate

```go
// internal/localapi/state.go — shape, not final field list
type Status struct {
    Version  VersionInfo     // portal version, git SHA, wire ProtoVersion
    Host     string          // configured dev box ("" = unconfigured)
    Service  ServiceStatus   // launchd loaded/state lines
    Master   MasterStatus    // up, pid
    Agent    *AgentStatus    // nil until handshake: pid, sha, kernel, bootID, connectedFor
    Ports    []PortStatus    // remote loopback listeners (cached Snapshot + seq)
    Forwards []ForwardStatus // active local forwards (verbatim lsof NAME lines — ground truth)
    Allowed  []int           // current allowlist (allow-file ground truth)
    Features map[string]bool // clip-image / clip-text / notify gates
    Health   Health          // lastDisconnectErr, droppedNotifyCount, eventsSubscribers
}
```

Everything `runStatus` prints today is derivable from this struct; Stage 2 re-renders the CLI from
it byte-compatibly.

### 4.5 Endpoints (v1)

| Method + path | Semantics |
|---|---|
| `GET /v1/status` | The `Status` aggregate. |
| `GET /v1/events` | ndjson stream (see 4.6). |
| `GET /v1/ports` | Remote loopback listeners from the cached Snapshot (`503 not_connected` before the first cached Snapshot, i.e. while `Client.Snapshot()` reports ok=false — note this is a *later* boundary than the handshake). |
| `PUT /v1/allow/{port}` | Validate 1–65535 → `config.Store.Allow` → **in-process** `AgentClient.Subscribe` → return new allowlist. This is the endpoint that makes "~100ms" true. |
| `DELETE /v1/allow/{port}` | Mirror via `Unallow`. |
| `GET /v1/features` / `PUT /v1/features/{name}` | Read/write the capability gates via `config.Store.FeatureEnabled`/`SetFeature`; unknown name → `404 feature_unknown`. |
| `POST /v1/reconcile` | `engine.Kick()`; returns 202. |
| `POST /v1/doctor` | Run the doctor self-test; return the structured report (long-running: honors client disconnect). |
| `GET /v1/version` | Version info (works even before host configured). |
| `GET /v1/openapi.yaml` | The embedded spec. |

There is deliberately **no** `GET /v1/forwards` and **no** `GET /v1/allowed` — `Status` carries
both (`Forwards`, `Allowed`), and the allow mutations return the new allowlist inline; redundant
read endpoints would violate the endpoint-consumer rule (§10). Every v1 endpoint has a named
consumer by end of Stage 2: `status`/`ports` → CLI `status`/`ports`; `events` → CLI
`status --watch` (§5.2); `allow` mutations → CLI `allow`/`unallow` (which print the returned
allowlist); `features` → new CLI `features` command (§5.2); `reconcile` → CLI `once`; `doctor` →
CLI `doctor`; `version`/`openapi.yaml` → the single-instance probe and tooling.

### 4.6 Events stream

```
{"type":"snapshot","status":{...}}            ← always first
{"type":"state","status":{...}}               ← coalesced full-Status on any state change
{"type":"notify","notify":{"title":"…","verified":true,...}}
{"type":"tick"}                               ← every 30s; liveness for hung-daemon detection
```

State deltas carry the **full Status object** (latest-wins coalescing makes this cheap and spares
every client a merge implementation — Status is small). Notify events let a shell render
notifications in-app; the daemon still raises the native macOS notification itself.

### 4.7 Lifecycle & security

- Startup: ensure ConfigDir 0700 → if `api.sock` exists, probe with a 1s `GET /v1/version`; live
  responder → log + exit non-zero (D7); else unlink → listen → chmod socket 0600 → serve.
- Every accepted connection passes the peer-uid check before any routing; mismatch → close.
- Shutdown: on ctx cancel, `http.Server.Shutdown` with a short deadline, then unlink the socket.
- The events handler must tolerate slow/dead clients without backpressuring the hub (per-subscriber
  buffers already guarantee this; the handler additionally sets write deadlines).

### 4.8 Exit criteria

Machine-checkable (the implementation must include tests proving each):

1. `make build`, `go vet ./...`, `make test`, and `go test -race ./...` green; new packages
   gofmt-clean.
2. Integration test: in-process daemon (fake transport, real `agent.Server` wired over `io.Pipe`
   pairs) serving on a real UDS; `GET /v1/status` reports agent pid/sha after handshake.
3. Events test: connect → snapshot first; simulated agent reconnect → coalesced state event;
   teed notify delivered; tick observed with a shortened test interval.
4. Conformance test: spec ↔ mux parity in both directions.
5. Socket file mode 0600 and parent dir 0700 asserted; peer-cred checker unit-tested with a
   mismatched uid (function-level fake).
6. Single-instance: starting a second server against a live socket fails; against a dead socket
   file succeeds (takeover).
7. Handler-level tests cover every v1 endpoint's success AND primary failure path: `ports` 503
   before the first Snapshot; `features` 404 on unknown name; `allow` 400 on invalid port;
   `reconcile` 202 with the `Engine.Kick()` → Reconcile trigger observed; `doctor` JSON shape with
   byte-compatible CLI rendering asserted against today's output.

Human validation (live box, via the DESIGN-split-daemon §10 staging harness): daemon under
launchd serves `curl --unix-socket ~/.config/portal/api.sock http://portal/v1/status`; paste and
notification round-trips unaffected; `portal doctor` green.

---

## 5. Stage 2 contract — CLI becomes the first client

### 5.1 New files

| Path | Purpose |
|---|---|
| `internal/localclient/client.go` | Thin typed client over the UDS (custom `http.Transport` dialing the socket). `Available()` fast-probe, typed getters/mutators matching 4.5, small timeouts (status 2s). |
| `internal/localclient/client_test.go` | Against a real `localapi.Server` on a temp socket; plus daemon-down behaviors (no socket, dead socket, hung server → timeout). |
| `cmd/portal/features.go` | New `portal features [name on\|off]` command consuming `GET/PUT /v1/features` — today users toggle gates by echoing into files; this gives the gates a first-class consumer. Falls back to `config.Store` when the daemon is down (same file, same semantics). |

### 5.2 Modified files & fallback policy

Per-command policy — fallback is per command, not blanket:

| Command | Daemon up | Daemon down |
|---|---|---|
| `status` | Render from `GET /v1/status` — now **with** the agent line | Current file/lsof path, agent line elided (unchanged) |
| `status --watch` (new flag) | Stream `GET /v1/events`; re-render on each state event (live forwards/agent view) | Clear one-line error pointing at `portal status` (a watch has nothing to watch) |
| `ports` | `GET /v1/ports` (fixes the in-process-snapshot dependency) | Current behavior |
| `allow`/`unallow` | Write file, then `PUT/DELETE /v1/allow/{port}` — push is real | Write file; print honest "takes effect when the daemon reconciles" |
| `allowed` | Local file read (unchanged — no daemon needed; shells read `Status.Allowed`) | unchanged |
| `doctor` | `POST /v1/doctor` — runs inside the daemon against the **live** transport (better ground truth than a fresh CLI-side master probe) | Current in-process run (unchanged) |
| `once` | `POST /v1/reconcile` + poll status (no second AgentClient spun up against the same box) | Current behavior (own short-lived client) |
| `features` (new) | `GET/PUT /v1/features` | `config.Store` directly |
| `logs`, `install`, `uninstall`, `host` | unchanged in Stage 2 | unchanged |

Output stays byte-identical where scripts might parse it (status layout, allow/unallow lines
except the corrected latency claim). This is enforced by a golden-output test: daemon-up `status`
rendering must match today's layout apart from the added agent line.

### 5.3 Exit criteria

1. Integration test: daemon in-process on a temp socket; `runStatus` output includes
   `agent: pid=… sha=…` sourced over the socket.
2. Fallback tests: no socket / dead socket / timeout each produce today's behavior, no error spam
   — covering `status`, `ports`, `allow`, `doctor`, `once`, and `features`; `status --watch`
   errors politely when the daemon is down.
3. Allow round-trip test: CLI `allow` with daemon up → agent receives a new Subscribe (assert
   rsid advanced on the fake agent) without waiting for a reconcile.
4. Golden-output test: daemon-up `status` rendering byte-identical to today's layout apart from
   the added agent line; allow/unallow lines unchanged except the corrected latency text.
5. `status --watch` integration test: renders on the snapshot event, re-renders on a state event,
   exits cleanly when the daemon shuts down.
6. All Stage 1 criteria still green (including `go test -race ./...`).

---

## 6. Stage 3 — service registration (direction)

Decouple features from the platform on the wire. ProtoVersion 4 adds one generic frame and
capability negotiation:

```go
Msg *Msg `cbor:"msg,omitempty"`
// Msg { Service string, Kind string, Seq uint64, Payload cbor.RawMessage }
// HelloAck gains: Services map[string]uint32
```

Agent side: `Server.RegisterService(svc Service)` where the interface **forces** today's
structurally-enforced invariants: `Name/Version`, `CmdVerbs` (cmd-socket claims), `HandleCmd`,
`HandleFrame`, `MaxPayload` (control-plane discipline, codec-enforced). Services emit via bounded
per-service outboxes drained by the single-writer Serve loop; per-service seq counters never touch
the port-event staleness counter. Client side: `RegisterHandler(name, deliveryClass)`; the
nonce+epoch correlation from clip is lifted into a reusable call/waiter primitive.

Migration order by semantic weight: `openurl` → `notify` → `clip`. **Ports stays native** for now —
its Subscribe/Snapshot/seq semantics are the session layer today; migrating it is a separate
decision (and the eventual test that a consumer can *omit* it). Mixed versions stay safe via
bootstrap re-upload + loud version mismatch.

## 7. Stage 4 — transport shrink + native SSH (direction)

Core interface shrinks to `Ensure/Exec/Stream/Close`; optional capabilities via type assertion:
`PortForwarder` (`Forward/Cancel/ListForwards`) and `Uploader` (default composed from `Exec`+`cat`).
`ListForwards` absorbs the lsof-against-master-pid SSH-ism so `forward.Engine`'s stateless
reconcile keeps its "never trust memory" invariant without knowing lsof exists. Then a native
`x/crypto/ssh` transport: one `*ssh.Client` + keepalives standing in for the ControlMaster;
forwards as in-process listeners dialing `direct-tcpip`; agent-socket→key-file auth,
`known_hosts` enforcement. System-ssh stays default behind a config switch until doctor + soak
pass. A transport conformance suite runs against system-ssh, native, and a local-subprocess fake
(which doubles as dev mode).

## 8. Stage 5 — bootstrap matrix + exec capability (direction)

Bootstrap gains an artifact-provider seam: probe `uname -sm` per connect (cached against BootID),
select from an embedded GOOS/GOARCH matrix (add `portald-linux-arm64`), parameterized remote
dir/name; plus the generic `EnsureArtifact(ctx, name, content)` for app-supplied payloads. The
marquee shell capability: `POST /v1/exec` upgrading to a bidirectional stream (WebSocket over the
UDS) bridged to `Transport.Stream` — deliberately bypassing the CBOR pipe (interactive bytes are
bulk, not control), audited like clip/notify. Supervised agents (portald as process supervisor:
start/list/reattach — sessions surviving Mac sleep) is design-doc-only in this stage
(`DESIGN-supervised-agents.md`).

## 9. Stage 6 — extraction, reference shell, wire spec (direction)

Extract platform packages to `pkg/`/separate module with feature-neutral naming; portal becomes the
reference consumer. Build a minimal Tauri/Electron shell against a TS client generated from
`openapi.yaml` (status panel, live port list, in-app notifications, exec console) — the third
consumer that forces API gaps out. Write CDDL for the wire protocol + golden frame vectors
generated from the Go types in a test.

---

## 10. Cross-cutting

**Sequencing.** 1 → 2 strictly ordered; 3 can run parallel to 2; 4 independent of 1–3; 5 needs
3+4; 6 last. Sizes: 1 = M (~1–1.5k LOC; the hub is the subtle part), 2 = S, 3 = L (most regression
surface), 4 = L (ssh auth/hostkey edges dominate), 5 = M, 6 = M + shell.

**Rollout/rollback.** Every stage validates against the live box from a parallel staging instance
(`PORTAL_CONFIG_DIR`/`PORTAL_SOCK`/`PORTAL_API_SOCK` overrides — DESIGN-split-daemon §10 harness)
before cutover. Rollback stays `git checkout && make build && portal install`. Config files,
launchd plist, and remote layout are untouched through Stage 4; Stage 5 only adds remote artifacts.

**Risk register.**

| Risk | Mitigation |
|---|---|
| Hub becomes a second event-ordering authority | Hub tees; it never feeds the engine. Enforced by wiring (engine keeps its channel) and stated in `hub.go` docs. |
| A slow API client stalls the demux/heartbeat path | Hub `Publish` is non-blocking by contract; events handler owns write deadlines. Test with a deliberately wedged subscriber. |
| Spec/handler drift | Conformance test in both directions (D2). |
| Stage-3 services reintroduce heartbeat starvation | `Service` interface *requires* `MaxPayload` + delivery class; outboxes bounded. |
| Native-transport forwards change failure modes (daemon crash kills listeners immediately vs. ControlMaster persisting) | Called out as a behavior change at Stage 4 cutover; doctor check added. |
| API surface grows ahead of consumers | Rule: every endpoint ships with a consumer in the same or next stage. The v1 surface is audited against this rule in §4.5 (which is also why there is no `GET /v1/forwards` or `GET /v1/allowed`). |

**Non-goals (v1).** No TCP listener; no auth tokens; no remote (network) exposure of the API; no
ports-service migration; no supervised agents; no audit-log read endpoint (revisit with the first
shell); no fsnotify on the allow file (the API mutation path supersedes it).

---

## 11. Manual verification checklist (post-Stage-2, live box)

1. Staging instance (`PORTAL_CONFIG_DIR=/tmp/portal-staging/config PORTAL_SOCK=… PORTAL_API_SOCK=…`
   `./portal run`) comes up; `curl --unix-socket $PORTAL_API_SOCK http://portal/v1/status | jq .agent`
   shows the live agent.
2. `curl --unix-socket … http://portal/v1/events` shows snapshot-first, then a state event when a
   listener starts on the box, and a notify event when a Claude Code hook fires.
3. `./portal status` (staging env) prints the agent line from a separate process.
4. `./portal allow 41234` with `python3 -m http.server 41234` on the box (an **ephemeral-range**
   port — non-ephemeral loopback listeners are forwarded regardless of the allowlist, so only an
   ephemeral port actually exercises allow/unallow gating) → forward live well under the old 60s
   worst case; `unallow 41234` drops the forward within ~100ms.
5. Second `./portal run` against the same env exits loudly (single-instance lock).
6. Kill the daemon → `./portal status` falls back to today's output, agent line elided, no errors.
7. Paste (image + text) and notification round-trips unchanged; `portal doctor` RESULT: PASS.
8. `portal uninstall` removes `api.sock`.
