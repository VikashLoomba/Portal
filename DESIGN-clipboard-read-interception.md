# Portal: Transparent Clipboard-Read Interception + Notification Relay (cc-clip model, portal infra)

**Status:** Shipped. This document describes how the feature works as implemented.
**Audience:** repo maintainer
**Supersedes:** the PTY-keystroke clipboard interception that used to live in `cmd/portal/ssh.go`
**Reference:** [ShunmeiCho/cc-clip](https://github.com/ShunmeiCho/cc-clip)

---

## 1. Problem statement & core architectural insight

Portal solves "paste a Mac screenshot (or text) into a coding agent on a remote box," and relays the
agent's notifications back to the Mac. It used to do the paste half by wrapping `ssh` in a ~1271-line
PTY proxy that intercepted the **Ctrl+V keystroke** in every terminal encoding (`0x16`, Kitty
`ESC[118;5u`, xterm `ESC[27;5;118~`), tracked bracketed-paste regions, stripped remote OSC 52
clipboard-writes, uploaded the PNG over the ControlMaster, and **typed the remote path** into the
agent's input. It was fragile (per-terminal keystroke matching) and forced users through `portal ssh`
instead of plain `ssh`.

cc-clip solves the same problem the better way: it intercepts the clipboard **read** instead of the
keystroke. The coding agent itself owns Ctrl+V; on paste it execs `xclip`/`wl-paste`; cc-clip puts
shims earlier on `PATH` that serve the Mac clipboard back. But cc-clip pays for a whole sidecar to do
it: a Mac HTTP daemon on `127.0.0.1:18339`, an SSH `RemoteForward` reverse tunnel, `ControlMaster no`,
a 30-day bearer token, a notification nonce server, and a second launchd job.

**The insight portal exploits: it already has every transport piece cc-clip builds a sidecar for.**

- A persistent remote agent `portald` runs over the ControlMaster CBOR pipe whenever the Mac
  `portal run` daemon is up — **independent of any interactive ssh session** (`agentclient.Client.Run`
  reconnect loop, launchd `RunAtLoad`+`KeepAlive`).
- A 0600 owner-only cmd Unix socket (`~/.cache/portal/cmd-<pid>.sock`) plus the `OpenURL` envelope is
  already a **reverse request channel** riding that pipe.
- The `~/.local/bin/xdg-open` wrapper + stable `~/.cache/portal/portald` symlink + content-addressed
  bootstrap upload is already a **shim-deploy machine**.
- `internal/clip` already reads the Mac clipboard cgo-free; `internal/clipupload` already pushes bytes
  over the ControlMaster with path sanitization.

So the design is: **deploy `xclip`/`wl-paste` read shims; relay a tiny clipboard *request* up the
existing CBOR pipe; push image/text *bytes* out-of-band over the ControlMaster (reusing `clipupload`);
have the shim `cat` the file.** The same machinery carries notifications the other way: a Claude Code
hook fires `portald notify`, which relays a `Notify` envelope up the pipe; the Mac raises a native
notification. **No Mac HTTP daemon, no `RemoteForward`, no bearer token, no nonce server, no second
launchd job.** Plain `ssh <host>` then "just works" for Claude Code and opencode — image paste, text
paste, and notifications — with no special command.

**One genuine exclusion: Codex.** Codex links `arboard` and reads X11/Wayland **in-process**, so a
PATH shim cannot intercept it (see §8.1). That is the only cc-clip capability portal does not port.

---

## 2. End-to-end data flow (image / text paste)

```
 MAC (launchd: `portal run`)                          REMOTE dev box
 ┌───────────────────────────────┐                    ┌────────────────────────────────────────┐
 │ agentclient.Client.Run        │                    │ coding agent (Claude Code / opencode)    │
 │ engine.Run                    │                    │  user hits Ctrl+V → agent's own handler  │
 │ runClipHandler (own goroutine │                    │  spawns /bin/sh -c                        │
 │   + worker semaphore)         │                    │   "xclip -selection clipboard            │
 │  ├ internal/clip (read)       │                    │     -t image/png -o"                     │
 │  └ internal/clipupload (push) │                    │        │  (~/.local/bin first on PATH)     │
 └───────────────┬───────────────┘                    │        ▼                                  │
                 │                                     │  ~/.local/bin/xclip  (portal shim)        │
                 │  ControlMaster pipe (framed CBOR)   │   exec "$HOME/.cache/portal/portald       │
                 │  internal/protocol — CONTROL ONLY   │        clip image png"                    │
   (3) ClipReq   │                                     │        │                                  │
   {nonce,epoch, │◄──────── UP the pipe ───────────────┼── (2) "clip\timage\tpng\n" on cmd sock    │
    image,png} ──┤                                     │        ▲ portald clip dials cmd-<pid>.sock │
   (4) read clip │                                     │        │ (0600), blocks on read            │
   internal/clip │  (6) ClipResp{nonce,epoch,ok,sha}   │  (7) agent writes "ok\t<sha>\n" back      │
   (5) UPLOAD ───┤────────── DOWN the pipe ────────────┼──────► (8) portald clip:                  │
                 │                                     │          - reconstruct path FROM sha      │
   (5) bytes ────┼──── ssh exec (clipupload.Upload) ───┼──────►   $HOME/.cache/portal/clip/        │
   pushed FIRST, │     to ~/.cache/portal/clip/        │            clip-<sha>.png                 │
   awaited, THEN │     clip-<sha>.png  (0600 file)     │          - O_NOFOLLOW open, verify        │
   (6) ClipResp  │                                     │            size>0 + PNG magic (image)     │
                 │                                     │          - io.Copy → OWN stdout, exit 0   │
                 │                                     │   (9) shim's stdout = portald stdout      │
                 │                                     │       → agent ingests PNG → "[Image #1]"  │
                 └─────────────────────────────────────└──────────────────────────────────────────┘
```

**Critical ordering invariant:** the Mac uploads the bytes over the side channel and **confirms exit
0** *before* it emits `ClipResp{ok:true}`. By the time the agent receives `ok`, the file is guaranteed
present. Clipboard bytes **never** touch the CBOR frame. Text is identical, with a `text-<sha>.txt`
file and no PNG-magic check.

### 2.1 Notification data flow (agent → Mac)

```
 REMOTE: Claude Code Stop/Notification hook            MAC: runNotifyHandler
   fires ~/.local/bin/portal-notify-hook                  (own goroutine, sibling of runClipHandler)
     │ pipes hook JSON on stdin                             │
     ▼                                                      │ KindNotify event on dedicated channel
   portald notify --hook                                    ▼
     │ classify (internal/notify.ClassifyHook)            capability gate (feature.notify)
     │ → verified=true                                      │ + audit log
     ▼                                                      ▼
   notify\t<json>\n on cmd-<pid>.sock  ── UP the pipe ──► Notify envelope (Serve loop, sole writer)
                                                            │
                                                            ▼ osascript / terminal-notifier
                                                          native macOS notification
                                                          ([unverified] prefix when verified=false)
```

A generic caller (`portald notify --title … --body …`) takes the same path but is marked
`verified=false`, so the Mac prefixes the title with `[unverified] ` — mirroring cc-clip's trust model
(structured hook events are trusted; arbitrary JSON is not).

---

## 3. How the multi-MB byte payload crosses

`protocol.MaxFrameBytes = 1<<20` (1 MiB; the decoder rejects before allocating) vs. clipboard images
up to tens of MiB.

| Option | Verdict |
|---|---|
| (A) Bump `MaxFrameBytes` | **Rejected.** The cap is an anti-OOM property; the decoder pre-allocates by length, so a hostile/buggy peer could force large allocations. |
| (B) Chunk the payload over CBOR | **Rejected.** Head-of-line blocks the *control* pipe behind hundreds of KB, starving heartbeats; adds reassembly/flow-control surface. |
| **(C) Out-of-band side channel; CBOR pipe carries control only** | **Implemented.** |

The Mac pushes bytes over the **existing ControlMaster** via a short-lived `ssh exec` (what
`internal/clipupload.Upload` does, hardened with path validation), content-addressed to
`~/.cache/portal/clip/clip-<sha>.png` / `text-<sha>.txt`. The CBOR pipe carries only a tiny
nonce-correlated request/response. This keeps the control pipe small and low-latency (preserves
heartbeat responsiveness — see §5), reuses already-hardened code, and needs **no `MaxFrameBytes`
change**; the shim becomes a NUL-safe `cat` of a local file.

**Size cap.** The interactive-paste payload (image AND text) is capped at a hard limit
(`clipupload.MaxUploadBytes`, 8 MiB) before upload; larger clipboards fail fast and the shim falls
through. Screenshots and pasted text are well under this; multi-MB pastes are not worth stalling
heartbeats for.

---

## 4. Protocol & cmd-socket design

### 4.1 Protocol (`internal/protocol`)

`ProtoVersion` is **3**. The bump is not for wire compatibility (the fields are additive and old
decoders ignore unknown CBOR keys) but for **honest version negotiation**: the bootstrap SHA-match
re-uploads the agent from the same tree, and a loud mismatch beats a silent feature no-op if re-upload
is ever blocked. v2 added `ClipRequest`/`ClipResponse`; v3 added `Notify`.

`envelope.go` — agent → client / client → agent tagged-union fields:
```go
ClipRequest  *ClipRequest  `cbor:"clip_req,omitempty"`  // agent → client
ClipResponse *ClipResponse `cbor:"clip_resp,omitempty"` // client → agent
Notify       *Notify       `cbor:"notify,omitempty"`    // agent → client
```

`messages.go`:
```go
// ClipRequest — agent → client. A remote shim hit the cmd socket asking the
// Mac to read its clipboard. Nonce+Epoch correlate the response (§4.4).
// Kind ∈ {"targets","image","text"}; Format is "png" for images.
type ClipRequest struct {
    Nonce  uint64 `cbor:"n"`
    Epoch  uint64 `cbor:"e"`   // agent process identity; echoed back
    Kind   string `cbor:"kind"`
    Format string `cbor:"fmt,omitempty"`
}

// ClipResponse — client → agent. Answers a ClipRequest by (Nonce,Epoch).
//   targets: Has indicates content is available; Kind ∈ {"image","text"} tells
//            the agent WHICH target lines to advertise.
//   image:   OK=true with SHA only (NO path — agent reconstructs it; §7).
//   text:    OK=true with SHA of a side-channel text file (NOT inline; §7).
type ClipResponse struct {
    Nonce uint64 `cbor:"n"`
    Epoch uint64 `cbor:"e"`
    OK    bool   `cbor:"ok"`
    Has   bool   `cbor:"has,omitempty"`
    SHA   string `cbor:"sha,omitempty"`
    Kind  string `cbor:"k,omitempty"`   // "image" | "text" for a targets probe
    Err   string `cbor:"err,omitempty"`
}

// Notify — agent → client (v3). A remote event relayed for native display.
// Verified distinguishes a structured Claude Code hook (true) from an arbitrary
// caller (false → rendered "[unverified]" on the Mac).
type Notify struct {
    Title    string `cbor:"t"`
    Body     string `cbor:"b,omitempty"`
    Subtitle string `cbor:"sub,omitempty"`
    Urgency  uint8  `cbor:"u,omitempty"`
    Verified bool   `cbor:"v,omitempty"`
    Source   string `cbor:"src,omitempty"`
    Sound    string `cbor:"snd,omitempty"`
    Seq      uint64 `cbor:"seq,omitempty"`
}
```

`codec.go` `countEnvelopeFields` counts `ClipRequest`, `ClipResponse`, **and** `Notify` so the
one-non-nil-field-per-frame invariant still validates.

**Clipboard bytes are NOT in `ClipResponse` — and neither is the path.** The response carries only the
**SHA**; the agent reconstructs the single legal path itself (§7). `ClipResponse` and `Notify` are
always a few hundred bytes — the 1 MiB cap is never at risk from clipboard or notification content.

### 4.2 cmd-socket grammar (`internal/agent/server.go`)

The cmd socket is a tab-framed verb dispatcher, **0600 owner-only, default-deny**:

```
open\t<url>\n          → relay URL to the Mac (5s deadline)            "ok\n"|"no-client\n"|"rejected\n"|"dropped\n"
clip\ttargets\n        → which kind is available                       "ok\timage\n" | "ok\ttext\n" | "none\n"
clip\timage\tpng\n     → serve the Mac clipboard image                 "ok\t<sha>\n" | "none\n"
clip\ttext\n           → serve the Mac clipboard text (gated)          "ok\t<sha>\n" | "none\n"
notify\t<json>\n       → relay a notification to the Mac               "ok\n"|"no-client\n"|"dropped\n"|"rejected\n"
<anything else>        → "rejected\n"   (default-deny preserved)
```

A single bounded read is used (verbs are tiny; bytes never traverse the socket inbound). The notify
body is additionally bounded at `notifyBodyMax` (3072 bytes) and parsed as strict JSON (malformed /
oversized / title-less → `rejected\n`). Unknown/over-length tokens → `rejected\n`.

The `targets` reply carries the **canonical kind** (`image` / `text`) the Mac decided; `portald clip
targets <tool>` maps that kind to the **tool-specific target lines** the agent greps for (xclip →
`UTF8_STRING`/`TEXT`/`STRING`; wl-paste → `text/plain`; image → `image/png`). The agent stays
tool-agnostic.

### 4.3 Agent side (`server.go`)

- `clipReqCh chan *protocol.ClipRequest` (cap 8) and `notifyCh chan *protocol.Notify` (cap 8) alongside
  `openURLCh`; `clipWaiters map[uint64]chan *protocol.ClipResponse` guarded by `s.mu`; `clipSeq` and
  `notifySeq`; an `epoch` seeded **randomly per `Server`** (closes cross-generation nonce collision).
- `handleCmdConn`: split on `\t`; `open` → `handleOpenReq`; `clip` → `handleClipReq`; `notify` →
  `handleNotifyReq`; default → `rejected\n`.
- `handleClipReq`: immediate `none\n` if `!hasClient`; `maxInflightClip` (4) bound; register a waiter;
  **non-blocking** send to `clipReqCh`; `select` on the waiter / `clipTimeout` / ctx; map `resp.OK` to
  `ok\t…\n` (targets → the canonical kind; image/text → the SHA) or `none\n`; always delete the waiter.
- `handleNotifyReq`: bounded JSON parse, `no-client\n` if no Mac, else **non-blocking** send to
  `notifyCh` (full → `dropped\n`), `ok\n` on enqueue.
- The **Serve loop is the SOLE writer** of every agent → client frame: it has a `case` arm for
  `clipReqCh` (writes `ClipRequest`) and one for `notifyCh` (writes `Notify`, stamping `notifySeq`),
  each gated on `hasClient`, neither blocking Serve. `ClipRequest`/`Notify` use their own counters and
  **never touch `s.seq`** (the port-event staleness counter); a test asserts this.
- The frame dispatcher drops a `ClipResponse` whose `Epoch != s.epoch` (stale/cross-generation), else
  non-blocking-sends to the registered waiter.

### 4.4 Correlation

Correlation = `(Nonce, Epoch)`. `Epoch` is random per `Server` process, so a stale `ClipResponse`
arriving down a **new** pipe after a reconnect (where `clipSeq` reset to 0) is dropped on the epoch
check rather than mis-delivered. Notify is fire-and-forget (no response frame), so it needs no
correlation.

Every agent → client frame is a **blocking** `enc.Write` on the single Serve goroutine. The agent
**never** blocks the Serve loop on a clip response — it uses `clipWaiters` + the separate
`handleCmdConn` goroutine; the only things the Serve loop writes are the tiny `ClipRequest`/`Notify`
frames, which cannot meaningfully stall.

### 4.5 Timeout budget (all under `HeartbeatTimeout`=12s)

The entire paste round trip must complete well under 12s or the client declares heartbeat timeout and
reconnects. Strictly-decreasing budget:

| Layer | Value | Why |
|---|---|---|
| shim `portald clip` dial+read deadline | **13s** | largest; never gives up before the agent answers |
| agent cmd-socket deadline (clip only) | **11s** | < shim, > clipTimeout |
| agent `clipTimeout` (waiting on Mac) | **9s** | < socket deadline so agent always writes `none\n` first |
| Mac total: coerce + Upload | **≤ 8s** (`clipCoerceTimeout`) | osascript coercion capped at 5s; Upload ~3s |

`open` keeps its **5s** deadline; the longer deadline is **scoped strictly to clip**. The notify path
runs tight deadlines (`portald notify`: 2s dial / 4s read) because a hook runs synchronously in the
coding agent's process — a missed notification must never stall it.

### 4.6 Behavior when daemon/client unavailable

- `!hasClient` (Mac daemon down, or mid-reconnect): the agent replies `none\n` (clip) / `no-client\n`
  (notify) **immediately** → the shim/hook falls through cleanly.
- Daemon down / clipboard empty / mid-reconnect all collapse to "agent reports no content." `portal
  status` surfaces daemon connectivity and `portal doctor` (§10) verifies the whole path so this isn't
  a silent mystery.

---

## 5. Mac-side handlers — never block the demux

`publish()` drops on a full 64-cap `events` channel; `demuxLoop` runs the heartbeat watchdog inline.
If a handler did clipboard/notification work on the demux or engine goroutine, the reader would stall,
heartbeats would pile up unread, and the client would self-trigger a reconnect. So:

- `KindClipRequest` and `KindNotify` each have **dedicated buffered channels** off the agentclient,
  not the shared 64-cap `events` channel (a port-event burst can't evict a pending paste/notify).
- `runClipHandler` (sibling of `runOpenURLHandler` / `runNotifyHandler` in `run.go`) runs on its own
  goroutine with a **worker semaphore of 1** so two rapid pastes can't fork two osascript calls; while
  a read is in flight, additional requests answer `OK=false` immediately (→ shim falls through).
- On `KindClipRequest`:
  - `targets`: decide image-vs-text on the **Mac** (`HasImage` first, else `HasText`); for the chosen
    kind, **eagerly read + upload now** (overlapping the upload with user intent), cache `{sha,
    deadline ~10s}` per kind, reply `ClipResponse{Has:true, Kind, SHA}`. This collapses the
    TARGETS→fetch TOCTOU window to a single read. Text is gated (§7) and oversized text is skipped.
  - `image png` / `text`: serve from the probe cache if fresh; else read + `clipupload.Upload`. Reply
    `OK=true` **only after** Upload returns exit 0 with a validated path. Any error → `OK=false`.
- `runNotifyHandler`: on `KindNotify`, gate on `feature.notify`, audit, then raise a native
  notification via `terminal-notifier` (preferred when present) or `osascript -e 'display
  notification …'`. Title/body/subtitle/sound are sanitized for AppleScript injection (escape `\` and
  `"`, strip control bytes — stricter than cc-clip's bare `%q`). `verified=false` → `[unverified] `
  title prefix. Subtitle defaults to the host. A default sound is added only for the critical tier.

---

## 6. The shims, `portald clip`, and `portald notify`

### 6.1 `portald clip` (the sole arbiter of success)

`runClip` fans out over `cmd-*.sock` and **refuses (exit 1) if more than one distinct connected agent
answers** (multi-client safety, §7.3). Subcommands:

```
portald clip targets [xclip|wl-paste]  → prints the tool-specific target lines, exit 0; else exit 1
portald clip image png                 → cats the PNG to stdout, exit 0; else exit 1
portald clip text                      → cats the text to stdout, exit 0; else exit 1
```

**`portald clip image png` exits non-zero unless ALL hold** (closes the Claude `>${tmp} || …`
poisoning blocker — the `||` chain only advances on a *non-zero* exit, so a wrong-but-exit-0 shim
truncates `tmp` and skips the fallback):
1. socket reply is `ok\t<sha>\n` with `sha` matching `^[0-9a-f]{32}$`;
2. **path is reconstructed locally** as `$HOME/.cache/portal/clip/clip-<sha>.png` — the reply's SHA is
   the only input; no path from the wire (§7);
3. opened `O_NOFOLLOW`, a regular file under the 0700 dir, `Size() > 0`;
4. first 8 bytes are the PNG magic `\x89PNG\r\n\x1a\n`;
5. `io.Copy` completes with bytes copied == `Size()`.

`portald clip text` is identical minus the PNG-magic check (text has no magic; size ≥ 0 suffices), with
prefix `text-` and extension `.txt`. **Buffer-then-verify, never stream-then-discover:** read + verify
the whole file first; only then write to stdout, because Claude's `>` already truncated `tmp`.

Every non-`ok` socket reply (`none`, `no-client`, `rejected`, `dropped`, EOF, dial failure) maps to a
clean **exit 1** (also handles a new-shim/old-agent transition where `clip` hits the `rejected`
default). `portald clip targets` prints **byte-exact** target lines and nothing else (clean stderr).

### 6.2 `xclip` shim — `~/.local/bin/xclip`

A `/bin/sh` script (survives agent-SHA bumps via the stable `portald` symlink). It intercepts TARGETS
probes, image/png reads, and text reads, matching cc-clip's flag surface:

```
*"-t TARGETS"*"-o"*                      → portald clip targets xclip
*"-t image/png"*"-o"*                    → portald clip image png
# image/bmp and any non-png image fall through (format honesty — never hand PNG bytes mislabeled)
*"-t UTF8_STRING"*-o* | *-t TEXT*-o*
  | *-t STRING*-o* | *-t text/plain*-o*
  | *"-selection clipboard -o"*
  | *"-o -selection clipboard"*          → portald clip text
```

The Mac decides image-vs-text and gates text behind its capability + concealed-clipboard skip (§7); a
disabled/concealed/empty read answers `none` and falls through here. Every interception is `… && exit
0`; recursion is avoided by resolving the real xclip from a PATH with our own dir excluded (`grep -vxF`).
A headless box with no real xclip degrades to empty stdout = "no content."

### 6.3 `wl-paste` shim — `~/.local/bin/wl-paste`

wl-paste is opencode's *primary* image path (it tries `wl-paste -t image/png` before xclip), so it is
in scope with the same machinery, and it serves text too:

```
*--list-types*                       → portald clip targets wl-paste
*"--type image/png"* | *"-t image/png"*  → portald clip image png
*"--type image/"* | *"-t image/"*    → fall through (non-png image)
*"--type text/"* | *"-t text/"* | "" (EMPTY-ARGS)  → portald clip text
```

Bare `wl-paste` (EMPTY-ARGS) is the text-read form agents use; it routes to `clip text`.

### 6.4 `portald notify` + the Claude Code hook

`portald notify` relays a notification up the cmd socket (fanning out like `runOpen`, but **without**
multi-client refusal — a notification is broadcast-safe). Two modes:

```
portald notify --hook                  reads a Claude Code hook JSON object on stdin, classifies it via
                                       internal/notify.ClassifyHook, posts VERIFIED
portald notify --title T [--body B …]  posts a generic notification UNVERIFIED ([unverified] on the Mac)
```

`internal/notify.ClassifyHook` is a transport-free port of cc-clip's classifier: `notification` /
`permission_prompt` → "Tool approval needed" (urgency 2); `idle_prompt` → "Claude is idle" (urgency 1);
`stop` end-of-turn → "Claude finished" (urgency 0); other `stop` → "Claude stopped" (urgency 1);
unknown event → "Claude hook: <event>" (generic). Body is truncated to 280 runes on a UTF-8 boundary.

The hook itself is `~/.local/bin/portal-notify-hook` (reads stdin → `portald notify --hook`, always
exits 0). It is wired by merging `Stop` and `Notification` hook entries into `~/.claude/settings.json`,
each carrying the `PORTAL_MANAGED=1` ownership marker so the merge is idempotent and preserves
user-authored hooks (mirroring cc-clip's `CC_CLIP_MANAGED=1`). The merge is a python3 program (robust
JSON editing); if python3 is absent the script still deploys and the merge is skipped (graceful
degrade, not a corrupt settings file).

### 6.5 `xsel`: skipped. Neither target agent uses it for image reads.

---

## 7. Security

Portal's security posture **matches cc-clip's** — not more, not less — adapted to portal's transport.

### 7.1 cmd-socket trust boundary

The cmd socket returns Mac clipboard *contents down* to any same-uid process and raises notifications.
The realistic attacker is a compromised-but-same-uid process (malicious npm/pip/cargo postinstall,
backdoored IDE extension, supply-chain build script). The controls:

1. **Path pinning (closes arbitrary file read).** `portald clip` ignores any wire path; it
   reconstructs `$HOME/.cache/portal/clip/<prefix>-<sha><ext>` from a `^[0-9a-f]{32}$` SHA, opens
   `O_NOFOLLOW`, and verifies it is a regular file under the 0700 dir. A malicious socket reply cannot
   make `portald` cat `~/.ssh/id_ed25519`.
2. **File perms.** `clipupload.Upload` writes the file **0600 explicitly**.
3. **Capability gate (`internal/config`).** Per-feature toggle files `feature.clip-image`,
   `feature.clip-text`, `feature.notify` under the config dir, **re-read on every serve** (no daemon
   restart). Default **ON** for all three (cc-clip's posture); a user disables one by writing
   `off`/`false`/`0`/`no` into the file (or via `SetFeature`). The gate is checked **Mac-side** in
   `serveClipRequest` / `runNotifyHandler`: a disabled feature short-circuits to `OK=false`/`Has=false`
   (clip) or drops the event (notify), which the agent maps to `none`/`no-client` so the shim falls
   through. (The remote agent cannot know the Mac's config; the Mac-side short-circuit is the
   architecturally correct place — there is no remote-side toggle to spoof.)
4. **Concealed-clipboard skip (TEXT only).** Before serving text, the Mac probes
   `NSPasteboard.types` (via a cgo-free AppleScript-ObjC-bridge osascript one-liner — `clipboard info`
   does not surface reverse-DNS UTIs) for `org.nspasteboard.ConcealedType` / `TransientType`. Password
   managers set these; concealed/transient text is never served (→ `none`). cc-clip does not do this,
   but portal's text serve is a standing pull endpoint, so this is the minimum to not auto-exfiltrate
   secrets, and it is cheap. The probe fails *open* (the capability gate is the primary control).
5. **Audit log (`internal/audit`).** An append-only `<ConfigDir>/audit.log` (0600, RFC3339 +
   tab-separated, control-bytes stripped) records every served clip read (ts, host, kind, image
   sha / text length), every denied read (with reason: `disabled` / `concealed`), every notify (title,
   verified, urgency) and every denied notify, and every OpenURL. `portal doctor` can point at it.
6. **DoS bounds.** `maxInflightClip` waiters (≤4); clip socket deadline scoped to 11s; the notify body
   bounded at 3072 bytes and JSON-validated; unknown verbs → `rejected\n`; the notify relay is a
   non-blocking channel send (full → `dropped\n`) so it can't fork unbounded relays.

### 7.2 Bearer / nonce token: intentionally NOT ported (replace-by-equivalent)

This is the ONE cc-clip mechanism portal replaces by equivalent rather than porting verbatim. cc-clip
needs a clipboard bearer token AND a separate notification nonce because its transport is a **loopback
HTTP server** (and an SSH `RemoteForward` reverse tunnel) reachable by *any* local process. Portal's
transport is the **authenticated ssh ControlMaster pipe**, and the only local entry point is the
**0600 owner-only `cmd-<pid>.sock`**. The ssh channel *is* the network trust boundary; the socket perm
*is* the local ACL. Those two together provide exactly the network + local trust boundary cc-clip's
token and nonce stand in for, so portal adds neither. The compensating controls for "every same-uid
process is authorized by socket reachability" are the on-Mac capability gate + concealed skip + audit
(§7.1), not the 0600 perm alone.

### 7.3 Multi-client safety

If `portald clip` discovers **>1 distinct connected agent socket**, it refuses (exit 1 → fall through)
rather than risk answering user A's Ctrl+V from user B's Mac clipboard. Notifications are
broadcast-safe (no content cross-leak) and are NOT refused on multiple clients.

### 7.4 Injection surface — stated honestly

Portal trades shell-path-*injection* (the old proxy typed a path into the TTY) for
agent-content-*influence* (a same-uid attacker can feed the agent a forged PNG/text — a UI-spoofing
primitive, since the human acts on the rendered `[Image #1]`). Path-pinning + capability gate +
multi-client refusal + audit bound it. The notification path's AppleScript-injection vector is closed
by stricter-than-cc-clip sanitization (§5).

---

## 8. Scope decisions

### 8.1 Codex: the single genuine exclusion

Codex links `arboard` (x11rb + wl-clipboard-rs) and reads X11/Wayland **in-process** — a PATH shim
cannot intercept it. Feeding it headlessly would require cc-clip's approach: a real `Xvfb` + a pure-Go
CLIPBOARD selection-owner answering `SelectionRequest` (incl. INCR chunking), a system `Xvfb` package,
ideally passwordless sudo, and a `DISPLAY=127.0.0.1:N` TCP workaround for Codex's `/tmp/.X11-unix`
sandbox block. This undermines "just plain ssh," so Codex is **out of scope**. Claude Code + opencode
cover the bulk with zero X server.

> **Follow-up note (Codex):** if Codex support is ever revisited, the byte source already exists
> (`portald clip image png`); only the X11 selection-owner + Xvfb lifecycle would be net-new, gated
> behind an explicit opt-in flag.

### 8.2 OSC 52 write-filter: delegated to the terminal

The old PTY proxy's `osc52Filter` stripped remote→Mac clipboard *writes*. With the proxy retired
(`portal ssh` is now a thin `exec ssh` passthrough alias), portal **delegates OSC 52 write control to
the terminal**: keeping the terminal's OSC 52 clipboard-*write* disabled is documented as a hard
prerequisite (surfaced in `portal install` output) so a hostile remote can't write your clipboard via
OSC 52 and immediately read it back. A thin non-PTY OSC 52 stripper is possible but is not preferred
over the capability gate + audit.

---

## 9. Deployment & lifecycle

### 9.1 Shims are daemon-deployed and content-versioned

Shim deployment (`internal/clipshim.Ensure`) is idempotent and **daemon-driven**: it is called from
both `portal install` (first run) and the agentclient reconnect loop after `EnsureUploaded` + a
HelloAck SHA match. A `Version` content marker (`Installed by portal clip-shim v<N>`) makes the
steady-state case a cheap grep; bumping `Version` re-converges every user on the next reconnect with no
manual reinstall. `Ensure` deploys the xclip + wl-paste shims, the `portal-notify-hook` script + the
`~/.claude/settings.json` merge (best-effort — a notify-hook failure does not block the clip PATH
convergence or port forwarding), and the PATH-prepend block.

### 9.2 PATH ordering is the single make-or-break

The shim only fires if `~/.local/bin` resolves `xclip`/`wl-paste` **before** any real binary. `Ensure`
writes a **dedup-prepend** marker block (removes any existing `~/.local/bin` from PATH and re-adds it
at the front) into `~/.bashrc`, `~/.zshrc`, `~/.zshenv`, **and** `~/.profile` (tool managers
re-export PATH later, and the agent may run from a non-login/non-interactive context). `portal doctor`
(§10) runs `command -v` in a representative login+interactive shell and verifies the resolved path is
OUR shim — this check is the whole feature's make-or-break.

### 9.3 Backup/restore for package-managed binaries

Real `xclip` is usually `/usr/bin/xclip` (apt), so there's typically nothing at `~/.local/bin/xclip` to
back up. When `~/.local/bin/xclip` pre-exists and is *not* our shim, `deployShim` backs it up
**preserving type** (`cp -P`, so a symlink stays a symlink) once. Uninstall never touches `/usr/bin`.

### 9.4 Uninstall / prune scope

`clipshim.Remove` (called from uninstall and `bootstrap.PruneAll`) restores backups or removes the
xdg-open wrapper + xclip/wl-paste shims + `portal-notify-hook`, removes the `portald` symlink and the
env snippet, strips the portal-managed `Stop`/`Notification` entries from `~/.claude/settings.json`
(by the `PORTAL_MANAGED=1` marker, preserving user hooks), and strips the PATH-prepend marker block and
env-source line from every rc file.

### 9.5 Mixed-version matrix

`(new client + old agent)`, `(new shim + old agent)`, `(old shim + new agent)`. Because shims + agent
ship from one tree and shims are daemon-deployed, drift is bounded; the `ProtoVersion` bump makes a
true mismatch loud. `portald clip` mapping `rejected` → exit 1 covers the transient new-shim/old-agent
window. During a SHA-mismatch the `portald` symlink may briefly dangle; `[ -x "$_portald" ]` then fails
and the shim falls through cleanly (tested).

### 9.6 Install is loud + self-tests

Shim-install failure is surfaced prominently with named remediation. After deploy, `portal install`
runs the `portal doctor` self-test (§10) and prints a pass/fail verdict so a broken PATH order becomes
a loud failure, not a silent dead feature.

### 9.7 Non-Linux / BusyBox remotes

The shim scripts use `grep -vxF` / `tr` / `xargs -I{}` — fine on GNU coreutils; verify on BusyBox/Alpine
(common dev containers). The settings.json merge needs python3 (graceful skip otherwise). Remote OS
assumption: Linux + a reasonably POSIX shell.

---

## 10. Doctor / self-test (`portal doctor`)

`portal doctor` verifies the whole path over ssh and is run at the end of `portal install`:

1. **ssh master** is up (else everything downstream is moot; bail with a clear FAIL).
2. **PATH winner** — for each of `xclip`/`wl-paste`, run `command -v` in a representative
   login+interactive shell and confirm the resolved path carries the clipshim `Marker` (PASS), is a
   real binary winning ahead of the shim (FAIL, names the cause), or resolves to nothing (FAIL).
3. **Shim version** — compare the deployed `Version` against the embedded one (drift → WARN, still
   usable).
4. **portald present** + **agent verb support** — confirm `~/.cache/portal/portald` is executable and
   advertises the `clip` and `notify` subcommands.
5. **End-to-end smoke** — run the exact `portald clip targets xclip` the shim runs; content served →
   PASS; clean exit-1 (nothing on the Mac clipboard) → WARN (the round trip worked).

The report renders a loud `RESULT: PASS` / `RESULT: FAIL`, and any FAIL exits non-zero. `runDoctor`
takes a `Transport`, so it is unit-tested with a fake transport scripting each probe's reply (no live
box needed) — covering all-green, master-down, real-binary-wins-PATH, no-shim-resolves,
portald-missing, empty-clipboard-smoke, and version-drift.

---

## 11. What's kept / retired

| Component | Fate |
|---|---|
| `internal/clip/*` (Mac reader) | **Kept**, driven by `runClipHandler`; gained `IsConcealed()` (concealed-clipboard skip). |
| `internal/clipupload/*` | **Kept and reused** as the side channel (0600 file, 8 MiB cap, `UploadImage`/`UploadText`). |
| `internal/clipshim/*` | Deploys the xclip/wl-paste shims + notify hook + settings.json merge + PATH block. |
| `internal/notify/*` | Transport-free port of cc-clip's hook classifier. |
| `internal/audit/*` | Append-only security audit log. |
| `internal/config` feature toggles | Capability gate (`clip-image`/`clip-text`/`notify`, default on). |
| cmd socket / OpenURL / agent / agentclient / protocol | **Extended** (clip + notify verbs, `ClipRequest`/`ClipResponse`/`Notify`, ProtoVersion 3). |
| `cmd/portal/ssh.go` PTY proxy + `creack/pty`, `golang.org/x/term` | **Retired** — `portal ssh` is a thin `exec ssh` passthrough alias. |
| `osc52Filter` | **Retired with the proxy**; OSC 52 write control delegated to the terminal (§8.2). |

---

## 12. Manual verification checklist

1. `portal install <host>` → the self-test prints `RESULT: PASS`.
2. On the dev box: `command -v xclip` and `command -v wl-paste` (in a login+interactive shell) resolve
   to `~/.local/bin/...` carrying the clipshim marker.
3. Copy a screenshot on the Mac; in Claude Code on the dev box, Ctrl+V → `[Image #1]` ingests.
4. Copy text on the Mac; Ctrl+V in the agent → the text pastes.
5. Copy from a password manager (concealed) → text paste returns nothing (concealed skip); check
   `audit.log` shows a `concealed` denial.
6. `portal doctor` after copying an image/text → `smoke: clip targets` is PASS.
7. Trigger a Claude Code Stop/Notification hook → a native macOS notification appears; a verified hook
   has no `[unverified]` prefix, a `portald notify --title …` does.
8. Disable a feature (`echo off > ~/.config/portal/feature.clip-text`) → text paste falls through to
   the real binary; `audit.log` shows a `disabled` denial. Re-enable and confirm it works again.
9. `portal uninstall` → shims, hook, symlink, PATH block, env line, and the portal-managed
   settings.json hooks are all gone; any backed-up real binary is restored.
