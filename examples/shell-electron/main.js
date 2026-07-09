"use strict";

const fs = require("node:fs/promises");
const { stripTypeScriptTypes } = require("node:module");
const os = require("node:os");
const path = require("node:path");
const { pathToFileURL } = require("node:url");

const { app, BrowserWindow, ipcMain, protocol } = require("electron");

const appRoot = __dirname;
const rendererScheme = "portal-shell";
const socketPath = resolveSocketPath();

let portalApi;
let mainWindow;
let nextExecId = 1;
const execSessions = new Map();

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

app.whenReady().then(async () => {
  registerRendererProtocol();
  registerIpc();
  mainWindow = createWindow();
  startStatusLoop(mainWindow);
  startEventsLoop(mainWindow);

  app.on("activate", () => {
    if (BrowserWindow.getAllWindows().length === 0) {
      mainWindow = createWindow();
      startStatusLoop(mainWindow);
      startEventsLoop(mainWindow);
    }
  });
});

app.on("window-all-closed", () => {
  closeAllExecSessions();
  if (process.platform !== "darwin") {
    app.quit();
  }
});

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
    return { socketPath };
  });

  ipcMain.handle("portal:exec:start", async (event, input) => {
    const request = normalizeExecStart(input);
    const id = String(nextExecId++);
    const stdin = createInputQueue();
    const api = await loadPortalApi();
    const owner = event.sender;

    const session = api.exec(socketPath, {
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
    const url = pathToFileURL(path.resolve(appRoot, "../../clients/ts/src/index.ts"));
    portalApi = import(url.href);
  }
  return portalApi;
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
      const client = api.createClient(socketPath);
      const status = await client.status({ signal: controller.signal });
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
        for await (const event of api.events(socketPath, { signal: controller.signal })) {
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

function resolveSocketPath() {
  return process.env.PORTAL_API_SOCK
    || process.env.PORTAL_APISOCK
    || process.env.PORTAL_SOCKET
    || path.join(os.homedir(), ".config", "portal", "api.sock");
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
