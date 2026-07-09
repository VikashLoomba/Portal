# Portal Shell Electron Reference

Thin Electron reference app for the Stage 6 local API. It is manually validated, not built or run by the gate.

```sh
npm install
npm start
```

The app talks to the daemon over the local Unix socket. Socket path resolution is:

1. `PORTAL_API_SOCK`
2. `PORTAL_APISOCK`
3. `PORTAL_SOCKET`
4. `~/.config/portal/api.sock`

`main.js` is CommonJS so Electron can load `main.js` and `preload.js` directly. It dynamically imports the TypeScript `clients/ts/src/index.ts` barrel at runtime; the client source is erasable TypeScript, matching the Node 24+ gate and recent Electron Node runtimes. The renderer is context-isolated and only uses `window.portal`, the preload bridge. A small app protocol serves `renderer.ts` after stripping erasable TypeScript so `npm start` does not need a separate renderer build.

The `types/xterm.d.ts` file is a gate-only shim because the u12 smoke gate must type-check without `examples/shell-electron/node_modules`. After `npm install`, the real `@xterm/xterm` package supplies the runtime CSS and JS loaded by `index.html`.

Manual validation checklist from `DESIGN-extraction.md` section 8.7:

- Status panel shows live daemon status.
- A notification appears in the events/notifications feed.
- Exec terminal runs an interactive program such as `vim` with live window resize.
