# v2 task list (single source of truth for remaining work)

Rule: no silent TODOs in code. Anything deferred lives HERE, explicitly.

## Done
- [x] portal-proto: v4 wire (golden-vector byte-exact), async codec, clipsync types, Hello.box
- [x] portal-transport: native-ssh (russh: ssh -G, agent+key auth, strict known_hosts,
      exec/stream/direct-tcpip, ProxyJump/ProxyCommand), ListenerForwarder, localexec,
      conformance + in-process sshd e2e. system-ssh deliberately absent.
- [x] portal-core: config (+v1 migration), portmap (indexed+fallback), engine
      (planner + pass + event loop), agent client (handshake/demux/reconnect/shim deploy),
      bootstrap (atomic agent upload + shim deploy), clipsync publisher, supervisor
      composition (e2e-tested vs scripted portald)
- [x] portal-clip: native NSPasteboard (objc2), watcher state machine (conceal/gates)
- [x] portal-cred: policy core (gates→cooldown→TouchID→dialog→keychain, v1-exact
      deny vocabulary/caps) — BACKENDS PENDING (see below)
- [x] portald: clip store + local paste verbs + blob put + store-backed shims
- [x] DESIGN-clipsync.md

## Native SwiftUI + BoltFFI application migration — COMPLETE
- [x] accepted architecture: `docs/DESIGN-swiftui-boltffi.mdx`
- [x] reusable `portal-client` crate (bounded async requests + reconnecting,
      cancellation-aware state subscription)
- [x] pinned BoltFFI 0.30.1 static macOS XCFramework, deterministic Swift 6
      generator patches, macOS 13 C/Rust deployment target, ownerless public
      Swift façade
- [x] one Swift executable dispatching GUI, Rust CLI, Rust daemon, legacy
      compatibility, and native Swift prompt modes
- [x] app-owned `portal` launcher with argument/stdio/exit-code tests
- [x] SwiftUI menu bar, management window, boxes, forwards, atomic allowlist
      editor, features, logs, updates, daemon reconnect state
- [x] one-executable app assembly and embedded-agent/minimum-OS verification
- [x] launchd/app-install/update paths migrated to the Swift executable and
      bundled launcher while standalone compatibility modes remain accepted
- [x] signed/notarized local release transaction through the new bundle layout:
      Accepted app/binary/DMG tickets, staples validated, Gatekeeper accepted,
      canonical `/Applications` install, healthy agents/doctor, and real post-
      swap rollback fault injection
- [x] interactive visual/accessibility QA on the shipping Mac desktop: AX-tree
      labels, menu commands/shortcuts, status colors, prompt controls, window
      close, app quit, and daemon independence are scripted release gates
- [x] remove the Rust macOS binary target and Rust prompt fallback; the native
      Swift executable is the only compiled shipping host
- [x] remove the temporary Rust/AppKit tray implementation and its presentation-
      only dependencies after native parity tests passed
- [x] update README screenshots and `docs/releases/v2.0.28.md` for SwiftUI

## In flight (this phase: launchd + CLI + agent mode) — COMPLETE
- [x] TASKS.md (this file)
- [x] portald agent mode: serve loop (Hello/HelloAck, Subscribe/Snapshot/deltas,
      heartbeats, Ping echo, Shutdown/Bye, clipsync update/clear→store apply→ack),
      /proc/net/tcp[6] loopback watcher, cmd socket (notify/open), notify CLI
      (--hook classifier + generic), open CLI — both-halves e2e test
      (portal-core session client ↔ portald agent over duplex pipes)
- [x] launchd service manager (plist render, bootstrap/bootout/kickstart/print
      via Runner, unit-tested)
- [x] CLI: install/uninstall/start/stop/restart/status/logs,
      box add|remove|list, allow/unallow, features (smoke-tested live)
- [x] daemon: config load+v1 auto-migrate (loud port-shift warning), instance
      lock + minimal read-only status socket (JSON), supervisor wiring
      (pasteboard watcher, feature-gate files, osascript notifications,
      open-url), SIGTERM/ctrl-c clean shutdown (verified live)
- [x] fix: BoxStack holds filter_tx + set_filter(); session fuses the filter
      branch when the sender drops (no busy-loop)
- [x] fix: watcher change_id clock-seeded (survives Mac daemon restarts
      against persisted box stores)

## Known gaps in this phase (deliberate, tracked)
- allow/unallow + box add/remove require `portal restart` to apply (config
      hot-reload is the "multi-box polish" phase; BoxStack::set_filter is the
      plumbing, the daemon just doesn't watch the file yet)
- `portal ports` verb: status JSON already carries the forwards table; a
      dedicated pretty-printer lands with doctor
- release embedding: dev builds read PORTAL_AGENT_AMD64/ARM64 env at runtime;
      include_bytes! embedding is the release-pipeline task

## Credentials phase — COMPLETE (design: NO osascript in security paths)
- [x] prompt backend: the app executable's native Swift NSAlert +
      NSSecureTextField process mode; JSON stdin/stdout; allow/deny/cancel/
      timeout/remember/forget automation; empty-secret allow = deny
- [x] biometry backend: in-process LAContext (objc2-local-authentication),
      typed LAError mapping (UserCancel/AppCancel/SystemCancel → Canceled;
      lockout/not-enrolled → Err → dialog fallback), invalidate() on timeout
- [x] keychain backend: security-framework generic passwords (service
      "portal.credentials"), list via ItemSearch, absent-delete tolerated
- [x] supervisor cred handler task: dedicated channel → spawn_blocking policy
      core → CredResponse; box attribution in the dialog requester; ONE
      cooldown map and global FIFO prompt gate shared across boxes; `cred@1`
      advertised
- [x] portald: `keychain run --label [--env|--stdin] -- cmd` (secret → child
      env/stdin ONLY; exit 111 on deny) + `keychain askpass`; cmd-socket
      `cred` verb (base64 JSON, queue-depth-aware outer timeout); agent serve
      loop mints nonces + correlates CredResponse by nonce+epoch (pid), bounded
      FIFO with one active request; e2e test shim→agent→Mac→shim
- [x] box shims: sudo wrapper (fires ONLY with no controlling tty — fail-safe
      around human sessions; respects user SUDO_ASKPASS) + portal-askpass;
      shim VERSION bumped to 10 (auto-redeploy on reconnect)
- [x] CLI: keychain list (+ Touch ID availability line) / keychain forget
- [x] zeroize: Decision.secret + PromptDecision.secret zeroized on drop

## Deferred from the cred phase (tracked, deliberate)
- [ ] audit log (Mac side): cred served/denied/forgotten, clip published —
      currently tracing-only; a persistent append-only audit file is a small
      follow-up
- [x] RELEASE-GATED: signed builds compile in SecAccessControl +
      biometryCurrentSet item binding, verify `_signed-build-mode=enabled`, and
      fail closed if access-control creation fails
- [x] interactive dialog QA on a real desktop session, including AX secure-
      field labeling and allow/deny/cancel/timeout/remember/forget outcomes

## Later phases (ordered)
- [x] clip-write relay (box→Mac) — complete (store-first copy verb, cmd-socket
      clipwrite, Mac pull-by-sha + verify + pasteboard set + banner, shims v11,
      clipwrite@1)
- [x] doctor: status-view + box-side checks; `portal doctor` per box
- [x] config hot-reload: daemon polls config.toml (2s mtime) and reconciles
      live — in-place index/allow/deny via BoxStack::set_filter; add/remove
      spawns/tears down stacks; host change = replacement; TESTED
      (supervisor.reconcile unit test + live smoke). CLI messages updated.
- [x] release pipeline: LOCAL-FIRST (no CI secrets). `release.sh <tag>`
      builds musl portald (both arches via zigbuild) → embeds → Developer ID
      codesign (hardened runtime + keychain-access-groups) → notarizes →
      minisign → publishes via gh. PROVEN end-to-end locally (notarization
      Accepted; minisig verified against minisign.pub). Nothing sensitive
      in the repo: NOTARY_KEY_ID/NOTARY_ISSUER env-required, the Developer ID
      cert + .p8 + minisign private key all live outside it. CI workflow
      deleted by decision. `portal upgrade` verifies the minisig against the
      checked-in public key.
- [x] Keychain item binding: SecAccessControl biometryCurrentSet via
      set_generic_password_options on signed builds (PORTAL_SIGNED=1 env
      gate until the signed pipeline ships; unsigned falls back to plain
      generic passwords + LAContext gate — no regression vs v1)
- [ ] exec bridge (`portal exec`, PTY) — golden vectors already cover frames
- [ ] full localapi (DOWNGRADED by decision): only if something needs it

## Open questions (need user decision)
- netlink sock_diag vs /proc/net/tcp polling for the box watcher: v2 ships /proc
  at 75ms (identical latency to v1's 75ms netlink dump cadence, testable off-linux);
  netlink + destroy-multicast is a possible later optimization. OK?
- russh pins ssh-key 0.7.0-rc (release candidate): revisit when russh stabilizes.
