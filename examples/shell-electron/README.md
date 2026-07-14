# Portal Shell Electron Reference

Electron reference embedding for the portal sidecar and TypeScript client. It supervises the bundled portal binary for the application's lifetime, renders first-run setup over the local API, then opens the shell after `/v1/events` reports a configured host.

```sh
npm install
npm start
```

In development, build the repository-root `portal` binary first or set `PORTAL_BIN` to one. The app resolves the binary in this order:

1. `PORTAL_BIN`
2. packaged: `path.join(process.resourcesPath, "portal")`
3. development: the repository-root `../../portal`

The sidecar receives three app-scoped paths. Each can be overridden, otherwise `PORTAL_CONFIG_DIR` is `<app userData>/portal`, `PORTAL_API_SOCK` is `<configDir>/api.sock`, and `PORTAL_SOCK` is `<configDir>/cm.sock`. The control socket must be explicit: portal's normal `PORTAL_SOCK` fallback is the global `~/.ssh/cm-portal.sock`, which would share an SSH ControlMaster with a system-installed portal even when `PORTAL_CONFIG_DIR` differs.

The main process creates a window before starting the sidecar, so directory, spawn, SDK-load, and readiness failures appear in a visible error panel rather than leaving a blank app. The child stays alive after every window closes on macOS, respawns with capped backoff after an unexpected exit, and is terminated with an awaited `SIGTERM`/`SIGKILL` teardown only when the app actually quits.

After `waitReady`, the app reads `status.host`. An empty host shows onboarding and streams `setup()` events; the existing `/v1/events` connection reports the configured transition and reveals the shell without changing sockets or restarting the child. Recreated windows recompute first-run state from a live status read. To avoid a missed-broadcast race, the renderer subscribes to sidecar ready/error before it reads the authoritative sidecar state; guards prevent duplicate entry and ignore a late ready after an error.

`main.js` and `preload.js` remain CommonJS. The main process dynamically imports the TypeScript SDK barrel using this resolution:

1. `PORTAL_SDK`
2. packaged: `path.join(process.resourcesPath, "clients-ts", "index.ts")`
3. development: `../../clients/ts/src/index.ts`

The raw SDK relies on the Electron/Node runtime's native TypeScript stripping. Packaged `.ts` files must resolve as ESM, so `build.extraResources` contains three entries: the portal binary, `clients/ts/src` copied to `clients-ts`, and the real `clients/ts/package.json` copied to `clients-ts/package.json`. The third copy provides the required sibling `type: "module"` package scope; omitting it makes Node parse the SDK barrel as CommonJS and fail on `export`. Both SDK resources live outside `app.asar` so the file-URL import targets real files.

The renderer is context-isolated and only uses `window.portal`. A small app protocol serves `renderer.ts` after stripping erasable TypeScript, so `npm start` does not need a renderer build. `types/xterm.d.ts` is a gate-only shim for type-checking without this example's `node_modules`; after `npm install`, `@xterm/xterm` supplies the runtime CSS and JavaScript loaded by `index.html`.

See the [canonical embedding recipe](../../docs/embedding.md) for lifecycle, packaging, Electron Forge/hand-rolled layouts, socket-length constraints, and platform caveats.

## Manual validation

These steps require a live dev box and are not claimed by the automated gate:

- With a fresh `PORTAL_CONFIG_DIR`, launch `electron .`. The window appears immediately in the connecting state, `status.host === ""` opens onboarding, setup steps stream through `activate` and `done`, `/v1/events` flips to the configured host on the same socket, and the shell opens without a restart.
- Confirm the status panel updates, a notification appears in the events feed, and an interactive program such as `vim` runs in the terminal with live resize.
- Launch with a bogus `PORTAL_BIN`. The sidecar-error panel must be visible; the window must not be blank.
- Exercise a fast-ready launch and confirm the UI never remains stuck on connecting.
- On macOS, close every window and activate the app again. The sidecar stays alive and the recreated window opens the configured shell rather than onboarding.
- Quit and relaunch with the same `PORTAL_CONFIG_DIR`. The live status read must open the configured shell, and final quit must await sidecar teardown.
- Optional packaging check: run electron-builder, then verify the package has `<resources>/portal`, `<resources>/clients-ts/index.ts`, and `<resources>/clients-ts/package.json` with `type: "module"`; launch it and confirm the packaged SDK import succeeds.
