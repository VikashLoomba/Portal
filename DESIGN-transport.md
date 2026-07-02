# Portal Generic Transport + Native SSH — Stage 4 Contract

**Status:** Approved direction (Stage 4 of the platformization roadmap; Stages 1–3 merged to main).
**Audience:** repo maintainer + implementation agents.
**Related:** `DESIGN-local-core-api.md` §7 (direction this contract locks), `DESIGN-split-daemon.md`
(ControlMaster semantics the system transport preserves), `DESIGN-service-registration.md`
(the agentclient Stream consumer).

---

## 1. Problem & decision

`sshctl.Transport` is the declared swap point, but it leaks SSH through three seams: master
lifecycle (`MasterPID`/`EnsureMaster`/`Exit`), port-forward verbs bound to `ssh -O`, and the
engine's "current truth" coming from lsof against the master pid (`proc.PortLister.MasterForwards`).
Stage 4 shrinks the core to transport-agnostic primitives, moves forward-truth behind an optional
capability, and proves the seam with a second real implementation: **native `x/crypto/ssh`** (no
dependence on the user's ssh binary/config — what a shippable desktop app needs), plus a
**localexec** implementation used by a shared conformance suite and future dev mode.

**Evidence the primitives are right:** every existing consumer already composes from them —
bootstrap (`Exec` with binary stdin + its own atomic upload script), clipupload (`Exec`),
agentclient (`Stream`), doctor probes (`Exec`), forwarding (`Forward`/`Cancel` + list).

**Explicitly out of scope (v1):** ssh_config alias resolution for the native transport (it accepts
`user@host[:port]`; the system transport remains the default so nothing regresses); encrypted-key
passphrase prompting (clear error instead); an `Uploader` capability (bootstrap and clipupload keep
their own hardened Exec-composed uploads — do NOT refactor them beyond the interface rename);
making native the default; Windows.

---

## 2. Locked decisions

| # | Decision | Detail |
|---|---|---|
| T1 | **New package `internal/transport`** owns the interfaces | `Transport` (core), `PortForwarder` (optional capability), `Health`/`Desc` types, `ForwardError` (moved from sshctl). Implementations: `internal/sshctl` (system ssh — default, behavior-identical), `internal/sshnative` (x/crypto), `internal/transport/localexec` (local subprocess; conformance + dev mode). Conformance suite in `internal/transport/conformance`. |
| T2 | **Core = 6 methods** | `Ensure(ctx) (rebuilt bool, err error)` (absorbs EnsureMaster; idempotent), `Health(ctx) (Health, error)` (absorbs MasterPID; `Health{Up bool, Pid int, Detail string}` — Pid is impl-specific ground truth where one exists: system-ssh fills the master pid, native fills 0; Detail is the human string, system-ssh `pid=N`), `Exec(ctx, stdin []byte, argv ...string) (stdout, stderr string, err error)` (merges Exec/ExecBytes; note the RETURN-ARITY change too: today's string-Exec callers read 2 values and must become `out, _, err :=` — the compiler enforces the sweep), `Stream(ctx, argv...)` (= today's ExecStream signature), `Close(ctx) (stopped bool, err error)` (absorbs Exit), `Describe() Desc` (`Desc{Impl, Host, Endpoint string}` — Impl=`"system-ssh"`/`"native-ssh"`/`"localexec"`; replaces `Host()`/`Sock()`). **Gating rule: NOTHING outside sshctl may gate behavior on `Pid > 0` — liveness gates use `Health.Up`** (a native connection has no pid; see the run.go/inspect.go/clipcheck.go rows in §3.2). |
| T3 | **PortForwarder capability** | `Forward(ctx, local, remote int) error`, `Cancel(...)`, `ListForwards(ctx) ([]int, error)`, `ForwardLines(ctx) ([]string, error)`. system-ssh implements List/Lines via lsof against its own master pid (absorbing today's `proc.PortLister.MasterForwards/MasterForwardLines` call sites); native implements them from its in-process listener registry (more truthful than lsof). Acquired by type assertion at the composition root; the daemon requires it (portal without forwarding is not a thing yet — assert loudly at wiring, not deep in the engine). |
| T4 | **Engine decoupling with truth preserved** | `forward.Engine` swaps `T sshctl.Transport, PL proc.PortLister` for `T transport.Transport, PF transport.PortForwarder` and derives current-truth from `PF.ListForwards` — the stateless-reconcile invariant (never trust in-process memory) is unchanged, just re-homed. The engine KEEPS a narrow local-port interface (`LocalHolder`/`ProcessName`, satisfied by `*proc.Lsof`) for its conflict messages — those query LOCAL ports, not the master, and are transport-agnostic. `proc` stays a package; only its master-forward call sites move. |
| T5 | **Native ssh (`internal/sshnative`)** | One `*ssh.Client`; auth order: `SSH_AUTH_SOCK` agent, then unencrypted `~/.ssh/id_ed25519`/`id_rsa` (encrypted key → clear error naming the workaround); host key via `knownhosts` STRICT (unknown/mismatched → error telling the user to `ssh <host>` once manually); keepalive `keepalive@openssh.com` every 15s, 3 strikes → mark dead; `Ensure` re-dials a dead client. Forward = local `net.Listen` on 127.0.0.1:N + per-conn `direct-tcpip` dial; Cancel closes the listener; Stream = `ssh.Session` with pipes; Exec = session run with captured output. Accepts `user@host[:port]`. **Constructor: `New(target string, opts ...Option)` with explicit injection seams — `WithKnownHostsPath(string)`, `WithIdentityFiles(paths ...string)`, `WithAgentSocket(string)` (empty string disables agent auth), `WithHostKeyCallback(cb)` (test escape hatch). Defaults resolve `~/.ssh/known_hosts`, `id_ed25519`/`id_rsa`, `$SSH_AUTH_SOCK`. The T6 in-process-server tests and the conformance factory use these Options EXCLUSIVELY (temp-dir fixtures) — hermetic in CI, never touching the runner's real `~/.ssh`.** Only new dependency: `golang.org/x/crypto`. |
| T6 | **In-process ssh server for CI** | `internal/sshnative` tests run against an in-process `x/crypto/ssh` SERVER (test-only: publickey auth with a generated key, exec handler running argv locally, direct-tcpip handler dialing locally). This gives the native transport full conformance + knownhosts-failure coverage in CI with no live box. |
| T7 | **Conformance suite** | `conformance.Run(t, name, factory)` covering: Exec stdout/stderr/exit-code/binary-stdin round-trip; Stream bidirectional + stdin-close EOF + wait; Ensure idempotency (second call rebuilt=false); Health up/down; Close; PortForwarder loopback round-trip + ListForwards truth + Cancel. Runs in CI for `localexec` and `sshnative` (vs T6 server); for `sshctl` it runs only when `PORTAL_TEST_SSH_HOST` is set (else `t.Skip` naming the variable). |
| T8 | **Selection** | New config file `<ConfigDir>/transport` (`system` default when absent, or `native`), read via `config.Store` at composition; invalid value → loud error at startup, not silent fallback. Consumer rule honored: new CLI `portal transport [system\|native]` (get/set; the no-arg form prints the active Impl and is the UNCONDITIONAL way to see it). `status` and `doctor` surface `Describe().Impl` **only when the active transport is not `system`** (one additional line each) — the default path stays byte-identical per T9; this conditional rule is the reconciliation of T8 with T9, not an oversight. Every transport-construction site (app wiring, doctor's daemon-down fallback probe, install) goes through ONE selection-aware factory `app.NewTransport(paths, host, runner, cfg, sshStderr io.Writer)` so a `native` selection is honored everywhere — no direct `sshctl.New` outside the factory (install runs before config exists → factory defaults to system). The final `sshStderr` param is the explicit, caller-supplied ssh-stderr sink for the SYSTEM transport ONLY: `NewProd` passes `os.Stderr` so ssh warnings reach launchd's log (the DESIGN-split-daemon invariant), while every DOCTOR-path caller passes `nil` so raw ssh stderr is never tee'd into the doctor report (the native transport has no ambient ssh-stderr stream — each session captures its own — so the param does not apply to it). `localexec` is NOT selectable via config (test/dev only). |
| T9 | **Byte-compat on the default path** | With `system` selected, `portal status`, `doctor`, and log lines stay byte-identical (Health carries the pid; the "master established (pid=N)" log renders from it). Enforcement: the Stage-2 golden tests (which must pass unmodified in intent) **plus a NEW engine test pinning the "master established (pid=N)" log line** (no existing test asserts it — grep confirms; add one in u2). localapi `Status.Master` keeps `{up, pid}` and gains additive `transport` (Impl) + `detail` fields; `pid` is 0 for native (documented). |
| T10 | **Failure-mode honesty** | Native forwards die with the daemon (no ControlPersist analogue) — documented in the doc + surfaced by `doctor` when native is active. agentclient consumes only `Stream`, whose exact semantics (bidirectional piping, stdin-close EOF, wait-after-close) the conformance suite pins per-implementation — that is the machine-verifiable coverage; full agentclient-over-native (heartbeats, reconnect supervisor) is deliberately deferred to the live-box validation (§7 item 2), NOT claimed as harness coverage. |

### 2.1 Shell-join argv contract (Exec / Stream)

`Exec` and `Stream` share ONE argv contract, and it is a *shell-join* contract, not
exec-vector semantics: the `argv ...string` is joined with single ASCII spaces into ONE
command string, and a shell **on the TARGET** re-splits that string. So a caller who needs
multiple tokens, redirection, globbing, or any other shell metacharacter preserved MUST
pre-quote them into a single argv element — `Exec(ctx, nil, "sh", "-c", shellQuote("echo x >&2"))`,
never `Exec(ctx, nil, "sh", "-c", "echo x >&2")`. bootstrap, clipupload, and doctor already do
this via their `shellQuote` helpers; this contract is what makes their pre-quoted scripts portable
across all three implementations.

The three implementations realize the SAME contract by different mechanics, **without changing
intent**:

- **sshctl (system-ssh):** appends `argv` VERBATIM as trailing args to the `ssh` invocation and
  lets the ssh BINARY perform the space-join + remote re-shell. It MUST NOT wrap in `sh -c`. This
  argv path is byte-for-byte UNCHANGED from the pre-Stage-4 daemon (→ T9 byte-compat), and is
  locked by u2's sshctl argv-passthrough test (`TestExec_ArgvByteCompat`, which also guards against
  any `sh -c` drift).
- **sshnative (native-ssh):** space-joins `argv` itself into the one string it hands to
  `ssh.Session.Run`/`.Start` (an `ssh.Session` takes a single command string, so the join is
  explicit here rather than delegated to a binary).
- **localexec:** space-joins `argv` and runs `sh -c <joined>` on THIS machine — the local shell is
  the "target shell" that re-splits, keeping the re-split semantics identical to the ssh path.

Because all three re-split on a target shell, a payload that is safe on one is safe on all; the
byte-compat obligation lands only on sshctl (T9), and the other two must match the *observable
re-split behavior*, which the conformance suite's Exec/Stream cases pin.

---

## 3. File contract

### 3.1 New files

| Path | Purpose |
|---|---|
| `internal/transport/transport.go` | Interfaces (`Transport`, `PortForwarder`), `Health`, `Desc`, `ForwardError` (moved), doc comments stating the composition rules (bootstrap/clipupload compose uploads over `Exec`; capability acquisition by type assertion at the root). |
| `internal/transport/localexec/localexec.go` (+`_test.go`) | Local subprocess implementation: `Exec`/`Stream` honor the §2.1 shell-join contract by space-joining `argv` and running `sh -c <joined>` on THIS machine via `exec.CommandContext` (the local shell is the "target shell" that re-splits — this is the localexec realization of the shared argv contract, NOT a raw exec-vector spawn), `Ensure`/`Health`/`Close` trivial, `Describe{Impl:"localexec"}`. Implements `PortForwarder` with plain local listeners? **No** — localexec does NOT implement PortForwarder (forwarding to yourself is meaningless); the conformance suite runs its PortForwarder section only for implementations that assert the capability. |
| `internal/transport/conformance/conformance.go` | The T7 suite as an exported `Run(t *testing.T, name string, newT func(t *testing.T) transport.Transport)`; PortForwarder section gated on capability assertion. Uses only stdlib + the transport package. |
| `internal/sshnative/native.go`, `auth.go`, `forward.go` (+ tests) | The T5 implementation. |
| `internal/sshnative/testserver_test.go` | The T6 in-process server harness (test-only; `_test.go` so it never ships). |
| `internal/sshnative/conformance_test.go` | Runs the T7 suite vs the T6 server; knownhosts strict-failure test (wrong host key → actionable error). |
| `internal/transport/localexec/conformance_test.go` | Runs the T7 suite for localexec. |
| `internal/sshctl/conformance_test.go` | Runs the T7 suite behind `PORTAL_TEST_SSH_HOST` (t.Skip otherwise). |
| `cmd/portal/transport.go` (+`_test.go`) | `portal transport [name]` get/set command (T8). |

### 3.2 Modified files

| Path | Change |
|---|---|
| `internal/sshctl/transport.go` (+tests) | Delete the `Transport` interface (moves to `internal/transport`); `*SSH` implements the new core + `PortForwarder` (List/Lines via lsof + its master pid, absorbing the engine's proc call sites); `Exec` takes `[]byte` stdin (delete `ExecBytes`); `EnsureMaster`→`Ensure`, `MasterPID`→`Health`, `Exit`→`Close`, `Host()/Sock()`→`Describe()`. Behavior byte-identical. |
| `internal/forward/engine.go` (+tests) | Per T4: fields swap to `transport.Transport` + `transport.PortForwarder` + narrow local-port interface; Reconcile reads current from `PF.ListForwards`; conflict messages unchanged. |
| `internal/bootstrap/manager.go`, `internal/clipupload/upload.go`, `internal/agentclient/client.go`, `internal/clipshim/clipshim.go` | Mechanical: `sshctl.Transport` → `transport.Transport`; `Exec(ctx, "", …)` → `Exec(ctx, nil, …)`; `ExecBytes(ctx, b, …)` → `Exec(ctx, b, …)`. Upload scripts and semantics untouched. |
| `cmd/portal/doctor.go`, `run.go`, `install.go`, `inspect.go`, `lifecycle.go`, `notify.go`, `clipcheck.go` | The full a.Transport consumer set in cmd/portal is these SEVEN files — all must migrate in u2. Mostly mechanical (`sshctl.Transport` → `transport.Transport`; `Host()` → `Describe().Host` in notify.go's four call sites), but three are NOT mechanical and must be re-gated per T2's rule: **run.go** `ensureForwardedForURL` (drops `MasterPID`+`Ports.MasterForwards(pid)` for `Health.Up` + `App.PF.ListForwards`; its `Forward` call likewise becomes `a.PF.Forward` — `Forward`/`Cancel` are NEVER core-interface methods and are only ever reached through `App.PF`, per T3; do NOT widen the core interface to silence a compile error), **inspect.go** `statusView` (masterUp from `Health.Up`, not `pid>0`; forwards via `PF.ForwardLines`; system render keeps `pid=N` via Health), **clipcheck.go** (`EnsureMaster` pid-gate → `Ensure` + `Health.Up`). **doctor.go**'s daemon-down fallback probe must construct its transport via the T8 selection-aware factory (NOT `sshctl.New` — a native selection must be honored and surfaced even with the daemon down). status/doctor Impl surfacing is conditional per T8. |
| `internal/app/app.go`, `paths.go` | The T8 selection-aware factory `NewTransport(paths, host, runner, cfg, sshStderr io.Writer)` lives here and is the ONLY place transports are constructed; both concrete impls satisfy `transport.Transport` AND `transport.PortForwarder` at compile time (no runtime assertion needed); `App.Transport transport.Transport` **and `App.PF transport.PortForwarder`** (run.go/inspect.go need List/ForwardLines after `App.Ports` narrows to the engine's local-port queries only). The `sshStderr` sink is threaded to sshctl's `StderrSink` for the system path (nil = quiet); `NewProd` passes `os.Stderr`, doctor paths pass `nil` (see T8). |
| `internal/config/config.go` (+tests) | `Transport() (string, error)` / `SetTransport(string) error` with validation (`system`/`native`). |
| `internal/localapi/state.go` (+tests) | `MasterStatus` gains additive `transport`/`detail` json fields (T9); Deps' `MasterProber`/`ForwardLister` re-typed to the new shapes. |
| `go.mod` | + `golang.org/x/crypto` (the ONLY new dependency). |

## 4. Implementation order (green after every unit)

| Unit | Scope |
|---|---|
| u1 | `internal/transport` package (interfaces/types/ForwardError) + `*sshctl.SSH` implements the new methods ALONGSIDE the old ones (dual-stack; old interface still consumed everywhere). Conformance package + localexec + their tests. |
| u2 | Migrate ALL consumers to `transport.Transport`/`PortForwarder` (engine, bootstrap, clipupload, clipshim, agentclient, doctor, cmd/portal, app, localapi Deps); delete the old sshctl interface + `ExecBytes` + old method names; goldens prove byte-compat. |
| u3 | `internal/sshnative`: core (dial/auth/knownhosts/keepalive/Ensure/Health/Exec/Stream/Close/Describe) + in-process test server + conformance green vs it. |
| u4 | sshnative `PortForwarder` (listeners + direct-tcpip + registry List/Lines) + conformance forward section + knownhosts strict-failure test. |
| u5 | Selection (config file + `portal transport` + app wiring + status/doctor surfacing + T10 doctor note) + e2e: daemon-level test with localexec? NO — the daemon needs portald; keep e2e at the existing io.Pipe/fake level. Unit-test the selection matrix (absent→system, native→sshnative, junk→error). |
| u6 | Hardening: full-suite pass, greps (no `sshctl.Transport` outside sshctl, no `MasterForwards` outside sshctl/proc), doc-comment sweep, EC audit fills gaps. |

## 5. Exit criteria

1. `make build`, `go vet ./...`, `make test`, `go test -race ./...` green; changed packages gofmt-clean.
2. Conformance suite green in CI for `localexec` AND `sshnative` (vs the in-process server); `sshctl` conformance skips with a message naming `PORTAL_TEST_SSH_HOST` when unset.
3. Byte-compat: Stage-2 golden tests for `status`/`doctor` (system transport) pass unmodified in intent; a NEW engine test pins the "master established (pid=N)" log line (previously unasserted).
4. Decoupling greps: `internal/forward` contains no reference to `proc.PortLister.MasterForwards`, `MasterPID`, or `sshctl`; `sshctl.Transport` (the old interface) no longer exists.
5. Native forwards: local listener → direct-tcpip → in-process server round-trip; `ListForwards` reflects reality; `Cancel` closes the listener (connection refused after).
6. knownhosts: mismatched host key → error containing the host and remediation hint; no connection proceeds.
7. Selection matrix: absent file → system; `native` → sshnative; invalid → loud startup error; `portal transport` get/set round-trips and its no-arg form prints the active Impl unconditionally; `status`/`doctor` show the Impl line iff non-system (system path byte-identical); doctor's daemon-down fallback honors the selection (factory-constructed).
10. Non-engine pid-gate migration: tests prove `ensureForwardedForURL`, `statusView`, and clipcheck's
    gate behave correctly with a healthy transport reporting `Pid=0` (native-shaped Health fake).
8. `go.mod` delta is exactly `golang.org/x/crypto` (+ its transitive entries).
9. Native auth: agent-socket path and key-file path each covered vs the in-process server; encrypted-key and no-credentials paths produce actionable errors (unit-tested).

## 6. Risks

| Risk | Mitigation |
|---|---|
| Interface migration ripple breaks mid-sequence | u1 is dual-stack (old+new on `*SSH`); u2 is one atomic consumer sweep with the compiler as the net; goldens pin behavior. |
| Native semantics drift from ControlMaster (persistence, `-O` quirks) | System stays default; T10 documents the forward-lifetime difference; doctor surfaces it; conformance pins the shared contract. |
| knownhosts strictness locks users out | Error message names the exact remedy (`ssh <host>` once); covered by EC6. |
| In-process ssh server drifts from real sshd behavior | It only backs the NATIVE client tests (same x/crypto stack); the system transport still exercises real ssh, and `PORTAL_TEST_SSH_HOST` enables real-host conformance for both. |
| `Exec` stdin merge changes quoting/latency behavior | `*SSH.Exec` keeps the exact same argv/quoting path as today (only the stdin plumbing merges); bootstrap's shellQuoted scripts and clipupload are untouched. |

## 7. Manual verification (live box, post-merge)

1. Default (system) staging run: §11-style spot checks pass unchanged; `portal status` byte-identical.
2. `portal transport native` + staging restart: handshake completes, agent uploads, ports forward
   (visit a forwarded port), paste/notify round trips work, `portal status` shows `native-ssh`.
3. Kill the daemon under native: forwards drop immediately (T10) — observed and expected.
4. `portal transport system` restores the default; doctor PASS on both.
