# Embedding and controlling the local portal daemon

Portal.app communicates with the local macOS daemon over its owner-only Unix
socket. The daemon owns SSH sessions, remote `portald` agents, forwarding, and
API configuration mutations; UI clients do not connect to remote agents
directly. The CLI retains direct lifecycle/recovery paths while sharing the
same validated configuration model.

## Resolve paths

The normal socket is `~/.config/portal/api.sock`. An embedding application that
owns a private daemon can isolate it with environment overrides:

```ts
const configDir = Deno.env.get("PORTAL_CONFIG_DIR") ??
  `${Deno.env.get("HOME")}/.portal-shell`;
const apiSock = Deno.env.get("PORTAL_API_SOCK") ?? `${configDir}/api.sock`;

const child = new Deno.Command(binPath, {
  args: ["daemon"],
  env: {
    ...Deno.env.toObject(),
    PORTAL_CONFIG_DIR: configDir,
    PORTAL_API_SOCK: apiSock,
    HOME: Deno.env.get("HOME") ?? "",
  },
  stdin: "null",
  stdout: "null",
  stderr: "null",
}).spawn();
```

`PORTAL_CONFIG_DIR` relocates the configuration directory and default API
socket; `PORTAL_API_SOCK` overrides the socket alone. The socket also acts as
the daemon's single-instance lock, so two instances must never share it.

## Versioned local API

API v1 is newline-delimited JSON. A new client writes one request immediately;
the daemon returns one response with the same `id` and closes. Every envelope
contains `api_version: 1`.

```json
{"api_version":1,"id":1,"method":"get_state"}
```

A successful response is typed by `result`:

```json
{"api_version":1,"id":1,"result":"state","state":{"version":"2.0.15","build_sha":"…","boxes":[],"statuses":[],"features":{}}}
```

Errors are explicit and do not overload transport failure:

```json
{"api_version":1,"id":1,"result":"error","code":"operation_failed","message":"…"}
```

The Rust request and response schema is defined in
[`portal_core::localapi`](../crates/portal-core/src/localapi.rs). API v1 methods:

| Method | Purpose |
|---|---|
| `get_state` | Build, configured boxes, live status, and feature gates |
| `subscribe_state` | Keep the connection open and emit changed state snapshots |
| `add_box` | Add and enable a configured SSH box |
| `remove_box` | Remove a configured box |
| `set_box_enabled` | Connect/disconnect without forgetting configuration |
| `set_allow` / `set_allow_exact` | Mutate or atomically replace forced remote ports |
| `set_process_group_discovery` | Include companion listeners from a box's Linux process groups |
| `set_feature` | Toggle a known capability gate |
| `get_logs` | Read a bounded tail of the local daemon log |

`subscribe_state` emits a state response immediately and then wakes only on a
real supervisor/configuration/feature invalidation; it has no polling interval.
Clients should reconnect after EOF or a daemon restart.

Configuration-changing methods validate and atomically write `config.toml` and
reconcile SSH stacks before returning. The filesystem watcher remains only for
external edits made outside the API.
The socket verifies the peer uid in addition to filesystem mode `0600`; there
are no bearer tokens, so it must not be proxied to a less-trusted process.

## Legacy status compatibility

Portal 2.0 clients connect and wait without sending a request. For compatibility,
a client that writes nothing during the short protocol-detection window still
receives the old pretty-printed bare JSON status array and EOF. New code should
always use the versioned API.

The stale HTTP `/v1/*` client under [`clients/ts`](../clients/ts) and the
unmaintained [`examples/shell-desktop`](../examples/shell-desktop) do not
implement this API. Their wire-protocol CBOR fixtures remain useful, but their
local HTTP control layer must not be used for Portal v2.
