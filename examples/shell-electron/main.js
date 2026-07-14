"use strict";

const { spawn } = require("node:child_process");
const fs = require("node:fs/promises");
const { stripTypeScriptTypes } = require("node:module");
const os = require("node:os");
const path = require("node:path");
const { pathToFileURL } = require("node:url");

const { app, BrowserWindow, ipcMain, protocol } = require("electron");

const appRoot = __dirname;
const rendererScheme = "portal-shell";
const readyTimeoutMs = 15000;
const stableUptimeMs = 30000;
const maxRespawnAttempts = 5;

let portalApi;
let portalPaths;
let mainWindow;
let nextExecId = 1;
let sidecarProcess = null;
let sidecarGeneration = 0;
let sidecarPhase = "starting";
let sidecarError = "";
let sidecarReadyOnce = false;
let respawnAttempts = 0;
let respawnTimer = null;
let setupRunning = false;
let needsSetup = true;
let quitting = false;
let allowQuit = false;
let teardownPromise = null;
const sidecarAbort = new AbortController();
const execSessions = new Map();
const attachedWindows = new WeakSet();

protocol.registerSchemesAsPrivileged([
  {
    scheme: rendererScheme,
    privileges: {
      standard: true,
      secure: true,
      supportFetchAPI: true,
    },
  },
]);

app.whenReady().then(() => {
  registerRendererProtocol();
  portalPaths = resolvePaths();
  registerIpc();
  mainWindow = createWindow();
  void bootstrap();

  app.on("activate", () => {
    if (BrowserWindow.getAllWindows().length === 0) {
      mainWindow = createWindow();
      if (sidecarPhase === "ready") {
        attachWindow(mainWindow);
      }
    }
  });
});

app.on("window-all-closed", () => {
  closeAllExecSessions();
  if (process.platform !== "darwin") {
    app.quit();
  }
});

app.on("before-quit", (event) => {
  quitting = true;
  if (allowQuit) {
    return;
  }

  event.preventDefault();
  if (teardownPromise === null) {
    closeAllExecSessions();
    teardownPromise = stopSidecar().finally(() => {
      allowQuit = true;
      app.quit();
    });
  }
});

async function bootstrap() {
  try {
    await startSidecar();
  } catch (error) {
    if (!quitting) {
      setSidecarError(`Unable to start portal: ${toErrorMessage(error)}`);
    }
  }
}

function createWindow() {
  const win = new BrowserWindow({
    width: 1180,
    height: 820,
    minWidth: 760,
    minHeight: 520,
    title: "Portal Shell",
    backgroundColor: "#101418",
    webPreferences: {
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: true,
      preload: path.join(appRoot, "preload.js"),
    },
  });

  win.on("closed", () => {
    closeWindowExecSessions(win.webContents);
  });
  void win.loadURL(`${rendererScheme}://app/index.html`);
  return win;
}

function registerRendererProtocol() {
  protocol.handle(rendererScheme, async (request) => {
    const url = new URL(request.url);
    if (url.hostname !== "app") {
      return new Response("not found", { status: 404 });
    }

    const pathname = decodeURIComponent(url.pathname === "/" ? "/index.html" : url.pathname);
    const filePath = path.resolve(appRoot, "." + pathname);
    if (!isPathInside(appRoot, filePath)) {
      return new Response("not found", { status: 404 });
    }

    try {
      const data = await fs.readFile(filePath);
      if (filePath.endsWith(".ts")) {
        if (typeof stripTypeScriptTypes !== "function") {
          return new Response("Electron's Node runtime must support stripTypeScriptTypes", { status: 500 });
        }
        const source = stripTypeScriptTypes(data.toString("utf8"), {
          mode: "strip",
          sourceMap: false,
        });
        return new Response(source, {
          headers: {
            "Content-Type": "text/javascript; charset=utf-8",
          },
        });
      }
      return new Response(data, {
        headers: {
          "Content-Type": contentType(filePath),
        },
      });
    } catch (error) {
      if (error && error.code === "ENOENT") {
        return new Response("not found", { status: 404 });
      }
      return new Response(toErrorMessage(error), { status: 500 });
    }
  });
}

function registerIpc() {
  ipcMain.handle("portal:config", () => {
    return { socketPath: portalPaths.apiSock };
  });

  ipcMain.handle("portal:sidecar:state", () => {
    return { phase: sidecarPhase, error: sidecarError };
  });

  ipcMain.handle("portal:first-run:state", async () => {
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 3000);
    try {
      const api = await loadPortalApi();
      const status = await api.createClient(portalPaths.apiSock).status({ signal: controller.signal });
      needsSetup = status.host === "";
    } catch {
      // The cache is refreshed by setup and the status loop when this short live read fails.
    } finally {
      clearTimeout(timeout);
    }
    return { needsSetup };
  });

  ipcMain.handle("portal:setup:start", async (event, input) => {
    if (setupRunning) {
      throw new Error("portal setup is already running");
    }

    setupRunning = true;
    const request = normalizeSetupStart(input);
    const owner = event.sender;
    const controller = new AbortController();
    const abort = () => controller.abort();
    let done = null;
    owner.once("destroyed", abort);

    try {
      const api = await loadPortalApi();
      for await (const setupEvent of api.setup(portalPaths.apiSock, request, { signal: controller.signal })) {
        sendToOwner(owner, "portal:setup:step", setupEvent);
        if (setupEvent.step === "done") {
          done = setupEvent;
          if (setupEvent.status === "ok") {
            needsSetup = false;
          }
        }
      }
      return done;
    } finally {
      owner.removeListener("destroyed", abort);
      setupRunning = false;
    }
  });

  ipcMain.handle("portal:exec:start", async (event, input) => {
    const request = normalizeExecStart(input);
    const id = String(nextExecId++);
    const stdin = createInputQueue();
    const api = await loadPortalApi();
    const owner = event.sender;

    const session = api.exec(portalPaths.apiSock, {
      argv: request.argv,
      pty: {
        term: request.term,
        rows: request.rows,
        cols: request.cols,
      },
      stdin,
      stdout(chunk) {
        sendToOwner(owner, "portal:exec:data", execDataPayload(id, "stdout", chunk));
      },
      stderr(chunk) {
        sendToOwner(owner, "portal:exec:data", execDataPayload(id, "stderr", chunk));
      },
    });

    execSessions.set(id, { id, owner, session, stdin });
    session.result.then(
      (result) => {
        sendToOwner(owner, "portal:exec:exit", { id, code: result.code });
      },
      (error) => {
        sendToOwner(owner, "portal:exec:error", { id, message: toErrorMessage(error) });
      },
    ).finally(() => {
      stdin.close();
      execSessions.delete(id);
    });

    return { id };
  });

  ipcMain.on("portal:exec:stdin", (event, input) => {
    const session = ownedExecSession(event.sender, input && input.id);
    if (session === null) {
      return;
    }
    const data = typeof input.data === "string" ? input.data : "";
    if (data.length > 0) {
      session.stdin.push(Buffer.from(data, "utf8"));
    }
  });

  ipcMain.on("portal:exec:resize", (event, input) => {
    const session = ownedExecSession(event.sender, input && input.id);
    if (session === null) {
      return;
    }
    const size = normalizeSize(input);
    if (size === null) {
      return;
    }
    session.session.resize(size.rows, size.cols).catch((error) => {
      sendToOwner(event.sender, "portal:exec:error", {
        id: session.id,
        message: toErrorMessage(error),
      });
    });
  });

  ipcMain.on("portal:exec:close", (event, input) => {
    const session = ownedExecSession(event.sender, input && input.id);
    if (session === null) {
      return;
    }
    session.stdin.close();
    session.session.close();
    execSessions.delete(session.id);
  });
}

async function loadPortalApi() {
  if (portalApi === undefined) {
    const url = pathToFileURL(portalPaths.sdkEntry);
    portalApi = import(url.href);
  }
  return portalApi;
}

async function startSidecar() {
  await fs.mkdir(portalPaths.configDir, { recursive: true });
  await spawnSidecar();
}

function spawnSidecar() {
  if (quitting) {
    return Promise.resolve();
  }

  let child;
  try {
    child = spawn(portalPaths.binPath, ["run"], {
      env: {
        ...process.env,
        PORTAL_CONFIG_DIR: portalPaths.configDir,
        PORTAL_API_SOCK: portalPaths.apiSock,
        PORTAL_SOCK: portalPaths.controlSock,
        HOME: os.homedir(),
      },
      stdio: "ignore",
    });
  } catch (error) {
    const message = toErrorMessage(error);
    setSidecarError(`Unable to spawn portal: ${message}`);
    scheduleRespawn(message, true);
    return Promise.resolve();
  }

  const generation = ++sidecarGeneration;
  const startedAt = Date.now();
  let settled = false;
  let spawnFailure = null;
  sidecarProcess = child;

  child.once("error", (error) => {
    spawnFailure = error;
    setSidecarError(`Unable to spawn portal: ${toErrorMessage(error)}`);
    handleSidecarDeparture(child, startedAt, null, null, spawnFailure, settled);
    settled = true;
  });
  child.once("exit", (code, signal) => {
    handleSidecarDeparture(child, startedAt, code, signal, spawnFailure, settled);
    settled = true;
  });

  return observeSidecarReady(generation);
}

async function observeSidecarReady(generation) {
  try {
    const api = await loadPortalApi();
    await api.waitReady(portalPaths.apiSock, {
      timeoutMs: readyTimeoutMs,
      signal: sidecarAbort.signal,
    });
    if (quitting || generation !== sidecarGeneration || sidecarProcess === null) {
      return;
    }

    if (!sidecarReadyOnce) {
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), 5000);
      try {
        const status = await api.createClient(portalPaths.apiSock).status({ signal: controller.signal });
        if (quitting || generation !== sidecarGeneration || sidecarProcess === null) {
          return;
        }
        needsSetup = status.host === "";
      } finally {
        clearTimeout(timeout);
      }
      sidecarReadyOnce = true;
    }

    setSidecarReady();
    for (const win of BrowserWindow.getAllWindows()) {
      attachWindow(win);
    }
  } catch (error) {
    if (!quitting && generation === sidecarGeneration) {
      setSidecarError(`Portal did not become ready: ${toErrorMessage(error)}`);
    }
  }
}

function handleSidecarDeparture(child, startedAt, code, signal, spawnFailure, alreadySettled) {
  if (alreadySettled) {
    return;
  }
  if (sidecarProcess === child) {
    sidecarProcess = null;
  }
  if (quitting) {
    return;
  }

  if (Date.now() - startedAt >= stableUptimeMs) {
    respawnAttempts = 0;
  }

  const message = spawnFailure === null
    ? `portal exited (${code === null ? signal || "unknown" : `code ${code}`})`
    : toErrorMessage(spawnFailure);
  scheduleRespawn(message, spawnFailure !== null);
}

function scheduleRespawn(message, preserveError) {
  if (quitting) {
    return;
  }

  respawnAttempts += 1;
  if (respawnAttempts >= maxRespawnAttempts) {
    setSidecarError(`Portal stopped repeatedly: ${message}`);
    return;
  }

  if (!preserveError) {
    sidecarPhase = "starting";
    sidecarError = "";
  }
  const backoffMs = Math.min(250 * (2 ** (respawnAttempts - 1)), 4000);
  clearTimeout(respawnTimer);
  respawnTimer = setTimeout(() => {
    respawnTimer = null;
    void spawnSidecar();
  }, backoffMs);
}

async function stopSidecar() {
  sidecarAbort.abort();
  clearTimeout(respawnTimer);
  respawnTimer = null;

  const child = sidecarProcess;
  sidecarProcess = null;
  if (child === null || child.exitCode !== null || child.signalCode !== null) {
    return;
  }

  const exited = waitForChildExit(child, 2000);
  try {
    child.kill("SIGTERM");
  } catch {
    return;
  }
  if (await exited) {
    return;
  }

  const killed = waitForChildExit(child, 1000);
  try {
    child.kill("SIGKILL");
  } catch {
    return;
  }
  await killed;
}

function waitForChildExit(child, timeoutMs) {
  if (child.exitCode !== null || child.signalCode !== null) {
    return Promise.resolve(true);
  }

  return new Promise((resolve) => {
    const finish = (exited) => {
      clearTimeout(timer);
      child.removeListener("exit", onExit);
      child.removeListener("error", onExit);
      resolve(exited);
    };
    const onExit = () => finish(true);
    const timer = setTimeout(() => finish(false), timeoutMs);
    child.once("exit", onExit);
    child.once("error", onExit);
  });
}

function setSidecarReady() {
  sidecarPhase = "ready";
  sidecarError = "";
  broadcast("portal:sidecar:ready", {});
}

function setSidecarError(message) {
  sidecarPhase = "error";
  sidecarError = message;
  broadcast("portal:sidecar:error", { message });
}

function broadcast(channel, payload) {
  for (const win of BrowserWindow.getAllWindows()) {
    sendToOwner(win.webContents, channel, payload);
  }
}

function attachWindow(win) {
  if (attachedWindows.has(win) || win.isDestroyed()) {
    return;
  }
  attachedWindows.add(win);
  startStatusLoop(win);
  startEventsLoop(win);
}

function startStatusLoop(win) {
  let stopped = false;
  win.on("closed", () => {
    stopped = true;
  });

  const tick = async () => {
    if (stopped || win.webContents.isDestroyed()) {
      return;
    }
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 5000);
    try {
      const api = await loadPortalApi();
      const client = api.createClient(portalPaths.apiSock);
      const status = await client.status({ signal: controller.signal });
      needsSetup = status.host === "";
      sendToOwner(win.webContents, "portal:status", status);
    } catch (error) {
      sendToOwner(win.webContents, "portal:status:error", { message: toErrorMessage(error) });
    } finally {
      clearTimeout(timeout);
    }
    if (!stopped) {
      setTimeout(tick, 2000);
    }
  };

  void tick();
}

function startEventsLoop(win) {
  const controller = new AbortController();
  win.on("closed", () => {
    controller.abort();
  });

  const run = async () => {
    while (!controller.signal.aborted && !win.webContents.isDestroyed()) {
      try {
        const api = await loadPortalApi();
        for await (const event of api.events(portalPaths.apiSock, { signal: controller.signal })) {
          sendToOwner(win.webContents, "portal:event", event);
          if (controller.signal.aborted || win.webContents.isDestroyed()) {
            break;
          }
        }
      } catch (error) {
        if (!controller.signal.aborted) {
          sendToOwner(win.webContents, "portal:event:error", { message: toErrorMessage(error) });
        }
      }
      if (!controller.signal.aborted) {
        await delay(1500, controller.signal);
      }
    }
  };

  void run();
}

function createInputQueue() {
  const chunks = [];
  const waiters = [];
  let closed = false;

  return {
    push(chunk) {
      if (closed) {
        return;
      }
      const waiter = waiters.shift();
      if (waiter !== undefined) {
        waiter({ value: chunk, done: false });
      } else {
        chunks.push(chunk);
      }
    },
    close() {
      if (closed) {
        return;
      }
      closed = true;
      for (const waiter of waiters.splice(0)) {
        waiter({ value: undefined, done: true });
      }
    },
    async next() {
      if (chunks.length > 0) {
        return { value: chunks.shift(), done: false };
      }
      if (closed) {
        return { value: undefined, done: true };
      }
      return new Promise((resolve) => {
        waiters.push(resolve);
      });
    },
    [Symbol.asyncIterator]() {
      return this;
    },
  };
}

function execDataPayload(id, stream, chunk) {
  const view = chunk instanceof Uint8Array ? chunk : Buffer.from(chunk);
  const data = view.buffer.slice(view.byteOffset, view.byteOffset + view.byteLength);
  return { id, stream, data };
}

function normalizeSetupStart(input) {
  if (input === null || typeof input !== "object") {
    return { host: "", force: false };
  }
  return {
    host: typeof input.host === "string" ? input.host : "",
    force: input.force === true,
  };
}

function normalizeExecStart(input) {
  if (input === null || typeof input !== "object") {
    throw new Error("exec start request must be an object");
  }
  const rows = normalizeDimension(input.rows, 6, 200, 24);
  const cols = normalizeDimension(input.cols, 20, 500, 80);
  const argv = Array.isArray(input.argv)
    ? input.argv.filter((item) => typeof item === "string").slice(0, 32)
    : [];
  const term = typeof input.term === "string" && input.term.length > 0 && input.term.length < 64
    ? input.term
    : "xterm-256color";
  return { argv, rows, cols, term };
}

function normalizeSize(input) {
  if (input === null || typeof input !== "object") {
    return null;
  }
  return {
    rows: normalizeDimension(input.rows, 6, 200, 24),
    cols: normalizeDimension(input.cols, 20, 500, 80),
  };
}

function normalizeDimension(value, min, max, fallback) {
  if (!Number.isInteger(value)) {
    return fallback;
  }
  return Math.max(min, Math.min(max, value));
}

function ownedExecSession(owner, id) {
  if (typeof id !== "string") {
    return null;
  }
  const session = execSessions.get(id);
  if (session === undefined || session.owner !== owner) {
    return null;
  }
  return session;
}

function closeWindowExecSessions(owner) {
  for (const session of execSessions.values()) {
    if (session.owner === owner) {
      session.stdin.close();
      session.session.close();
      execSessions.delete(session.id);
    }
  }
}

function closeAllExecSessions() {
  for (const session of execSessions.values()) {
    session.stdin.close();
    session.session.close();
  }
  execSessions.clear();
}

function sendToOwner(owner, channel, payload) {
  if (!owner.isDestroyed()) {
    owner.send(channel, payload);
  }
}

function delay(ms, signal) {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve();
      return;
    }
    const timer = setTimeout(resolve, ms);
    signal.addEventListener("abort", () => {
      clearTimeout(timer);
      resolve();
    }, { once: true });
  });
}

function resolvePaths() {
  const configDir = process.env.PORTAL_CONFIG_DIR
    || path.join(app.getPath("userData"), "portal");
  return {
    configDir,
    apiSock: process.env.PORTAL_API_SOCK || path.join(configDir, "api.sock"),
    controlSock: process.env.PORTAL_SOCK || path.join(configDir, "cm.sock"),
    binPath: process.env.PORTAL_BIN || (app.isPackaged
      ? path.join(process.resourcesPath, "portal")
      : path.resolve(appRoot, "../../portal")),
    sdkEntry: process.env.PORTAL_SDK || (app.isPackaged
      ? path.join(process.resourcesPath, "clients-ts", "index.ts")
      : path.resolve(appRoot, "../../clients/ts/src/index.ts")),
  };
}

function isPathInside(root, candidate) {
  const relative = path.relative(root, candidate);
  return relative === "" || (!relative.startsWith("..") && !path.isAbsolute(relative));
}

function contentType(filePath) {
  if (filePath.endsWith(".html")) {
    return "text/html; charset=utf-8";
  }
  if (filePath.endsWith(".css")) {
    return "text/css; charset=utf-8";
  }
  if (filePath.endsWith(".js")) {
    return "text/javascript; charset=utf-8";
  }
  return "application/octet-stream";
}

function toErrorMessage(error) {
  return error instanceof Error ? error.message : String(error);
}
