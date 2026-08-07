# DESIGN: clipsync — push-based clipboard replication (v2)

Status: ACCEPTED (supersedes v1's pull-on-paste clip service)

## 1. Problem

v1 proxies clipboard READS at paste time: Ctrl+V → PATH shim → cmd socket →
agent → CBOR pipe → Mac reads NSPasteboard **via osascript** → uploads bytes
over a side-channel exec → answers with a SHA → shim cats the file. Every
paste crosses the network under a nested timeout tower (shim 13s > socket 11s
> agent 9s > Mac coerce+upload 8s > osascript 5s), the upload competes with
the RPC pipe for bandwidth (hence the 8 MiB cap), and every failure mode is a
silent fall-through. Image paste is broken in practice.

The fragility is structural, not parametric: **the network is inside the
paste's critical path**. Local paste "just works" because the content already
sits in the pasteboard before Ctrl+V.

## 2. Design: replicate, don't proxy

Invert the dataflow. The Mac daemon WATCHES the pasteboard; on change it
PUSHES the content to every connected box. At paste time the box-side read is
**local and instant** — exactly like pasting locally.

```
Mac                                          Box
────────────────────────────────            ──────────────────────────────
NSPasteboard changeCount poll                ~/.cache/portal/clip/
  (objc2, in-process, ~250ms)                  current.manifest   (0600)
      │ change detected                        blob-<sha256>.png  (0600)
      ├─ concealed? feature-gated? → skip      blob-<sha256>.txt
      ├─ read bytes IN-PROCESS (no osascript)
      ├─ small text → inline Update frame ────► portald applies atomically
      └─ image/large → Update manifest ───────► portald
             └─ blob stream (dedicated ssh     (content-addressed, dedup by
                channel, paced, no cap) ──────► sha; re-push skipped if the
                                                box already has the blob)

paste: agent runs `xclip -o -t image/png` → shim → portald reads LOCAL file.
No network. No timeout tower. Works offline with last-known content.
```

### 2.1 Mac side (watcher + publisher)

- **Watch**: poll `NSPasteboard.generalPasteboard().changeCount()` (~250ms,
  an int read via objc2 — microseconds; there is no macOS change
  notification). No osascript anywhere.
- **Classify + gate** per change: `org.nspasteboard.ConcealedType` /
  `TransientType` → never published (fail-closed on probe error);
  `clip-text` / `clip-image` capability gates re-read live (file-per-toggle,
  same as v1); per-box `clip = false` config disables publishing to that box.
- **Read in-process**: text as UTF-8; images converted to PNG via
  NSBitmapImageRep (no temp file, no 5s coercion cliff).
- **Publish**: text ≤ 256 KiB rides inline in the Update frame (under the
  1 MiB frame cap). Images and large text go content-addressed: the Update
  frame carries (kind, format, sha256, size, change_id); the bytes stream on
  a DEDICATED ssh channel (native-ssh, §DESIGN-native-transport) with flow
  control, so bulk transfer cannot starve protocol heartbeats. No 8 MiB
  cliff; cap is a generous sanity bound (256 MiB).
- **Dedup**: blobs are keyed by sha256; re-copying the same screenshot or
  reconnecting does not re-transfer bytes the box already confirmed.

### 2.2 Box side (portald store + local paste)

- Store: `~/.cache/portal/clip/` (0700): `current.manifest` (kind, format,
  sha, size, change_id, mac_change_count, received_at) + content-addressed
  blobs. Manifest updates are atomic (tmp+rename) and applied only after the
  blob is fully present and sha-verified — a paste can NEVER observe a
  half-transferred image.
- Shims (xclip/wl-paste/pbpaste/xsel) stay — they are how CLI agents read
  clipboards on Linux and the interception mechanism is proven — but they
  become trivial: `portald clip paste [--type …]` answers from the local
  store in microseconds. TARGETS answers from the manifest kind. No cmd-
  socket wait, no fall-through-on-timeout.
- **Staleness semantics = local clipboard semantics**: last-known content
  stays served across disconnects (a local clipboard keeps its content too).
  `portal status` and `portald clip paste --verbose` expose staleness
  (change_id + age) for diagnosis. An explicit Mac-side Clear propagates.
- GC: keep the current blob + N previous (paste-after-rapid-copy race);
  prune the rest.

### 2.3 Write side (box → Mac) — unchanged shape, better transport

wl-copy/pbcopy/xclip -i shims still relay UP (that direction is inherently
event-driven push): request/response frames with bytes on the same dedicated
blob channel machinery (no size cliff), same clip-write capability gate,
same security banner on the Mac. OSC 52 posture unchanged (keep terminal
OSC 52 write off; portal never proxies the terminal).

### 2.4 Protocol

Wire framing stays **v4**. clipsync is a NEW service negotiated through the
existing Hello/HelloAck `services` maps (this is exactly what S4 symmetric
advertisement was designed for):

- `clipsync@1` Mac→agent `update` — ClipSyncUpdate {change_id, kind, format,
  sha?, size?, inline?}; `clear` — ClipSyncClear {change_id}.
- `clipsync@1` agent→Mac `ack` — ClipSyncAck {change_id, have_blob} (have_blob
  =false ⇒ Mac streams the blob, then re-sends update; acks make sync state
  observable in `portal status`).
- Blob transfer: native-ssh exec channel `portald blob put <sha> <size>`
  (stdin = bytes, box verifies sha before install). Never inside CBOR frames.
- The v1 `clip` (read) service DIES. A Go portald that doesn't advertise
  `clipsync` simply gets no clipboard (portal install deploys the v2 portald
  anyway); everything else keeps working via v4 compatibility.

### 2.5 Security posture (better than v1, argued explicitly)

- v1's pull design handed the box a standing "read the Mac clipboard NOW"
  primitive — any same-uid box process could invoke it at any time, and the
  concealment probe ran per-request via a fragile osascript bridge. Push
  REMOVES the box-initiated read primitive entirely: the box receives only
  what the Mac watcher chose to publish, gated at publish time.
- Concealed/transient content is never published; the gate fails closed.
- Exposure window: published content persists on the box disk (0600, same
  uid) — equivalent to v1's side-channel files. Same-uid box compromise
  could always request the clipboard under v1; under v2 it sees only
  published (non-concealed, gated) content, and per-box `clip=false` cuts
  a sensitive box out entirely.
- Auditing: every publish/clear/write is an audit event with box name,
  kind, size, sha — replacing v1's per-request audit.

### 2.6 Failure modes

| failure | v1 behavior | v2 behavior |
|---|---|---|
| paste while disconnected | silent fall-through, empty paste | last-known content (like a local clipboard); staleness visible in status |
| large image | fails (8 MiB cap / 5s coercion) | background transfer, paced; paste waits only if racing the very first sync |
| slow link | paste times out silently | copy→paste race on a cold blob: shim answers when blob lands or falls through with a LOGGED reason |
| agent burst traffic | competes with paste on one pipe | dedicated blob channel; RPC heartbeats unaffected |
| concealed copy | per-request osascript probe (fail-closed) | never leaves the Mac (publish-time gate, fail-closed) |

The one new race: copy → instantly ssh + paste before the blob lands. The
shim handles it by waiting briefly on the in-flight transfer (manifest says
"incoming, change_id N"), so the common case is a short wait, not a failure.

## 3. Why this also kills the timeout tower

There is no paste-time RPC to budget. The only cross-machine deadline left
is the blob transfer itself, which is asynchronous, size-aware, and
observable — not a constant baked into two codebases.

## 4. Implementation order

1. `portal-proto`: clipsync payload types (done with this doc).
2. `portal-clip`: objc2 pasteboard watcher (changeCount poll, in-process
   reads, conceal gate).
3. `portald` (Rust): clip store + manifest + `clip paste` verbs + shim text
   updated to the local-read form.
4. Mac publisher task per box in the supervisor + blob channel over
   native-ssh.
5. Doctor: end-to-end "copy on Mac → manifest converges on box" check.
