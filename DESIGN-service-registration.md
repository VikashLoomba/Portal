# Portal Service Registration — Stage 3 Contract (ProtoVersion 4)

**Status:** Approved direction (Stage 3 of the platformization roadmap; predecessor:
`DESIGN-local-core-api.md`, Stages 1–2 merged to main at `69bad4d`).
**Audience:** repo maintainer + implementation agents.
**Related:** `DESIGN-split-daemon.md` (wire protocol, single-writer serve loop, seq semantics),
`DESIGN-clipboard-read-interception.md` (clip/notify relay, cmd-socket grammar, timeout budget),
`DESIGN-local-core-api.md` (hub tee, QoS classes).

---

## 1. Problem & decision

The wire Envelope is a closed tagged union that fuses two layers: **session frames**
(Hello/HelloAck/Subscribe/Snapshot/Port*/Heartbeat/Ping/ReqSnap/Shutdown/Bye/AgentError) and
**feature frames** (OpenURL, ClipRequest/ClipResponse, Notify). Every new feature edits
`internal/protocol` (3 files), the agent Serve loop, the client demux, and `countEnvelopeFields`.
For a base platform, features must become **registered services** on both ends: one generic `Msg`
frame, capability negotiation in the handshake, and per-service isolation contracts that turn
today's structurally-enforced invariants (sole writer, seq isolation, payload discipline,
QoS-classed delivery) into **declared, enforced registration parameters**.

**Explicitly out of scope:** migrating the ports/session layer (Subscribe/Snapshot/Port* stay
native — they *are* the session); dynamic/third-party service loading (services are compiled in);
exposing service plumbing over the local API; any remote-shim redeployment (the cmd-socket grammar
is byte-preserved, so deployed shims keep working untouched).

---

## 2. Locked decisions

| # | Decision | Detail |
|---|---|---|
| S1 | **ProtoVersion 4, hard cut** | The legacy `OpenURL`, `ClipRequest`, `ClipResponse`, `Notify` Envelope fields are **deleted**, not dual-pathed. Both binaries ship from one tree and bootstrap re-uploads on SHA mismatch; a blocked re-upload surfaces as the existing loud `CodeProtocolMismatch` fatal. No negotiation, no fallback — same doctrine as v2/v3. |
| S2 | **One generic frame** | `Msg { Service string, Kind string, Seq uint64, Payload cbor.RawMessage }`. The payload structs (`OpenURL`, `ClipRequest`, `ClipResponse`, `Notify`) survive **unchanged with their existing cbor tags** — they just travel inside `Msg.Payload` instead of as Envelope fields. `countEnvelopeFields` counts `Msg`; the one-field-per-frame invariant is unchanged. |
| S3 | **Per-service, per-direction Seq** | `Msg.Seq` is monotonic per (service, direction) within one agent process, stamped by the registry — **never** touching the port-event staleness counter `s.seq` (the invariant clipSeq/notifySeq already enforce today; a test asserts it). Purely for log correlation on fire-and-forget services. |
| S4 | **Symmetric capability advertisement** | `Hello` gains `Services map[string]uint32` (client's registered handlers) and `HelloAck` gains the same (agent's registered services). A client-side handler whose service the agent lacks logs one warning and stays dormant. An agent-side service whose client counterpart is missing answers its cmd-socket callers exactly as it answers `!hasClient` today (`none\n` / `no-client\n`) — shims fall through cleanly. Protocol-version mismatch stays fatal; **service**-version mismatch is per-service disable + one warning (loud, not fatal). Version rule: **exact equality**; on inequality BOTH sides treat the service as absent (client handler dormant + one warning; agent answers its cmd-socket verbs as if the client lacked the service). Cross-version pairs only exist in transient upgrade windows — one tree, SHA-keyed re-upload. |
| S5 | **Sole-writer discipline via outboxes** | The Serve loop remains the ONLY writer of agent→client frames. Services never call `enc.Write`; they emit into a per-service bounded **outbox** channel the Serve select drains (generalizing today's `openURLCh`/`clipReqCh`/`notifyCh`). Outbox capacity and overflow policy (`DropNewest` for fire-and-forget) are declared at registration. **Overflow reply text is owned by each service's cmd-verb handler and must be grammar-preserving:** clip answers `none\n` on a full outbox (matching today's `handleClipReq`; `dropped\n` is never a valid clip reply), notify and open answer `dropped\n` (matching today). |
| S6 | **Per-service payload cap** | Registration declares `MaxPayload int` (≤ `MaxFrameBytes`). Enforced at dispatch on BOTH ends: an oversized inbound `Msg.Payload` for that service is dropped with a `slog` warning — the **session lives** (frame-level `MaxFrameBytes`/codec behavior is unchanged and still fatal). |
| S7 | **Panic isolation** | Service dispatch on both ends runs under `recover()`: a panicking service handler logs loudly and drops that frame; it never kills the session or the Serve loop. (The stdout guard from DESIGN-split-daemon still catches rogue writes.) |
| S8 | **cmd-socket verbs are claimed at registration** | The tab-framed verb dispatcher stays; each agent-side service declares the verbs it owns (`open`; `notify`; `clip`) plus its per-verb socket deadline (clip keeps 11s, others 5s — the §4.5 budget is byte-preserved). Duplicate verb claims panic at startup (programmer error). Unknown verbs still answer `rejected\n` (default-deny). |
| S9 | **Correlation generalized** | The clip machinery (`clipWaiters` keyed by nonce, random per-process `epoch`, non-blocking waiter delivery, stale-epoch drop) is lifted into a registry-level request/response helper: `Call(ctx, kind, payload) (response, error)` available to any agent-side service. Epoch semantics are identical to today's (DESIGN-clipboard §4.4). |
| S10 | **Client delivery classes** | Each client-side handler registration declares its channel capacity + drop policy, preserving today's QoS isolation (clip cap 8, notify cap 16, both non-blocking sends — a port-event burst can never evict a paste). The demux never blocks; that stays structural. |
| S11 | **Facade compatibility** | `Client.ClipEvents()`, `Client.NotifyEvents()`, `Client.SendClipResponse()`, and the OpenURL flow keep their exported signatures as thin facades over the registry, so `cmd/portal/run.go`'s handlers and the hub tee change minimally. The hub tee moves to the notify dispatch site with identical behavior (`hubtee_test.go` must stay green unmodified in intent). |

---

## 3. Wire format after v4

```go
// internal/protocol/envelope.go — Envelope after Stage 3
type Envelope struct {
    // client → agent (session, unchanged)
    Hello     *Hello
    Subscribe *Subscribe
    Ping      *Ping
    ReqSnap   *ReqSnap
    Shutdown  *Shutdown

    // agent → client (session, unchanged)
    HelloAck     *HelloAck
    SubscribeAck *SubscribeAck
    Snapshot     *Snapshot
    PortAdded    *PortAdded
    PortRemoved  *PortRemoved
    Heartbeat    *Heartbeat
    AgentError   *AgentError
    Bye          *Bye

    // services (v4): the ONLY feature frame, either direction.
    Msg *Msg `cbor:"msg,omitempty"`
}

// Msg is the generic service frame. Payload is the service's own CBOR struct
// (the v3 message types, tags unchanged).
type Msg struct {
    Service string          `cbor:"svc"`
    Kind    string          `cbor:"k"`
    Seq     uint64          `cbor:"seq,omitempty"`
    Payload cbor.RawMessage `cbor:"p,omitempty"`
}

// Hello / HelloAck each gain:
//   Services map[string]uint32 `cbor:"services,omitempty"`
```

Service names and kinds (v1 of each service):

| Service | Version | Kinds (direction) | Payload struct |
|---|---|---|---|
| `openurl` | 1 | `open` (agent→client) | `OpenURL` |
| `notify` | 1 | `event` (agent→client) | `Notify` |
| `clip` | 1 | `req` (agent→client), `resp` (client→agent) | `ClipRequest` / `ClipResponse` |

---

## 4. Agent-side contract

### 4.1 New files

| Path | Purpose |
|---|---|
| `internal/agent/service.go` | `Service` interface + `registry`: registration (verb claims, outbox construction, dup-verb panic), inbound `Msg` dispatch (payload-cap check, per-service decode, recover), outbox drain plumbing for the Serve select, per-service seq stamping, and the generalized `Call` request/response helper (nonce+epoch waiters — the lifted clip machinery). |
| `internal/agent/service_test.go` | Registry unit tests with a fake service: registration/dup-verb panic, dispatch/decode, MaxPayload drop (session lives), panic isolation, outbox overflow policy, seq isolation from `s.seq`, `Call` timeout/epoch-mismatch/late-response cases. |
| `internal/agent/svc_openurl.go` | `openurl` service: claims verb `open`, relays URL upward (today's `handleOpenReq` semantics: 5s deadline, `ok/no-client/rejected/dropped` replies). |
| `internal/agent/svc_notify.go` | `notify` service: claims verb `notify`, bounded JSON parse (`notifyBodyMax`), classify passthrough, fire-and-forget emit (today's `handleNotifyReq` semantics). |
| `internal/agent/svc_clip.go` | `clip` service: claims verb `clip`, `targets/image/text` subcommand handling via `Call` with the existing timeout budget (`clipTimeout` 9s, `clipSockDeadline` 11s, `maxInflightClip` 4), replies byte-identical to today's grammar. |

### 4.2 Modified files

| Path | Change |
|---|---|
| `internal/protocol/envelope.go` | Delete `OpenURL`/`ClipRequest`/`ClipResponse`/`Notify` fields; add `Msg`. `ProtoVersion = 4` with a doc comment continuing the v2/v3 honest-negotiation rationale. |
| `internal/protocol/messages.go` | Add `Msg`; add `Services` to `Hello` and `HelloAck`. Payload structs stay (tags unchanged) with comments noting they now travel in `Msg.Payload`. |
| `internal/protocol/codec.go` | `countEnvelopeFields` updated (session fields + `Msg`). Add `MarshalPayload`/`UnmarshalPayload` helpers using the package `encMode`/`decMode` so services never construct their own CBOR modes. `codec_test.go`'s per-field roundtrip/count tests (`TestRoundtripClipRequest/Response`, the count assertions building legacy fields) migrate to `Msg` in the units that delete their fields. |
| `internal/agent/server.go` | Serve loop: dispatch inbound `Msg` to the registry; drain registered outboxes (replacing the `openURLCh`/`clipReqCh`/`notifyCh` arms); handshake sends/receives `Services` maps; `hasClient` gating generalized to "client advertised this service". cmd-socket dispatcher routes claimed verbs to services. The clip/notify/openurl-specific fields and handlers move out to their service files. `clipSeq`/`notifySeq`/`epoch`/`clipWaiters` migrate into the registry/`Call` helper. |
| `internal/agent/server_test.go` | Frame expectations move to `Msg`; existing handshake/snapshot/delta/shutdown coverage stays green in intent. |
| `internal/agent/clip_test.go`, `internal/agent/notify_test.go` | Migrate to `Msg` frames **in the same unit that deletes the fields they construct** (they reference `env.ClipRequest`/`Envelope{ClipResponse}`/`env.Notify` directly, so u4/u5 fail to compile otherwise). They hold `TestClip_DoesNotAdvanceSeq` and the clip timeout-budget tests that EC4/EC9 generalize — migrate, never delete. As on the client side: **any test speaking raw frames** migrates with the field it uses. |
| `cmd/portald/main.go` | Construct the registry: `agent.New(cfg, agent.WithServices(openurl, notify, clip))` (or equivalent explicit registration before `Serve`). |

Everything in `internal/agent/filter.go` and `internal/agent/watcher/` is untouched.

---

## 5. Client-side contract

### 5.1 New files

| Path | Purpose |
|---|---|
| `internal/agentclient/registry.go` | `RegisterHandler(spec HandlerSpec)` where `HandlerSpec{Service string, Version uint32, ChanCap int, OnEvent …typed decode hook…}`; demux `Msg` routing (per-service decode, payload cap, recover, non-blocking send per S10); `Send(service, kind, payload)` for client→agent service frames; negotiation bookkeeping (agent's advertised services from HelloAck; dormant-handler warning). |
| `internal/agentclient/registry_test.go` | Routing, decode failure (logged drop, session lives), unknown-service inbound drop, QoS non-eviction (port burst + pending clip event), dormancy warning, `Send` before connect (buffered/err semantics identical to today's `SendClipResponse`). |

### 5.2 Modified files

| Path | Change |
|---|---|
| `internal/agentclient/client.go` | Demux: `case env.Msg != nil` → registry; legacy OpenURL/Clip/Notify arms deleted. `Hello` sends the client's `Services` map. Facades preserved per S11: `ClipEvents()`/`NotifyEvents()`/`SendClipResponse()` delegate to the registry; the hub tee fires at notify dispatch. `envType()` updated. |
| `internal/agentclient/client_test.go` / `hubtee_test.go` | Updated to v4 frames; behavioral assertions unchanged in intent. |
| `cmd/portal/run.go` | Ideally unchanged (facades); at most constructor-site registration of the three client handlers. |
| `internal/localapi/server_test.go` (and any test speaking raw frames) | v4 frame shapes. |

---

## 6. Implementation order (green after every unit)

| Unit | Scope | Key tests |
|---|---|---|
| u1 | Protocol additions, **additive only**: `Msg`, `Services` fields, codec counting + payload helpers. ProtoVersion stays 3 (nothing consumes yet). | Roundtrip incl. `Msg` + RawMessage passthrough; one-field invariant with `Msg`; dup-key fail-closed unchanged. |
| u2 | Agent registry + client registry, **dual-stack** (legacy paths still live; new machinery exercised by fakes only). | `service_test.go` + `registry_test.go` full matrices (S5–S10). |
| u3 | Migrate `openurl` end-to-end; delete `OpenURL` field; **ProtoVersion → 4**. | io.Pipe e2e: negotiate, `open` verb → `Msg` → client URL sink; `rejected/no-client` grammar byte-identical. |
| u4 | Migrate `notify`; delete `Notify` field; hub tee relocates. | e2e notify round trip; `hubtee_test` green; classify + `[unverified]` semantics preserved. |
| u5 | Migrate `clip` onto `Call`; delete `ClipRequest`/`ClipResponse` fields; remove migrated fields/handlers from server.go/client.go. | e2e clip targets/image/text with timeout budget asserted (9s/11s ordering), epoch-stale drop, `maxInflightClip` bound. |
| u6 | Hardening + full e2e: zero-service client, unknown service/kind drops, seq isolation, panic isolation, mixed-version fatal, cmd-socket grammar golden test. | See exit criteria. |

## 7. Exit criteria

1. `make build`, `go vet ./...`, `make test`, `go test -race ./...` green; new/changed packages gofmt-clean.
2. io.Pipe e2e (real `agent.Server` + real `Client`): handshake advertises services both ways; openurl, notify, and clip round-trip via `Msg` frames only.
3. **Grammar golden test:** the cmd-socket byte grammar (`open\t…`, `clip\ttargets` → `ok\timage\n`…, `notify\t…` → `ok\n`, unknown → `rejected\n`) is asserted against hard-coded strings identical to the v3 behavior — deployed shims must keep working with zero redeployment.
4. Seq isolation: a burst of service traffic never advances the port-event staleness counter (assert snapshot seq unchanged; the existing "never touch `s.seq`" test generalized).
5. Payload cap: oversized `Msg.Payload` for a service → dropped + logged, session alive; oversized *frame* still fatal (frame-size behavior unchanged; codec_test's legacy per-field roundtrips migrate to `Msg`).
6. Panic isolation both ends: a deliberately-panicking fake service drops the frame and the session continues (heartbeats keep flowing).
7. Mixed version: `Hello{pv:3}` → `AgentError{CodeProtocolMismatch, Fatal}` (existing test updated to 3-vs-4).
8. Zero-service consumer: a client with no registered handlers completes the handshake and receives ports/snapshots normally (the "a consumer can omit features" proof).
9. Clip timeout budget: agent answers `none\n` before the 11s socket deadline when the client never responds (fake clock or shortened test constants — same technique as today's server tests).
10. Service-version mismatch: client `openurl@1` vs agent `openurl@2` → per-service disable with one
    warning on each side, session healthy, other services unaffected (io.Pipe test).

## 8. Risks

| Risk | Mitigation |
|---|---|
| Outbox drain starves heartbeats or vice versa | Outboxes are small and drained in the same select as today's three channels; heartbeat emission logic untouched. e2e asserts heartbeats flow during service bursts. |
| Clip budget regression during the `Call` lift | The agent-side 9s < 11s budget (DESIGN-clipboard §4.5; the 13s shim-side deadline lives in `cmd/portald/main.go` and is remote-side, untouched) is restated as constants owned by `svc_clip.go`; EC9 asserts the ordering behaviorally. |
| Facade drift breaks run.go handlers silently | S11 facades keep exported signatures; `cmd/portal` tests (clip/notify handler paths) must pass unmodified in intent. |
| Registry becomes a second writer | Structural: services get no encoder reference; only outbox channels. A test asserts a service cannot emit after Serve exits (no goroutine writes). |
| Deleting envelope fields breaks an unnoticed consumer | `grep -rn "env\.\(OpenURL\|ClipRequest\|ClipResponse\|Notify\)"` must be empty at u6; the compiler enforces the rest. |
| Upgrade window (old installed daemon + new box agent or vice versa) | Same story as v2/v3: SHA-keyed bootstrap re-upload; version mismatch is loud and self-healing. `portal doctor` unchanged. |

## 9. Manual verification (live box, post-merge)

1. Build + `portal install`-free staging run (the §10 harness from DESIGN-split-daemon): paste
   (image + text) into Claude Code on the box works; notification hook pops natively.
2. `portal doctor` → `RESULT: PASS` (shim version marker unchanged — no redeploy occurred).
3. `xdg-open <url>` on the box opens on the Mac with auto-forward.
4. `portal status` agent line shows the new SHA; `/v1/events` still streams notify events (hub tee
   relocation invisible to the local API).
