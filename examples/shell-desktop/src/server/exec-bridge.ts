import { exec } from "@portal/sdk";
import type { ExecSession } from "@portal/sdk";

import {
  isRecord,
  normalizeExecSize,
  normalizeExecStart,
} from "./exec-normalize.ts";
import type { ExecStartRequest } from "./exec-normalize.ts";

interface RegisteredExecSession {
  owner: number;
  socket: WebSocket;
  stdin: InputQueue;
  session: ExecSession | null;
}

const capabilities = new Map<string, number>();
const sessions = new Set<RegisteredExecSession>();
const encoder = new TextEncoder();
let apiSocketPath = "";

export function configureExecBridge(socketPath: string): void {
  apiSocketPath = socketPath;
}

export function registerWindowCapability(owner: number): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  const token = Array.from(
    bytes,
    (value) => value.toString(16).padStart(2, "0"),
  ).join("");
  capabilities.set(token, owner);
  return token;
}

export function invalidateWindowCapability(token: string): void {
  const owner = capabilities.get(token);
  capabilities.delete(token);
  if (owner !== undefined) {
    closeExecSessionsForOwner(owner);
  }
}

export function closeExecSessionsForOwner(owner: number): void {
  for (const registered of sessions) {
    if (registered.owner === owner) {
      closeRegisteredSession(registered);
    }
  }
}

export function closeAllExecSessions(): void {
  for (const registered of sessions) {
    closeRegisteredSession(registered);
  }
  capabilities.clear();
}

export function handleExecUpgrade(request: Request): Response {
  const url = new URL(request.url);
  if (!sameOriginRequest(request, url)) {
    return new Response("forbidden", { status: 403 });
  }
  const token = url.searchParams.get("cap") ?? "";
  const owner = capabilityOwner(token);
  if (owner === null) {
    return new Response("forbidden", { status: 403 });
  }
  if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
    return new Response("websocket upgrade required", { status: 426 });
  }

  const upgraded = Deno.upgradeWebSocket(request);
  const registered: RegisteredExecSession = {
    owner,
    socket: upgraded.socket,
    stdin: new InputQueue(),
    session: null,
  };
  sessions.add(registered);
  upgraded.socket.addEventListener(
    "message",
    (event) => handleMessage(registered, event.data),
  );
  upgraded.socket.addEventListener(
    "close",
    () => closeRegisteredSession(registered),
  );
  upgraded.socket.addEventListener(
    "error",
    () => closeRegisteredSession(registered),
  );
  return upgraded.response;
}

function handleMessage(
  registered: RegisteredExecSession,
  data: string | ArrayBuffer | Blob,
): void {
  if (typeof data !== "string") {
    sendErrorAndClose(
      registered,
      "exec bridge accepts JSON control messages only",
    );
    return;
  }

  let value: unknown;
  try {
    value = JSON.parse(data);
  } catch {
    sendErrorAndClose(registered, "invalid exec control message");
    return;
  }
  if (!isRecord(value) || typeof value.type !== "string") {
    sendErrorAndClose(registered, "invalid exec control message");
    return;
  }

  if (value.type === "start") {
    if (registered.session !== null) {
      sendErrorAndClose(registered, "exec session already started");
      return;
    }
    const request = normalizeExecStart(value);
    if (request === null) {
      sendErrorAndClose(registered, "invalid exec start request");
      return;
    }
    startSession(registered, request);
    return;
  }
  if (value.type === "stdin" && typeof value.data === "string") {
    if (value.data.length > 0) {
      registered.stdin.push(encoder.encode(value.data));
    }
    return;
  }
  if (value.type === "resize") {
    if (registered.session === null) {
      return;
    }
    const size = normalizeExecSize(value);
    if (size !== null) {
      registered.session.resize(size.rows, size.cols).catch(
        (error: unknown) => {
          sendJson(registered.socket, {
            type: "error",
            message: toErrorMessage(error),
          });
        },
      );
    }
    return;
  }
  if (value.type === "close") {
    closeRegisteredSession(registered);
    return;
  }
  sendErrorAndClose(registered, "unsupported exec control message");
}

function startSession(
  registered: RegisteredExecSession,
  request: ExecStartRequest,
): void {
  if (apiSocketPath === "") {
    sendErrorAndClose(registered, "portal paths are unavailable");
    return;
  }

  const session = exec(apiSocketPath, {
    argv: request.argv,
    pty: {
      term: request.term,
      rows: request.rows,
      cols: request.cols,
    },
    stdin: registered.stdin,
    stdout(chunk) {
      sendBinary(registered.socket, 1, chunk);
    },
    stderr(chunk) {
      sendBinary(registered.socket, 2, chunk);
    },
  });
  registered.session = session;
  session.result.then(
    (result) =>
      sendJson(registered.socket, { type: "exit", code: result.code }),
    (error: unknown) =>
      sendJson(registered.socket, {
        type: "error",
        message: toErrorMessage(error),
      }),
  ).finally(() => {
    if (registered.socket.readyState === WebSocket.OPEN) {
      registered.socket.close();
    }
    closeRegisteredSession(registered);
  });
}

function sameOriginRequest(request: Request, url: URL): boolean {
  const origin = request.headers.get("origin");
  const host = request.headers.get("host");
  return origin === url.origin && host === url.host;
}

function capabilityOwner(candidate: string): number | null {
  for (const [token, owner] of capabilities) {
    if (constantTimeEqual(candidate, token)) {
      return owner;
    }
  }
  return null;
}

function constantTimeEqual(left: string, right: string): boolean {
  const length = Math.max(left.length, right.length);
  let mismatch = left.length ^ right.length;
  for (let index = 0; index < length; index += 1) {
    mismatch |= (left.charCodeAt(index) || 0) ^ (right.charCodeAt(index) || 0);
  }
  return mismatch === 0;
}

function sendBinary(
  socket: WebSocket,
  stream: number,
  chunk: Uint8Array,
): void {
  if (socket.readyState !== WebSocket.OPEN) {
    return;
  }
  const frame = new Uint8Array(chunk.byteLength + 1);
  frame[0] = stream;
  frame.set(chunk, 1);
  socket.send(frame);
}

function sendJson(socket: WebSocket, value: Record<string, unknown>): void {
  if (socket.readyState === WebSocket.OPEN) {
    socket.send(JSON.stringify(value));
  }
}

function sendErrorAndClose(
  registered: RegisteredExecSession,
  message: string,
): void {
  sendJson(registered.socket, { type: "error", message });
  if (registered.socket.readyState === WebSocket.OPEN) {
    registered.socket.close(1008, "invalid exec request");
  }
  closeRegisteredSession(registered);
}

function closeRegisteredSession(registered: RegisteredExecSession): void {
  if (!sessions.delete(registered)) {
    return;
  }
  registered.stdin.close();
  registered.session?.close();
  if (registered.socket.readyState === WebSocket.OPEN) {
    registered.socket.close();
  }
}

class InputQueue
  implements AsyncIterable<Uint8Array>, AsyncIterator<Uint8Array> {
  #chunks: Uint8Array[] = [];
  #waiters: Array<(result: IteratorResult<Uint8Array>) => void> = [];
  #closed = false;

  push(chunk: Uint8Array): void {
    if (this.#closed) {
      return;
    }
    const waiter = this.#waiters.shift();
    if (waiter === undefined) {
      this.#chunks.push(chunk);
    } else {
      waiter({ value: chunk, done: false });
    }
  }

  close(): void {
    if (this.#closed) {
      return;
    }
    this.#closed = true;
    for (const waiter of this.#waiters.splice(0)) {
      waiter({ value: undefined, done: true });
    }
  }

  next(): Promise<IteratorResult<Uint8Array>> {
    const chunk = this.#chunks.shift();
    if (chunk !== undefined) {
      return Promise.resolve({ value: chunk, done: false });
    }
    if (this.#closed) {
      return Promise.resolve({ value: undefined, done: true });
    }
    return new Promise((resolve) => this.#waiters.push(resolve));
  }

  [Symbol.asyncIterator](): AsyncIterator<Uint8Array> {
    return this;
  }
}

function toErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
