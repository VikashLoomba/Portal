import { LifecycleStore } from "./lifecycle.ts";
import { resolvePortalPaths } from "./paths.ts";
import type { PortalPaths } from "./paths.ts";
import { SidecarSupervisor } from "./supervisor.ts";
import {
  closeAllExecSessions,
  configureExecBridge,
  invalidateWindowCapability,
  registerWindowCapability,
} from "./exec-bridge.ts";

export const lifecycle = new LifecycleStore();

let paths: PortalPaths | null = null;
let supervisor: SidecarSupervisor | null = null;
let quitPromise: Promise<void> | null = null;
const windows = new Map<
  number,
  { window: Deno.BrowserWindow; token: string }
>();

// The framework dev server (`deno desktop --hmr`, or plain Vite) loads this
// server entry in a process where the desktop APIs are absent, so
// `Deno.BrowserWindow`/`Deno.dock` must never be touched there. Packaged builds
// run inside the desktop runtime where both are present. Feature-detect the
// window constructor once and drive window/dock/menu management only then.
const desktopAvailable = typeof Deno.BrowserWindow === "function";

if (desktopAvailable) {
  createWindow(true);
}
try {
  paths = resolvePortalPaths();
  lifecycle.setSocketPath(paths.apiSock);
  configureExecBridge(paths.apiSock);
  supervisor = new SidecarSupervisor(paths, lifecycle);
  void supervisor.start();
} catch (error) {
  lifecycle.setError(`Unable to initialize portal: ${toErrorMessage(error)}`);
}

installSignal("SIGINT");
installSignal("SIGTERM");
if (desktopAvailable && Deno.build.os === "darwin") {
  Deno.dock.addEventListener("reopen", (event) => {
    if (!event.detail.hasVisibleWindows && windows.size === 0) {
      createWindow(false);
    }
  });
}

export function portalPaths(): PortalPaths {
  if (paths === null) {
    throw new Error(
      lifecycle.snapshot().error || "portal paths are unavailable",
    );
  }
  return paths;
}

function createWindow(initial: boolean): void {
  const window = new Deno.BrowserWindow({
    title: "Portal Shell",
    width: 1180,
    height: 820,
  });
  const token = registerWindowCapability(window.windowId);
  windows.set(window.windowId, { window, token });
  // BrowserWindow.bind requires Promise-returning callbacks.
  // deno-lint-ignore require-await
  window.bind("portalBootstrap", async () => ({ execToken: token }));
  window.setApplicationMenu([
    {
      submenu: {
        label: "Portal Shell",
        items: [
          {
            item: {
              label: "Quit Portal Shell",
              id: "quit",
              accelerator: "CmdOrCtrl+Q",
              enabled: true,
            },
          },
        ],
      },
    },
    {
      submenu: {
        label: "Edit",
        items: [
          { role: { role: "undo" } },
          { role: { role: "redo" } },
          "separator",
          { role: { role: "cut" } },
          { role: { role: "copy" } },
          { role: { role: "paste" } },
          { role: { role: "selectAll" } },
        ],
      },
    },
  ]);
  window.addEventListener("menuclick", (event) => {
    if (event.detail.id === "quit") {
      void requestQuit(0);
    }
  });
  window.addEventListener("close", () => {
    windows.delete(window.windowId);
    invalidateWindowCapability(token);
    if (windows.size === 0 && Deno.build.os !== "darwin") {
      void requestQuit(0);
    }
  });

  if (!initial) {
    window.navigate(desktopOrigin());
  }
}

function desktopOrigin(): string {
  const address = Deno.env.get("DENO_SERVE_ADDRESS") ?? "";
  const port = address.split(":").pop();
  if (port === undefined || !/^\d+$/.test(port)) {
    throw new Error(
      "DENO_SERVE_ADDRESS does not contain a desktop server port",
    );
  }
  return `http://127.0.0.1:${port}/`;
}

function installSignal(signal: Deno.Signal): void {
  try {
    Deno.addSignalListener(signal, () => void requestQuit(0));
  } catch {
    // Signal availability is platform-specific; the native Quit item remains authoritative.
  }
}

function requestQuit(code: number): Promise<void> {
  if (quitPromise !== null) {
    return quitPromise;
  }
  quitPromise = shutdown(code);
  return quitPromise;
}

async function shutdown(code: number): Promise<void> {
  closeAllExecSessions();
  await supervisor?.stop();
  Deno.exit(code);
}

function toErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
