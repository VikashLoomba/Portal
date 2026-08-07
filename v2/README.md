# portal v2 (Rust)

Rust rewrite of portal: multi-box SSH port forwarding, clipboard paste
(fixed), notification relay. The Go implementation at the repo root stays the
reference (and the deployed product) until parity phases land.

## Workspace

| Crate | Status | Contents |
|---|---|---|
| `portal-proto` | **done** | Wire protocol, v4-compatible with Go `pkg/protocol`. Framed CBOR (`"PF"` + u32 BE len + envelope). Golden-vector tested against `docs/vectors/protocol_*.hex` — decode AND byte-exact re-encode. |
| `portal-transport` | **mostly done** | Async `Transport`/`PortForwarder` contract (v2: forwards are local≠remote **pairs**), argv shell-join contract, working `localexec` + conformance suite, the **ListenerForwarder** (daemon-owned local listeners spliced onto `Dialer` streams; conflicts = bind errors), and the **full native-ssh transport (russh)**: `ssh -G` config resolution (Include/Match/ProxyJump/ProxyCommand), ssh-agent + identity-file auth, STRICT known_hosts, keepalive 15s/3, single-flight connection rebuild with timeout, exec with faithful exit codes + stderr demux, long-lived streams with channel-EOF-on-stdin-close, and direct-tcpip `Dialer`. E2E-tested against an in-process russh sshd (real handshake/auth/exec/forward-splice, unknown-host-key and wrong-client-key rejection). **There is deliberately no system-ssh/ControlMaster implementation.** |
| `portal-core` | mostly done | Multi-box config + validation + v1 migration; derived paths; **port mapping**; **reconcile engine**; **agent client** (shim deploy after SHA match, clipsync ack routing, honest service advertisement); **bootstrap**; **clipsync publisher**; **supervisor** — the composition root: per-box task trees (agent client + reconcile loop + clipsync publisher + notify/open-url routing) under per-box cancellation, ONE pasteboard-watcher fan-out, cross-box local-port taken-set, watch-channel status snapshots, injectable transport factory — **composition-tested end-to-end against a scripted portald over real v4 frames** (forwards converge, notifications route box-attributed, image copy → blob push → synced). localapi is DOWNGRADED to a future minimal status socket. |
| `portal-clip` | **done** | `Clipboard`/`ClipboardWriter` traits + mock; **native NSPasteboard backend (objc2)** — in-process reads/writes (no osascript, no 5s coercion cliff), changeCount-consistent snapshots (torn-read retry), image conversion via NSBitmapImageRep (TIFF/JPEG/HEIC → PNG), concealment types read directly off the type list (fail-closed); **the clipsync watcher** — pure, tested state machine: monotonic change_ids, concealed copies invisible (not even a Clear), live per-kind capability gates that clear stale content on gate-off. Real-pasteboard roundtrip test behind `--ignored`. |
| `portal-cred` | partial | Credential sharing (`portal keychain` / askpass / transparent sudo). **Policy core fully ported + tested** (`serve.rs`: gates → cooldown → Touch ID → dialog → keychain, v1-exact deny vocabulary and 200/300/4096-byte caps). Dialog/biometry/Keychain backends are documented seams. |
| `portal-cli` | **mostly done** | `portal` binary: **daemon mode** (config load + v1 auto-migration with loud port-shift warning, single-instance lock doubling as the read-only JSON status socket, supervisor wiring with NSPasteboard watcher / feature-gate files / osascript notifications / open-url, SIGTERM-clean shutdown — verified live), **launchd manager** (v1-shape plist render, bootstrap/bootout/kickstart via Runner, unit-tested), and working verbs: install/uninstall/start/stop/restart/status/logs, box add/remove/list, allow/unallow, features. keychain verbs land with the credentials phase (TASKS.md). |
| `portald` | **mostly done** | Clip store + local paste path + shims (clipsync §2.2); **agent mode**: v4 stdio serve loop (handshake w/ fatal proto-mismatch, Subscribe→Ack→Snapshot RESET, seq'd deltas from the /proc/net/tcp[6] loopback watcher at 75ms, heartbeats + Ping echo, clipsync update/clear→store→ack incl. inline installs, Shutdown/Bye); **cmd socket** (default-deny verbs, http(s)-only open, JSON notify) + `portald notify --hook` (Claude Code hook classifier, verified) / `--title` (unverified) / `portald open`. Both-halves e2e test: portal-core's session client against this agent over duplex pipes. Remaining: keychain verbs (credentials phase), write-side relay. |

## Build / test

```sh
cd v2
cargo test          # includes cross-language golden vectors (needs the repo checkout)
cargo build --release
```

Cross-compiling `portald` for dev boxes (phase 5): `rustup target add
x86_64-unknown-linux-musl aarch64-unknown-linux-musl`, build with
cargo-zigbuild, embed via `include_bytes!` in `portal-cli`.

## Port mapping (multi-box)

Remote port `p` is forwarded to **`localhost:p`** whenever that local port is
free. Same-number mapping is not cosmetic: forwarded services see a truthful
`Host`/`Origin` header, so MCP servers, Vite, `create-react-app`, and Django
(all of which reject mismatched origins) work through the tunnel without
per-app allowlists.

Fallbacks apply only under contention, in order (see
`portal-core/src/portmap.rs`):

1. **identity** — `p` → `localhost:p`; skipped for privileged remotes (< 1024,
   unbindable by a user LaunchAgent).
2. **indexed slot** — box index `n` in 1..=5 reserves `n*10000 + p`, giving
   each box a collision-free lane when two boxes both listen on `:8000`.
3. **allocator** — a deterministic FNV-seeded port in 60000–64999, for remote
   ports ≥ 10000, indexes ≥ 6, or when both tiers above are held.

Contention is discovered by the bind itself (`AddrInUse`/`PermissionDenied`),
and retried down the list inside the same reconcile pass, so a busy port never
costs a full safety interval of downtime. Two WARNs cover the fallout: one
names the local holder (pid + process name, deduped per port), and one flags
the translated forward itself, since that is exactly when an origin-checking
service may start rejecting requests. When the local port later frees, the
next pass reclaims the same-number mapping unprompted — otherwise a one-time
collision would keep `Host`/`Origin` wrong until the daemon restarted.
`portal status`/`ports` always render the actual mapping table, and
`portal doctor` repeats the translation as a warn.

## Phase plan

1. **proto + transports** — golden vectors (done), ListenerForwarder (done),
   native-ssh connection layer (done: russh, ssh -G resolution, agent +
   identity auth, strict known_hosts, exec/stream/direct-tcpip, in-process
   sshd e2e tests).
2. **single-box daemon parity** — DONE: reconcile loop, agent client,
   bootstrap, supervisor composition (all e2e-tested); the Rust `portal`
   drives the deployed Go portald (wire-compatible v4).
3. **clipsync + clip-write** (docs/DESIGN-clipsync.md) — DONE end-to-end,
   both directions: watcher, publisher, box store+shims, blob push/pull.
4. **launchd + CLI + agent mode** — DONE: daemon (status socket, v1
   migration, config hot-reload verified live), launchd manager, full CLI
   verb set, portald agent mode (both-halves e2e), `portal doctor`.
5. **credentials** — DONE: helper-process dialog (no osascript), in-process
   LAContext, security-framework Keychain (+ signed-build SecAccessControl
   binding), `portald keychain run/askpass`, sudo/askpass shims.
6. **release pipeline** — codeable parts DONE: build.rs embedding (verified),
   `portal upgrade` (verify-then-swap), CI workflow (musl cross-compile,
   Developer ID codesign + notarize, minisign, gh release). Remaining: first
   real CI run (needs repo secrets), exec bridge, full localapi (deferred).

## Compatibility invariants

- Wire protocol stays v4; either binary (Mac/box) may be upgraded first.
  clipsync and `Hello.box` are ADDITIVE (negotiated via the v4 services map /
  ignored-unknown-keys); the golden vectors still pass byte-exact.
- Shell shims keep the xclip/wl-paste interception mechanism but become
  local reads against portald's clip store (DESIGN-clipsync §2.2).
- A Go portald that doesn't advertise `clipsync` gets forwards/notify/cred
  but no clipboard — `portal install` deploys the v2 portald anyway.
- v1 single-host installs migrate via `Config::migrate_from_v1`; v1's
  same-port forwarding is preserved, because identity is now the preferred
  mapping and index 1 only supplies a fallback slot under contention.
- Env seams keep their names: `PORTAL_CONFIG_DIR`, `PORTAL_API_SOCK`.
