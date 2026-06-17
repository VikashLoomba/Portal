# Portal Split-Daemon Architecture — Final Spec

This is the implementation contract. The protocol-angle proposal is the backbone (CBOR framing, three-stream model, Snapshot-as-reset semantics, full sequence-number ordering, three-state bootstrap manager). We graft from the other two: filename layout `agent-<sha>` (bootstrap angle), `internal/protocol` naming and FakeWatcher in-process test harness (agent-internals angle), 60s safety reconcile, and SHA-keyed cache cleanup.

CBOR is preserved over JSON: PortAdded is the hot path, the message set is small, and `cbor.DupMapKeyEnforcedAPF` makes desync fail closed. Git-SHA (not content-SHA) is the version token: both binaries ship from one tree, and `go:embed` baking ties them together.

---

## 1. Top-level architecture

```
                         macOS                         |              Linux dev box (amd64)
                                                       |
 +-----------------------------+                       |
 |        cmd/portal           |  user CLI (unchanged) |
 |  install/status/logs/allow  |                       |
 +--------------+--------------+                       |
                |                                      |
                v                                      |
 +-----------------------------+                       |
 |      internal/app           |  wires everything     |
 +--------------+--------------+                       |
       |        |          |                           |
       v        v          v                           |
 +---------+ +-------+ +--------------+                |
 | sshctl  | |bootst.| | agentclient  |                |
 |Transport| |Manager| |   .Client    |                |
 +----+----+ +---+---+ +------+-------+                |
      |          |            |                        |
      |   embed +cat>file     |  ssh -S sock host AGENT
      |   chmod+atomic mv     |  (single long-lived exec)
      |          |            |                        |
      |          |   stdin/stdout/stderr framed pipes  |
      |          v            v                        |
      |   ~/.cache/portal/agent-<sha>  ----[exec]---> +-------------------------+
      |                                               |   cmd/portald (linux)   |
      |                                               |   internal/agent.Server |
      v                                               |          |              |
 +---------+                                          |          v              |
 |forward. |  reconcile loop (event-driven):          |  internal/agent/watcher |
 |Engine   |   - on EngineEvent  -> Reconcile()       |  (NETLINK_SOCK_DIAG     |
 |         |   - 60s safety net  -> Reconcile()       |   periodic dump 75ms +  |
 |         |   - lsof master FDs = current truth      |   DESTROY multicast)    |
 +---------+                                          |          |              |
      |                                               |          v              |
      v                                               |   internal/agent/filter |
 ssh -S sock -O forward / -O cancel  <----------------+   (loopback / deny /    |
 (binds 127.0.0.1:N on Mac side ONLY)                 |    ephem / allow)       |
                                                      +-------------------------+
```

Hot path latency: bind+listen on remote → netlink dump tick (≤75ms) → CBOR frame on stdout → Mac decode → 50ms debounce → forward.Engine.Reconcile → ssh -O forward. Median ~80ms, p99 ~150ms. Removes via DESTROY multicast: <10ms.

---

## 2. Monorepo layout

### NEW files

| Path | Purpose |
|---|---|
| `cmd/portald/main.go` | linux/amd64 entry. Parses flags, builds NetlinkWatcher + agent.Server, runs Serve(ctx). |
| `internal/protocol/envelope.go` | `Envelope` tagged-union, `ProtoVersion` const, `MaxFrameBytes`, `FrameMagic`. |
| `internal/protocol/messages.go` | All wire structs: Hello, HelloAck, Subscribe, SubscribeAck, Snapshot, PortAdded, PortRemoved, Heartbeat, Ping, ReqSnap, Shutdown, Bye, AgentError, Port. |
| `internal/protocol/codec.go` | Length+magic framed CBOR Encoder/Decoder. |
| `internal/protocol/codec_test.go` | Roundtrip, oversize reject, bad-magic, partial-EOF tests. |
| `internal/agent/server.go` | Agent RPC top loop (linux build tag NOT required; consumes a Watcher interface). |
| `internal/agent/server_test.go` | Drives Server with a FakeWatcher and an in-process pipe pair; full handshake/snapshot/delta/shutdown coverage. |
| `internal/agent/filter.go` | Pure-Go: loopback predicate, deny set, ephemeral range, allow override. |
| `internal/agent/filter_test.go` | Table-driven coverage replacing `discover/discover_test.go` fixtures. |
| `internal/agent/watcher/watcher.go` | `Watcher` interface, `Event`, `Listen` types (no build tag). |
| `internal/agent/watcher/fake.go` | FakeWatcher for cross-platform tests of agent.Server. |
| `internal/agent/watcher/netlink_linux.go` | `//go:build linux` — INET_DIAG dump (75ms tick) + SKNLGRP_*_TCP_DESTROY multicast, diff bookkeeping. |
| `internal/agent/watcher/netlink_other.go` | `//go:build !linux` stub returning ErrUnsupported (lets darwin tests of agent.Server compile). |
| `internal/agent/watcher/netlink_linux_test.go` | `//go:build linux` integration: real bind/close, asserts add ≤200ms, remove ≤50ms. |
| `internal/bootstrap/embed.go` | `//go:embed agent/portald-linux-amd64` + `//go:embed agent/sha.txt` + accessors. |
| `internal/bootstrap/manager.go` | `Manager` with `EnsureUploaded(ctx) (path, error)` and `PruneOld(ctx) error`. |
| `internal/bootstrap/manager_test.go` | Fake Transport recording exec calls; asserts mkdir/stat/cat/chmod/mv ordering, mismatch-triggers-reupload, prune. |
| `internal/bootstrap/agent/.gitkeep` | Placeholder so `go:embed` directive doesn't fail before first `make agent`. |
| `internal/bootstrap/agent/portald-linux-amd64` | Build artifact. Gitignored. Produced by `make agent`. |
| `internal/bootstrap/agent/sha.txt` | Build artifact. Git SHA the agent was compiled from. Gitignored. |
| `internal/agentclient/client.go` | Mac-side client: ssh-exec lifecycle, handshake, demux, reconnect, coalescing. |
| `internal/agentclient/coalesce.go` | 50ms debounce of PortAdded/PortRemoved into one EngineEvent. |
| `internal/agentclient/events.go` | `EngineEvent` struct (kinds: Connected, Disconnected, SnapshotReplaced, Delta). |
| `internal/agentclient/client_test.go` | net.Pipe pair binding a real agent.Server (FakeWatcher) to a real Client; covers handshake, snapshot, delta, reconnect, Shutdown. |
| `internal/discover/agent.go` | `AgentDiscoverer` adapter implementing `RemoteDiscoverer`; pulls from `Client.Snapshot()`. |
| `cmd/portal/agent_version.go` | Hidden subcommand `portal agent-version` printing embedded SHA. |
| `Makefile` | Targets: `agent`, `portal`, `build` (default), `test`, `clean`. |
| `.gitignore` updates | `internal/bootstrap/agent/portald-linux-amd64`, `internal/bootstrap/agent/sha.txt`. |

### MODIFIED existing files

| Path | Change |
|---|---|
| `internal/sshctl/transport.go` | Add `ExecStream(ctx, argv...) (stdin io.WriteCloser, stdout, stderr io.ReadCloser, wait func() error, err error)` to `Transport` interface and `*SSH` impl. Existing methods unchanged. |
| `internal/sshctl/transport_test.go` | Tests for ExecStream using a real `cat` over the impl's exec.Cmd plumbing. |
| `internal/forward/engine.go` | New field `AgentEvents <-chan agentclient.EngineEvent`. `Run` rewritten to select on events + 60s safety ticker + ctx.Done. `Reconcile` body unchanged in spirit; reads desired from the discoverer (now O(memcpy)). |
| `internal/forward/engine_test.go` | New tests: scripted EngineEvents drive Reconcile; Disconnected does NOT cancel; Connected+SnapshotReplaced converges. |
| `internal/discover/discover.go` | Keep `RemoteDiscoverer` interface. Delete `SS` struct and `remote.sh` embed. |
| `internal/discover/discover_test.go` | Delete `ss`/`remote.sh` fixture coverage (moved to `internal/agent/filter_test.go`). |
| `internal/app/app.go` | `NewProd` constructs `bootstrap.Manager`, `agentclient.Client`, `discover.AgentDiscoverer`; wires Client.Events() into forward.Engine.AgentEvents. |
| `cmd/portal/run.go` | Spawns `Client.Run(ctx)` goroutine alongside `Engine.Run(ctx)`. |
| `cmd/portal/lifecycle.go` | `uninstall`/`stop` calls `Client.Shutdown(ctx)` before tearing down master. |
| `cmd/portal/inspect.go` | `status` adds `agent: pid=N sha=abcdef uptime=12m` from HelloAck/Heartbeat. `ports` reads from `Client.Snapshot()`. |
| `cmd/portal/allow.go` | After mutating allow file, push `Subscribe` (ResubscribeID++) via Client; agent emits new Snapshot. |
| `go.mod` | Add `github.com/fxamacker/cbor/v2`, `github.com/mdlayher/netlink`, `golang.org/x/sys/unix`. |

### DELETED files

| Path | Reason |
|---|---|
| `internal/discover/remote.sh` | ss-based remote discovery is replaced by the agent. |
| `internal/discover/ss.go` (and any `SS` struct file) | Same. |

---

## 3. Wire protocol

### Framing (decisive)

Every frame:

```
+--------+--------+--------+--------+--------+--------+========================+
|  magic 'P' 'F'  |   payload length (uint32, big-endian)   |  CBOR payload    |
+--------+--------+--------+--------+--------+--------+========================+
```

- **Magic**: `0x50 0x46` (`"PF"`). Two-byte sentinel preceding every length prefix. On magic mismatch the reader closes the connection — no in-band recovery (reconnect is fast).
- **Length**: uint32 big-endian, capped at `MaxFrameBytes = 1 << 20` (1 MiB). Decoder rejects oversized frames before allocating.
- **Payload**: CBOR (`github.com/fxamacker/cbor/v2`), encoded with `EncOptions{Sort: cbor.SortNone}`, decoded with `DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF}` so duplicate keys fail closed.
- **CBOR over JSON**: hot-path PortAdded compactness, faster decode, single-key-map tagged-union pattern keeps the Envelope cheap to extend.

### Three-stream model on one ssh exec

One `ssh -S <ControlPath> <host> ~/.cache/portal/agent-<sha> --proto-version=1` invocation per client lifetime. Reuses the existing ControlMaster — no new ssh per RPC.

- **stdin** (Mac → agent): framed CBOR commands.
- **stdout** (agent → Mac): framed CBOR events. Any non-framed byte aborts the client with `ErrProtocolDesync`. The agent's `os.Stdout` is wrapped in a guard `Writer` that panics on direct use; only the Encoder is permitted to write.
- **stderr** (agent → Mac launchd log): plain `slog.NewTextHandler` lines, byte-copied by the Mac to `~/Library/Logs/portal.log` with `agent: ` prefix. Never parsed.

### Envelope (tagged union)

```go
// internal/protocol/envelope.go
package protocol

const ProtoVersion uint32 = 1
const MaxFrameBytes = 1 << 20
var FrameMagic = [2]byte{'P', 'F'}

// Envelope is a 1-key CBOR map. Exactly one field is non-nil per frame.
type Envelope struct {
    // client → agent
    Hello     *Hello     `cbor:"hello,omitempty"`
    Subscribe *Subscribe `cbor:"subscribe,omitempty"`
    Ping      *Ping      `cbor:"ping,omitempty"`
    ReqSnap   *ReqSnap   `cbor:"req_snap,omitempty"`
    Shutdown  *Shutdown  `cbor:"shutdown,omitempty"`

    // agent → client
    HelloAck     *HelloAck     `cbor:"hello_ack,omitempty"`
    SubscribeAck *SubscribeAck `cbor:"subscribe_ack,omitempty"`
    Snapshot     *Snapshot     `cbor:"snapshot,omitempty"`
    PortAdded    *PortAdded    `cbor:"port_added,omitempty"`
    PortRemoved  *PortRemoved  `cbor:"port_removed,omitempty"`
    Heartbeat    *Heartbeat    `cbor:"heartbeat,omitempty"`
    AgentError   *AgentError   `cbor:"agent_error,omitempty"`
    Bye          *Bye          `cbor:"bye,omitempty"`
}
```

### Messages

```go
// internal/protocol/messages.go
package protocol

type Port struct {
    Port    uint16 `cbor:"port"`
    Family  uint8  `cbor:"fam"`  // 4 or 6
    Addr    string `cbor:"addr"` // "127.0.0.1" or "::1"
    InodeNS uint32 `cbor:"ns"`
}

// Client → agent. First frame on the connection.
type Hello struct {
    ProtoVersion  uint32 `cbor:"pv"`
    ClientGitSHA  string `cbor:"sha"`
    ClientPID     int    `cbor:"pid"`
    PollIntervalMs uint32 `cbor:"poll_ms"`  // 0 = agent default (75)
    WantDestroyMC bool   `cbor:"destroy_mc"`
}

// Agent → client. Sent after validating Hello.
type HelloAck struct {
    ProtoVersion uint32 `cbor:"pv"`
    AgentGitSHA  string `cbor:"sha"`
    AgentPID     int    `cbor:"pid"`
    Kernel       string `cbor:"kern"`
    BootID       string `cbor:"boot"`   // /proc/sys/kernel/random/boot_id
    EphemMin     uint16 `cbor:"emin"`
    EphemMax     uint16 `cbor:"emax"`
    NowUnixNano  int64  `cbor:"now"`
}

// Client → agent. Allow/deny lists. Sent after HelloAck and on every allow-file change.
type Subscribe struct {
    Deny             []uint16 `cbor:"deny"`
    Allow            []uint16 `cbor:"allow"`
    ExcludeEphemeral bool     `cbor:"exc_eph"`
    ResubscribeID    uint64   `cbor:"rsid"`  // monotonic; agent ignores rsid <= last-processed
}

// Agent → client. Confirms filter swap.
type SubscribeAck struct {
    ResubscribeID uint64 `cbor:"rsid"`
}

// Agent → client. Authoritative desired-set as of Seq. Sent immediately after every SubscribeAck.
// forward.Engine treats Snapshot as "reset to this".
type Snapshot struct {
    Seq         uint64 `cbor:"seq"`
    GeneratedAt int64  `cbor:"ts"`
    Ports       []Port `cbor:"ports"`
}

// Agent → client. Seq strictly > last Snapshot.Seq.
type PortAdded struct {
    Seq  uint64 `cbor:"seq"`
    Port Port   `cbor:"p"`
    At   int64  `cbor:"ts"`
}

type PortRemoved struct {
    Seq    uint64 `cbor:"seq"`
    Port   uint16 `cbor:"port"`
    Family uint8  `cbor:"fam"`
    At     int64  `cbor:"ts"`
    Source uint8  `cbor:"src"` // 1=dump-diff, 2=destroy-multicast
}

// Agent → client. Sent every 5s when no other agent→client frame has gone in that window.
type Heartbeat struct {
    Seq        uint64 `cbor:"seq"`
    UptimeNano int64  `cbor:"up"`
    Now        int64  `cbor:"now"`
}

type Ping     struct { Nonce uint64 `cbor:"n"` }   // agent responds with Heartbeat
type ReqSnap  struct {}                             // forces fresh full Snapshot
type Shutdown struct { Reason string `cbor:"reason,omitempty"` }
type Bye      struct { Reason string `cbor:"reason,omitempty"` }

type AgentError struct {
    Code  uint16 `cbor:"code"`
    Msg   string `cbor:"msg"`
    Fatal bool   `cbor:"fatal"`
}

const (
    CodeProtocolMismatch uint16 = 1
    CodeBadSubscribe     uint16 = 2
    CodeWatcherFailed    uint16 = 3
    CodeUnauthorized     uint16 = 4
    CodeInternalPanic    uint16 = 5
)
```

### Version handshake

1. Mac client sends `Hello{ProtoVersion: 1, ClientGitSHA: <baked>}`.
2. Agent validates `Hello.ProtoVersion == ProtoVersion`. Mismatch → `AgentError{Code: CodeProtocolMismatch, Fatal: true}` then exit(2). Mac surfaces ERROR and aborts (no ss-fallback per constraint #4).
3. Agent emits `HelloAck{ProtoVersion: 1, AgentGitSHA: <baked>, BootID, EphemMin, EphemMax}`.
4. Mac asserts `HelloAck.AgentGitSHA == bootstrap.EmbeddedSHA`. Mismatch is impossible in steady state (bootstrap stat'd `agent-<sha>` before exec); if it happens, log ERROR and reconnect after re-uploading.
5. Mac sends `Subscribe{Deny, Allow, ExcEphem, ResubscribeID: 1}`.
6. Agent emits `SubscribeAck{rsid: 1}` then `Snapshot{Seq: S0}`. Watcher starts.
7. Steady state: every PortAdded/PortRemoved carries `Seq > S0`. Heartbeat every 5s with current Seq.

`ProtoVersion` is bumped only on incompatible schema changes. There is no negotiation: both binaries ship from one tree; mismatch = stale upload = bootstrap re-upload.

### Ordering & sequence guarantees

- All agent→client frames except Hello/Subscribe Acks, AgentError, Bye carry `Seq`.
- `Seq` is monotonic uint64 per agent **process** (resets on agent restart).
- Watcher events serialize through one channel → single encoder goroutine under a write mutex → totally-ordered stdout.
- After SubscribeAck+Snapshot{Seq:S0}, every event has `Seq > S0`. Client drops stale events with `if ev.Seq <= snapshotSeq { drop }`.

### Backpressure

- Watcher→encoder channel cap = 256. If client stalls, watcher blocks on send.
- Adds and removes self-heal: the watcher does not advance its diff baseline until the encoder confirms the frame was written. A blocked send means the next dump tick recomputes from the un-advanced baseline.
- If channel stays full for >5s → `AgentError{Code: CodeInternalPanic, Fatal: true}` then exit(3). Client reconnects, gets fresh Snapshot, converges.

---

## 4. Agent

### Package layout

```
cmd/portald/
  main.go                       linux/amd64 entry; flags, signals, slog→stderr
internal/agent/
  server.go                     Server.Serve(): read-loop + write-loop + heartbeat
  filter.go                     Filter.Apply(): loopback / deny / ephem / allow
  watcher/
    watcher.go                  Watcher interface, Event, Listen (no build tag)
    fake.go                     FakeWatcher for tests
    netlink_linux.go            //go:build linux  — production impl
    netlink_other.go            //go:build !linux — ErrUnsupported stub
```

### Listen-watch mechanism (chosen)

**Periodic INET_DIAG netlink dump (TCP_LISTEN, AF_INET + AF_INET6) every 75ms, diffed in-memory; PLUS SKNLGRP_INET_TCP_DESTROY + SKNLGRP_INET6_TCP_DESTROY multicast for instant remove events.**

- Library: `github.com/mdlayher/netlink` for transport + multicast group join (`Conn.JoinGroup`); hand-rolled `inet_diag_req_v2` marshaling (~80 LOC) using `golang.org/x/sys/unix` constants.
- 75ms (not 100ms) gives median-add latency ~37ms, p99 ~75ms — comfortably under 100ms SLO.
- Kernel walks only the listen hash; payload ~80 bytes/socket; ~0.05% CPU at <100 listeners.
- Loopback filter is user-space: `addr[0]==127` (AF_INET) or `addr == ::1` (AF_INET6). 0.0.0.0 / :: explicitly rejected.
- Multicast handler filters on inode appearing in our last LISTEN snapshot to avoid noise from non-listen TCP closes.
- Re-reads `/proc/sys/net/ipv4/ip_local_port_range` every 5 minutes; a change re-applies Filter and pushes a Snapshot if the result diverges.

### Main loop pseudocode

```go
// internal/agent/server.go
func (s *Server) Serve(ctx context.Context) error {
    enc := protocol.NewEncoder(s.Out)  // mutex-guarded inside
    dec := protocol.NewDecoder(s.In)

    // 1. Wait for Hello.
    env, err := dec.Read()
    if err != nil || env.Hello == nil { return errProtocol }
    if env.Hello.ProtoVersion != protocol.ProtoVersion {
        enc.Write(&protocol.Envelope{AgentError: &protocol.AgentError{
            Code: protocol.CodeProtocolMismatch, Fatal: true,
        }})
        return errVersionMismatch
    }

    // 2. Send HelloAck.
    enc.Write(&protocol.Envelope{HelloAck: &protocol.HelloAck{
        ProtoVersion: protocol.ProtoVersion,
        AgentGitSHA:  s.AgentSHA,
        AgentPID:     os.Getpid(),
        BootID:       readBootID(),
        EphemMin:     s.Filter.EphemMin,
        EphemMax:     s.Filter.EphemMax,
        NowUnixNano:  s.Clock.Now().UnixNano(),
    }})

    // 3. Wait for first Subscribe; then start watcher.
    var seq uint64
    events, err := s.Watcher.Start(ctx)
    if err != nil { return err }

    cmdCh := readLoop(ctx, dec)         // command goroutine
    hb := time.NewTicker(s.HeartbeatInterval)
    defer hb.Stop()

    var lastFiltered map[uint16]protocol.Port

    for {
        select {
        case <-ctx.Done():
            enc.Write(&protocol.Envelope{Bye: &protocol.Bye{Reason: "ctx-cancel"}})
            return nil

        case cmd, ok := <-cmdCh:
            if !ok { return nil }       // stdin EOF = clean exit
            switch {
            case cmd.Subscribe != nil:
                if cmd.Subscribe.ResubscribeID <= s.lastRSID { continue }
                s.lastRSID = cmd.Subscribe.ResubscribeID
                s.Filter.SetAllowDeny(cmd.Subscribe.Allow, cmd.Subscribe.Deny, cmd.Subscribe.ExcludeEphemeral)
                enc.Write(&protocol.Envelope{SubscribeAck: &protocol.SubscribeAck{ResubscribeID: s.lastRSID}})
                seq++
                snap := s.Filter.Apply(s.Watcher.SnapshotNow())
                lastFiltered = indexByPort(snap)
                enc.Write(&protocol.Envelope{Snapshot: &protocol.Snapshot{
                    Seq: seq, GeneratedAt: s.Clock.Now().UnixNano(), Ports: snap,
                }})
            case cmd.ReqSnap != nil:
                seq++
                snap := s.Filter.Apply(s.Watcher.SnapshotNow())
                lastFiltered = indexByPort(snap)
                enc.Write(&protocol.Envelope{Snapshot: &protocol.Snapshot{Seq: seq, Ports: snap}})
            case cmd.Ping != nil:
                enc.Write(&protocol.Envelope{Heartbeat: &protocol.Heartbeat{Seq: seq, UptimeNano: s.uptime(), Now: s.Clock.Now().UnixNano()}})
            case cmd.Shutdown != nil:
                enc.Write(&protocol.Envelope{Bye: &protocol.Bye{Reason: cmd.Shutdown.Reason}})
                return nil
            }

        case ev := <-events:
            // Apply filter; only emit if filtered set actually changes.
            after := s.Filter.AcceptOne(ev.Listen)
            if !after { continue }
            switch ev.Kind {
            case watcher.KindAdd:
                if _, dup := lastFiltered[ev.Listen.Port]; dup { continue }
                seq++
                p := toWirePort(ev.Listen)
                lastFiltered[p.Port] = p
                enc.Write(&protocol.Envelope{PortAdded: &protocol.PortAdded{Seq: seq, Port: p, At: ev.At.UnixNano()}})
            case watcher.KindRemove:
                if _, ok := lastFiltered[ev.Listen.Port]; !ok { continue }
                seq++
                delete(lastFiltered, ev.Listen.Port)
                enc.Write(&protocol.Envelope{PortRemoved: &protocol.PortRemoved{
                    Seq: seq, Port: ev.Listen.Port, Family: ev.Listen.Family,
                    At: ev.At.UnixNano(), Source: ev.Source,
                }})
            }

        case <-hb.C:
            enc.Write(&protocol.Envelope{Heartbeat: &protocol.Heartbeat{Seq: seq, UptimeNano: s.uptime(), Now: s.Clock.Now().UnixNano()}})
        }
    }
}
```

### Filtering pipeline

```
raw netlink rows
    │
    ▼
Watcher.netlink_linux.go: TCP_LISTEN-only dump
    │  (server-side filter via idiag_states)
    ▼
agent.Filter.Apply():
    1. Loopback predicate: AF_INET addr[0]==127, OR AF_INET6 addr==::1.
    2. Allow-override (allow always wins; bypasses 3 and 4).
    3. Deny set (config.go-supplied: 22,25,53,...).
    4. Ephemeral exclusion if ExcludeEphemeral && port in [EphemMin, EphemMax].
    ▼
filtered []protocol.Port  ──► Snapshot/PortAdded/PortRemoved
```

### Graceful shutdown

`agent.Server.Serve` returns on:
- `ctx.Done()` (SIGTERM/SIGINT) — emit `Bye{Reason: "ctx-cancel"}`, drain buffered writer ≤500ms, return nil → exit 0.
- stdin EOF (Mac client died) — return nil → exit 0.
- `Shutdown` frame received — emit `Bye{Reason: <reason>}`, return nil → exit 0.
- Watcher unrecoverable — emit `AgentError{Code: CodeWatcherFailed, Fatal: true}` → exit 3.
- Protocol violation — emit `AgentError{Code: CodeProtocolMismatch|CodeBadSubscribe, Fatal: true}` → exit 2.

### Logging

- `log/slog` with `slog.NewTextHandler(os.Stderr, ...)` only.
- Format: `time=... level=info msg=... key=val`.
- `os.Stdout` is wrapped at startup in a guard `*panicWriter` — only `protocol.Encoder` calls `Write`; anything else panics, which routes via stderr (not stdout — runtime panic prints to stderr).
- Mac client byte-copies stderr lines into `~/Library/Logs/portal.log` with `agent: ` prefix.
- No on-disk log on the remote — zero footprint between sessions.

### Resource limits

- `GOGC=50`, `debug.SetMemoryLimit(32 << 20)`.
- Steady RSS ≤ 8 MB on a typical dev box.

---

## 5. Bootstrap

### Build-time SHA injection

```makefile
# Makefile
GIT_SHA       := $(shell git rev-parse HEAD)
AGENT_PATH    := internal/bootstrap/agent/portald-linux-amd64
SHA_PATH      := internal/bootstrap/agent/sha.txt

.PHONY: build agent portal test clean
build: portal

agent:
    @mkdir -p internal/bootstrap/agent
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w -X main.gitSHA=$(GIT_SHA)" \
        -o $(AGENT_PATH) ./cmd/portald
    @printf "%s" "$(GIT_SHA)" > $(SHA_PATH)

portal: agent
    go build -trimpath -ldflags "-X github.com/vikashl/portal/internal/bootstrap.gitSHA=$(GIT_SHA)" \
        -o portal ./cmd/portal

test: agent
    go test ./...

clean:
    rm -f portal $(AGENT_PATH) $(SHA_PATH)
```

The agent's `main.gitSHA` is reported in HelloAck. The Mac's `bootstrap.gitSHA` is the embedded SHA used to name the cache file. Both are the **same** git SHA from the same `git rev-parse HEAD` invocation (Make sets `GIT_SHA` once).

### Embed

```go
// internal/bootstrap/embed.go
package bootstrap

import _ "embed"

//go:embed agent/portald-linux-amd64
var embeddedAgent []byte

//go:embed agent/sha.txt
var embeddedSHARaw string

// gitSHA is set via -ldflags as a redundant compile-time pin (must equal sha.txt).
var gitSHA string

func EmbeddedSHA() string { return strings.TrimSpace(embeddedSHARaw) }
func EmbeddedAgent() []byte { return embeddedAgent }
```

`init()` panics if `len(embeddedAgent) == 0` (build forgot `make agent`) or `EmbeddedSHA() != gitSHA` (drift between sha.txt and ldflags — should be impossible from Make, but cheap insurance).

### Upload protocol over the existing ControlMaster

```go
// internal/bootstrap/manager.go
type Manager struct {
    T   sshctl.Transport
    Log *slog.Logger
}

const remoteDir = "~/.cache/portal"

// EnsureUploaded is idempotent.
func (m *Manager) EnsureUploaded(ctx context.Context) (string, error) {
    sha := EmbeddedSHA()
    remotePath := remoteDir + "/agent-" + sha
    expectedSize := strconv.Itoa(len(embeddedAgent))

    // 1. Probe via sshctl.Transport.Exec (one-shot).
    out, _, err := m.T.Exec(ctx, "test -x "+remotePath+" && stat -c %s "+remotePath+" || echo MISSING")
    if err == nil && strings.TrimSpace(out) == expectedSize {
        return remotePath, nil
    }

    // 2. Upload atomically: cat > tmp ; chmod 0755 ; mv.
    script := fmt.Sprintf(
        `mkdir -p %s && cat > %s.tmp.$$ && chmod 0755 %s.tmp.$$ && mv %s.tmp.$$ %s`,
        remoteDir, remotePath, remotePath, remotePath, remotePath,
    )
    _, _, err = m.T.ExecWithStdin(ctx, embeddedAgent, "bash", "-c", script)
    if err != nil {
        return "", fmt.Errorf("agent upload failed: %w", err)
    }

    // 3. Best-effort prune of older agent-* files.
    _, _, _ = m.T.Exec(ctx, fmt.Sprintf(`ls %s/agent-* 2>/dev/null | grep -v %s | xargs -r rm -f`, remoteDir, remotePath))
    return remotePath, nil
}
```

`ExecWithStdin` is the stdin-feeding variant of the existing `Exec` (one-shot, NOT `ExecStream`). Reuses ControlMaster — single ssh invocation, no scp, no sftp dependency.

**Why exec-cat over sftp?** Sftp requires the remote sshd to advertise the subsystem; exec-cat works on every host where ssh works. Atomic via tmp+rename. PID-suffixed tmp (`$$`) avoids collisions if two Macs upload concurrently to the same dev box.

### On-remote layout

```
~/.cache/portal/                   (mode 0700, mkdir -p)
├── agent-<sha1>                   (mode 0755, current)
├── agent-<sha2>                   (mode 0755, older — pruned best-effort)
└── ...
```

### Update-on-mismatch flow

```
Mac client startup
    │
    ▼
bootstrap.Manager.EnsureUploaded(ctx)
    │  stat ~/.cache/portal/agent-<EmbeddedSHA> matches expected size?
    ├── yes ──► return remotePath
    └── no  ──► upload via cat > tmp + chmod + mv ; prune ; return remotePath
    ▼
agentclient.Client.Run(ctx)
    │  Transport.ExecStream(ctx, remotePath, "--proto-version=1")
    │  Hello → HelloAck (assert AgentGitSHA == EmbeddedSHA)
    │     mismatch (e.g. tampered binary) → close exec, log ERROR, EnsureUploaded again, retry
    ▼
steady state
```

A `portal upgrade` (= `make build && portal install`) yields a new Mac binary with a new EmbeddedSHA; on next launchd restart the bootstrap probes, sees mismatch, uploads `agent-<newsha>`, prunes the old one. Concurrent old/new clients during upgrade are safe — they exec different SHA-keyed files.

### Cleanup

`PruneOld` runs after every successful Ensure. On `portal uninstall`: also `rm -rf ~/.cache/portal/agent-*` over a single Exec. Failures are logged and ignored.

---

## 6. Local client (event-driven Engine)

### `agentclient.Client`

```go
// internal/agentclient/client.go
type Client struct {
    Transport sshctl.Transport
    Bootstrap *bootstrap.Manager
    Log       *slog.Logger

    cfgMu  sync.Mutex
    deny   []uint16
    allow  []uint16
    excEph bool
    rsid   uint64

    snapMu     sync.RWMutex
    snapSeq    uint64
    snapPorts  map[uint16]protocol.Port

    events chan EngineEvent  // buffered, 64
}

func New(t sshctl.Transport, b *bootstrap.Manager, log *slog.Logger) *Client { ... }

// Run blocks until ctx.Done(); reconnects internally with backoff.
func (c *Client) Run(ctx context.Context) error

// Snapshot returns the cached Snapshot; ok=false until first SubscribeAck arrives.
func (c *Client) Snapshot() (seq uint64, ports []uint16, ok bool)

// Subscribe pushes a new filter; safe to call any time after Run started.
func (c *Client) Subscribe(deny, allow []uint16, excludeEphemeral bool) error

// Events returns the coalesced event channel for forward.Engine.
func (c *Client) Events() <-chan EngineEvent

// RequestSnapshot sends ReqSnap.
func (c *Client) RequestSnapshot(ctx context.Context) error

// Shutdown sends Shutdown, awaits Bye, cancels Run.
func (c *Client) Shutdown(ctx context.Context) error
```

```go
// internal/agentclient/events.go
type EngineEventKind uint8
const (
    KindConnected EngineEventKind = iota + 1
    KindDisconnected
    KindSnapshotReplaced
    KindDelta
)

type EngineEvent struct {
    Kind     EngineEventKind
    Snapshot []uint16  // populated when Kind=SnapshotReplaced
    Added    []uint16  // populated when Kind=Delta
    Removed  []uint16  // populated when Kind=Delta
    Err      error     // populated when Kind=Disconnected
}
```

50ms debounce coalesces bursts (`docker compose up`) into one Delta event.

### `discover.AgentDiscoverer`

```go
// internal/discover/agent.go
type AgentDiscoverer struct {
    C        *agentclient.Client
    lastAllow []int
    lastDeny  []int
}

func NewAgent(c *agentclient.Client) *AgentDiscoverer { return &AgentDiscoverer{C: c} }

// DesiredPorts returns the agent's cached Snapshot. If allow/deny differ from
// the last Subscribe, push a new Subscribe (rsid++) before returning.
func (a *AgentDiscoverer) DesiredPorts(ctx context.Context, deny, allow []int) ([]int, error) {
    if !slices.Equal(allow, a.lastAllow) || !slices.Equal(deny, a.lastDeny) {
        if err := a.C.Subscribe(toU16(deny), toU16(allow), true); err != nil {
            return nil, err
        }
        a.lastAllow, a.lastDeny = slices.Clone(allow), slices.Clone(deny)
    }
    _, ports, ok := a.C.Snapshot()
    if !ok { return nil, ErrNoSnapshot }
    return toInt(ports), nil
}

var _ RemoteDiscoverer = (*AgentDiscoverer)(nil)
```

### `forward.Engine.Run` — event-driven shape

```go
// internal/forward/engine.go
type Engine struct {
    // ... existing fields ...
    AgentEvents <-chan agentclient.EngineEvent  // NEW
    SafetyInterval time.Duration                // 60 * time.Second
}

func (e *Engine) Run(ctx context.Context) error {
    // Initial reconcile in case agent already has a snapshot.
    _ = e.Reconcile(ctx)

    var debounce *time.Timer
    fire := func() {
        if debounce != nil { debounce.Stop() }
        debounce = time.AfterFunc(50*time.Millisecond, func() {
            if err := e.Reconcile(ctx); err != nil {
                e.Log.Warn("reconcile error", "err", err)
            }
        })
    }

    safety := time.NewTicker(e.SafetyInterval)
    defer safety.Stop()

    for {
        select {
        case <-ctx.Done():
            return nil

        case ev := <-e.AgentEvents:
            switch ev.Kind {
            case agentclient.KindConnected, agentclient.KindSnapshotReplaced, agentclient.KindDelta:
                fire()
            case agentclient.KindDisconnected:
                // KEEP existing forwards in place (matches old "transient blip" semantics).
                e.Log.Warn("agent disconnected; preserving forwards", "err", ev.Err)
            }

        case <-safety.C:
            // Insurance net: catches master-side forward drops (user kills master,
            // someone runs ssh -O cancel out of band, etc.) that the agent can't see.
            fire()
        }
    }
}
```

`Reconcile` is unchanged in spirit:
1. `desired = e.RD.DesiredPorts(ctx, deny, allow)` — now O(memcpy) from the cached Snapshot.
2. `current = e.Master.MasterForwards(ctx)` — still `lsof` against the ControlMaster (ground truth, untouched).
3. `add = desired - current; cancel = current - desired` — same set math.
4. For each add/cancel, call `Transport.Forward(ctx, port)` / `Transport.Cancel(ctx, port)`.

This preserves the stateless reconcile invariant: the engine never trusts its own memory of what it asked for; it always re-derives `current` from the live ControlMaster on every desired-set delta.

### `internal/sshctl/transport.go` — new method

```go
type Transport interface {
    // ... existing methods unchanged ...

    // ExecStream spawns ssh -S sock host argv... with live pipes.
    // Caller closes stdin to signal EOF; wait() returns ssh's exit error after streams close.
    ExecStream(ctx context.Context, argv ...string) (
        stdin io.WriteCloser,
        stdout io.ReadCloser,
        stderr io.ReadCloser,
        wait func() error,
        err error,
    )
}
```

Implemented via `exec.CommandContext("ssh", "-S", sock, sshOpts..., host, argv...)` with explicit `StdinPipe`/`StdoutPipe`/`StderrPipe` (unbuffered, unlike the existing `Exec`).

---

## 7. Failure modes

| Failure | Detection | Response |
|---|---|---|
| **Agent upload fails** (no disk, sftp/cat denied, network) | `bootstrap.Manager.EnsureUploaded` returns error | `Client.Run` surfaces `agent upload failed: <err>` to stderr + ~/Library/Logs/portal.log; exits with non-zero. launchd restarts; same failure logged. No ss-fallback (constraint #4). User is expected to `git revert` if a regression. |
| **Agent crashes mid-stream** (panic, OOM, kill -9) | ssh exec child exits non-zero; client sees stdout EOF + non-zero `wait()` | `Client` emits `EngineEvent{Kind: Disconnected, Err}`; engine **keeps existing forwards** (old behavior preserved); reconnect with backoff 500ms→1s→2s→5s→10s. New connection re-runs bootstrap, gets new Snapshot, engine reconciles to it. Last agent stderr lines reach the launchd log. |
| **SSH master drops** (network blip, server reboot) | ssh exec child exits; ControlMaster either rebuilt by `EnsureMaster` or fails | Client treats as Disconnected; if EnsureMaster fails, backoff retries; engine holds forwards through transient (≤30s) drops. After remote reboot, BootID in next HelloAck differs from cached → log INFO "remote rebooted"; old `agent-<sha>` was wiped from `/tmp` if there, but `~/.cache` survives, so usually no re-upload needed. |
| **SHA mismatch** (manual tamper, partial upload) | `HelloAck.AgentGitSHA != EmbeddedSHA` after `EnsureUploaded` succeeded | Client logs ERROR, force-deletes `agent-<sha>` on remote, calls `EnsureUploaded` again, reconnects. If it happens twice in a row → fatal: `bootstrap integrity check failed`, exit non-zero. |
| **ProtoVersion mismatch** (impossible if bootstrap is correct, but defensive) | Agent sends `AgentError{Code: CodeProtocolMismatch, Fatal: true}` | Client logs ERROR, treats as bootstrap failure, re-uploads + retries once. Persistent → fatal exit. |
| **User changes allowlist locally** (`portal allow N`, edit `~/.config/portal/allow`) | `cmd/portal/allow.go` modifies file; existing `app.Cfg` reload triggers via fsnotify (or 1s ticker fallback) | `discover.AgentDiscoverer.DesiredPorts` next call sees `allow` differs → calls `Client.Subscribe` (rsid++); agent emits new `SubscribeAck` + `Snapshot{Seq: S}`; client emits `EngineEvent{Kind: SnapshotReplaced}`; engine reconciles. End-to-end latency: <100ms. |
| **Kernel doesn't allow netlink dump** (hardened ns, missing CAP) | Agent self-test at startup (binds 127.0.0.1:0, dumps, asserts visible) | `AgentError{Code: CodeWatcherFailed, Fatal: true}` then exit 3. Mac surfaces `agent self-test failed: netlink dump returned no loopback`. No ss-fallback. |
| **Stdout pollution** (rogue lib writes to os.Stdout) | Guard `*panicWriter` panics on direct Write | Panic routed to stderr (runtime); ssh exec child exits; client treats as Disconnected. Loud failure beats silent desync. |
| **Heartbeat timeout** (>10s no agent→client frame) | Client's read-deadline timer | Kill ssh child; reconnect (same path as Disconnected). |
| **Truncated frame on read** | Decoder returns ErrUnexpectedEOF or magic mismatch | Client closes stream, treats as Disconnected. |
| **Subscribe race on reconnect** (rsid 42 in flight, retry sends 43) | Agent compares `rsid <= s.lastRSID` | Drops stale Subscribe silently. Latest Subscribe wins. |

---

## 8. Subcommand impact

| Subcommand | Status | Change |
|---|---|---|
| `portal install` | **needs change** | After writing plist, runs `bootstrap.EnsureUploaded` once eagerly to surface upload errors at install-time rather than first `run`. Plist itself unchanged (same launchd LaunchAgent, same env). |
| `portal uninstall` | **needs change** | Calls `Client.Shutdown(ctx)` → waits for Bye, then tears down master, then `rm -rf ~/.cache/portal/agent-*` over Exec. |
| `portal run` | **needs change** | RunE now spawns `Client.Run(ctx)` alongside `Engine.Run(ctx)` (errgroup). Removes 10s ticker. |
| `portal status` | **needs change** | Reads agent fields from cached HelloAck/Heartbeat. Adds two lines: `agent: pid=N sha=<8> uptime=12m bootid=<8>`. Reads ports from `Client.Snapshot()` instead of ssh ss. |
| `portal logs` | **works** | Unchanged — still tails `~/Library/Logs/portal.log`. Agent stderr is now interleaved here. |
| `portal allow N` / `portal unallow N` | **needs change** | After mutating allow file, calls `Client.Subscribe(deny, allow, true)` directly to push immediately (don't wait for fsnotify). |
| `portal allowed` | **works** | Reads local allow file only. No change. |
| `portal host <hostname>` | **needs change** | After updating host file and rebuilding ControlMaster, the existing exec child dies with the old master → Client reconnects to new host; new bootstrap uploads agent there. |
| `portal once` | **needs change** | Calls `Client.RequestSnapshot(ctx)` then runs one Reconcile. |
| `portal inspect` | **works** | Unchanged. |
| `portal agent-version` | **NEW (hidden)** | Prints `bootstrap.EmbeddedSHA()` for diagnostics. |

---

## 9. Implementation order

Each step compiles and tests independently; CI green at every step.

| Step | Scope | Tests added |
|---|---|---|
| **1. Wire protocol package** | `internal/protocol/{envelope,messages,codec}.go` + `codec_test.go`. No callers yet. | Roundtrip per message variant; oversize reject; bad-magic; partial-EOF EOF behavior. |
| **2. Watcher interface + FakeWatcher** | `internal/agent/watcher/{watcher.go, fake.go}`. No netlink yet. | Compile-only on darwin. |
| **3. Filter** | `internal/agent/filter.go` + `filter_test.go`. Pure function. | Table-driven: loopback, deny, ephem, allow-override, IPv4+IPv6. |
| **4. agent.Server** | `internal/agent/server.go` + `server_test.go`. Uses FakeWatcher + bytes.Buffer pipes. | Full handshake/snapshot/delta/shutdown coverage; protocol-violation paths. |
| **5. Netlink watcher (linux)** | `internal/agent/watcher/netlink_{linux,other}.go` + `netlink_linux_test.go`. | Linux-only integration: real bind/listen → add ≤200ms; close → remove ≤50ms via DESTROY mc. |
| **6. cmd/portald + Makefile target** | `cmd/portald/main.go` + Makefile `agent` target. Cross-compile works. | `make agent` produces a runnable binary; `./portald --help` exits 0. |
| **7. sshctl.ExecStream** | Add to `Transport` + `*SSH`. | Spawns real `cat` over impl; bidirectional streaming; ctx cancel kills child. |
| **8. bootstrap.Manager** | `internal/bootstrap/{embed.go, manager.go}` + `manager_test.go`. | Fake Transport recording Exec calls: stat-hits-skip, stat-misses-uploads, prune order. |
| **9. agentclient.Client** | `internal/agentclient/{client,coalesce,events}.go` + `client_test.go`. | net.Pipe-paired with real agent.Server (FakeWatcher); handshake, snapshot, delta, reconnect-on-EOF, Shutdown handshake. |
| **10. discover.AgentDiscoverer + delete SS** | `internal/discover/agent.go`; delete `discover/remote.sh`, `SS` struct. | AgentDiscoverer DesiredPorts pulls from cached snapshot; pushes Subscribe on allow change. |
| **11. forward.Engine.Run rewrite** | Add `AgentEvents` field; replace ticker with select. | Scripted EngineEvents drive Reconcile; Disconnected does NOT cancel; 60s safety net fires. Existing reconcile-correctness tests still pass with a fake Discoverer. |
| **12. app.NewProd + cmd/portal wiring** | Wire bootstrap, agentclient, AgentDiscoverer in `internal/app/app.go`. Update `run`, `status`, `allow`, `lifecycle`, `host`. Add `agent-version`. | End-to-end test in `cmd/portal/...` against a fake remote (in-process agent.Server piped through a fake Transport). |
| **13. Makefile `build` target final wiring** | Default `make build` runs `agent` then `portal` with matching `-X gitSHA`. | `make build && ./portal agent-version` prints the git SHA. |

---

## 10. Migration plan

The goal: build the new Mac binary, validate end-to-end against the live dev box without disturbing the running daemon, then atomically swap.

### Phase A — Build new binary, leave existing service running

```bash
git checkout -b split-daemon
# ... implement steps 1–13 ...
make build              # produces ./portal with embedded agent
```

### Phase B — Out-of-band validation against live dev box

Use `PORTAL_CONFIG_DIR` and `PORTAL_SOCK` (already-supported overrides in `cmd/portal/root.go`) to run a parallel instance with a separate ControlMaster socket and config dir, so the live launchd-managed daemon is untouched.

```bash
mkdir -p /tmp/portal-staging/{config,run,logs}
cp ~/.config/portal/host ~/.config/portal/allow /tmp/portal-staging/config/

PORTAL_CONFIG_DIR=/tmp/portal-staging/config \
PORTAL_SOCK=/tmp/portal-staging/run/master.sock \
PORTAL_LOG_FILE=/tmp/portal-staging/logs/portal.log \
./portal run &
STAGING_PID=$!

# In another terminal, validation matrix:
PORTAL_CONFIG_DIR=/tmp/portal-staging/config ./portal status     # agent: pid=... sha=...
PORTAL_CONFIG_DIR=/tmp/portal-staging/config ./portal allowed
PORTAL_CONFIG_DIR=/tmp/portal-staging/config ./portal allow 8123
# On dev box: python3 -m http.server 8123 ; expect 127.0.0.1:8123 forward live within ~100ms
PORTAL_CONFIG_DIR=/tmp/portal-staging/config ./portal once

# Live agent log:
tail -f /tmp/portal-staging/logs/portal.log

kill $STAGING_PID
```

Validation checklist:
1. `./portal agent-version` matches `git rev-parse HEAD`.
2. `./portal status` shows `agent: pid=N sha=<short> uptime=...`.
3. New listener on remote → forward appears within 100ms (vs old 10s).
4. Killing the listener → forward removed within 50ms (DESTROY multicast).
5. `./portal allow 9999` → if 9999 listening, forwards within 100ms.
6. Manually `kill -9` the agent on remote (`pgrep -f agent- | xargs kill -9`) → reconnect within 1–2s, snapshot reconciled, no forward churn.
7. `ssh -S /tmp/portal-staging/run/master.sock -O exit <host>` → master rebuild path; client reconnects.
8. `./portal uninstall` (against staging) → agent receives Bye, exits clean, `~/.cache/portal/` empty.

### Phase C — Cutover

Once all checks pass:

```bash
# Stop the existing live service.
launchctl unload ~/Library/LaunchAgents/com.vikashl.portal.plist

# Replace the binary in its install location and reload via portal install,
# which writes the (unchanged-shape) plist and starts the new daemon.
sudo cp ./portal /usr/local/bin/portal
portal install              # idempotent; overwrites plist, reloads launchd

# Verify.
portal status
portal logs --tail
```

The plist path, env vars (`PORTAL_CONFIG_DIR`, `PORTAL_SOCK`, `PORTAL_LOG_FILE`), and `~/.config/portal/{host,allow}` files are unchanged — the new binary reads exactly what the old one did. Only `~/.cache/portal/agent-<sha>` appears on the remote.

### Rollback

If the new daemon misbehaves:

```bash
launchctl unload ~/Library/LaunchAgents/com.vikashl.portal.plist
git checkout main
make build
sudo cp ./portal /usr/local/bin/portal
portal install
```

The rollback binary's first action is `bootstrap.EnsureUploaded` against the old SHA — but the old binary doesn't have a bootstrap. Resolution: the old `main` binary spawns its old ss-poll path against the dev box; it ignores any leftover `agent-<sha>` files in `~/.cache/portal/` (they're harmless). Optionally `ssh <host> rm -rf ~/.cache/portal` after rollback to clean up.