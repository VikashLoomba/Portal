# Embedding portal as an application sidecar

An embedding application owns one `portal daemon` child for the application's
full lifetime and reads state from that instance's status socket.

> **Scope note.** v2 exposes a **read-only** status socket. The streamed control
> API this document used to describe — `/v1/events`, `/v1/setup`, `/v1/exec`, and
> the in-process host hot-swap that let a UI drive onboarding — is **not part of
> v2**. Onboarding a box from an embedding app means shelling out to
> `portal install` / `portal box add` and watching the status socket, not
> streaming setup events.
>
> The `/v1/*` client code in [`clients/ts`](../clients/ts) and
> [`examples/shell-desktop`](../examples/shell-desktop) targets that removed API.
> Note that `clients/ts` is mixed: its CBOR/framing layer is current and is
> checked against [`docs/vectors/`](vectors) in CI alongside the Rust
> implementation, while only its HTTP control-API half is stale. See that
> package's README for the split.

## Resolve app-scoped paths

Keep the embedded instance separate from a CLI-installed portal. Point both
portal path variables at an app-owned directory:

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

`PORTAL_CONFIG_DIR` relocates the configuration directory and the default API
socket; `PORTAL_API_SOCK` overrides the socket path on its own. Set both
explicitly so an embedded instance and a system-installed one can never share a
socket — the socket doubles as portal's single-instance lock, so two instances
pointed at one path will fight over it.

There is deliberately **no `PORTAL_SOCK`**. v2 speaks SSH in-process (russh), so
there is no `ControlMaster` socket to isolate; v1's warning about two portals
sharing a multiplexed SSH connection does not apply.

`portal daemon` is the hidden foreground entry point launchd uses. `portal run`
also starts the daemon, but exists only so a v1 LaunchAgent plist keeps working
across a v1→v2 upgrade; new embedders should use `daemon`.

## Read status

Connect to the socket and read until EOF. The daemon writes one
pretty-printed JSON snapshot per connection and closes — there is no request
framing and no streaming:

```ts
const conn = await Deno.connect({ path: apiSock, transport: "unix" });
const snapshot = JSON.parse(new TextDecoder().decode(await toArrayBuffer(conn)));
```

The snapshot is an array with one object per configured box:

| Field | Type | Meaning |
|---|---|---|
| `name` | string | box name |
| `host` | string | ssh alias or `user@host` |
| `index` | number | port-mapping slot |
| `connected` | bool | SSH session is up |
| `agent_sha` | string \| null | build SHA of the running dev-box agent |
| `forwards` | `[remote, local][]` | live forwards |
| `clipsync_synced` | bool | deployed shims match this build |
| `clipsync_change_id` | number | monotonic clipboard-config generation |

An unconfigured instance returns `[]`. A missing socket file means the daemon is
not running yet; poll rather than assuming failure, since the daemon binds the
socket after startup.

Treat `connected: false` as transient — portal reconnects on its own with
backoff, so surface it as "reconnecting" rather than an error state. An
`agent_sha` that differs from the app's bundled portal build means the box agent
is mid-reconvergence.

Auth is peer uid over an owner-only (`0600`) Unix socket. There are no bearer
tokens, so an embedding app must not expose the socket or its contents to a less
trusted process.
