# Embedding portal as an application sidecar

An embedding application should own one `portal run` child for the application's full lifetime. The child starts before onboarding, stays alive while the app changes from unconfigured to configured, and is not restarted after setup. [`examples/shell-desktop`](../examples/shell-desktop) is the Deno Desktop + TanStack Start reference implementation.

## Resolve app-scoped paths

Keep the portal instance separate from a CLI-installed portal. Resolve short paths under an app-owned directory and set all three portal variables:

```ts
const configDir = Deno.env.get("PORTAL_CONFIG_DIR") ?? `${Deno.env.get("HOME")}/.portal-shell`;
const apiSock = Deno.env.get("PORTAL_API_SOCK") ?? `${configDir}/api.sock`;
const controlSock = Deno.env.get("PORTAL_SOCK") ?? `${configDir}/cm.sock`;

const child = new Deno.Command(binPath, {
  args: ["run"],
  env: {
    ...Deno.env.toObject(),
    PORTAL_CONFIG_DIR: configDir,
    PORTAL_API_SOCK: apiSock,
    PORTAL_SOCK: controlSock,
    HOME: Deno.env.get("HOME") ?? "",
  },
  stdin: "null",
  stdout: "null",
  stderr: "null",
}).spawn();
```

`PORTAL_SOCK` is not optional. `PORTAL_CONFIG_DIR` changes the configuration directory and the default API socket, but it does not change `Paths.Sock`. Without an explicit `PORTAL_SOCK`, portal uses the global `~/.ssh/cm-portal.sock`; an embedded and a system-installed portal could then share an SSH ControlMaster and silently route work through the wrong instance.

On macOS, the `sun_path` field is 104 bytes including its terminator. Both socket paths must encode to at most 103 UTF-8 bytes. Keep `api.sock` and `cm.sock` directly under a shallow directory, reject overlong paths before spawn, and retain a short `PORTAL_CONFIG_DIR` override for long home-directory names.

Resolve the binary in this order:

1. `PORTAL_BIN`.
2. The packaged `portal` resource.
3. The repository-root `../../portal` in development.

Null child stdio, as above, cannot fill a pipe. An application that selects piped stdout or stderr must continuously consume both streams.

## Start and onboard over one socket

Import the unmodified TypeScript SDK directly and use the same `apiSock` throughout:

```ts
import { createClient, events, setup, waitReady } from "@portal/sdk";

await waitReady(apiSock, { timeoutMs: 15_000, signal });
const status = await createClient(apiSock).status({ signal });

if (status.host === "") {
  for await (const event of setup(apiSock, { host, force }, { signal })) {
    renderSetupStep(event);
  }
}

for await (const event of events(apiSock, { signal })) {
  if (event.status?.host !== "") {
    showConfiguredApplication();
  }
}
```

The setup response renders the ordered `validate`, `configure`, remote-deploy, `activate`, `doctor`, and `done` events. Failures after streaming starts are in-band, so the UI must show `event.line` and `event.error?.message` and use the final `done.status` as the verdict. The already-open `/v1/events` stream owns the configured transition when `event.status.host` becomes non-empty. Setup hot-swaps the daemon's host stack in-process: the API socket and child PID stay live, with no restart or socket change.

For every new or recreated window, seed first-run state from a live `status().host` read. Keep the events subscription above route selection so onboarding-to-shell navigation cannot unmount or reopen it. Subscribe to lifecycle state before reading its current snapshot to avoid missing a fast ready or error transition.

Create or adopt the desktop window before awaiting any fallible sidecar work. Render a connecting view immediately, and convert directory, binary extraction, spawn, SDK, and readiness failures into a visible error panel. A missing or invalid `PORTAL_BIN` must never leave a blank app.

## Supervise the application-lifetime child

The sidecar belongs to the application, not a window:

- Keep it running after the last window closes on macOS; use the dock `reopen` event to create a new window against the same child. On other desktop platforms, last-window close may enter the actual quit path.
- On unexpected exit, respawn with capped exponential backoff. Reset the retry count only after stable uptime, cap attempts, and surface terminal failure. A readiness failure must terminate its child before replacement so two daemons cannot overlap.
- Set the quitting flag before teardown. On an actual native Quit action or termination signal, close exec sessions, send `SIGTERM`, await exit with a bound, use `SIGKILL` only as a fallback, then exit. A per-window `close` request must not tear down the app-owned sidecar on macOS.
- Close window-owned exec sessions and invalidate their capabilities with the window.

## Package with Deno Desktop

`deno desktop` is experimental and requires Deno 2.9 or newer. Pin the example and CI to the 2.9 line while the API is experimental. TanStack Start detection comes from the `@tanstack/react-start` dependency in `package.json`, and packaging consumes the Nitro `.output/server/index.*` production server. Generate routes and build the framework before packaging:

```sh
npm run build
deno desktop --unstable-no-legacy-abort \
  --allow-env --allow-net --allow-read --allow-run --allow-write \
  --include ../../portal -o PortalShellDesktop.app .
```

The no-legacy-abort flag is required for long-lived setup and events responses under Deno 2.9: without it, `request.signal` aborts as soon as a successful streaming `Response` is returned. The OS webview backend is the default; `--backend cef` is available when a bundled renderer is preferred. The output extension selects formats such as `.app`, `.dmg`, or `.AppImage`.

Configure the SDK alias in both places: a Deno import-map entry pointing to `../../clients/ts/src/index.ts`, and an absolute Vite `resolve.alias` for `@portal/sdk`. Nitro then follows the static server import and bundles the SDK into `.output`; do not fork or vendor the client.

`--include ../../portal` places the native binary in Deno's compiled virtual filesystem. The packaged process cannot execute that in-memory resource path directly. Read its bytes relative to the extracted framework module tree, hash them, write a temporary file under an app-owned cache, apply mode `0755`, and atomically rename it before passing the real path to `Deno.Command`. This is the same extract-and-chmod pattern portal uses for its embedded `portald` agent.

Deno Desktop bindings are the privileged bootstrap channel. Mint a random per-window exec capability and return it only from `BrowserWindow.bind`; do not expose it through `/api/config` or another loopback HTTP endpoint. Validate that capability and the app origin/host before upgrading `/exec`, and close both the SDK session and its stdin queue on WebSocket close, window close, or app quit.

## Manual verification

These are live checks for a human; the repository gate does not simulate them:

1. With a fresh config directory, start the desktop example. Confirm the window appears immediately, status reports `host: ""`, and onboarding renders every setup event.
2. Hold the proxied `/v1/events` stream open, run setup against a live box, and confirm it ends in `done: ok`; the same events connection reports a configured host and the daemon PID does not change.
3. Re-run setup for the same host and confirm activation is a no-op and the daemon PID remains unchanged.
4. Run setup against a bad host without force. Confirm validation and `done` fail, then use the force-retry affordance.
5. Switch from host A to host B with an exec session open. Confirm the A session ends at activation while the events stream survives and converges on B.
6. Run an interactive program such as `vim` in xterm, resize the window, and confirm the remote PTY follows.
7. Launch with a bogus `PORTAL_BIN` and confirm a visible error panel. Relaunch with the same config and confirm the live status read skips onboarding.
8. Close every window on macOS, reopen from the dock, and confirm the child is reused. Quit through the native Quit item and confirm awaited `SIGTERM` to `SIGKILL` teardown.
9. Dial `/exec` from a normal browser or another localhost process without the in-process capability and confirm rejection.
10. Optionally package the app, launch it away from the repository, and confirm the included binary extracts with mode `0755` and starts.
