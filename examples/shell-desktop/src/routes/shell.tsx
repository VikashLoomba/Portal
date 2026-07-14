import { createFileRoute } from "@tanstack/react-router";
import { Terminal } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { useEffect, useRef, useState } from "react";

import { usePortal } from "./__root.tsx";

export const Route = createFileRoute("/shell")({
  component: ShellRoute,
});

function ShellRoute() {
  const portal = usePortal();
  const terminalHost = useRef<HTMLDivElement>(null);
  const startExec = useRef<((argv: string[]) => void) | null>(null);
  const stopExec = useRef<(() => void) | null>(null);
  const [command, setCommand] = useState("");
  const [terminalState, setTerminalState] = useState("starting");

  useEffect(() => {
    const host = terminalHost.current;
    if (host === null) {
      return;
    }
    const terminal = new Terminal({
      cursorBlink: true,
      convertEol: true,
      fontFamily: "SFMono-Regular, Menlo, Consolas, monospace",
      fontSize: 13,
      lineHeight: 1.15,
      scrollback: 2_000,
      theme: {
        background: "#0b1015",
        foreground: "#e6eaee",
        cursor: "#66c2c9",
        selectionBackground: "#31535c",
      },
    });
    terminal.open(host);

    let disposed = false;
    let token = "";
    let socket: WebSocket | null = null;
    let rows = 24;
    let cols = 80;
    let resizeTimer: ReturnType<typeof setTimeout> | null = null;

    const sizeTerminal = (): void => {
      cols = clamp(Math.floor(Math.max(host.clientWidth, 320) / 8.4), 20, 500);
      rows = clamp(Math.floor(Math.max(host.clientHeight, 180) / 16), 6, 200);
      terminal.resize(cols, rows);
    };
    const scheduleResize = (): void => {
      if (resizeTimer !== null) {
        clearTimeout(resizeTimer);
      }
      resizeTimer = setTimeout(() => {
        resizeTimer = null;
        sizeTerminal();
        if (socket?.readyState === WebSocket.OPEN) {
          socket.send(JSON.stringify({ type: "resize", rows, cols }));
        }
      }, 80);
    };
    const observer = new ResizeObserver(scheduleResize);
    observer.observe(host);
    sizeTerminal();

    const openExec = (argv: string[]): void => {
      if (token === "") {
        setTerminalState("desktop exec binding is not ready");
        return;
      }
      socket?.close();
      terminal.reset();
      terminal.write(
        argv.length === 0
          ? "starting shell\r\n"
          : `starting ${argv.join(" ")}\r\n`,
      );
      setTerminalState("starting");
      const protocol = location.protocol === "https:" ? "wss:" : "ws:";
      const next = new WebSocket(
        `${protocol}//${location.host}/exec?cap=${encodeURIComponent(token)}`,
      );
      next.binaryType = "arraybuffer";
      socket = next;
      next.addEventListener("open", () => {
        if (socket !== next) {
          return;
        }
        next.send(
          JSON.stringify({
            type: "start",
            argv,
            term: "xterm-256color",
            rows,
            cols,
          }),
        );
        setTerminalState("running");
        terminal.focus();
      });
      next.addEventListener(
        "message",
        (event) => handleExecMessage(event.data, terminal, setTerminalState),
      );
      next.addEventListener("close", () => {
        if (socket === next) {
          socket = null;
          setTerminalState((current) =>
            current === "running" ? "stopped" : current
          );
        }
      });
      next.addEventListener(
        "error",
        () => setTerminalState("exec bridge connection failed"),
      );
    };
    const stop = (): void => {
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ type: "close" }));
      }
      socket?.close();
      socket = null;
      setTerminalState("stopped");
    };
    startExec.current = openExec;
    stopExec.current = stop;

    const dataSubscription = terminal.onData((data) => {
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ type: "stdin", data }));
      }
    });

    void acquireExecToken().then((value) => {
      if (!disposed) {
        token = value;
        openExec([]);
      }
    }).catch((error: unknown) => setTerminalState(toErrorMessage(error)));

    return () => {
      disposed = true;
      startExec.current = null;
      stopExec.current = null;
      observer.disconnect();
      dataSubscription.dispose();
      if (resizeTimer !== null) {
        clearTimeout(resizeTimer);
      }
      socket?.close();
      terminal.dispose();
    };
  }, []);

  const status = portal.status;
  if (status === null) {
    return null;
  }

  return (
    <div className="app-shell">
      <header>
        <div>
          <p className="eyebrow">Deno Desktop reference</p>
          <h1>Portal Shell</h1>
        </div>
        <span className="socket">{portal.socketPath}</span>
      </header>
      <main className="shell-main">
        <aside className="sidebar">
          <section className="panel">
            <div className="panel-heading">
              <h2>Status</h2>
              <span className="message is-ok">live</span>
            </div>
            <dl className="status-grid">
              <StatusItem label="host" value={status.host} />
              <StatusItem
                label="master"
                value={status.master.up ? "up" : "down"}
              />
              <StatusItem
                label="transport"
                value={status.master.transport || "—"}
              />
              <StatusItem
                label="agent"
                value={status.agent === null
                  ? "offline"
                  : `pid ${status.agent.pid}`}
              />
              <StatusItem
                label="ports"
                value={String(status.ports?.length ?? 0)}
              />
              <StatusItem
                label="forwards"
                value={String(status.forwards?.length ?? 0)}
              />
            </dl>
          </section>
          <section className="panel events-panel">
            <div className="panel-heading">
              <h2>Events</h2>
              <span
                className={portal.eventsError === ""
                  ? "message"
                  : "message is-warn"}
              >
                {portal.eventsError || `${portal.events.length} received`}
              </span>
            </div>
            <ol className="events-list">
              {portal.events.slice().reverse().map((event, index) => (
                <li key={`${event.type}-${portal.events.length - index}`}>
                  <strong>{event.type}</strong>
                  <span>{eventDescription(event)}</span>
                </li>
              ))}
            </ol>
          </section>
        </aside>
        <section className="terminal-panel">
          <div className="terminal-toolbar">
            <form
              onSubmit={(event) => {
                event.preventDefault();
                startExec.current?.(parseCommand(command));
              }}
            >
              <input
                value={command}
                onChange={(event) => setCommand(event.target.value)}
                placeholder="command (empty starts a login shell)"
              />
              <button type="submit">Run</button>
              <button
                type="button"
                className="secondary"
                onClick={() => stopExec.current?.()}
              >
                Stop
              </button>
            </form>
            <span
              className={terminalState.includes("fail") ||
                  terminalState.includes("error")
                ? "message is-error"
                : terminalState === "running"
                ? "message is-ok"
                : "message"}
            >
              {terminalState}
            </span>
          </div>
          <div className="terminal-host" ref={terminalHost} />
        </section>
      </main>
    </div>
  );
}

function StatusItem({ label, value }: { label: string; value: string }) {
  return (
    <div className="stat-item">
      <dt>{label}</dt>
      <dd>{value}</dd>
    </div>
  );
}

function handleExecMessage(
  data: unknown,
  terminal: Terminal,
  setState: (state: string) => void,
): void {
  if (data instanceof ArrayBuffer) {
    const frame = new Uint8Array(data);
    if (frame.byteLength > 1 && (frame[0] === 1 || frame[0] === 2)) {
      terminal.write(frame.slice(1));
    }
    return;
  }
  if (typeof data !== "string") {
    return;
  }
  try {
    const value: unknown = JSON.parse(data);
    if (!isRecord(value) || typeof value.type !== "string") {
      return;
    }
    if (value.type === "exit" && typeof value.code === "number") {
      terminal.write(`\r\n[exit ${value.code}]\r\n`);
      setState(`exit ${value.code}`);
    } else if (value.type === "error" && typeof value.message === "string") {
      terminal.write(`\r\n[error] ${value.message}\r\n`);
      setState(value.message);
    }
  } catch {
    setState("invalid exec bridge response");
  }
}

function eventDescription(
  event: {
    status?: { host: string; master: { up: boolean } } | null;
    notify?: { title: string; body: string } | null;
  },
): string {
  if (event.notify !== undefined && event.notify !== null) {
    return [event.notify.title, event.notify.body].filter(Boolean).join(" · ");
  }
  if (event.status !== undefined && event.status !== null) {
    return `${event.status.host} · master ${
      event.status.master.up ? "up" : "down"
    }`;
  }
  return "event";
}

// Packaged desktop mode delivers the exec capability through the injected
// `bindings.portalBootstrap` channel. Under the framework dev server that
// channel is absent, so the renderer fetches the capability from the
// loopback-only, origin-checked dev-exec-token endpoint instead.
async function acquireExecToken(): Promise<string> {
  if (
    typeof bindings !== "undefined" &&
    typeof bindings.portalBootstrap === "function"
  ) {
    const bootstrap = await bindings.portalBootstrap();
    return bootstrap.execToken;
  }
  const response = await fetch("/api/dev-exec-token");
  if (!response.ok) {
    throw new Error(`dev exec token request failed (${response.status})`);
  }
  const value: unknown = await response.json();
  if (!isRecord(value) || typeof value.execToken !== "string") {
    throw new Error("dev exec token response was malformed");
  }
  return value.execToken;
}

function parseCommand(command: string): string[] {
  const trimmed = command.trim();
  return trimmed === "" ? [] : trimmed.split(/\s+/).slice(0, 32);
}

function clamp(value: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, value));
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function toErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
