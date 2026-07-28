# Portal: Transparent Clipboard-Write Interception (dev box → Mac)

**Status:** Proposed. Companion/amendment to `DESIGN-clipboard-read-interception.md` (READ).
**Audience:** repo maintainer
**Amends:** READ §6.2/§6.3 (shim fall-through on writes), READ §6.5 (xsel skip), READ §8.2 (posture
context for remote→Mac writes)

---

## 1. Problem statement

READ shipped clipboard **reads** (Mac → dev box paste) and deliberately scoped out **writes**
(dev box → Mac copy). That scoping breaks tools users run every day when SSH'd into the box:

- `xclip -selection clipboard` (and tmux `copy-pipe`, nvim's clipboard provider, git aliases) hits
  our shim, falls through to the real xclip, and on a headless box either errors
  (`Error: Can't open display`) or — when no real xclip exists — hits the shim's fall-through
  `exit 0` and **silently discards the data while reporting success**. Silent data loss is the
  worst failure mode a copy can have.
- `wl-copy` has no shim and is usually not installed → `command not found`, and nvim/tmux
  provider probes that *prefer* wl-copy fail their primary path.
- `pbcopy` / `pbpaste` don't exist on Linux at all, but they are exactly the muscle memory a Mac
  user brings to the box.
- `xsel -i` (READ §6.5 skipped xsel because *agents* don't read with it — but humans write with it).

The read-only cut was justified by the OSC 52 concern (READ §8.2): a hostile remote must not be
able to write the Mac clipboard invisibly. That concern is real, but the answer is **visibility,
not absence**: OSC 52 is unauthenticated, invisible, and reachable by anything that can write to
your TTY (even `cat`-ing a malicious file). A portal write path is none of those things — it is
capability-gated, size-capped, audited, and **raises a native macOS notification on every remote
write**. Breaking everyday tools to avoid building the safe version was the wrong trade.

**Decision (owner-approved):** ship clipboard writes, default **ON**, with a per-write native
notification as the poisoning-visibility control. Same infrastructure, same invariants, reversed
direction.

---

## 2. End-to-end data flow (copy)

```
 REMOTE dev box                                        MAC (launchd: `portal run`)
 ┌────────────────────────────────────────┐            ┌───────────────────────────────────┐
 │ user / tmux / nvim / script            │            │ agentclient.Client.Run            │
 │   echo secret | xclip -sel clipboard   │            │ runClipWriteHandler (own goroutine│
 │        │  (~/.local/bin first on PATH) │            │   + worker semaphore 1)           │
 │        ▼                               │            │  ├ pull bytes over ControlMaster  │
 │  ~/.local/bin/xclip (portal shim v8)   │            │  ├ verify sha + size cap          │
 │   parses argv → WRITE mode             │            │  ├ capability gate feature.       │
 │   exec portald clip copy text          │            │  │   clip-write (re-read per op)  │
 │        │                               │            │  ├ set pasteboard (pbcopy /       │
 │  (1) portald clip copy:                │            │  │   osascript PNGf)              │
 │   - read stdin (≤ cap), ShortSHA       │            │  ├ audit ClipWritten              │
 │   - write $HOME/.cache/portal/clip/    │            │  └ native notification            │
 │       copy-<sha>.txt  (0600, atomic)   │            │    "Clipboard set from <host>"    │
 │  (2) "copy\ttext\t<sha>\t<size>\n"     │            └───────────┬───────────────────────┘
 │      on cmd-<pid>.sock ────────────────┼── UP the pipe ──► (3) ClipWriteRequest          │
 │                                        │   {nonce,epoch,kind,fmt,sha,size}               │
 │  (6) agent writes "ok\n" back ◄────────┼── DOWN the pipe ─ (5) ClipWriteResponse         │
 │  (7) portald: unlink copy file, exit 0 │   {nonce,epoch,ok}                              │
 │  (8) shim exit 0 — copy landed         │   (4) bytes pulled FIRST over ssh exec          │
 │                                        │◄──── cat ~/.cache/portal/clip/copy-<sha>.txt ── │
 └────────────────────────────────────────┘   (--noprofile --norc; sha re-verified on Mac)  │
```

**Critical ordering invariant (mirror of READ §2):** the Mac pulls the bytes over the side
channel, **verifies the SHA**, and sets the pasteboard *before* it emits
`ClipWriteResponse{ok:true}`. By the time the shim sees exit 0, the content is on the Mac
pasteboard. Clipboard bytes **never** touch the CBOR frame in either direction.

---

## 3. How the bytes cross (mirror of READ §3)

Same three options, same verdict, opposite direction. The CBOR pipe stays control-only
(`MaxFrameBytes` untouched); the bytes ride the **existing ControlMaster** as a content-addressed
0600 file:

1. `portald clip copy` reads stdin fully (fail-fast over the cap), computes
   `clipupload.ShortSHA`, and writes `~/.cache/portal/clip/copy-<sha>.txt|.png` itself — it is
   already on the dev box, so unlike the read path **no upload exec is needed**; a local atomic
   write (unique tmp → chmod 0600 → mv, dir `install -d -m 0700`) suffices.
2. The frame carries only `{kind, fmt, sha, size}`. `size` lets the Mac refuse an oversized
   payload *before* pulling a byte.
3. The Mac pulls with a short-lived `ssh exec` (`bash --noprofile --norc -c 'cat …'`) against a
   path it **reconstructs itself** from the `^[0-9a-f]{32}$` SHA — no path from the wire, exactly
   READ §7.1's path pinning, reversed. It then re-verifies `ShortSHA(bytes) == sha` and
   `len(bytes) == size`. rc-file stdout injection or a same-uid race that swaps the file both
   land in the same place: SHA mismatch → `ok=false` → shim falls through. Content addressing is
   the integrity check.
4. After the response (ok or not) — which per the ordering invariant means the Mac is done with
   the file — `portald clip copy` unlinks it. A GC sweep in the same code path removes any
   `copy-*` litter older than 1 hour (crashed runs, timed-out waiters).

**Size caps** mirror the read path: `clipupload.MaxUploadBytes` (8 MiB) for images, the read
path's text cap for text. Enforced twice: locally in `portald clip copy` (fail fast, no socket
round trip) and on the Mac (defense against a forged frame).

---

## 4. Protocol & cmd-socket design

### 4.1 A new service, no ProtoVersion bump

`ProtoVersion` stays **4**. Post-service-registration, capabilities negotiate symmetrically via
`Hello`/`HelloAck` `Services`; a new `clipwrite@1` service riding the generic `Msg` frame is
exactly the additive case that machinery exists for. An old Mac simply never advertises
`clipwrite` → the agent answers `none` → the shim falls through. Loud drift detection is already
covered by the bootstrap SHA match.

`messages.go` additions:

```go
// ClipWriteRequest — agent → client (service "clipwrite", kind "req"). A remote
// shim asked the Mac to SET its clipboard. Bytes are NOT here: they sit in
// ~/.cache/portal/clip/copy-<sha>.<ext> on the dev box, and the Mac pulls them
// over the side channel (§3). Kind ∈ {"text","image","clear"}; Format is "png"
// for images; SHA/Size are empty/0 for "clear".
type ClipWriteRequest struct {
    Nonce  uint64 `cbor:"n"`
    Epoch  uint64 `cbor:"e"`
    Kind   string `cbor:"kind"`
    Format string `cbor:"fmt,omitempty"`
    SHA    string `cbor:"sha,omitempty"`
    Size   int64  `cbor:"sz,omitempty"`
}

// ClipWriteResponse — client → agent (service "clipwrite", kind "resp").
// OK=true only after pull + SHA verify + pasteboard set all succeeded.
type ClipWriteResponse struct {
    Nonce uint64 `cbor:"n"`
    Epoch uint64 `cbor:"e"`
    OK    bool   `cbor:"ok"`
    Err   string `cbor:"err,omitempty"`
}
```

### 4.2 cmd-socket grammar (additive)

```
copy\ttext\t<sha>\t<size>\n      → set Mac clipboard text     "ok\n" | "none\n" | "rejected\n"
copy\timage\tpng\t<sha>\t<size>\n → set Mac clipboard PNG     "ok\n" | "none\n" | "rejected\n"
copy\tclear\n                    → clear the Mac clipboard    "ok\n" | "none\n" | "rejected\n"
```

Malformed SHA (`^[0-9a-f]{32}$` fails), non-positive/oversized size, unknown kind → `rejected\n`
(default-deny preserved). Bytes never traverse the socket — the verb stays a single bounded read.

### 4.3 Agent side: `svc_clipwrite.go`

A structural clone of `clipService` (it is the second consumer of the generalized `Call` helper):
claims the `copy` verb, gates on `host.HasClient() && host.ClientHas("clipwrite")` (either missing
→ immediate `none\n`), mints `(nonce, epoch)` via `host.Call`, `maxInflight` 4, and maps every
adverse path (no client, cap hit, outbox full, timeout, ctx cancel, `ok=false`) to `none\n` —
never an error string the shim would have to parse.

### 4.4 Correlation & timeout budget

Identical to READ §4.4/§4.5 — `(Nonce, Epoch)` correlation, and the same strictly-decreasing
budget under `HeartbeatTimeout` (12s): shim/`portald clip copy` read deadline **13s** > socket
deadline **11s** > agent `clipWriteTimeout` **9s** > Mac total (pull + verify + pasteboard set +
notification dispatch) **≤ 8s**. The Mac-side pull (~3s worst case at 8 MiB) + `pbcopy`/osascript
set (~1–2s) fit the 8s slot with margin; the notification is raised **after** the response is
sent (fire-and-forget) so it never eats the budget. The 13s deadline covers socket discovery
and the reply together; per-socket dial time does not accumulate outside it.

### 4.5 Behavior when daemon/client unavailable

`none\n` immediately (no client, mid-reconnect, feature off, oversized, timeout). `portald clip
copy` maps it to **exit 1** and the shim falls through — see §6.1 for why write fall-through must
never be silent.

---

## 5. Mac-side handler — never block the demux (mirror of READ §5)

- New dedicated buffered channel (cap 8) + `runClipWriteHandler` in `run.go`, a sibling of
  `runClipHandler` with the same shape: own goroutine, **worker semaphore 1** (two rapid copies
  can't fork two pasteboard writers; an in-flight write answers `OK=false` immediately → shim
  falls through), handler work on a tracked worker goroutine. Each event carries its agent
  session context; disconnect cancels queued and in-flight work before it can mutate the
  pasteboard.
- On `KindClipWriteRequest`:
  1. Capability gate `feature.clip-write` (re-read per op, Mac-side — READ §7.1(3)); disabled →
     `OK=false` + `ClipWriteDenied(host, kind, "disabled")`.
  2. `size > cap` or SHA malformed → `OK=false` + audit (`oversize` / `badsha`).
  3. Pull + verify (§3). Pull failure or mismatch → `OK=false` + audit `shamismatch`.
  4. Set the pasteboard via a new `clip.Writer` (see §11): text → `pbcopy` (argv-free, stdin);
     PNG → 0600 temp file + `osascript` `«class PNGf»` read, temp removed after; clear → empty
     set. All cgo-free (CGO_ENABLED=0 cross-compile constraint holds).
  5. Reply `OK=true`, audit `ClipWritten(host, kind, "sha=… size=…")`, then notify (§5.1).

### 5.1 The per-write notification (the poisoning-visibility control)

Every successful remote write raises a native macOS notification via the existing
`raiseNotification` path (same AppleScript sanitization as READ §5):

```
title:    Clipboard set from <host>
subtitle: text, 42 bytes        (or: image/png, 118 KB · or: cleared)
```

- **No content preview.** A preview would render a just-copied password on screen (shoulder-surf
  + screen-share leak) — the notification's job is *that* a remote write happened, not *what* it
  wrote. Residual risk is stated honestly in §7.3.
- **Coalescing.** tmux `copy-pipe` fires on every selection; banner-per-copy would train users to
  disable the feature. Leading edge: first write notifies immediately. Then a 5s window: further
  writes are audited but not bannered; when the window closes with N > 0 suppressed, one summary
  banner (`N more clipboard writes from <host>`) is raised. Every write is always in the audit
  log regardless.
- **Deliberately NOT gated on `feature.notify`.** The relay-notification toggle must not be able
  to silence the security mitigation that makes default-on writes acceptable. The only way to
  stop the banners is to turn `clip-write` itself off. (If real-world use demands a separate
  banner knob, it is a follow-up config flag — explicitly out of v1.)
- **No replacement group.** The `terminal-notifier` path omits the ordinary shared `portal`
  group so the leading banner and summary coexist in Notification Center and later relay
  notifications cannot evict either one.

---

## 6. The shims and `portald clip copy`

### 6.1 Failure semantics: a write must never silently succeed-and-discard

Reads degrade correctly to "empty stdout, exit 0" (= no content). Writes do not: exit 0 with the
data dropped is silent data loss. Rules, applied to every write interception:

1. Portal path succeeds → exit 0.
2. Portal path fails and a **real binary** exists later on PATH → fall through to it (it may work
   — the box might actually have a display).
3. Portal path fails and **no real binary** exists → print one line to stderr
   (`portal: clipboard write failed (no Mac client connected)`) and **exit 1**.

Rule 3 changes the existing shim tail behavior *for write invocations only*; read invocations
keep the empty-stdout `exit 0` degrade (READ §6.2's correct answer for "no content").

### 6.2 `xclip` shim v8 — argv parsing replaces `case "$*"` for writes

Write forms can't be matched with the read path's fixed-string `case` patterns — xclip accepts
unique-prefix flag abbreviations (`-sel c`, `-i`) and its **default mode is input**. The v8 shim
adds a conservative token loop over `"$@"`: classify `-o*`/`-out*` (read), `-i*`/`-in*` (write),
`-sel*` + following token (selection), `-t <target>`/`-target <target>`, `-r*`/`-rmlastnl` (trim
trailing newline), and known no-arg/arg flags. Outcomes:

- Read shapes route exactly as today (READ §6.2 behavior preserved byte-for-byte for the shapes
  it matched; the parser strictly widens coverage).
- Write to selection `clipboard` **or `primary`** with target text/none →
  `portald clip copy text --empty-clears` (also `--trim` when `-rmlastnl`). macOS has one
  pasteboard; X primary vs clipboard is meaningless on a headless box, and `echo x | xclip`
  (defaults: input, PRIMARY) is exactly the muscle-memory case that must work. Empty input clears
  the pasteboard and exits 0, preserving xclip's legal empty-write behavior. §8.1 records this
  mapping.
- Write with `-t image/png` → `portald clip copy image png`.
- Write with any other `-t image/*` → fall through (format honesty, mirror of the read rule).
- **Any token the parser does not recognize → fall through to the real binary** (never misroute;
  status-quo-or-better, never worse).

### 6.3 `wl-copy` shim — new

All wl-copy invocations are writes. Flag surface: `-p`/`--primary` (same mapping as §6.2),
`-n`/`--trim-newline` → `--trim`, `-t`/`--type` (text/* and none → text with
`--empty-clears`; `image/png` → png; other image → **exit 1 + stderr**, there is no real wl-copy
to fall through to on most boxes and silently dropping is forbidden), `-c`/`--clear` →
`portald clip copy clear` (supports the
`wl-copy secret; sleep 30; wl-copy --clear` hygiene pattern), `-o`/`--paste-once` and `-f` →
ignore (daemon semantics don't apply). **Positional args are the text** (wl-copy joins argv with
spaces): the shim routes `printf '%s' "$*-remainder"` into the copy; otherwise stdin.

### 6.4 `pbcopy` / `pbpaste` shims — new

- `pbcopy`: stdin → `portald clip copy text --empty-clears`. Failure → stderr + exit 1 (rule 3).
  The internal flag maps empty stdin to `copy clear` (matches macOS pbcopy semantics and the
  legal empty-write behavior of the Linux writers); the text entrypoint rejects empty input when
  the flag is absent.
- `pbpaste`: → `portald clip text`; on failure, execute a preserved pre-shim binary or a real
  binary later on PATH when present; otherwise use the empty-stdout, exit-0 read degrade.
- Caveat, accepted: a script that probes `command -v pbcopy` to *platform-detect macOS* would now
  misfire on the dev box. Judged rare and low-harm next to the value of the muscle-memory path;
  documented here so it's a known trade, not a surprise (§8.2).

### 6.5 `xsel` shim — new (reverses READ §6.5 for the write half)

READ skipped xsel because agents don't *read* with it; humans *write* with it. Shim handles both
directions so the deployed shim is never worse than the (usually absent) real binary:
`-i`/`--input` or (no mode flag AND stdin is not a tty — xsel's own default rule, testable in the
shim via `[ -t 0 ]`) → write; `-o`/`--output` → `portald clip text` read; `-b`/`-p`/`-s`
selections all map to the one Mac pasteboard; empty write input maps to clear; `-c`/`--clear` →
clear; unrecognized → fall through.

### 6.6 `portald clip copy` (the write-side arbiter)

```
portald clip copy text [--trim] [--empty-clears]   reads stdin, ≤ text cap; empty may map to clear
portald clip copy image png        reads stdin, ≤ 8 MiB, verifies PNG magic BEFORE sending
portald clip copy clear
```

Fans out over `cmd-*.sock` and **refuses (exit 1) if >1 distinct connected agent answers** —
READ §7.3's multi-client rule matters *more* for writes (user A's copy must never land on user
B's Mac). Sequence: read stdin (fail fast over cap) → for image, check the PNG magic locally
(format honesty at the source; a mislabeled JPEG never leaves the box) → ShortSHA → atomic 0600
write of `copy-<sha>.<ext>` → `copy\t…` verb → map `ok` to exit 0, anything else to exit 1 →
release the invocation's lease and unlink after the last identical concurrent copy
finishes → opportunistic GC of stale `copy-*`/leases (>1h).

`--trim` strips exactly one trailing `\n` if present (implements `xclip -rmlastnl` /
`wl-copy -n` in Go, where trailing-byte handling is exact — `$(…)` in sh is not).

---

## 7. Security

### 7.1 Threat model: why this is not OSC 52

READ §8.2 keeps terminal OSC 52 writes disabled because OSC 52 is (a) invisible, (b)
unauthenticated, (c) reachable by anything that can influence TTY output — `cat` of a hostile
file suffices. The portal write path differs on every axis: it requires same-uid code execution
on the dev box (already the read/exec/cred threat actor, READ §7.1), rides the authenticated
ControlMaster, is **capability-gated Mac-side** (re-read per op, no restart), **audited per
write**, **size-capped**, and **raises a banner per write** (§5.1). OSC 52 write control stays
delegated to the terminal exactly as READ §8.2 states — this path does not reopen it; it replaces
it with an accountable equivalent. The controls carried over from READ §7.1 unchanged: path
pinning via SHA-reconstructed paths (now enforced on the **Mac** for the pull), 0600 files under
a 0700 dir, Mac-side capability gate (`feature.clip-write`, default **on**), append-only audit
(`ClipWritten` / `ClipWriteDenied` with reasons `disabled` / `oversize` / `badsha` /
`shamismatch` / `inflight`), DoS bounds (semaphore 1, `maxInflight` 4, bounded verb read,
notification coalescing bounds banner floods).

### 7.2 What a hostile same-uid remote CAN do with this feature on

Set your Mac clipboard to attacker-chosen content, at most as fast as the semaphore allows, with
every write audited and bannered. The classic exploit (swap a copied wallet address / inject a
`curl | bash` one-liner for your next ⌘V) now requires the victim to ignore a
`Clipboard set from <host>` banner that they did not cause. That is the designed mitigation:
**unexpected writes are loud.**

### 7.3 Residual risks, stated honestly (mirror of READ §7.4)

- **The well-timed overwrite.** An attacker who watches for the user's own `xclip` write and
  immediately overwrites it produces a *second* banner adjacent to the expected one. Two banners
  for one copy is an anomaly a user *can* notice, but we do not claim they will. The audit log
  disambiguates after the fact. Accepted: the alternative (confirm-per-write) breaks
  transparency, and OSC 52-style silence is strictly worse.
- **No content preview** (§5.1) means the banner proves a write happened, not what it wrote — the
  deliberate trade against rendering copied secrets on screen.
- **Banners require eventually looking at the screen.** Notification Center retains them; the
  audit log is the ground truth.
- **Clipboard overwrite is inherently destructive** of whatever the Mac clipboard held. `clear`
  included. Gated + audited + bannered is the containment, not prevention.

---

## 8. Scope decisions

### 8.1 X primary/secondary selections map to the one Mac pasteboard

macOS has no primary-selection concept, and on a headless box the X distinction is vestigial.
Mapping both to the Mac pasteboard makes the default forms (`echo x | xclip`, `xsel`) work.
Consequence: setups that sync `*` and `+` registers may double-write — idempotent same-content
writes, absorbed by notification coalescing.

### 8.2 pbcopy-on-Linux platform-detection caveat

Accepted and documented (§6.4). Deploying `pbcopy`/`pbpaste` is the point (muscle memory), and a
copy-capability probe that finds a *working* pbcopy is not actually wrong.

### 8.3 `clear` is in scope

It costs one verb arm and enables the copy-a-secret-then-clear hygiene pattern (`wl-copy
--clear`, `xsel -c`). Remote clearing is a write like any other: gated, audited, bannered.

### 8.4 Non-PNG image writes fall through or fail loudly

Mirror of the read path's format honesty: portal never relabels bytes. `image/png` only; the PNG
magic is verified **at the source** before anything crosses.

### 8.5 One feature knob, not two

`clip-write` gates the whole path; the banner is not separately disableable in v1 (§5.1). A
`clip-write-notify` knob is a possible follow-up if real usage demands it — kept out so the
default posture ships un-weakenable-by-accident.

### 8.6 OSC 52: unchanged

Terminal OSC 52 write support stays disabled (READ §8.2). This feature is the sanctioned
replacement, not a reason to re-enable the unsanctioned one.

---

## 9. Deployment & lifecycle

- **`clipshim.Version` → 8.** The shims table gains `wl-copy`, `pbcopy`, `pbpaste`, `xsel`; the
  `xclip` script is rewritten (v8 parser); `wl-paste`, `xdg-open`, `portal`, `portal-askpass`,
  `sudo` re-deploy with the bumped marker. Daemon-driven convergence (READ §9.1) means every
  connected box picks v8 up on the next reconnect with no manual reinstall.
- **Backup/restore** (READ §9.3) applies unchanged to the new names — a pre-existing user
  `~/.local/bin/wl-copy` (etc.) that is not portal-marked is backed up once (`cp -P`) and
  restored on uninstall. `clipshim.Remove`'s bin list and the uninstall loop gain the four new
  names.
- **PATH ordering** (READ §9.2) is already converged by the existing marker blocks; the new shims
  ride the same `~/.local/bin` prepend.
- **Shell portability:** every new/changed script is `/bin/sh`, and is verified under
  **`/bin/dash`** and BusyBox `ash` before commit (the v7 lesson: macOS bash masks dash-isms; the
  §6.2 token parser especially must avoid bashisms — no arrays, no `[[`, no substring expansion).
- **Mixed-version matrix** (READ §9.5): new shim + old agent → `copy` verb hits the default-deny
  `rejected` → exit 1 → fall-through/stderr per §6.1. New agent + old Mac → `clipwrite` not
  advertised → `none` → same. Old shim + new everything → writes keep failing as today until the
  v8 marker converges (bounded by one reconnect).

---

## 10. Doctor / self-test additions

- **PATH winner** checks extend to `wl-copy`, `pbcopy`, `pbpaste`, `xsel` (same
  login+interactive `command -v` probe, same marker verification).
- **Verb support:** confirm `portald clip` advertises `copy` and the agent/client both advertise
  `clipwrite@1`; report the Mac-side `clip-write` feature state.
- **No destructive smoke by default.** A write smoke test would overwrite the user's real
  clipboard on every `portal doctor` run; the round trip is only exercised end-to-end by the
  manual checklist (§12). (A `--write-smoke` opt-in flag may be added later; out of v1.)

---

## 11. Touched components

| Component | Change |
|---|---|
| `pkg/protocol/messages.go` | `ClipWriteRequest`/`ClipWriteResponse` (ride `Msg`; **no ProtoVersion bump**). |
| `pkg/agent/svc_clipwrite.go` | New compiled-in service `clipwrite@1`, verb `copy`, clone of `clipService`'s Call pattern. |
| `pkg/agentclient` | Auto-registered `clipwrite` handler, dedicated cap-8 channel, `ClipWriteEvents()`, `SendClipWriteResponse`. |
| `cmd/portal/run.go` | `runClipWriteHandler` (sibling of `runClipHandler`): gate → pull → verify → set → audit → notify (§5). |
| `cmd/portald/main.go` | `clip copy` subcommand (§6.6): stdin → local content-addressed file → `copy` verb → unlink + GC. |
| `internal/clip` | New `Writer` (SetText / SetImagePNG / Clear), darwin via pbcopy + osascript `«class PNGf»`, cgo-free. |
| `internal/clipshim` | Version 8; xclip v8 parser; new wl-copy/pbcopy/pbpaste/xsel shims; Remove/uninstall lists extended. |
| `internal/config` | `FeatureClipWrite = "clip-write"`, default on; added to `featureNames`. |
| `internal/audit` | `ClipWritten` / `ClipWriteDenied`. |
| `cmd/portal/doctor.go` | §10 checks. |

---

## 12. Manual verification checklist

1. `portal install <host>` (or one daemon reconnect on an existing box) → shims report marker v8;
   `command -v wl-copy pbcopy pbpaste xsel` all resolve to `~/.local/bin/*` with the marker.
2. `echo hello | xclip -selection clipboard` on the box → ⌘V on the Mac pastes `hello`; a
   `Clipboard set from <host>` banner appeared; `audit.log` has the `ClipWritten` line.
3. Same for `echo hello | xclip` (bare — PRIMARY default), `printf hi | wl-copy`,
   `wl-copy some words` (argv form), `echo hi | pbcopy`, `echo hi | xsel -ib`.
4. `pbpaste` and `xsel -ob` on the box print the Mac clipboard (read path through the new shims).
5. tmux `copy-pipe` with `xclip -in -selection clipboard` → selection lands on the Mac; rapid
   repeated copies produce a leading banner + one coalesced summary, and one audit line each.
6. PNG write: `cat s.png | xclip -selection clipboard -t image/png` → image pastes on the Mac.
   `cat s.jpg | xclip -selection clipboard -t image/jpeg` → falls through (no mislabeled bytes).
7. `wl-copy --clear` → Mac clipboard empties; audited + bannered as `cleared`.
8. `portal features clip-write off` → writes fall through/exit 1 with the stderr line;
   `audit.log` shows `disabled` denials. Re-enable → works again.
9. Oversized write (`head -c 9M /dev/urandom | base64 | xclip -sel c`) → fast local exit 1, no
   socket round trip.
10. Daemon down → `echo x | pbcopy` exits 1 with the one-line stderr message (never silent).
11. All shim scripts pass under `/bin/dash` and BusyBox `ash`.
12. `portal uninstall` → the four new shims are removed (or user backups restored), rc blocks
    stripped; `portal doctor` on a re-install goes green.
