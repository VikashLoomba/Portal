# Embedding portal as an application sidecar

An embedding application should own one `portal run` child for the application's full lifetime. The child starts before onboarding, stays alive while the app changes from unconfigured to configured, and is not restarted after setup. [`examples/shell-electron`](../examples/shell-electron) is the reference implementation.

## Resolve app-scoped paths

Keep the portal instance separate from a CLI-installed portal. Under the application's user-data directory, resolve short paths such as:

```text
<userData>/portal/
  api.sock
  cm.sock
```

Set all three variables in the child's environment:

```js
const configDir = process.env.PORTAL_CONFIG_DIR || path.join(app.getPath("userData"), "portal");
const apiSock = process.env.PORTAL_API_SOCK || path.join(configDir, "api.sock");
const controlSock = process.env.PORTAL_SOCK || path.join(configDir, "cm.sock");

spawn(binPath, ["run"], {
  env: {
    ...process.env,
    PORTAL_CONFIG_DIR: configDir,
    PORTAL_API_SOCK: apiSock,
    PORTAL_SOCK: controlSock,
    HOME: os.homedir(),
  },
  stdio: "ignore",
});
```

`PORTAL_SOCK` is not optional. `PORTAL_CONFIG_DIR` changes the configuration directory and the default API socket, but it does not change `Paths.Sock`. Without an explicit `PORTAL_SOCK`, portal uses the global `~/.ssh/cm-portal.sock`; an embedded and a system-installed portal could then share an SSH ControlMaster and silently route work through the wrong instance.

Unix-domain socket paths have a small platform limit (`sun_path` is about 104 bytes on macOS). Keep `api.sock` and `cm.sock` directly under a shallow app-owned directory. An environment override is useful when an application's normal user-data path is too long.

Resolve the binary as:

```js
const binPath = process.env.PORTAL_BIN || (app.isPackaged
  ? path.join(process.resourcesPath, "portal")
  : path.resolve(appRoot, "../../portal"));
```

`process.resourcesPath` is a directory, so the packaged branch must append the resource filename and must be gated by `app.isPackaged`. The repository fallback is only for development. This example is Unix-only; a Windows port would bundle and resolve `portal.exe` instead.

Ignoring stdio, as above, satisfies the requirement to drain the child streams. If an application uses pipes instead, it must continuously consume both stdout and stderr so the sidecar cannot block on a full pipe.

## Start and onboard over one socket

After spawning the child, use the TypeScript barrel API and the same `apiSock` throughout:

```js
const { createClient, events, setup, waitReady } = await import(pathToFileURL(sdkEntry).href);

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

The setup response renders the ordered `validate`, `configure`, remote-deploy, `activate`, `doctor`, and `done` events. Failures after streaming starts are in-band, so the UI must show `event.line` and `event.error?.message` and use the final `done.status` as the verdict. The long-lived `/v1/events` stream provides the configured transition when `event.status.host` becomes non-empty. Setup hot-swaps the daemon's host stack in-process: the API socket and child PID stay live, and there is no restart or reconnect step.

For every new or recreated window, recompute first-run state from a live `status().host` read. A cache may be used only as a short-read fallback and must be refreshed by successful setup and status responses. This prevents a macOS `activate` window or a later app launch from showing stale onboarding after setup has completed.

Renderer lifecycle gating must subscribe to the sidecar ready and error channels before reading the authoritative sidecar state. Reading first leaves a race in which a `starting` to `ready` or `error` transition can be broadcast with no listener, leaving the UI stuck. Guard application entry so the state response and ready event cannot initialize it twice, and make a ready event received after a terminal error a no-op for that renderer.

Create the window before any fallible sidecar startup work. Show a connecting view immediately; convert directory, spawn, SDK-load, and readiness failures into an authoritative error state and a visible error panel. A missing or invalid `PORTAL_BIN` must never leave a blank window.

## Supervise the application-lifetime child

The sidecar belongs to the application, not a window:

- Keep it running through `window-all-closed` on macOS so `activate` can recreate a window against the live process.
- On an unexpected exit, respawn with capped exponential backoff. Reset the retry count only after stable uptime, cap attempts, and surface the terminal failure in the UI. Handle both the child `error` event (including `ENOENT`) and its exit.
- Set a quitting flag before teardown so an exit cannot schedule a replacement. On actual application quit, prevent the first quit, send `SIGTERM`, await exit with a bound, use `SIGKILL` as a fallback, then allow the final quit.
- Close window-owned exec sessions with their window, but do not tie the sidecar or first-run state to any one `BrowserWindow`.

## Package the binary and TypeScript SDK

The reference resolves the SDK entry as:

```js
const sdkEntry = process.env.PORTAL_SDK || (app.isPackaged
  ? path.join(process.resourcesPath, "clients-ts", "index.ts")
  : path.resolve(appRoot, "../../clients/ts/src/index.ts"));

const api = import(pathToFileURL(sdkEntry).href);
```

The raw `.ts` client uses the Electron/Node runtime's native type stripping. Native loading must see the source as ESM: Node determines a `.ts` file's module system like `.js`, from the nearest `package.json` `type` field. Copying only `clients/ts/src` into a package leaves `index.ts` without an ESM package scope and causes its `export` syntax to fail as CommonJS.

For electron-builder, place the SDK and its real `package.json` outside `app.asar` with three `extraResources` entries:

```json
{
  "build": {
    "appId": "dev.portal.shell-electron",
    "extraResources": [
      { "from": "../../portal", "to": "portal" },
      { "from": "../../clients/ts/src", "to": "clients-ts" },
      { "from": "../../clients/ts/package.json", "to": "clients-ts/package.json" }
    ]
  }
}
```

The third entry is required: it puts the real `type: "module"` package scope beside `<resources>/clients-ts/index.ts`, and that scope covers the barrel's relative imports. The client has no runtime dependencies. Keep both the TypeScript files and `package.json` in `extraResources`, not inside `app.asar`, because the native file-URL import needs real filesystem paths.

Electron Forge and hand-rolled packages must produce the same outside-ASAR layout. They can copy the two inputs into `clients-ts/{index.ts,...,package.json}`, or use the equivalent single-directory copy:

```js
{
  from: "../../clients/ts",
  to: "clients-ts",
  filter: ["**/*", "!node_modules{,/**}", "!test{,/**}"],
}
```

With the single-directory layout, resolve the packaged entry as `path.join(process.resourcesPath, "clients-ts", "src", "index.ts")`. In either shape, keep the files outside ASAR and verify that the nearest package scope for the entry declares `type: "module"`.

## Manual verification

These are live checks for a human; the repository gate does not simulate them:

1. With a fresh config directory, run `portal run`. Confirm status reports `host: ""` and host-dependent exec and doctor calls return `503 not_configured`.
2. Hold `/v1/events` open, run setup against a live box, and confirm the full stream ends in `done: ok`; the events connection stays open, reports a configured host, and the daemon PID does not change.
3. Re-run setup for the same host and confirm activation is a no-op and the daemon PID remains unchanged.
4. Run setup against a bad host without force. Confirm validation and `done` fail, the host file is unchanged, and existing connectivity remains live.
5. Switch from host A to host B with an exec session open. Confirm setup deploys to B, the A session ends at activation, and the events stream survives and converges on B.
6. Run `portal install <box>` from scratch and confirm the CLI-managed install still loads its service and finishes with a passing doctor report.
7. For the Electron example, use a fresh `PORTAL_CONFIG_DIR` and launch `electron .`. Confirm the window appears immediately in a connecting state, onboarding streams every setup step, `/v1/events` reveals the shell on the same socket without a restart, and quit waits for teardown. Then:
   - launch with a bogus `PORTAL_BIN` and confirm the visible sidecar-error panel appears;
   - exercise a fast-ready launch and confirm the renderer never sticks on connecting;
   - close every window on macOS and activate the app again, then confirm the live sidecar and configured shell are reused;
   - quit and relaunch with the same config directory, then confirm the live status read skips onboarding.

An optional packaging check is to run electron-builder, inspect the package for `<resources>/portal`, `<resources>/clients-ts/index.ts`, and `<resources>/clients-ts/package.json`, and launch the packaged app. The sibling `package.json` must declare `type: "module"`; removing it should reproduce the CommonJS parse failure that this layout prevents.
