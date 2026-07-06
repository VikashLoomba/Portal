# Portal Bootstrap Matrix + Exec Capability — Stage 5 Contract

**Status:** Approved direction (Stage 5 of the platformization roadmap; Stages 1–4 merged to main).
**Audience:** repo maintainer + implementation agents (codex GPT-5.5 xhigh implements; Opus gates; Fable principal reviews).
**Related:** `DESIGN-local-core-api.md` §8 (the direction this locks), §4.5 (localapi audit/route conventions),
`DESIGN-transport.md` (the `Transport.Stream` this bridges + T2 argv contract), `DESIGN-service-registration.md`
(the `registry.call` waiter machinery this hardens).

---

## 1. Problem & decision

Two things ship in Stage 5, plus two debts the prior stages' principals flagged as preconditions.

**Bootstrap matrix.** Today bootstrap embeds exactly one agent (`portald-linux-amd64`) and uploads it
unconditionally. A platform base must serve heterogeneous dev boxes: probe the box's `uname -sm` once per
connect (cached against the agent's BootID), select the matching artifact from an embedded GOOS/GOARCH matrix
(adding `linux-arm64`), and expose a generic `EnsureArtifact(ctx, name, content)` seam so an app built on the
platform can push its own payloads through the same verified upload path.

**Exec capability (the marquee).** `POST /v1/exec` upgrades (WebSocket over the UDS) to a bidirectional byte
stream bridged to `Transport.Stream` — deliberately bypassing the CBOR control pipe, because interactive
terminal bytes are bulk data, not control frames. It is audited exactly like clip/notify (feature-gated,
peer-uid checked, logged). This is what makes "local shell, remote brain" real: a desktop shell opens an exec
channel and drives a remote process with live stdin/stdout/stderr.

**Debt 1 (Stage-3 principal).** `registry.call`'s `maxInflight` guard counts the SHARED `waiters` map while
the limit is per-call-site — a second Call-based service (exec's control-plane, or any future one) would let
one service's inflight calls starve another's budget. Must become per-service BEFORE a second caller exists.

**Debt 2 (Stage-4 principal).** Callers need a structured way to read a remote exit code from `Transport.Exec`/
`Stream` errors. Add an additive `transport.ExitError` type + `transport.ExitCode(err) (int, bool)` helper; the
exec bridge needs it to report the remote process's exit status to the WebSocket client.

**Explicitly out of scope (v1):** supervised agents / process supervisor (portald reattachable sessions) — that
is design-doc-only this stage (`DESIGN-supervised-agents.md`, not written here); a PTY/allocated-tty on the
remote — Stage 5 deliberately bridges `Transport.Stream`'s pipe model (line/byte streams, no termios, no
window-size propagation, and — because a tty-less channel close does not signal the remote — no
process-kill-on-disconnect; see §8.3). **FULL PTY is a planned, documented Stage 6 capability** (RequestPty +
termios + window-size + signal-on-disconnect), not an indefinite "someday"; this scoping was a Stage-5
drafting decision, ratified 2026-07-06. Also out of scope: authenticating exec to anything beyond the existing peer-uid + single-
instance UDS guard; multiplexing many exec sessions over one WebSocket (one session per connection);
non-`linux` dev boxes (the matrix adds `linux-arm64` only — `darwin`/`windows` agents are not built).

---

## 2. Locked decisions

| # | Decision | Detail |
|---|---|---|
| X1 | **`transport.ExitError` + `ExitCode` helper (Debt 2, do FIRST)** | New additive value type in `internal/transport`: `type ExitError struct { Code int; Signal string; Stderr string }` implementing `error` (`Error()` reads like `remote command exited with status N`); `func ExitCode(err error) (code int, ok bool)` unwraps via `errors.As`. Each transport populates it: **sshctl** and **sshnative** map a non-zero remote exit (ssh exit-status / `*ssh.ExitError` / `*ssh.ExitMissingError`) into it; **localexec** maps `*exec.ExitError`. `Exec`/`Stream`'s existing signatures are UNCHANGED — `ExitError` is returned in the `err` slot, so every existing caller still compiles and behaves the same (they treat any non-nil err as failure); only NEW callers that care call `ExitCode`. Conformance suite gains one case per impl: a command exiting 3 yields `ExitCode(err) == (3, true)`. `Signal`/`Stderr` are best-effort (empty when the impl cannot supply them); no caller may REQUIRE them. |
| X2 | **Per-service waiter budget (Debt 1, do SECOND)** | `registry.call`'s inflight guard counts waiters FOR THAT SERVICE, not the global map. Implementation: partition accounting by service key — either a `map[string]int` inflight counter guarded by `waiterMu` (incremented on register, decremented in the existing deferred delete) or per-service waiter maps; the nonce keyspace stays global (nonces are already unique). `ErrNoWaiterCapacity` still returned when THIS service is at its own `maxInflight`. Behavior for the sole existing caller (clip) is byte-identical (its budget is unchanged; it just no longer shares the denominator). A regression test proves two services with independent budgets do not starve each other: fill service A to its cap, assert service B's call still admits. |
| X3 | **Bootstrap artifact matrix** | `internal/bootstrap` embeds a matrix, not a single binary: `go:embed` `portald-linux-amd64` AND `portald-linux-arm64`; a typed `type artifact struct { goos, goarch string; bytes []byte; sha string }` keyed by `goos/goarch`. The Makefile builds both (`make agent` produces both arches; `linux-arm64` via `GOARCH=arm64`). Selection: **probe `uname -sm` on the box once per connect**, map (`Linux x86_64`→`linux/amd64`, `Linux aarch64`/`arm64`→`linux/arm64`), pick the matching artifact; an unmapped `uname` is a CLEAR error naming what was seen and the supported set (never a silent wrong-arch upload). |
| X4 | **BootID-cached arch probe** | The `uname -sm` result is cached against the box's BootID (the same BootID the agent already reports — a reboot invalidates the cache) so steady-state reconnects don't re-probe. Cache lives on the `Manager`; a BootID change (or first contact) re-probes. The probe is one `Transport.Exec(ctx, nil, "uname", "-sm")` — cheap, pre-upload. Cache is per-`Manager`-instance (in-memory), guarded for the concurrent-reconnect case. |
| X5 | **Generic `EnsureArtifact` seam** | Refactor: today's agent-specific `EnsureUploaded` becomes a caller of a generic `EnsureArtifact(ctx, name string, content []byte) (remotePath string, err error)` that does the existing probe-sha-then-upload-atomically dance for ANY named payload into `remoteDir` (`~/.cache/portal/<name>-<sha>`, symlink `<name>` → it). `EnsureUploaded` keeps its exact current behavior/return (arch-selected agent name) by delegating to `EnsureArtifact`. The hardened upload script (size+sha256 verify, atomic rename, portable sha probe) is UNCHANGED — only parameterized by name/content. App-supplied payloads (a future shell's helper) ride the same verified path. |
| X6 | **`POST /v1/exec` upgrade transport** | A new localapi route `POST /v1/exec` that performs a WebSocket upgrade over the UDS (RFC 6455 handshake on the existing `net.Listener`; a minimal in-tree server-side implementation over `http.Hijacker` — NO new dependency unless §6 says otherwise; if a dep is unavoidable it is called out and approved separately). Request carries the command (argv array, per the T2 shell-join contract — the caller pre-quotes) as a query/subprotocol/first-frame parameter (X7). The socket is the existing 0600, peer-uid-checked, single-instance UDS — exec inherits that trust boundary; NO new auth. |
| X7 | **Exec wire framing** | Client→server and server→client frames are length-delimited typed messages over the WebSocket: a small typed envelope `{stream: "stdin"|"stdout"|"stderr"|"exit"|"error", data: bytes, code: int}` (binary WebSocket frames; stdout/stderr carry raw bytes; `exit` carries the remote exit code from X1's `ExitCode`; `error` carries a transport/setup failure string). stdin close (client half-close) propagates to the remote via closing `Stream`'s stdin `io.WriteCloser`. The bridge copies both directions concurrently and calls `Stream`'s `wait func() error` to obtain the exit; a non-zero exit is delivered as an `exit` frame (NOT a WebSocket error close), reserving abnormal closes for transport failures. |
| X8 | **Exec bridge lifecycle** | The bridge is `Transport.Stream(ctx, argv...)` with ctx bound to the WebSocket connection: client disconnect cancels ctx → `Stream`'s pipes close → remote process is signalled (native: session close; system-ssh: channel close). Both copy goroutines and `wait()` are joined before the handler returns (no leaked goroutines/fds — asserted by a test). A per-connection byte cap is NOT imposed (interactive streams are unbounded by nature) but the read loops are chunked and backpressured by the WebSocket writer. |
| X9 | **Feature gate + audit** | Exec is behind a feature toggle in the existing `/v1/features` machinery (default: enabled? — **default ENABLED**, consistent with clip/notify being on; the toggle exists so an operator can disable it). Every exec session is logged at open (argv, peer uid) and close (exit code / error, duration) exactly as clip/notify audit — one open line, one close line, no per-byte logging. A disabled feature returns 403 before upgrade. |
| X10 | **Client + CLI surface** | `internal/localclient` gains an `Exec(ctx, argv []string, stdin io.Reader, stdout, stderr io.Writer) (exitCode int, err error)` that dials the WebSocket, pumps the local std streams, and returns the remote exit. A `portal exec -- <cmd...>` CLI command wires local os.Stdin/out/err to it and exits with the remote code (so `portal exec -- false` exits 1). This is the end-to-end proof the capability works and the first client of X6/X7. |

### 2.1 Reuse of the T2 shell-join argv contract

`POST /v1/exec` argv is the SAME shell-join contract as `Transport.Exec`/`Stream` (`DESIGN-transport.md` §2.1):
the argv array is joined with single ASCII spaces and re-split by a shell ON THE REMOTE. The exec client and CLI
therefore pass argv straight through to `Transport.Stream` without re-quoting; a caller who needs shell
metacharacters preserved pre-quotes into one argv element, exactly as bootstrap/clipupload/doctor do. The
bridge MUST NOT wrap in an extra `sh -c` (that would double-shell); it hands argv verbatim to `Stream`.

---

## 3. File contract

### 3.1 New files

| Path | Purpose |
|---|---|
| `internal/transport/exiterror.go` (+`_test.go`) | X1: `ExitError` type + `ExitCode` helper. Pure, no deps. |
| `internal/localapi/exec.go` (+`_test.go`) | X6/X7/X8/X9: `POST /v1/exec` handler, WebSocket upgrade, frame codec, the Stream bridge, feature gate + audit. |
| `internal/localapi/wsframe.go` (+`_test.go`) | Minimal RFC 6455 server framing (upgrade handshake + binary frame read/write + close) IF no dependency is used. Isolated so the codec is unit-tested without a live socket. |
| `internal/localclient/exec.go` (+`_test.go`) | X10: client-side `Exec` dialing the WebSocket, pumping std streams, returning the exit code. |
| `cmd/portal/exec.go` (+`_test.go`) | X10: `portal exec -- <cmd...>` command; exits with the remote code. |
| `internal/bootstrap/matrix.go` (+`_test.go`) | X3/X4: embedded arch matrix, `uname` mapping, BootID-cached selection. |

### 3.2 Modified files

| Path | Change |
|---|---|
| `internal/agent/service.go` (+tests) | X2: per-service waiter budget in `registry.call` (partition the inflight count by service key; clip behavior byte-identical). |
| `internal/sshctl/transport.go`, `internal/sshnative/native.go` (+ their tests / conformance) | X1: map remote non-zero exit into `transport.ExitError` from `Exec` and `Stream`'s `wait`. |
| `internal/transport/localexec/localexec.go` | X1: map `*exec.ExitError` into `transport.ExitError`. |
| `internal/transport/conformance/conformance.go` | X1: add the exit-code case (command exits 3 → `ExitCode==(3,true)`) run for every impl. |
| `internal/bootstrap/manager.go` (+tests) | X3/X4/X5: `EnsureArtifact` generic seam; `EnsureUploaded` delegates with arch-selected name; BootID-cached arch probe; matrix wiring. |
| `internal/localapi/server.go` (+tests) | X6: register `POST /v1/exec`; wire the exec handler's deps (Transport.Stream provider, feature gate, logger). |
| `internal/localapi/state.go` / features (+tests) | X9: add the `exec` feature toggle (default enabled) to the existing feature machinery. |
| `Makefile` | X3: `make agent` builds `portald-linux-amd64` AND `portald-linux-arm64`; both `go:embed`'d. |
| `internal/localapi/openapi.yaml` (+ the committed spec test) | X6/X7: document `POST /v1/exec` (upgrade semantics, frame envelope) so the Stage-6 generated client sees it. |

---

## 4. Implementation order (green after every unit)

| Unit | Scope |
|---|---|
| u1 | **X1** `transport.ExitError` + `ExitCode`; populate in all three transports; conformance exit-code case. Pure additive; nothing downstream yet. Green after unit. |
| u2 | **X2** per-service waiter budget in `registry.call` + the two-service no-starvation regression test. Clip byte-identical. Green after unit. |
| u3 | **X3/X4/X5** bootstrap matrix: Makefile two-arch build + dual embed, `artifact` matrix, `uname -sm` map, BootID-cached probe, generic `EnsureArtifact` with `EnsureUploaded` delegating. Green after unit (existing amd64 box path unchanged). |
| u4 | **X6/X7 codec only**: `wsframe.go` (upgrade handshake + binary frame read/write/close) and the exec frame envelope, both fully unit-tested WITHOUT the bridge or a live transport (drive the codec over an in-memory pipe). Green after unit. |
| u5 | **X6/X7/X8/X9 bridge**: `POST /v1/exec` handler wiring the codec to `Transport.Stream`, ctx/lifecycle join, feature gate + audit, route registration. Tested against a fake/localexec transport over a real UDS (hermetic). Green after unit. |
| u6 | **X10** `localclient.Exec` + `portal exec` CLI; end-to-end test (CLI → UDS → localexec transport → exit code round-trips, incl. non-zero exit and stdin half-close). openapi.yaml + spec test. Green after unit. |
| u7 | Hardening: full `-race` + conformance; greps (no double-`sh -c` in the bridge; no new deps beyond §6; exec feature gate enforced before upgrade); goroutine/fd-leak assertions on the bridge; doc-comment sweep; EC audit gap-fill. |

---

## 5. Exit criteria

1. `make build`, `go vet ./...`, `make test`, `go test -race ./...` green; changed packages gofmt-clean.
2. **X1:** `transport.ExitError`/`ExitCode` covered per-impl by the conformance suite (command exits 3 → `(3,true)`); a zero/normal exit → `ExitCode` returns `(_, false)` or `(0, true)` as documented; existing Exec/Stream callers compile and behave unchanged (goldens intact).
3. **X2:** a test fills service A's waiter budget to `maxInflight` and proves service B still admits a call (independent budgets); clip's own behavior is byte-identical (its existing tests pass unmodified in intent).
4. **X3:** matrix selects `linux/amd64` for `Linux x86_64` and `linux/arm64` for `Linux aarch64`; an unmapped `uname` yields a clear error naming the observed string and supported set (no upload attempted). Makefile builds+embeds both arches; `go.mod` unchanged by the matrix.
5. **X4:** the arch probe runs once and is reused across reconnects with an unchanged BootID; a BootID change re-probes (unit-tested with a fake transport recording `uname` invocations; concurrent reconnect does not double-probe or race — `-race`).
6. **X5:** `EnsureArtifact(name, content)` uploads an arbitrary payload through the verified size+sha+atomic-rename path and is idempotent (second call with the same content re-uses the remote copy, no re-upload); `EnsureUploaded` delegates and its return/behavior is unchanged for the amd64 agent.
7. **X6/X7:** `POST /v1/exec` completes the WebSocket upgrade over the UDS; the frame codec round-trips stdin/stdout/stderr/exit/error envelopes (unit-tested over an in-memory conn); malformed/oversized frames are rejected without panic.
8. **X8:** the bridge joins both copy goroutines and `wait()` before returning; client disconnect cancels ctx and tears down the remote stream; a leak test (goroutine count / no dangling fds) passes; no double-`sh -c`.
9. **X9:** exec is feature-gated (disabled → 403 before upgrade); one open + one close audit line per session (argv + peer uid on open, exit/error + duration on close), no per-byte logging.
10. **X10:** `portal exec -- <cmd>` round-trips over localexec end-to-end: stdout/stderr faithful, stdin half-close propagates, `portal exec -- false` exits 1 and `-- true` exits 0; `localclient.Exec` returns the remote exit code.
11. **Deps:** any new dependency introduced (e.g. a WebSocket lib) is exactly what §6 approved and no more; if the in-tree framing is used, `go.mod` is unchanged.

---

## 6. Dependency decision (WebSocket)

The exec upgrade needs server-side (localapi) and client-side (localclient) WebSocket framing over a UDS.
Two options; **the contract picks in-tree framing** unless implementation surfaces a blocker:

- **In-tree minimal RFC 6455 (preferred):** the traffic is a private, same-host, single-subprotocol byte
  stream over a trusted UDS — not a browser-facing server. A minimal server upgrade (`Sec-WebSocket-Accept`
  handshake via `http.Hijacker`) + binary frame read/write + close/ping handling is ~200–300 LOC, fully
  unit-testable, and keeps the "only new dep is x/crypto (Stage 4)" discipline intact. `wsframe.go` isolates it.
  Masking: client frames are masked per spec; server frames are not — both handled.
- **`nhooyr.io/websocket` / `gorilla/websocket` (fallback ONLY if in-tree framing proves fragile):** if
  hijack-based framing hits a real correctness wall (fragmentation, control-frame interleaving) that isn't worth
  hand-rolling, adopt ONE well-maintained lib — but this REVERSES the minimal-dependency stance and must be
  called out in the unit summary + this doc amended before merge, not decided silently.

Implementers: start in-tree (u4). Escalate to the fallback only with a concrete correctness reason.

---

## 6.1 Principal fast-follows (non-blocking — address in Stage 6)

Recorded by the Fable principal (verdict: approve) — minor edge findings, none merge-blocking:

1. **`localclient.Exec` stdin-pump ctx honor:** after the exit frame, `Exec` sets a stdin read deadline only
   if the reader implements `SetReadDeadline`, then blocks unconditionally on `<-stdinDone`. A deadline-less
   blocking reader (e.g. an embedding UI's `io.Pipe`) can hang past ctx cancellation. Fix: select on
   `ctx.Done()` alongside `stdinDone`.
2. **Exec audit correlation:** `exec-open` logs host+uid+argv; `exec-close` logs host+code+err+dur but no argv
   and no session id, so two concurrent same-host sessions can't be paired by an operator. Fix: a per-session
   id on both lines (or repeat argv on close).
3. **`EnsureArtifact` name validation (do BEFORE Stage 6 exports it):** `remotePath = remoteDir + name +
   digest` is spliced unquoted into the remote `bash -c` probe/upload/`ln -sf` scripts. In-process callers are
   trusted today, but a `name` with spaces/`../`/`$(...)` would break or inject. Validate `name` (charset +
   no path separators) before it reaches a shell — mandatory before the seam is exported to app authors.
4. **Non-PTY disconnect orphan (§8.3):** a tty-less remote process ignoring stdio survives client disconnect
   (standard ssh behavior, reproduced by plain ssh — see §8.3). Resolved by the **documented Stage 6 full-PTY
   capability** (RequestPty gives SIGHUP-on-close for free); not a Stage-5 defect.

Stage-6 watch items (principal): ~180 LOC of WS client framing is duplicated in `localclient` rather than
shared with `localapi`'s codec — consolidate at extraction; `openapi.yaml` documents `/v1/exec` as prose only
(no frame schema), so the generated TS client needs a hand-written WS/CBOR layer; dual-arch embed adds ~4MB to
the `portal` binary (acceptable now).

## 7. Risks

| Risk | Mitigation |
|---|---|
| Hand-rolled WebSocket framing is subtle (masking, fragmentation, control frames) | Isolated `wsframe.go` with exhaustive codec unit tests over in-memory conns (u4) BEFORE any bridge wiring; fallback to a vetted lib is pre-authorized in §6 with a stated reason. |
| Exec bridge leaks goroutines/fds on odd disconnect orderings | u5/u8 join both copy goroutines + `wait()` unconditionally; explicit leak test; ctx bound to the connection. |
| Per-service waiter refactor changes clip's observable behavior | X2 keeps clip's budget numerically unchanged (only the denominator de-shares); clip's existing tests must pass unmodified in intent + a new no-starvation test. |
| Wrong-arch agent uploaded to a box | X3 fail-closed on unmapped `uname` (clear error, no upload); BootID cache re-probes on reboot. |
| Interactive exec bytes bypass the audited CBOR pipe | By design (§8 direction: bulk not control); mitigated by same UDS trust boundary + feature gate + open/close audit (X9); NO new network exposure (same 0600 peer-uid UDS). |
| `ExitError` mapping differs across transports | Conformance suite pins `ExitCode==(3,true)` identically for all three impls (X1); `Signal`/`Stderr` explicitly best-effort so no caller depends on impl-specific richness. |

---

## 8. Manual verification (live box, post-merge)

1. `portal exec -- uname -sm` over the real box returns the box's arch and exit 0; `portal exec -- false` exits 1.
2. An interactive-ish stream (`portal exec -- sh -c 'echo hi; cat'` with piped stdin) round-trips stdout and
   propagates stdin EOF (process exits when stdin closes).
3. Disconnect mid-exec (Ctrl-C the client): the bridge tears down its side (local ssh subprocess exits,
   goroutines joined). **KNOWN LIMITATION (validated 2026-07-06):** a NON-PTY remote process that ignores
   stdio (e.g. `sleep`) SURVIVES client disconnect — sshd closes the channel's stdio but does not signal a
   tty-less remote command, so it runs to completion. This is standard OpenSSH behavior, reproduced
   IDENTICALLY by a plain `ssh -S <master> 'sleep 200'` with the local ssh killed (zero portal code
   involved). That control test — not the scope statement in §1 — is the evidence it is standard OpenSSH
   behavior and NOT a bridge defect (the bridge's own teardown is correct: local ssh subprocess exits,
   goroutines join). A remote process that DOES touch stdio (reads stdin / writes stdout) dies promptly on
   disconnect via EPIPE/EOF, which the §8.2 stdin-EOF case exercises. Killing a tty-less remote process on
   disconnect requires a PTY (SIGHUP on channel close) or an explicit remote signal — both land with the
   **documented Stage 6 full-PTY capability**.
4. Arch matrix: on the amd64 box, status/doctor unchanged and the agent SHA is the amd64 artifact; the probe
   is cached (logs show one `uname` per connect, not per reconcile). (arm64 box validation is best-effort/N/A
   without an arm64 dev box.)
5. Disable the exec feature (`portal` feature toggle) → `POST /v1/exec` returns 403 and `portal exec` reports it.
