# Portal Shell Desktop Reference

This example embeds portal in a TanStack Start app hosted by the experimental
`deno desktop` runtime. It requires Deno 2.9 or newer; the dependency and
workflow pins currently target Deno 2.9.x because the desktop APIs are still
experimental.

Build the repository-root sidecar first, then launch the desktop app in
development mode:

```sh
make portal
cd examples/shell-desktop
deno desktop --hmr \
  --unstable-no-legacy-abort \
  --allow-ffi --allow-sys=cpus,homedir,uid \
  --allow-env=HOME,PATH,NODE_ENV,TERM,TMPDIR,NO_COLOR,FORCE_COLOR,CI,DENO_SERVE_ADDRESS,PORTAL_BIN,PORTAL_CONFIG_DIR,PORTAL_API_SOCK,PORTAL_SOCK \
  '--allow-net=localhost,127.0.0.1,[::1]' \
  --allow-read --allow-run --allow-write .
```

`npm run desktop` is a convenience alias for the same `deno desktop --hmr`
command. The npm script name must be passed separately: `npm run:desktop` is not
valid npm syntax.

`deno desktop --hmr .` auto-detects TanStack Start and starts the framework's
Vite dev server. The desktop runtime owns the window and points its webview at
that server. Run `npm run dev` on its own to serve the same app in a browser
without the desktop shell — the sidecar, streams, and exec bridge all work there
too.

The desktop commands run the runtime least-privilege where a static scope is
practical: environment access is an explicit allowlist (the
`PORTAL_*` overrides plus the handful of runtime variables the runtime reads —
extend the list in `deno.json` if your setup needs more). The HMR launcher itself
is scoped to loopback. Read, write, subprocess, and the server's network access
stay broad by necessity: the sidecar binary, its extraction cache, app-scoped
config dir, and `PORTAL_API_SOCK` all live at per-user or overridden dynamic
paths that a static command cannot name. The network permission is also
baked into packaged binaries, so omitting it or listing only TCP loopback makes
the app prompt for (or deny) its Unix-socket connection at startup.

The `dev` and `build` scripts run the Vite toolchain (Vite, Rolldown, Nitro),
which reads too many environment variables to enumerate, so they grant
`--allow-env` in full. The `dev` script also needs broad `--allow-net` because it
connects to the dynamically configured Unix socket in addition to hosting the
Vite server. Vite advertises the HMR server on loopback by default; the packaged
app never runs it (see below).

The commands also opt into Deno's corrected request-abort behavior so closing an
SSE client aborts the proxied SDK stream without aborting it immediately after a
successful response. The webview appears before the supervisor waits for
portal, so path, spawn, and readiness errors are visible in the app instead of
producing a blank window.

The supervisor resolves the binary in this order:

1. `PORTAL_BIN`
2. the `portal` resource included in a packaged desktop binary
3. the repository-root `../../portal` during development

Every launch passes `PORTAL_CONFIG_DIR`, `PORTAL_API_SOCK`, and `PORTAL_SOCK` to
the child. The default scope is `$HOME/.portal-shell`; setting only
`PORTAL_CONFIG_DIR` derives both sockets beneath that override. `PORTAL_SOCK`
must remain explicit because portal's normal fallback is the global
`~/.ssh/cm-portal.sock`, which would share a ControlMaster with a
system-installed portal.

The macOS `sun_path` field is 104 bytes including its terminator, so both socket
paths must encode to at most 103 UTF-8 bytes. Startup fails visibly when either
path is too long. Use a short absolute `PORTAL_CONFIG_DIR` such as
`/tmp/ps-$USER` when a long home or override would exceed the limit.

## Packaging

`deno desktop` detects TanStack Start from `package.json` and packages its Nitro
`.output/server/index.*` artifact. The build script explicitly generates routes
and builds that artifact before invoking the packager, so the required
production output is present and directly checkable:

```sh
npm run build
deno desktop --unstable-no-legacy-abort \
  --allow-ffi --allow-sys=cpus,homedir,uid \
  --allow-env=HOME,PATH,NODE_ENV,TERM,TMPDIR,NO_COLOR,FORCE_COLOR,CI,DENO_SERVE_ADDRESS,PORTAL_BIN,PORTAL_CONFIG_DIR,PORTAL_API_SOCK,PORTAL_SOCK \
  --allow-net --allow-read --allow-run --allow-write \
  --include ../../portal -o PortalShellDesktop.app .
```

`npm run desktop:package` is a convenience alias that runs those build and
desktop commands in sequence.

The static Vite alias imports the unmodified SDK from
`../../clients/ts/src/index.ts`; Nitro bundles that module graph into the
packaged server output. The `--include` flag separately adds the native portal
binary to Deno's compiled virtual filesystem. Native executables cannot run in
place there, so startup reads the resource bytes, writes an atomically named
copy under the app config cache, applies mode `0755`, and spawns that extracted
path.

The default OS webview backend is used. Deno Desktop also supports
`--backend cef`, and the output extension may be changed to a supported format
such as `.dmg` or `.AppImage`.

The `/exec` bridge additionally requires a random exec capability. In packaged
desktop mode it is delivered through `Deno.BrowserWindow.bind` and is never
returned from an HTTP endpoint. The server validates that capability plus the
request origin/host before opening the SDK exec session, and window close or app
quit closes the remote PTY.

## Development mode

The framework dev server loads the server entry in a process where
`Deno.BrowserWindow` and `Deno.dock` are absent. The server feature-detects the
desktop APIs (`typeof Deno.BrowserWindow === "function"`): when they are missing
it skips all window, menu, and dock management and never touches them, so the
entry loads without crashing while the sidecar supervisor still starts. Packaged
builds run inside the desktop runtime where those APIs are present and the
window/menu/quit/dock behavior is unchanged.

Because the dev window has no bind channel, the renderer cannot receive the exec
capability the packaged way: the webview injects the `bindings` proxy in both
modes, but under `--hmr` no handler is registered, so calling it rejects. In dev
mode only, the server exposes a `GET /api/dev-exec-token` endpoint that mints
the same per-process capability and returns it to the renderer; the endpoint is
disabled (404) in packaged mode, where the capability is delivered only through
`Deno.BrowserWindow.bind`. The renderer asks the endpoint first in every mode
and treats the packaged 404 as the signal to await the native binding instead.
The endpoint checks the request `Origin` against the dev origin, so a browser
page served from any foreign origin is rejected, and the exec bridge still
requires the capability before opening a PTY.

The `/exec` WebSocket needs the same dev-mode detour: the framework dev server
answers only its own HMR upgrade requests and leaves every other WebSocket
handshake unanswered, so an upgrade to an app route never reaches the server
entry (and `Deno.upgradeWebSocket` requires a native `Deno.serve` request
regardless). In dev mode the exec bridge therefore listens on a dedicated
loopback `Deno.serve` port, and the dev-exec-token response advertises that
`execSocketUrl` to the renderer. The dedicated listener accepts only loopback
peers with a loopback (or absent) `Origin`, and the capability check is
unchanged. The packaged app serves `/exec` on the app origin and never starts
this listener.

Although the Nitro dev server binds every interface (see above), the token
endpoint requires the actual transport peer reported by `Deno.serve` and the
requested hostname to be loopback. It also checks any request `Origin` against
the dev origin. The development trust posture assumes loopback processes are
same-user and trusted: a non-browser loopback client can retrieve the token,
while the `Origin` check prevents arbitrary local browser pages from driving
exec. The rest of the dev server remains reachable from the local network, so
run development only on a trusted network. The packaged app carries none of
this: it never exposes the capability over HTTP and runs no dev server.

## Automated check

```sh
npm run check
```

The check regenerates the route tree, type-checks every config, route, server
module, and test with Deno Desktop types, then runs the path, binary-resolution,
and exec-normalization unit tests. It does not launch a GUI or claim access to a
live development box.

## Manual live-box validation

These items require a human with a reachable SSH development box and are never
faked as automated tests:

- Start with a fresh `PORTAL_CONFIG_DIR`. Confirm the window immediately shows a
  connecting state, then onboarding when live `status.host` is empty.
- Run setup and watch running, line, warning, failure, and final events render
  live. Exercise validation failure and the force-retry affordance.
- Confirm the already-open proxied `/v1/events` stream reveals the shell when it
  reports a configured host, without a socket change, sidecar restart, or daemon
  PID change.
- Run an interactive program such as `vim` in xterm and resize the window while
  it is active; confirm PTY dimensions follow.
- Set `PORTAL_BIN` to a bogus path and confirm the spawn error appears in the
  visible panel rather than a blank app.
- Relaunch with the same config directory and confirm the live status read skips
  onboarding.
- On macOS, close every window and click the dock icon. Confirm the same sidecar
  remains alive and the recreated window shows the configured shell.
- Quit through the native Quit item and confirm teardown awaits `SIGTERM`,
  escalates to `SIGKILL` only after the timeout, and closes active exec
  sessions.
- Try `/exec` from a normal browser or another localhost process without the
  in-process capability and with a foreign Origin. Confirm it is rejected.
- Optionally package the `.app`, launch it away from the repository, and confirm
  the `--include` resource extracts, receives mode `0755`, and starts
  successfully.
