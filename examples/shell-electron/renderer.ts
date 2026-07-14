import type { Event, Notify, SetupEvent, SetupRequest, Status } from "../../clients/ts/src/index.ts";
import type { Terminal as XtermTerminal, TerminalConstructor } from "@xterm/xterm";

type Unsubscribe = () => void;

interface PortalBridge {
  config(): Promise<{ socketPath: string }>;
  sidecarState(): Promise<SidecarState>;
  onSidecarReady(callback: () => void): Unsubscribe;
  onSidecarError(callback: (error: BridgeError) => void): Unsubscribe;
  firstRunState(): Promise<{ needsSetup: boolean }>;
  startSetup(request: SetupRequest): Promise<SetupEvent | null>;
  onSetupStep(callback: (event: SetupEvent) => void): Unsubscribe;
  onStatus(callback: (status: Status) => void): Unsubscribe;
  onStatusError(callback: (error: BridgeError) => void): Unsubscribe;
  onEvent(callback: (event: Event) => void): Unsubscribe;
  onEventError(callback: (error: BridgeError) => void): Unsubscribe;
  startExec(request: ExecStartRequest): Promise<{ id: string }>;
  writeExec(id: string, data: string): void;
  resizeExec(id: string, rows: number, cols: number): void;
  closeExec(id: string): void;
  onExecData(callback: (chunk: ExecChunk) => void): Unsubscribe;
  onExecExit(callback: (result: ExecExit) => void): Unsubscribe;
  onExecError(callback: (error: ExecError) => void): Unsubscribe;
}

interface BridgeError {
  message: string;
}

interface SidecarState {
  phase: "starting" | "ready" | "error";
  error: string;
}

interface ExecStartRequest {
  argv: string[];
  rows: number;
  cols: number;
  term: string;
}

interface ExecChunk {
  id: string;
  stream: "stdout" | "stderr";
  data: ArrayBuffer;
}

interface ExecExit {
  id: string;
  code: number;
}

interface ExecError {
  id: string;
  message: string;
}

const globals = globalThis as typeof globalThis & {
  portal?: PortalBridge;
  Terminal?: TerminalConstructor;
};

const portal = requireGlobal(globals.portal, "portal preload bridge");
const Terminal = requireGlobal(globals.Terminal, "xterm.js");

const sidecarPanelEl = requireElement("sidecar-panel");
const sidecarConnectingEl = requireElement("sidecar-connecting");
const sidecarErrorEl = requireElement("sidecar-error");
const sidecarErrorMessageEl = requireElement("sidecar-error-message");
const firstRunPanelEl = requireElement("first-run-panel");
const firstRunHostEl = requireInput("firstrun-host");
const firstRunForceEl = requireInput("firstrun-force");
const firstRunStartEl = requireButton("firstrun-start");
const firstRunStepsEl = requireElement("firstrun-steps");
const firstRunMessageEl = requireElement("firstrun-message");
const appShellEl = requireElement("app-shell");
const socketPathEl = requireElement("socket-path");
const statusGridEl = requireElement("status-grid");
const statusMessageEl = requireElement("status-message");
const eventsListEl = requireElement("events-list");
const eventsMessageEl = requireElement("events-message");
const terminalHostEl = requireElement("terminal-host");
const terminalMessageEl = requireElement("terminal-message");
const commandInputEl = requireInput("command-input");
const startButtonEl = requireButton("start-exec");
const stopButtonEl = requireButton("stop-exec");

let term: XtermTerminal | null = null;
let activeExecId: string | null = null;
let lastRows = 24;
let lastCols = 80;
let resizeTimer: number | null = null;
let entered = false;
let sidecarFailed = false;
let inFirstRun = false;
let shellRevealed = false;
let setupRunning = false;

void initialize();

async function initialize(): Promise<void> {
  const config = await portal.config();
  socketPathEl.textContent = config.socketPath;

  portal.onStatus(renderStatus);
  portal.onStatusError((error) => {
    statusMessageEl.textContent = error.message;
    statusMessageEl.className = "message is-error";
  });
  portal.onEvent((event) => {
    renderEvent(event);
    maybeRevealShell(event);
  });
  portal.onEventError((error) => {
    eventsMessageEl.textContent = error.message;
    eventsMessageEl.className = "message is-error";
  });
  portal.onExecData(writeExecData);
  portal.onExecExit(handleExecExit);
  portal.onExecError(handleExecError);
  portal.onSetupStep(handleSetupStep);

  startButtonEl.addEventListener("click", () => {
    void startExecFromInput();
  });
  stopButtonEl.addEventListener("click", stopExec);
  commandInputEl.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      void startExecFromInput();
    }
  });
  firstRunStartEl.addEventListener("click", () => {
    void startSetup();
  });
  firstRunHostEl.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      void startSetup();
    }
  });
  window.addEventListener("resize", scheduleResize);

  portal.onSidecarReady(() => {
    void enterApp();
  });
  portal.onSidecarError(showSidecarError);

  const boot = await portal.sidecarState();
  if (boot.phase === "error") {
    showSidecarError({ message: boot.error });
    return;
  }
  if (boot.phase === "ready") {
    void enterApp();
    return;
  }
  if (!entered && !sidecarFailed) {
    showConnecting();
  }
}

async function enterApp(): Promise<void> {
  if (entered || sidecarFailed) {
    return;
  }
  entered = true;

  try {
    const state = await portal.firstRunState();
    if (state.needsSetup) {
      showFirstRun();
      return;
    }
    revealShell();
  } catch (error) {
    showSidecarError({ message: toErrorMessage(error) });
  }
}

function showConnecting(): void {
  sidecarPanelEl.hidden = false;
  sidecarConnectingEl.hidden = false;
  sidecarErrorEl.hidden = true;
  firstRunPanelEl.hidden = true;
  appShellEl.hidden = true;
}

function showSidecarError(error: BridgeError): void {
  sidecarFailed = true;
  sidecarErrorMessageEl.textContent = error.message || "Portal sidecar failed to start.";
  sidecarPanelEl.hidden = false;
  sidecarConnectingEl.hidden = true;
  sidecarErrorEl.hidden = false;
  firstRunPanelEl.hidden = true;
  appShellEl.hidden = true;
}

function showFirstRun(): void {
  inFirstRun = true;
  sidecarPanelEl.hidden = true;
  firstRunPanelEl.hidden = false;
  appShellEl.hidden = true;
  firstRunHostEl.focus();
}

function revealShell(): void {
  if (shellRevealed || sidecarFailed) {
    return;
  }
  shellRevealed = true;
  inFirstRun = false;
  sidecarPanelEl.hidden = true;
  firstRunPanelEl.hidden = true;
  appShellEl.hidden = false;
  setupTerminal();
  void startExec([]);
}

async function startSetup(): Promise<void> {
  if (setupRunning) {
    return;
  }

  const host = firstRunHostEl.value.trim();
  if (host === "") {
    firstRunMessageEl.textContent = "Enter the SSH host for your dev box.";
    firstRunMessageEl.className = "message is-error";
    firstRunHostEl.focus();
    return;
  }

  setupRunning = true;
  firstRunStartEl.disabled = true;
  firstRunStepsEl.replaceChildren();
  firstRunMessageEl.textContent = "setting up";
  firstRunMessageEl.className = "message";
  const request: SetupRequest = {
    host,
    force: firstRunForceEl.checked,
  };

  try {
    const done = await portal.startSetup(request);
    if (done?.step === "done" && done.status === "ok") {
      revealShell();
    } else if (done === null) {
      firstRunMessageEl.textContent = "Setup ended without a verdict.";
      firstRunMessageEl.className = "message is-error";
    }
  } catch (error) {
    firstRunMessageEl.textContent = toErrorMessage(error);
    firstRunMessageEl.className = "message is-error";
  } finally {
    setupRunning = false;
    firstRunStartEl.disabled = false;
  }
}

function handleSetupStep(event: SetupEvent): void {
  const item = document.createElement("li");
  item.className = event.status === "fail"
    ? "setup-step is-error"
    : event.status === "warn"
      ? "setup-step is-warn"
      : event.status === "ok"
        ? "setup-step is-ok"
        : "setup-step";

  const summary = document.createElement("strong");
  summary.textContent = `${event.step} · ${event.status}`;
  item.append(summary);

  const detail = event.line || event.error?.message || "";
  if (detail !== "") {
    const line = document.createElement("span");
    line.textContent = detail;
    item.append(line);
  }
  firstRunStepsEl.append(item);
  item.scrollIntoView({ block: "nearest" });

  firstRunMessageEl.textContent = event.step === "done"
    ? event.status === "ok" ? "configured" : "setup failed"
    : `${event.step}: ${event.status}`;
  firstRunMessageEl.className = event.status === "fail"
    ? "message is-error"
    : event.status === "ok" && event.step === "done"
      ? "message is-ok"
      : "message";

  if (event.step === "done" && event.status === "ok") {
    revealShell();
  }
}

function maybeRevealShell(event: Event): void {
  if (inFirstRun && event.status !== null && event.status !== undefined && event.status.host !== "") {
    revealShell();
  }
}

function setupTerminal(): void {
  if (term !== null) {
    return;
  }
  term = new Terminal({
    cursorBlink: true,
    fontFamily: "SFMono-Regular, Menlo, Consolas, monospace",
    fontSize: 13,
    lineHeight: 1.15,
    scrollback: 2000,
    convertEol: true,
    theme: {
      background: "#0d1117",
      foreground: "#d7dde5",
      cursor: "#f4c95d",
      selectionBackground: "#315f72",
      black: "#101418",
      red: "#e26d5a",
      green: "#7bc47f",
      yellow: "#f4c95d",
      blue: "#70a5d8",
      magenta: "#b58ad8",
      cyan: "#66c2c9",
      white: "#d7dde5",
      brightBlack: "#58616d",
      brightRed: "#ff8b75",
      brightGreen: "#9fdb9f",
      brightYellow: "#ffe08a",
      brightBlue: "#9fc7ed",
      brightMagenta: "#d6b4ef",
      brightCyan: "#93dde2",
      brightWhite: "#ffffff",
    },
  });
  term.open(terminalHostEl);
  term.onData((data) => {
    if (activeExecId !== null) {
      portal.writeExec(activeExecId, data);
    }
  });
  applyTerminalSize();
  term.focus();
}

async function startExecFromInput(): Promise<void> {
  const argv = parseCommand(commandInputEl.value);
  await startExec(argv);
}

async function startExec(argv: string[]): Promise<void> {
  if (term === null) {
    return;
  }
  if (activeExecId !== null) {
    stopExec();
  }
  applyTerminalSize();
  term.reset();
  term.write(argv.length === 0 ? "starting shell\r\n" : `starting ${argv.join(" ")}\r\n`);
  setTerminalState("starting", false);

  try {
    const started = await portal.startExec({
      argv,
      rows: lastRows,
      cols: lastCols,
      term: "xterm-256color",
    });
    activeExecId = started.id;
    setTerminalState("running", true);
    term.focus();
  } catch (error) {
    setTerminalState(toErrorMessage(error), false, true);
  }
}

function stopExec(): void {
  if (activeExecId !== null) {
    portal.closeExec(activeExecId);
    activeExecId = null;
  }
  setTerminalState("stopped", false);
}

function writeExecData(chunk: ExecChunk): void {
  if (term === null || chunk.id !== activeExecId) {
    return;
  }
  const data = new Uint8Array(chunk.data);
  term.write(data);
}

function handleExecExit(result: ExecExit): void {
  if (result.id !== activeExecId) {
    return;
  }
  activeExecId = null;
  if (term !== null) {
    term.write(`\r\n[exit ${result.code}]\r\n`);
  }
  setTerminalState(`exit ${result.code}`, false);
}

function handleExecError(error: ExecError): void {
  if (error.id !== activeExecId) {
    return;
  }
  activeExecId = null;
  if (term !== null) {
    term.write(`\r\n[error] ${error.message}\r\n`);
  }
  setTerminalState(error.message, false, true);
}

function scheduleResize(): void {
  if (resizeTimer !== null) {
    window.clearTimeout(resizeTimer);
  }
  resizeTimer = window.setTimeout(() => {
    resizeTimer = null;
    applyTerminalSize();
    if (activeExecId !== null) {
      portal.resizeExec(activeExecId, lastRows, lastCols);
    }
  }, 80);
}

function applyTerminalSize(): void {
  if (term === null) {
    return;
  }
  const width = Math.max(terminalHostEl.clientWidth, 320);
  const height = Math.max(terminalHostEl.clientHeight, 180);
  lastCols = clamp(Math.floor(width / 8.4), 20, 500);
  lastRows = clamp(Math.floor(height / 16), 6, 200);
  term.resize(lastCols, lastRows);
}

function renderStatus(status: Status): void {
  statusMessageEl.textContent = "live";
  statusMessageEl.className = "message is-ok";
  statusGridEl.replaceChildren(
    statItem("host", status.host),
    statItem("master", status.master.up ? "up" : "down"),
    statItem("transport", status.master.transport || "unknown"),
    statItem("agent", formatAgent(status)),
    statItem("ports", String(status.ports?.length ?? 0)),
    statItem("forwards", String(status.forwards?.length ?? 0)),
    statItem("subscribers", String(status.health.eventsSubscribers)),
    statItem("notify drops", String(status.health.droppedNotifyCount)),
  );
}

function renderEvent(event: Event): void {
  eventsMessageEl.textContent = event.type;
  eventsMessageEl.className = "message is-ok";

  const item = document.createElement("li");
  item.className = event.notify === null || event.notify === undefined ? "event-row" : "event-row notify-row";

  const title = document.createElement("div");
  title.className = "event-title";
  title.textContent = event.notify === null || event.notify === undefined
    ? event.type
    : formatNotifyTitle(event.notify);

  const body = document.createElement("div");
  body.className = "event-body";
  body.textContent = formatEventBody(event);

  const time = document.createElement("time");
  time.dateTime = new Date().toISOString();
  time.textContent = new Date().toLocaleTimeString();

  item.append(title, body, time);
  eventsListEl.prepend(item);
  while (eventsListEl.children.length > 100) {
    eventsListEl.removeChild(requireGlobal(eventsListEl.lastElementChild, "event row"));
  }
}

function statItem(label: string, value: string): HTMLElement {
  const item = document.createElement("div");
  item.className = "stat-item";

  const labelEl = document.createElement("span");
  labelEl.className = "stat-label";
  labelEl.textContent = label;

  const valueEl = document.createElement("strong");
  valueEl.textContent = value;

  item.append(labelEl, valueEl);
  return item;
}

function formatAgent(status: Status): string {
  if (status.agent === null) {
    return "none";
  }
  return `${status.agent.pid} ${status.agent.sha.slice(0, 8)}`;
}

function formatNotifyTitle(notify: Notify): string {
  const title = notify.title === "" ? "notification" : notify.title;
  return notify.seq > 0 ? `${title} #${notify.seq}` : title;
}

function formatEventBody(event: Event): string {
  if (event.notify !== null && event.notify !== undefined) {
    const parts = [
      event.notify.subtitle,
      event.notify.body,
      event.notify.source,
      event.notify.verified ? "verified" : "",
    ].filter((part) => part !== "");
    return parts.join(" · ");
  }
  if (event.status !== null && event.status !== undefined) {
    return `${event.status.host} ${event.status.master.up ? "master up" : "master down"}`;
  }
  return "event";
}

function setTerminalState(message: string, running: boolean, failed = false): void {
  terminalMessageEl.textContent = message;
  terminalMessageEl.className = failed ? "message is-error" : running ? "message is-ok" : "message";
  startButtonEl.disabled = running;
  stopButtonEl.disabled = !running;
}

function parseCommand(text: string): string[] {
  const trimmed = text.trim();
  if (trimmed === "") {
    return [];
  }
  return trimmed.split(/\s+/u);
}

function requireElement(id: string): HTMLElement {
  const element = document.getElementById(id);
  if (element === null) {
    throw new Error(`missing #${id}`);
  }
  return element;
}

function requireInput(id: string): HTMLInputElement {
  const element = requireElement(id);
  if (!(element instanceof HTMLInputElement)) {
    throw new Error(`#${id} is not an input`);
  }
  return element;
}

function requireButton(id: string): HTMLButtonElement {
  const element = requireElement(id);
  if (!(element instanceof HTMLButtonElement)) {
    throw new Error(`#${id} is not a button`);
  }
  return element;
}

function requireGlobal<T>(value: T | undefined | null, name: string): T {
  if (value === undefined || value === null) {
    throw new Error(`missing ${name}`);
  }
  return value;
}

function toErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function clamp(value: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, value));
}
