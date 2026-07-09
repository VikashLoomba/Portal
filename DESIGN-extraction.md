# Stage 6: extraction, full PTY, wire spec, reference shell

Status: CONTRACT (locked before implementation). Follows `DESIGN-local-core-api.md` §9
("Stage 6 — extraction, reference shell, wire spec"), `DESIGN-transport.md` §6.1 watch items,
and `DESIGN-exec-bootstrap.md` §6.1 fast-follows. Decisions are numbered E1–E16; units u1–u12;
exit criteria EC1–EC17; manual live-box checks §8.

## 1. Goal and scope

Stage 6 turns devportal's internals into the reusable platform Option A promised: a public Go
surface under `pkg/` for "local shell, remote brain" app authors, a full PTY capability for
interactive remote commands (user-ratified 2026-07-06), a language-neutral wire spec with
cross-language test vectors, and a reference non-Go client (TypeScript library + Electron
example shell) proving a desktop app can drive the core daemon end-to-end.

In scope:
- Pre-export hardening owed from Stage 5 §6.1: WS-framing consolidation, exec audit session
  ids, `localclient.Exec` ctx honor, `EnsureArtifact` name validation (+ a deliberate verdict
  on the X5 consolidation forward-note — see E4).
- Full PTY: RequestPty + window-size changes + hangup-on-disconnect semantics, on all three
  transports, threaded through `/v1/exec` and `portal exec`.
- Extraction: `pkg/` promotion of the platform packages, exported service registration on both
  agent and client sides, typed `Impl` constants, conformance suite loopback made
  factory-declared.
- Wire spec: `docs/wire.cddl` (CBOR frame protocol + exec WS subprotocol) with golden test
  vectors verified from BOTH Go and TypeScript; `openapi.yaml` gains real schemas.
- Reference client: `clients/ts` (zero-runtime-dependency TypeScript, gate-tested) and
  `examples/shell-electron` (thin Electron app, NOT gate-built, manually validated).

Out of scope (documented, not silently dropped):
- Termios mirroring of the local terminal's exact modes over the exec subprotocol (E6 fixes a
  default mode table; full mirroring is a recorded v2 candidate).
- An explicit remote-signal frame (`kill -TERM` mid-session). Hangup-on-disconnect comes free
  with the PTY (E8); an addressable signal verb can layer on later without wire breakage.
- Per-language clients beyond TypeScript; packaging/publishing (`npm publish`, Go module
  tagging) — the spec + vectors are the escape hatch, per Option A.
- Windows anything.
- CLI-side caching of `ssh -G` resolution (E16.3 documents the deliberate keep).
- Hop-level ProxyCommand support in native ProxyJump chains — stays the clear error shipped in
  Stage 5 (`DESIGN-transport.md` §6.1.2 recorded full support as a later-stage candidate;
  still deferred, unchanged).

Compatibility guarantees (regression-tested, not aspirational):
- `go.mod`/`go.sum` stay byte-identical (third consecutive stage; E7 documents the
  pre-authorized escape hatch).
- Mac↔Linux ProtoVersion stays 4 — Stage 6 changes NO `PF` frame. portald recompiles because
  packages move, so exactly one SHA-keyed agent re-upload occurs after upgrade (expected,
  same as every stage).
- Remote artifact paths (`~/.cache/portal/agent-<gitsha>`, the `portald` symlink) stay
  byte-identical (E4) — an already-provisioned box must NOT re-upload or lose its wrapper
  symlink because of the quoting hardening.
- `portal status` output on the system transport stays byte-identical; non-PTY `portal exec`
  behavior is unchanged (§8.4).
- Module path stays `github.com/VikashLoomba/Portal`.

## 2. Decisions

### E1. WS framing consolidates into `internal/execws`

One new package `internal/execws` absorbs both duplicated RFC 6455 copies (server:
`internal/localapi/wsframe.go` + `writeFull`; client: ~180 LOC of `internal/localclient/exec.go`)
AND the application envelope (`ExecFrame`, its CBOR codec, and the stream-tag constants,
today in `internal/localapi/exec.go:18-49`):

- Opcode type + constants (`OpContinuation/OpText/OpBinary/OpClose/OpPing/OpPong`), the
  RFC 6455 GUID, `AcceptKey(key string) string`, `MaxPayload = 16<<20`.
- `ReadFrame(r io.Reader, requireMasked bool) (op Opcode, payload []byte, err error)` — one
  core parser; the masked-vs-unmasked assertion direction is the only parameter. Rejects
  fragmentation/continuation, reserved bits, oversize, and wrong mask direction exactly as
  both copies do today.
- `WriteFrame(w io.Writer, op Opcode, payload []byte, mask bool) error` — one writer; random
  4-byte mask key generated only when `mask=true`. Plus close/ping/pong helpers and the
  full-write loop (one name, `writeFull`).
- `ExecFrame{Stream, Data, Code, Rows, Cols}` + `Encode/DecodeExecFrame` + stream tags
  (`stdin/stdout/stderr/exit/error/winch` — winch is new, E8).

Direction-specific bits stay put: `wsUpgrade` + `headerContainsToken` (needs
`http.ResponseController`) remain in `localapi`; the raw upgrade-request builder and
`Sec-WebSocket-Key` generation remain in `localclient`. After u1, `localclient` no longer
imports `localapi` for frame types (this unblocks E11's clean `pkg/client` promotion), and a
grep for a second opcode table finds nothing (EC3).

The in-tree-vs-library question (Stage 5 §6 escape hatch) is now CLOSED deliberately: framing
stays in-tree. Rationale: one copy, exhaustive codec tests already exist and move with it, the
subprotocol needs only unfragmented binary frames + ping/pong/close, and zero-dep remains a
platform selling point. Escalation to a library requires amending THIS section first.

### E2. Exec audit gains a session id

`handleExec` mints a per-session id (8 lowercase hex chars from `crypto/rand`) before the
upgrade. Both audit lines carry it: `exec-open ... sid=<id>` and `exec-close ... sid=<id>`
(field order: sid immediately after `host=`). `audit.ExecOpen/ExecClose` signatures grow a
`sid string` parameter. Concurrent same-host sessions are now pairable (EC4). PTY sessions
additionally log `pty=1` on the open line (E8).

### E3. `localclient.Exec` honors ctx past a blocking stdin reader

The post-exit join (`internal/localclient/exec.go:170-180`) currently blocks unconditionally
on `<-stdinDone`. Fix per the Stage 5 principal: race `<-stdinDone` against `<-ctx.Done()`;
on ctx-cancel, abandon the pump goroutine (it exits when its blocked `Read` eventually
returns — accepted, documented leak-until-unblock) and return. The deadline-setting fast path
for `readDeadlineSetter` readers stays. Regression test: `Exec` with an `io.Pipe` stdin that
never delivers data returns promptly after the exit frame + ctx cancel (EC5; this hangs
forever today).

### E4. `EnsureArtifact` validates `name`; the two addressing schemes stay deliberately split

- Validation, before anything touches a shell: `name` must match `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`
  (must start alphanumeric; no `/`, no spaces, no shell metacharacters by construction; max 64).
  Violations return a typed error naming the rule. Applied in `EnsureArtifact` itself so every
  future caller inherits it.
- Defense in depth — interior-quoting VERDICT (principal-ratified amendment): interior
  single-quoting of the TILDE-LEADING path values is REJECTED because it defeats `bash -c`
  tilde expansion. `remoteDir` is the constant `~/.cache/portal` (`manager.go:34`);
  `remotePath` = `remoteDir+"/agent-"+sha` (`manager.go:122`) and
  `remoteDir+"/"+name+"-"+digest` (`manager.go:155`); these sit UNQUOTED inside the outer
  `shellQuoted` script so the remote shell tilde-expands them. Wrapping them in single quotes
  makes the probe `test -x '~/.cache/portal/agent-<sha>'` test a LITERAL `~` dir → always
  MISSING → spurious re-upload on EVERY connect, and the `ln -sf` portald symlink target
  becomes a literal `~/...` path → the remote xdg-open wrapper breaks (EC6/EC16 regression,
  INVISIBLE to the hermetic recording fake which never runs a shell). Corrected contract:
  name-validation (E4's regex `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`) is the SOLE injection
  boundary; the OUTER `shellQuoted` wrapping of each whole `bash -c` script is RETAINED; the
  interior byte layout is UNCHANGED so the EC6 golden equals current HEAD exactly (the digest
  is hex-by-construction and the gitSHA is 40-hex-by-construction, so no path value can carry a
  metacharacter).
- Consolidation VERDICT (closes the X5 "EnsureUploaded becomes a thin EnsureArtifact caller"
  forward-note): REJECTED, deliberately. The two paths use different addressing schemes ON
  PURPOSE and cannot unify without breaking provisioned boxes: `EnsureUploaded` keys the
  remote path by the 40-hex git commit SHA (`EmbeddedSHA()`, arch-independent — one path
  serves both arches) and maintains the **`portald`** stable symlink that the remote xdg-open
  wrapper resolves; `EnsureArtifact` is content-addressed (64-hex sha256 of content, per-arch
  by nature) with a `<name>` symlink. Forcing delegation would change every provisioned box's
  path (probe MISS → spurious re-upload) and rename the `portald` symlink. They already share
  `uploadVerified`; after this stage they also share the quoting helper. The split is
  documented at both functions.
- Compatibility proof (EC6): a golden test pins `EnsureUploaded`'s remote path + symlink
  construction byte-identical to today (gitSHA path, `portald` symlink), and the validation
  matrix covers `EnsureArtifact`.

### E5. PTY capability shape: optional interface, merged output, session object

Following the `PortForwarder` precedent (separate optional interface, asserted at composition
roots — `internal/transport/transport.go:16-25`), core `Transport` stays frozen at six methods:

```go
// package transport
type PtyRequest struct {
    Term string // e.g. "xterm-256color"; empty defaults to "xterm"
    Rows uint16 // initial size; 0,0 defaults to 24x80
    Cols uint16
}

type PtySession interface {
    io.Reader        // combined output (a PTY merges stdout+stderr by nature)
    io.Writer        // keystrokes / stdin bytes
    Resize(rows, cols uint16) error
    Wait() error     // terminal status; *ExitError on non-zero exit, same contract as Stream
    Close() error    // idempotent teardown; closing without Wait must not leak
}

type PtyStreamer interface {
    // StreamPty runs argv under a pseudo-terminal. Empty argv = interactive
    // login shell (matching ssh's no-command behavior). Non-empty argv follows
    // the T2 single-space shell-join contract exactly like Stream.
    StreamPty(ctx context.Context, req PtyRequest, argv ...string) (PtySession, error)
}
```

- All THREE transports implement it (unlike `PortForwarder`, which localexec skips): native
  and system ssh for real use; localexec so the conformance suite and the exec bridge can be
  tested hermetically.
- No `Signal(sig)` method in v1: sshctl cannot deliver one (no channel API to a `-tt`
  subprocess), and hangup-on-disconnect — the ratified requirement — is teardown semantics
  (E8/E9), not an API method. Recorded as the natural v2 extension point.
- ctx semantics match `Stream`: ctx cancel tears the session down (kills local child / closes
  ssh session). `Wait` after cancel returns promptly. `Resize` after the session has ended
  returns an error (any descriptive error; never panics); `Wait` after `Close` returns the
  session's terminal status or a teardown error — both are legal call orders.

### E6. Termios stance: fixed default mode table, not local mirroring

`sshnative` requests the PTY with a fixed mode map: `ssh.TerminalModes{ssh.ECHO: 1,
ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}` — the widely-used Go default. The remote
pty line discipline handles echo/canonical mode; the local CLI goes raw (E10) so bytes flow
uninterpreted. Full termios mirroring (shipping the client terminal's tcgetattr snapshot over
the subprotocol, as OpenSSH does) is explicitly out of scope v1 and recorded as the known gap:
edge-case interactive programs that re-read their termios assumptions may differ subtly from
OpenSSH behavior. sshctl inherits whatever the system ssh sends (it mirrors the LOCAL pty that
E9 allocates — closest to OpenSSH fidelity of the three); localexec inherits the ptyx defaults.

### E7. PTY primitives are in-tree: `pkg/transport/ptyx` + `internal/termx` (zero new deps)

- `pkg/transport/ptyx`: open a pty pair and start a command on the slave.
  - darwin: open `/dev/ptmx`, `TIOCPTYGRANT`/`TIOCPTYUNLK` ioctls, slave name via
    `TIOCPTYGNAME` (the creack/pty darwin approach, via existing direct dep
    `golang.org/x/sys/unix`).
  - linux: `/dev/ptmx` + `TIOCSPTLCK` unlock + `TIOCGPTN` for the slave index.
  - `Start(cmd *exec.Cmd) (master *os.File, err error)`: slave as stdin/stdout/stderr,
    `Setsid: true, Setctty: true`; `Setsize(master, rows, cols)` via `TIOCSWINSZ`.
  - Exhaustive unit tests run REAL ptys on the host OS (echo round-trip, winsize set/get via
    `TIOCGWINSZ`, child sees a tty via `isatty`-equivalent check, SIGHUP delivered to child on
    master close). These land and pass BEFORE any transport wiring (Stage 5's
    wsframe-before-bridge pattern).
- `internal/termx`: CLI-side terminal control — `IsTerminal(fd)`, `MakeRaw(fd) (restore func(),
  err)`, `GetSize(fd) (rows, cols)`, `WatchWinch(ctx) <-chan struct{}` — all via
  `x/sys/unix` (`IoctlGetTermios`/`IoctlSetTermios` with darwin `TIOCGETA/TIOCSETA`, linux
  `TCGETS/TCSETS`; `TIOCGWINSZ`; `signal.Notify(SIGWINCH)`).
- `creack/pty` and `golang.org/x/term` are REJECTED for v1 to keep `go.mod` frozen a third
  stage — both are trivial to swap in later since ptyx/termx mirror their shapes. Pre-authorized
  fallback (same rule as Stage 5 §6): if the darwin pty path hits a real correctness wall in
  implementation, adopt `creack/pty` — but the unit summary must say so and this section must be
  amended before merge, never silently.
- Path amendment (principal-ratified): `ptyx` lives at `pkg/transport/ptyx`, not
  `internal/ptyx`, because `pkg/transport/localexec` and `pkg/transport/sshctl` import it in
  NON-TEST code (`StreamPty`, u4/u5) and EC10 forbids any `pkg/*` non-test import of
  `internal/*` — leaving ptyx internal is UNBUILDABLE once those transports move to `pkg/`.
  E7's SUBSTANCE is fully preserved: in-tree, `golang.org/x/sys/unix`-only, zero new
  `go.mod`/`go.sum` entries (EC2 intact); ONLY the import path changes. `termx` stays
  `internal/termx` because only `cmd/portal` + internal packages import it. The alternative (a
  `PtyAllocator` interface injected into localexec/sshctl) is heavier and REJECTED — the
  primitive is genuinely platform-shaped.

### E8. `/v1/exec` PTY wire extension (capability-confirmed, fail-closed)

- Request: `POST /v1/exec?arg=...&pty=1&term=<name>&rows=<n>&cols=<n>`. `pty`, `term`, `rows`,
  `cols` are new query params; absent `pty` means the exact Stage 5 pipe behavior (zero change
  for old clients). Empty argv is allowed ONLY with `pty=1` (interactive shell); without pty it
  stays `invalid_request`.
- Capability confirmation: when the daemon accepts a PTY request it adds
  `X-Portal-Exec-Pty: 1` to the 101 response headers. A client that asked for a PTY and does
  NOT see the header MUST hard-fail with a clear "daemon does not support PTY (restart the
  daemon after upgrading)" error — this is the CLI-newer-than-running-daemon skew case, which
  live staging WILL hit. No silent pipe-mode degradation.
- If the selected transport does not assert `transport.PtyStreamer`, the daemon rejects
  `pty=1` BEFORE upgrading: HTTP 409, code `pty_unsupported` (mirrors the 403 feature-gate
  pattern; still fail-closed, still pre-hijack).
- In PTY mode the server bridges `PtySession`: output flows as `stdout` frames only (a pty
  merges streams; no `stderr` frames will be sent), `stdin` frames write keystrokes, a
  zero-length `stdin` frame is a documented NO-OP (principal-ratified amendment) — the E5
  `PtySession` interface exposes only `Close()` (there is NO `CloseWrite`) and a pty master
  cannot half-close its write side; interactive pty sessions end via process exit or client
  disconnect, NOT stdin EOF. Non-PTY zero-length-stdin EOF behavior is UNCHANGED (EC16). This
  NO-OP is mirrored in u10 (`docs/wire.cddl` stream-tag state machine) and u11 (`clients/ts`
  `exec.ts` never emits an EOF frame in PTY mode) so no client encodes an EOF-close-under-pty
  expectation; it matches the standard interactive-tty contract (`ssh -t` likewise does not
  half-close a remote pty on local stdin EOF — pty programs that read stdin to EOF terminate on
  process exit / disconnect). `exit`/`error` terminal frames and the WS close are unchanged.
- New client→server frame: `Stream: "winch"` with `Rows`/`Cols` set (`cbor:"rs"`/`cbor:"cs"`,
  omitempty — additive fields on `ExecFrame`, invisible to non-PTY sessions; VERIFIED against
  fxamacker/cbor v2.9.2: zero uint16 omitted, unknown keys ignored by the default decoder AND
  by `DupMapKeyEnforcedAPF`). Constraint: the `ExecFrame` decoder (here and after its u9 move
  to `pkg/api`) must NEVER enable `ExtraDecErrorUnknownField` — additivity depends on it.
  Server calls `PtySession.Resize`. Unknown-to-old-daemons is a non-issue: old daemons never
  grant the header, so a correct client never sends `winch` to one.
- Audit: `exec-open` gains `pty=1` when granted (E2's sid on both lines regardless).
- Hangup-on-disconnect (THE §8.3 fix): client disconnect cancels the bridge ctx (existing X8
  behavior) → `PtySession` teardown closes the remote pty → kernel delivers SIGHUP to the
  remote foreground process group → `portal exec -t -- sleep 300` + client kill leaves NO
  orphan (§8.2 live check, the control-test counterpart to Stage 5 §8.3).
- The exec feature gate (`FeatureExec`) covers PTY sessions identically — no new gate.

### E9. Per-transport PTY mechanics

- `sshnative`: `NewSession` → `RequestPty(term, rows, cols, modes)` (E6) → `StdinPipe` +
  `StdoutPipe` (no stderr pipe — merged by the pty) → `Start(joined)` or `Shell()` for empty
  argv. `Resize` = `sess.WindowChange(rows, cols)`. ctx teardown reuses `watchSessionCtx`.
  Additionally, live PTY sessions register with the client so `markDead` (keepalive strikes,
  `native.go:466-472`) promptly closes them instead of leaving them to discover death lazily —
  this is what makes hangup-on-disconnect timely on native when the NETWORK dies (not just
  when the local client disconnects). Registration is removed on Wait/Close (no leak; tested).
- `sshctl`: allocate a LOCAL pty pair via ptyx; run `ssh -S <sock> <opts> -tt <host> <argv...>`
  with all three stdio bound to the slave (`Setsid` + `Setctty`). The local ssh then owns a
  real controlling tty: it propagates window changes itself (SIGWINCH → SSH window-change) and
  applies OpenSSH's own termios negotiation. `Resize` = `ptyx.Setsize(master)` (kernel delivers
  SIGWINCH to ssh). Read/Write = the master. `Wait` maps the ssh child's exit code exactly as
  `Stream`'s wait does today (ssh exits with the remote status; 255 = ssh-transport failure
  maps to `ExitError{Code: 255}` as today). ctx cancel kills the child AND closes the master
  (remote gets SIGHUP via sshd's pty teardown). Unit tests use a PATH-stubbed fake `ssh`
  (a shell script asserting `-tt` placement and echoing through the tty); the env-gated
  `PORTAL_TEST_SSH_HOST` conformance run covers a real sshd.
- `localexec`: ptyx directly — `sh -c <joined>` (or `$SHELL`/`/bin/sh -i` for empty argv)
  under the pty. This is the hermetic backbone for conformance + bridge tests.

### E10. `portal exec` grows ssh-shaped TTY handling

- New flags: `-t` (force PTY), `-T` (never PTY). Default AUTO matches ssh: empty argv (shell)
  + stdin is a terminal → PTY; non-empty argv → pipe mode unless `-t`. `portal exec` with no
  `--` and no args becomes VALID (interactive shell) when a PTY is grantable; usage error
  otherwise (unchanged message when `-T`/non-tty).
- PTY mode: `termx.MakeRaw(stdin)` with unconditional restore on every exit path (defer +
  signal-safe), initial size from `termx.GetSize`, `WatchWinch` → `winch` frames, bytes
  shuttled verbatim (Ctrl-C is 0x03 to the remote — raw mode means the LOCAL process ignores
  it, exactly like ssh). Non-tty stdin/stdout with `-t` is an error (nothing to go raw on).
- Exit-code passthrough (`exitCodeErr` → `os.Exit(code)`) unchanged; terminal restored BEFORE
  the process exits (test via subprocess harness that asserts sane termios after exit).

### E11. Extraction map: what moves to `pkg/`, what stays internal

| New location | From | Notes |
|---|---|---|
| `pkg/protocol` | `internal/protocol` | zero-dep; THE wire package (CDDL companion) |
| `pkg/transport` | `internal/transport` | + `Impl` typed constants (below), PTY types (E5) |
| `pkg/transport/sshctl` | `internal/sshctl` | grouped under transport |
| `pkg/transport/sshnative` | `internal/sshnative` | " |
| `pkg/transport/localexec` | `internal/transport/localexec` | " |
| `pkg/transport/conformance` | `internal/transport/conformance` | + E13 loopback refactor |
| `pkg/transport/ptyx` | `internal/ptyx` | pty primitives; imported by localexec/sshctl non-test code (E7/E11 amendment) |
| `pkg/run` | `internal/run` | Runner seam appears in sshctl's public signature |
| `pkg/hub` | `internal/hub` | Notify/event types appear in client + agentclient surfaces |
| `pkg/doctor` | `internal/doctor` | report DTOs appear in client signatures |
| `pkg/api` | NEW (split from `internal/localapi/state.go` + error envelope) | types-only DTO package: `Status`, `VersionInfo`, `PortStatus`, `ForwardStatus`, `AgentStatus`, `MasterStatus`, `ServiceStatus`, `Health`, event-line envelope, error envelope. `internal/localapi` (server) and `pkg/client` both import it |
| `pkg/client` | `internal/localclient` | the embeddable control-API client (uses `internal/execws` — legal: pkg may import internal within this module, but see the signature rule below) |
| `pkg/agent` | `internal/agent` | + E12 registration export |
| `pkg/agent/watcher` | `internal/agent/watcher` | appears in agent construction |
| `pkg/agentclient` | `internal/agentclient` | + E12; bootstrap/clipshim dependencies become interfaces DECLARED IN `pkg/agentclient` — principal-ratified correction: the corrected `Bootstrapper` is `Bootstrapper { EnsureUploaded(ctx) (string, error); SetBootID(string); EmbeddedSHA() string }` (the earlier sketch's `Invalidate(...)` does not exist anywhere in the codebase (grep-clean) and was a sketch artifact — NO `Invalidate` is introduced); this matches `agentclient/client.go`'s real use (it needs `EmbeddedSHA()` at `client.go:472/489/494/499` to drop the `internal/bootstrap` import for EC10; `*bootstrap.Manager` already satisfies all three) — `internal/bootstrap`/`internal/clipshim` implement them |

Stays internal (portal-the-product, not platform): `internal/app` (composition root),
`internal/localapi` (server), `internal/bootstrap` (embeds THIS product's portald binaries),
`internal/clipshim`, `internal/config`, `internal/audit`, `internal/service`, `internal/proc`,
`internal/forward`, `internal/discover`, `internal/clip`, `internal/notify`, `internal/clock`,
`internal/logfile`, `internal/termx`, `cmd/*`. (`internal/execws` exists only
between u1 and u9 — see the signature rule below; it is NOT a final artifact.)

Rules:
- **Signature rule (enforced by a test, EC10):** no `pkg/*` package imports `internal/*` at
  all, and no exported identifier in any `pkg/*` package references an internal type —
  external consumers must be able to use everything they can see. Consequence for E1's
  consolidated package: its two halves end up public. Final shape: **`pkg/wsbits`** holds the
  parameterized RFC 6455 reader/writer/accept (tiny, dependency-free, documented as "not a
  general WebSocket library — exactly what the exec subprotocol needs"); **`pkg/api`** owns
  `ExecFrame` + stream tags + codec (they are wire DTOs; the CDDL documents them). Both
  `internal/localapi` (server) and `pkg/client` import both. `internal/execws` is a u1-only
  waypoint — u9 promotes its halves and deletes it. One copy of the framing, public, honest
  (EC3 holds at final HEAD).
- `Impl` becomes a typed string in `pkg/transport`: `type Impl string`; `const (ImplSystemSSH
  Impl = "system-ssh"; ImplNativeSSH Impl = "native-ssh"; ImplLocalExec Impl = "localexec";
  ImplUnavailable Impl = "unavailable")`; `Desc.Impl Impl`. All five literal sites
  (conformance, doctor ×3, app) switch to the constants; `MasterStatus.Transport` keeps its
  JSON string shape. The DISJOINT config-selection vocabulary (`"system"`/`"native"`,
  `internal/config`) deliberately stays separate and internal — it selects, `Impl` describes;
  a comment cross-references them.
- Moves are executed as move-only commits (`git mv`, import-path rewrite, zero semantic edits)
  followed by separate seam-edit commits within the same unit, so review reads clean.
- `cmd/*` and remaining `internal/*` update imports mechanically; behavior byte-identical.

### E12. Service registration exported on both sides

Agent side (`pkg/agent`):
```go
// ServiceHost is the exported facade over the registry, passed to service factories.
// NOTE: the registry stamps per-service Seq itself (S3 sole-writer invariant) — the
// facade exposes NO seq parameter anywhere; Emit mirrors registry.emit's admission
// bool (false = outbox full, frame dropped).
type ServiceHost interface {
    Emit(service, kind string, payload cbor.RawMessage) bool // wraps registry.emit verbatim
    Call(ctx context.Context, service, kind string, timeout time.Duration,
         maxInflight int, payload func(nonce, epoch uint64) cbor.RawMessage) (cbor.RawMessage, error) // wraps registry.call
    HasClient() bool
    ClientHas(service string) bool
}

type ServiceFactory func(host ServiceHost, log *slog.Logger) Service

type Config struct {
    // ... existing fields ...
    Services []ServiceFactory // registered after the built-ins, in order
}
```
`registry` stays unexported; a small adapter implements `ServiceHost`. Built-in services
(clip/notify/openurl) are REWIRED to construct via the same facade (proof the facade is
sufficient — no privileged path left behind; their behavior and tests unchanged). Duplicate
service/verb names keep panicking at registration (programmer error). Exact `Call` signature
may be adapted to the existing `registry.call` contract (nonce/epoch threading) — the unit
must keep clip's semantics byte-identical; if the payload-closure shape above proves wrong
against the real nonce/epoch flow, the implementer matches reality and the reviewer verifies
the facade exposes no less and no more than clip needs.

Client side (`pkg/agentclient`):
```go
type Config struct {
    // ... existing fields ...
    Handlers []HandlerSpec // registered after the built-ins; HandlerSpec is already exported
}
```
plus an exported `(*Client).Send(service, kind string, payload cbor.RawMessage) error`
wrapping the unexported `registry.send` (which threads the encoder and stamps Seq itself;
custom services need the client→agent direction — today only clip's response path uses it
internally).

Black-box proof (EC11): a test in a `_test` package (external test package, public API only)
registers a toy `echo` service on both sides via `Config.Services`/`Config.Handlers`, runs
agent+client over an in-process pipe transport, and round-trips a frame each direction —
including version-mismatch dormancy (client advertises v2, agent v1 → dormant, no error).
The harness pattern already exists: `e2eTransport`/`newE2ESession`
(`internal/agentclient/client_test.go:256-339`) wires a fake `Transport.Stream` over
`io.Pipe` into a real `agent.Server.Serve` with no portald; the EC11 test reuses that shape
through the PUBLIC surface (note: requires `make agent` first — the embedded-SHA handshake
gate skips otherwise, same as today's e2e tests).

### E13. Conformance suite: loopback becomes factory-declared

`conformance.Run(t, name, newT)` keeps its signature for the core suite. The port-forward
sub-suite changes: the `Impl != "system-ssh"` string probe (`conformance.go:243`) and the
inlined echo listener (`conformance.go:265-311`) are replaced by caller declaration:

```go
type ForwardTarget struct {
    // NewEchoServer returns an address (host:port) reachable from the transport's
    // REMOTE side, plus a cleanup func. Nil ForwardTarget = skip port-forward suite.
    NewEchoServer func(t *testing.T) (addr string, cleanup func())
}
func RunWithForward(t *testing.T, name string, newT func(*testing.T) transport.Transport, fw *ForwardTarget)
```
`Run` delegates to `RunWithForward(..., nil)`. COVERAGE (principal-ratified correction): ONLY
sshnative (in-process loopback, previously the `Describe().Impl != "system-ssh"` branch at
`conformance.go:243`) and sshctl's env-gated real-host run get caller-declared `ForwardTarget`s.
localexec does NOT implement `transport.PortForwarder` (`localexec.go:9` comment; only
`var _ transport.Transport`), so `conformance.runPortForward` SKIPS at
`pf, ok := tr.(transport.PortForwarder); if !ok { t.Skip }` (`conformance.go:235-237`) today and
STAYS skipped — a `ForwardTarget` on localexec would be dead code that never runs. localexec's
KEPT conformance coverage is the CORE suite + the NEW `PtyStreamer` PTY section (u4), NOT
port-forward. sshctl's real-host test may pass a remote-reachable listener factory or nil (nil
preserves today's effective skip). The suite
contains ZERO `Describe().Impl` comparisons afterward (EC12). A new PTY section runs for transports
asserting `PtyStreamer` (all three): `stty size` reports the requested size, resize observed
(`stty size` re-read after `Resize`), exit-code fidelity under pty, merged-output sanity,
EOF/teardown leaves no goroutines.

### E14. Wire spec: `docs/wire.cddl` + golden vectors, verified from Go AND TypeScript

- `docs/wire.cddl` — CDDL (RFC 8610) for everything CBOR on the wire:
  - The `PF` frame: 2-byte magic + uint32be length + CBOR `Envelope` (all 14 envelope arms,
    every message type with every field and its cbor tag, ProtoVersion semantics, the
    one-field-only rule, MaxFrameBytes).
  - The exec WS subprotocol: upgrade handshake (over UDS), `ExecFrame` incl. the E8 pty
    extension, stream-tag state machine (which tags in which direction, terminal frames,
    zero-length-stdin EOF, winch), masking rules, 16 MiB cap.
  - Prose pointers: the local HTTP+JSON API is specified by `openapi.yaml` (E15 schemas), auth
    is peer-uid (no tokens).
- `docs/vectors/`: golden test vectors — for each Envelope arm and each ExecFrame shape, a
  `.hex` file (CBOR bytes) + `.json` sidecar (semantic content). Generated by a Go test with
  `-update` (same pattern as any golden test; committed), VERIFIED two ways in the gate:
  1. Go: decode each vector, compare against the expected struct, re-encode, compare bytes
     (catches silent codec drift; `SortNone` caveat documented — vectors pin THIS encoder's
     byte output, semantic equality is the contract for foreign encoders).
  2. TypeScript (u11): `clients/ts` decodes every vector and asserts the same semantic content
     — the cross-language teeth that make the CDDL more than prose.
- The CDDL file itself is prose-validated in review (no cddl-tool dependency in the gate;
  a comment records the exact `cddl` CLI invocation used to validate it once by hand).

### E15. `openapi.yaml` gains real schemas

Every route gets `parameters`/`requestBody`/`responses` with `components/schemas` matching
`pkg/api` structs field-for-field (json tags = property names; required vs omitempty honored).
`/v1/exec` documents the upgrade: query params (incl. E8 pty params), 101 + headers (incl.
`X-Portal-Exec-Pty`), and a description pointing at `docs/wire.cddl` for frames. Formatting
contract (the strict line-scanner in `spec_test.go` depends on it): path entries stay 2-space,
method entries 4-space, method bodies (`summary`/`parameters`/`requestBody`/`responses`) at
6-space, `components:` at column 0, and no 4-space key under `components:` may be named
`get/put/post/delete` (schema names are PascalCase, so safe by convention — but it's now a
stated rule). The scanner is extended into a small state machine: a `paths:`-section guard
(so `components:` sub-keys are never misread as operations) plus a per-operation flag
asserting every method entry carries a `responses:` key (schema-presence smoke, not full
validation — the TS client + vectors are the deep check).

### E16. Deliberate public-shape calls (watch items closed)

1. `WithProxyCommandDialer`: the wrapped func type becomes exported —
   `type ProxyCommandDialer func(ctx context.Context, command string) (net.Conn, error)`;
   `WithProxyCommandDialer(d ProxyCommandDialer) Option`. (It's in `pkg/transport/sshnative`
   after E11; an Option over an unexported type would be unusable externally.)
2. Target-resolved-once-at-New vs hops-re-resolved-per-Ensure: KEPT, documented in
   `pkg/transport/sshnative` package docs (target identity is pinned at construction; hop
   config is re-read per redial like OpenSSH re-reads on reconnect).
3. `ssh -G` exec'd at App construction on every CLI invocation when native is selected: KEPT
   (correctness over startup latency; the degraded errTransport path bounds the damage),
   documented in the same package docs. A resolution cache is a recorded v2 candidate.
4. In-tree WS framing: closed by E1 (stays, consolidated, now `pkg/wsbits`).

## 3. Units

Sequential; each unit = codex implements → full gate (`make build && go vet ./... && gofmt -l
cmd internal && make test && go test -race ./...`) → gate agent commits. The gofmt target list
grows with the tree: `pkg` joins at u7, and u11 adds the TS checks (`tsc --noEmit` +
`node --test` under `clients/ts`) to the gate for that unit and every later one. Move-only
commits precede seam-edit commits inside u7–u9 (E11).

- **u1 — execws consolidation + audit sid + ctx fix (E1, E2, E3).** Create `internal/execws`
  (framing + ExecFrame together at this stage; the pkg split happens in u9), rewire both
  sides, delete both duplicated copies; add sid to audit lines + tests; fix the stdin join.
  All existing exec/ws tests move and pass; new: pairing test, io.Pipe ctx test, one-opcode-
  table grep test.
- **u2 — bootstrap hardening (E4).** Name validation + typed error; shared quoting helper
  applied to every path splice in BOTH upload paths; the two-scheme split documented at both
  functions; golden test pinning `EnsureUploaded`'s path + `portald` symlink construction
  unchanged; validation-matrix tests (spaces, `../`, `$(...)`, quotes, 65 chars, leading
  `-`/`.`, empty).
- **u3 — ptyx + termx primitives (E7).** Both packages, darwin+linux paths (linux
  compile-tagged; darwin fully tested locally), REAL-pty unit tests incl. SIGHUP-on-master-
  close and winsize round-trip. No transport wiring yet.
- **u4 — transport PTY: types + native + localexec + conformance (E5, E6, E9-native/localexec,
  E13-pty-section).** `PtyRequest/PtySession/PtyStreamer` in `internal/transport`; sshnative
  impl (RequestPty, WindowChange, Shell for empty argv, dead-client prompt close with
  registration/deregistration tests against the in-process ssh server); localexec impl via
  ptyx; conformance PTY section green for both. The in-process ssh test server today handles
  ONLY `exec` requests and replies false to everything else — u4 extends it: accept `pty-req`
  (parse the RFC 4254 payload), `window-change`, and `shell`, run the command under a REAL
  server-side pty (allocate via `internal/ptyx` — a test-only internal import, permitted
  because EC10 scopes to non-test imports), bridge channel↔master, send `exit-status`.
  `stty size` reads the TTY driver, so emulation without a real pty is insufficient.
- **u5 — sshctl PTY (E9-sshctl).** Local-pty + `-tt` mechanics with PATH-stubbed fake ssh
  unit tests (the `stream_test.go` fake-ssh pattern; sshctl sets no `cmd.Env`, so `t.Setenv`
  PATH injection is honored): argv placement, tty-ness of the child, resize→SIGWINCH observed
  (initial size then resize to a DIFFERENT size — the kernel only signals on change),
  exit-code map (255 passthrough preserved), ctx-cancel kill+SIGHUP; + env-gated real-host
  conformance wiring.
- **u6 — exec bridge + client + CLI PTY (E8, E10).** Server: pty params, 409 pty_unsupported,
  101 header, PtySession bridging, winch handling, pty=1 audit; client (`localclient`): pty
  request path, header check with the hard skew error, winch sending; CLI: `-t`/`-T`/auto,
  raw mode + restore, WINCH pump, empty-argv shell (cobra: keep the default `ArbitraryArgs` —
  do NOT add a MinimumNArgs validator; `portal exec -t` with no `--` must reach RunE with
  empty argv). Hermetic end-to-end test over localexec:
  run `stty size` through the FULL stack (CLI client lib → UDS → bridge → pty) and assert the
  size; resize mid-session; disconnect-kills-remote test (start `sleep`, drop the client conn,
  assert the process group dies — THE §8.3 regression, now in-gate).
- **u7 — extraction wave 1 (E11, E13, E16).** `pkg/protocol`, `pkg/transport` (+ sshctl,
  sshnative, localexec, conformance under it), `pkg/run`, `pkg/hub`, `pkg/doctor`; Impl typed
  constants + all literal sites; conformance loopback refactor; ProxyCommandDialer export;
  package-doc notes from E16.2/.3. Move-only commit(s) then seam commits. The
  pkg-imports-internal checker test (EC10) lands here and gates every later unit.
- **u8 — extraction wave 2: agent + agentclient + registration export (E11, E12).**
  `pkg/agent` (+watcher), `pkg/agentclient`; ServiceHost facade + Config.Services +
  Config.Handlers + (*Client).Send; built-ins rewired through the facade; bootstrap/clipshim
  interface-ization declared in pkg/agentclient and implemented by the internal packages;
  black-box echo-service test incl. dormancy.
- **u9 — pkg/api + pkg/client + pkg/wsbits + openapi schemas (E11, E15, E1-final-shape).**
  Split DTOs out of localapi into `pkg/api` (+ ExecFrame/tags/codec); `internal/execws` →
  `pkg/wsbits` (framing) with localapi + the promoted `pkg/client` consuming it; openapi
  schema enrichment + spec-test extension.
- **u10 — wire spec (E14).** `docs/wire.cddl`, `docs/vectors/`, the Go golden generator/
  verifier test.
- **u11 — clients/ts (E15-client, E14-TS-verification).** Zero-RUNTIME-dep TypeScript library:
  UDS HTTP (node:http socketPath), typed DTOs mirroring pkg/api, ndjson events iterator,
  exec WS client (hand-rolled framing + minimal CBOR codec for ExecFrame incl. pty/winch),
  version check helper, and a small `examples/smoke.ts` script (status + one events line +
  exec echo against a live daemon — the §8.7 manual artifact). Tests: vector decode/encode
  suite + a mocked-socket protocol test. Toolchain contract (probed on Node 25.6.1 +
  tsc 5.9.3): `package.json` declares `"type": "module"`, EMPTY/absent `dependencies`, and
  devDependencies ONLY `@types/node` + a pinned `typescript` (tsc cannot type `node:*`
  imports without `@types/node` — this is the one `npm ci` the gate runs; runtime stays
  zero-dep, which is what EC14 asserts). tsconfig pins: `module`/`moduleResolution`
  `"nodenext"`, `verbatimModuleSyntax: true`, `erasableSyntaxOnly: true`, `types: ["node"]`,
  `strict: true`, `noEmit: true`, `skipLibCheck: true`. BANNED (non-erasable, break node's
  type-stripping): enums (incl. const enums), `namespace`/`module` bodies, constructor
  parameter properties — `erasableSyntaxOnly` enforces the ban statically. Gate additions
  (a `test-ts` make target used from u11 on): assert `node --version` ≥ 24 with a clear
  failure, `npm ci` in `clients/ts`, `npx tsc --noEmit -p clients/ts`, `node --test` the
  `.ts` tests directly.
- **u12 — examples/shell-electron (E15-shell).** Thin Electron app consuming `clients/ts`:
  status panel, live events/notifications feed, exec terminal (xterm.js) with PTY + resize.
  Has its own `package.json` (electron + xterm devDeps); NOT built in the gate — gate runs
  `node --check` on its plain-JS main/preload files and `tsc --noEmit -p examples/shell-electron`
  if a tsconfig is present. README documents `npm install && npm start`. Full validation is
  §8.7 (manual).

## 4. Exit criteria

- **EC1** Full gate green at every unit boundary; final `-race` clean.
- **EC2** `go.mod`/`go.sum` byte-identical to Stage-5 merge (`git diff 1d21695 -- go.mod go.sum` empty).
- **EC3** Exactly ONE RFC 6455 opcode table / frame reader / frame writer in the repo (grep-
  proof test in u1, still true at final HEAD in `pkg/wsbits`).
- **EC4** `exec-open`/`exec-close` lines carry matching `sid=`; concurrent-session pairing test.
- **EC5** `localclient/pkg-client Exec` with never-yielding `io.Pipe` stdin returns promptly on
  ctx cancel after exit frame (regression test; hangs pre-Stage-6).
- **EC6** `EnsureArtifact` rejects the invalid-name matrix; golden test pins
  `EnsureUploaded`'s remote path + `portald` symlink construction byte-identical to today
  (identical probe-hit behavior on provisioned boxes).
- **EC7** All three transports assert `transport.PtyStreamer`; conformance PTY section passes
  hermetically for native (in-process sshd) + localexec, and for sshctl via PATH-stub tests
  (+ env-gated real host).
- **EC8** Full-stack PTY test: `stty size` via CLI-client→UDS→bridge→transport→pty returns the
  requested size; mid-session `Resize` observed; PTY disconnect kills the remote process group
  (in-gate §8.3 regression).
- **EC9** PTY capability skew: pty request without granted header → hard client error; daemon
  without PtyStreamer transport → 409 `pty_unsupported` pre-upgrade (both tested).
- **EC10** Checker test: no `pkg/*` package imports `internal/*` in its NON-TEST imports
  (`go list -json ./pkg/...`, fail on any `.Imports` entry with the
  `github.com/VikashLoomba/Portal/internal/` prefix — std-lib only, ~30 lines, runs in the
  normal gate). Test files (`.TestImports`/`.XTestImports`) MAY import internal packages
  (same-module tests; external consumers never run them) — this is what permits u4's
  test-server ptyx use. The import check subsumes the exported-identifier check: Go cannot
  name an internal type without importing its package.
- **EC11** Black-box custom-service test registers `echo` via public API on both sides and
  round-trips both directions + dormancy case — using ONLY `pkg/agent` + `pkg/agentclient`
  exported surface.
- **EC12** `pkg/transport/conformance` contains zero `Describe().Impl` comparisons; port-
  forward targets are caller-declared ONLY for the PortForwarder-implementing transports
  (sshnative in-process, sshctl real-host), and localexec's pre-existing (skipped) port-forward
  behavior is unchanged; typed `Impl` constants exist and no bare impl-string literal remains
  outside `pkg/transport` (grep test).
- **EC13** `docs/wire.cddl` covers every `Envelope` arm + `ExecFrame` (incl. pty extension);
  golden vectors round-trip in Go.
- **EC14** `clients/ts` decodes every golden vector to the expected semantics; `tsc --noEmit`
  and `node --test` run in the gate and pass; `package.json` declares zero RUNTIME
  dependencies (`dependencies` absent/empty; devDependencies limited to `@types/node` +
  `typescript`).
- **EC15** `openapi.yaml` has schemas for every route; route-conformance + responses-presence
  tests pass.
- **EC16** Byte-identical regressions: `portal status` (system transport) output unchanged;
  non-PTY `portal exec` frames/behavior unchanged (existing Stage-5 tests pass unmodified in
  intent); remote agent path construction unchanged (EC6).
- **EC17** ProtoVersion still 4; no `PF` frame shape changed (protocol package tests +
  vectors pin this).

## 5. Risks

| Risk | Mitigation |
|---|---|
| darwin pty allocation subtleties (ptmx grant/unlock ioctls) | u3 lands primitives with real-pty tests BEFORE any wiring; creack/pty fallback pre-authorized with mandatory doc amendment (E7). |
| `ssh -tt` semantics drift vs local-pty assumptions (stderr merge, exit 255) | PATH-stubbed fake-ssh tests pin argv + tty-ness; real-host conformance env-gated; 255 mapping documented + tested (E9). |
| Raw-mode CLI leaves the user's terminal broken on a panic/kill path | restore via defer + signal handler; subprocess test asserts post-exit termios sanity (E10). |
| Extraction churn breaks hidden couplings (white-box tests, embed paths) | move-only commits isolate mechanical churn; full gate at every unit; EC16 regressions pin user-visible behavior. |
| `pkg/` surface accidentally leaks internal types | EC10 checker test in the gate from u7 onward. |
| ServiceHost facade proves insufficient for clip's nonce/epoch flow | E12 explicitly authorizes matching the real contract; built-ins rewired through the facade is the sufficiency proof; reviewer verifies no privileged bypass remains. |
| Vectors pin non-canonical CBOR (`SortNone`) byte output too tightly | vectors assert Go byte-stability + cross-language SEMANTIC equality (TS decodes, compares content, re-encodes its own bytes which Go must accept — not byte-compare) (E14). |
| Node version drift breaks the TS gate | gate asserts node ≥ 24 with a clear message; zero runtime deps keeps the surface tiny (u11). |
| Electron example rots ungated | explicitly documented as manually-validated (§8.7); `node --check`/tsc smoke in gate; it consumes only `clients/ts`'s public API. |
| PTY dead-client close races Wait/Close (double-close, leaked registration) | u4 registration/deregistration tests + `-race` gate; Close idempotency in the E5 contract. |

## 6. Principal review (2026-07-09): APPROVE

Reviewed by the main-loop Fable session (per the user's Stage-6 direction: no in-workflow
principal). Basis: full read of the judgment-bearing surfaces (transport PTY types +
`PtySession` contracts, sshnative/sshctl/ptyx internals, the exec bridge PTY path,
`cmd/portal/exec.go` raw-mode/signal handling, `pkg/client` post-exit paths + skew check,
`ServiceHost` facade, `EnsureArtifact` validation, `wire.cddl`, TS codec bounds), adjudication
of the two late test-only fix commits the review loop never re-reviewed (`2fbbbf6`,
`aaebc69` — both sound), and an independent Opus exit audit that verified EC1–EC17 with the
full gate green at HEAD (`make test-ts`: 39/39; all 21 golden vectors decoded from TS).

The 9-round review loop was stopped by principal judgment after confirm rates collapsed
(rounds 4/8/9 mostly refuted; final two fixes test-only) — convergence-by-attrition on a
214-file diff was costing more than it caught.

Fast-follows (non-blocking, record for a later stage):
1. **CLI winch pump can drop the final resize:** `cmd/portal/exec.go` sends sizes through a
   1-buffered drop-newest channel; a resize burst ending in a drop leaves the remote pty one
   size stale until the next WINCH. Fix: drain-then-send (keep latest, not oldest).
2. **Zero-length-stdin single-frame count untested in Go:** the exactly-one-EOF-frame
   behavior is asserted by the TS suite only; a Go-side frame-count test would pin it against
   client refactors.
3. **CDDL never machine-validated:** per E14 the `cddl` CLI invocation recorded in
   `docs/wire.cddl` should be run once by a human before publishing the spec externally.
4. **E6 termios gap** (already documented): fixed mode table, no local-termios mirroring —
   revisit if an interactive tool misbehaves in a way OpenSSH handles.

## 7. Workflow notes

Same layout as Stage 5 (codex gpt-5.5 xhigh implements via read-only drivers; Opus gate agent
owns commits; 6-lens adversarial review + refute panels + fixer to one clean round; exit
audit) with ONE change per the user (2026-07-06): NO in-workflow principal phase — the
main-loop Fable session performs the principal review directly after the workflow exits clean.
Diff base: `git merge-base main HEAD` on `feat/stage6-extraction`.

## 8. Manual verification (live box, post-merge)

1. **Interactive PTY**: `portal exec -t -- vim` (and `htop`): renders correctly, window drag
   resizes it live, `q`/`:q` exits clean, terminal restored (echo/cursor sane afterward).
   Ctrl-C inside `htop` reaches htop, not the CLI.
2. **THE orphan regression**: `portal exec -t -- sleep 300`, kill the client (Ctrl-C /
   SIGKILL the portal process) → `ps` on the box shows the sleep GONE (SIGHUP via pty) —
   the documented Stage-5 §8.3 limitation resolved.
3. **Shell mode**: `portal exec -t` (no command) lands in an interactive login shell; `exit`
   returns cleanly with its status.
4. **Non-PTY regression**: `portal exec -- uname -sm` / `false` / `sh -c 'exit 7'` — exit
   codes 0/1/7, stdin-EOF behavior, stderr separation all unchanged; `audit.log` shows
   sid-paired open/close lines (pty=1 present only on PTY sessions).
5. **Skew check** (if the running daemon predates the upgrade): new CLI `-t` against the old
   daemon fails with the clear restart message, no silent pipe fallback.
6. **Transports**: 1–4 on system (default); native to its validated ceiling (hermetic PTY
   conformance stands in — the live box speaks Tailscale-SSH which native strict-knownhosts
   correctly rejects, per Stage 4).
7. **Reference clients**: `clients/ts` smoke script against the real daemon (status, an events
   tick, exec echo round-trip); `examples/shell-electron` via `npm install && npm start` —
   status panel live, a notification appears in the feed, exec terminal runs `vim` with
   resize.
8. **Bootstrap**: first connect after upgrade re-uploads the agent exactly once (new SHA),
   `portal status`/`doctor` PASS, subsequent connects probe-hit.
