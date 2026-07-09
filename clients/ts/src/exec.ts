import { randomBytes } from "node:crypto";
import { connect } from "node:net";
import type { Socket } from "node:net";

import {
  ExecStreamError,
  ExecStreamExit,
  ExecStreamStderr,
  ExecStreamStdin,
  ExecStreamStdout,
  ExecStreamWinch,
  decodeExecFrame,
  encodeExecFrame,
} from "./cbor.ts";
import type { ExecFrameInit } from "./cbor.ts";
import { apiErrorFromStatusBody } from "./http.ts";
import { OpBinary, OpClose, OpPing, WebSocketFrameReader, acceptKey, writeFrame, writePong } from "./wsframe.ts";

export const PTY_UNSUPPORTED_MESSAGE = "daemon does not support PTY (restart the daemon after upgrading)";

const responseHeadLimit = 64 * 1024;
const errorBodyLimit = 1 << 20;
const crlfcrlf = Uint8Array.of(13, 10, 13, 10);
const textDecoder = new TextDecoder();

export class PtyUnsupportedError extends Error {
  constructor() {
    super(PTY_UNSUPPORTED_MESSAGE);
    this.name = "PtyUnsupportedError";
  }
}

export interface ExecPtyOptions {
  term?: string;
  rows: number;
  cols: number;
}

export interface WinchSize {
  rows: number;
  cols: number;
}

export type ByteChunk = Uint8Array | string;
export type StdinSource = ByteChunk | Iterable<ByteChunk> | AsyncIterable<ByteChunk> | null;
export type ByteSink = ((chunk: Uint8Array) => void | Promise<void>) | NodeJS.WritableStream;
export type WinchSource = Iterable<WinchSize> | AsyncIterable<WinchSize>;

export interface ExecOptions {
  argv: string[];
  pty?: ExecPtyOptions;
  stdin?: StdinSource;
  stdout?: ByteSink;
  stderr?: ByteSink;
  winch?: WinchSource;
  signal?: AbortSignal;
}

export interface ExecResult {
  code: number;
}

export interface ExecSession {
  result: Promise<ExecResult>;
  resize(rows: number, cols: number): Promise<void>;
  close(): void;
}

export function exec(socketPath: string, options: ExecOptions): ExecSession {
  let socket: Socket | null = null;
  let closed = false;
  let startedSettled = false;
  let pumpError: Error | null = null;
  const writer = new SerialWriter();
  const lifecycle = new AbortController();

  let resolveStarted: () => void = () => {};
  let rejectStarted: (error: Error) => void = () => {};
  const started = new Promise<void>((resolve, reject) => {
    resolveStarted = resolve;
    rejectStarted = reject;
  });
  void started.catch(() => {});

  const markStarted = (): void => {
    if (!startedSettled) {
      startedSettled = true;
      resolveStarted();
    }
  };
  const failStarted = (error: Error): void => {
    if (!startedSettled) {
      startedSettled = true;
      rejectStarted(error);
    }
  };
  const closeSession = (): void => {
    closed = true;
    lifecycle.abort();
    socket?.destroy();
  };
  const sendExecFrame = (frame: ExecFrameInit): Promise<void> => {
    return writer.run(async () => {
      if (closed || socket === null || socket.destroyed) {
        throw new Error("exec: session is closed");
      }
      await writeFrame(socket, OpBinary, encodeExecFrame(frame), true);
    });
  };

  const result = runExecSession(socketPath, options, {
    setSocket(next: Socket): void {
      socket = next;
    },
    markStarted,
    failStarted,
    closeSession,
    lifecycle,
    recordPumpError(error: Error): void {
      pumpError = error;
      socket?.destroy();
    },
    getPumpError(): Error | null {
      return pumpError;
    },
    getSocket(): Socket | null {
      return socket;
    },
    sendExecFrame,
    writer,
  }).finally(() => {
    closeSession();
  });

  return {
    result,
    async resize(rows: number, cols: number): Promise<void> {
      if (options.pty === undefined) {
        throw new Error("exec: resize requires PTY mode");
      }
      await started;
      await sendExecFrame({ stream: ExecStreamWinch, rows, cols });
    },
    close(): void {
      closeSession();
    },
  };
}

export async function runExec(socketPath: string, options: ExecOptions): Promise<ExecResult> {
  return exec(socketPath, options).result;
}

interface SessionHooks {
  setSocket(socket: Socket): void;
  markStarted(): void;
  failStarted(error: Error): void;
  closeSession(): void;
  lifecycle: AbortController;
  recordPumpError(error: Error): void;
  getPumpError(): Error | null;
  getSocket(): Socket | null;
  sendExecFrame(frame: ExecFrameInit): Promise<void>;
  writer: SerialWriter;
}

async function runExecSession(socketPath: string, options: ExecOptions, hooks: SessionHooks): Promise<ExecResult> {
  if (options.signal?.aborted === true) {
    throw abortError(options.signal);
  }

  const socket = await connectUnix(socketPath, options.signal);
  hooks.setSocket(socket);
  const reader = new WebSocketFrameReader(socket);
  const abortHandler = (): void => {
    hooks.lifecycle.abort();
    socket.destroy();
  };
  options.signal?.addEventListener("abort", abortHandler, { once: true });

  try {
    const key = randomBytes(16).toString("base64");
    const target = execTarget(options.argv, options.pty);
    await writeRaw(socket, execUpgradeRequest(target, key));

    const response = await readHttpResponse(reader);
    if (response.statusCode !== 101) {
      const body = await readHttpBody(reader, response);
      throw apiErrorFromStatusBody(response.statusCode, response.statusMessage, body);
    }
    const gotAccept = (headerValue(response, "sec-websocket-accept") ?? "").trim();
    if (gotAccept !== acceptKey(key)) {
      throw new Error("websocket: Sec-WebSocket-Accept mismatch");
    }
    if (options.pty !== undefined && headerValue(response, "x-portal-exec-pty") !== "1") {
      throw new PtyUnsupportedError();
    }

    hooks.markStarted();
    launchPumps(options, hooks);
    return await readExecFrames(reader, options, hooks);
  } catch (error) {
    const err = isAbortSignalAborted(options.signal) ? abortError(options.signal) : toError(error);
    hooks.failStarted(err);
    const pumpError = hooks.getPumpError();
    if (pumpError !== null && !(err instanceof PtyUnsupportedError)) {
      throw pumpError;
    }
    throw err;
  } finally {
    options.signal?.removeEventListener("abort", abortHandler);
    hooks.closeSession();
  }
}

async function readExecFrames(reader: WebSocketFrameReader, options: ExecOptions, hooks: SessionHooks): Promise<ExecResult> {
  for (;;) {
    const frame = await reader.readFrame(false);
    switch (frame.opcode) {
      case OpBinary: {
        const execFrame = decodeExecFrame(frame.payload);
        switch (execFrame.stream) {
          case ExecStreamStdout:
            await writeSink(options.stdout, execFrame.data);
            break;
          case ExecStreamStderr:
            await writeSink(options.stderr, execFrame.data);
            break;
          case ExecStreamExit:
            return { code: execFrame.code };
          case ExecStreamError: {
            const message = textDecoder.decode(execFrame.data);
            throw new Error(message === "" ? "exec stream error" : message);
          }
        }
        break;
      }
      case OpPing:
        await hooks.writer.run(async () => {
          await writePong(mustSocket(hooks), frame.payload, true);
        });
        break;
      case OpClose:
        throw new Error("websocket: close before exec terminal frame");
    }
  }
}

function launchPumps(options: ExecOptions, hooks: SessionHooks): void {
  const pty = options.pty !== undefined;
  if (options.stdin !== undefined || !pty) {
    void pumpStdin(options.stdin ?? null, pty, hooks.sendExecFrame, hooks.lifecycle.signal).catch((error: unknown) => {
      if (!hooks.lifecycle.signal.aborted) {
        hooks.recordPumpError(toError(error));
      }
    });
  }
  if (pty && options.winch !== undefined) {
    void pumpWinch(options.winch, hooks.sendExecFrame, hooks.lifecycle.signal).catch((error: unknown) => {
      if (!hooks.lifecycle.signal.aborted) {
        hooks.recordPumpError(toError(error));
      }
    });
  }
}

async function pumpStdin(
  source: StdinSource,
  pty: boolean,
  sendExecFrame: (frame: ExecFrameInit) => Promise<void>,
  signal: AbortSignal,
): Promise<void> {
  if (source === null) {
    if (!pty) {
      await sendExecFrame({ stream: ExecStreamStdin });
    }
    return;
  }

  const iterator = toByteAsyncIterator(source);
  try {
    for (;;) {
      const next = await nextWithAbort(iterator, signal);
      if (next.done === true) {
        if (!pty) {
          await sendExecFrame({ stream: ExecStreamStdin });
        }
        return;
      }
      const chunk = byteChunk(next.value);
      if (chunk.byteLength > 0) {
        await sendExecFrame({ stream: ExecStreamStdin, data: chunk });
      }
    }
  } finally {
    await returnIterator(iterator);
  }
}

async function pumpWinch(
  source: WinchSource,
  sendExecFrame: (frame: ExecFrameInit) => Promise<void>,
  signal: AbortSignal,
): Promise<void> {
  const iterator = toWinchAsyncIterator(source);
  try {
    for (;;) {
      const next = await nextWithAbort(iterator, signal);
      if (next.done === true) {
        return;
      }
      await sendExecFrame({ stream: ExecStreamWinch, rows: next.value.rows, cols: next.value.cols });
    }
  } finally {
    await returnIterator(iterator);
  }
}

function mustSocket(hooks: SessionHooks): Socket {
  const socket = hooks.getSocket();
  if (socket === null || socket.destroyed) {
    throw new Error("exec: session is closed");
  }
  return socket;
}

class SerialWriter {
  tail: Promise<void>;

  constructor() {
    this.tail = Promise.resolve();
  }

  run(task: () => Promise<void>): Promise<void> {
    const next = this.tail.then(task, task);
    this.tail = next.catch(() => {});
    return next;
  }
}

interface HttpResponseHead {
  statusCode: number;
  statusMessage: string;
  headers: Map<string, string[]>;
}

async function readHttpResponse(reader: WebSocketFrameReader): Promise<HttpResponseHead> {
  const head = await reader.readUntil(crlfcrlf, responseHeadLimit);
  const lines = head.toString("latin1").split("\r\n");
  const statusLine = lines.shift() ?? "";
  const statusMatch = /^HTTP\/1\.[01] ([0-9]{3})(?: (.*))?$/.exec(statusLine);
  if (statusMatch === null) {
    throw new Error("websocket: invalid HTTP upgrade response");
  }
  const headers = new Map<string, string[]>();
  for (const line of lines) {
    const colon = line.indexOf(":");
    if (colon < 0) {
      continue;
    }
    const key = line.slice(0, colon).trim().toLowerCase();
    const value = line.slice(colon + 1).trim();
    const existing = headers.get(key);
    if (existing === undefined) {
      headers.set(key, [value]);
    } else {
      existing.push(value);
    }
  }
  return {
    statusCode: Number(statusMatch[1]),
    statusMessage: statusMatch[2] ?? "",
    headers,
  };
}

async function readHttpBody(reader: WebSocketFrameReader, response: HttpResponseHead): Promise<string> {
  const contentLength = headerValue(response, "content-length");
  if (contentLength !== undefined) {
    const length = Number(contentLength);
    if (!Number.isInteger(length) || length < 0 || length > errorBodyLimit) {
      throw new Error("websocket: invalid HTTP error body length");
    }
    return (await reader.readBytes(length)).toString("utf8");
  }
  return (await reader.readToEnd(errorBodyLimit)).toString("utf8");
}

function headerValue(response: HttpResponseHead, name: string): string | undefined {
  return response.headers.get(name.toLowerCase())?.[0];
}

function connectUnix(socketPath: string, signal: AbortSignal | undefined): Promise<Socket> {
  return new Promise((resolve, reject) => {
    const socket = connect({ path: socketPath });
    const cleanup = (): void => {
      socket.off("connect", onConnect);
      socket.off("error", onError);
      signal?.removeEventListener("abort", onAbort);
    };
    const onConnect = (): void => {
      cleanup();
      resolve(socket);
    };
    const onError = (error: Error): void => {
      cleanup();
      reject(error);
    };
    const onAbort = (): void => {
      cleanup();
      socket.destroy();
      reject(signal === undefined ? new Error("operation aborted") : abortError(signal));
    };

    socket.once("connect", onConnect);
    socket.once("error", onError);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

function writeRaw(socket: Socket, text: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const onDrain = (): void => {
      cleanup();
      resolve();
    };
    const onError = (error: Error): void => {
      cleanup();
      reject(error);
    };
    const cleanup = (): void => {
      socket.off("drain", onDrain);
      socket.off("error", onError);
    };

    socket.once("error", onError);
    const flushed = socket.write(text, "latin1");
    if (flushed) {
      cleanup();
      resolve();
      return;
    }
    socket.once("drain", onDrain);
  });
}

function execTarget(argv: string[], pty: ExecPtyOptions | undefined): string {
  const params: string[] = [];
  for (const arg of argv) {
    params.push(`arg=${queryEscape(arg)}`);
  }
  if (pty !== undefined) {
    params.push("pty=1");
    params.push(`term=${queryEscape(pty.term ?? "")}`);
    params.push(`rows=${queryEscape(String(pty.rows))}`);
    params.push(`cols=${queryEscape(String(pty.cols))}`);
  }
  return params.length === 0 ? "/v1/exec" : `/v1/exec?${params.join("&")}`;
}

function execUpgradeRequest(target: string, key: string): string {
  return (
    `POST ${target} HTTP/1.1\r\n` +
    "Host: unix\r\n" +
    "Upgrade: websocket\r\n" +
    "Connection: Upgrade\r\n" +
    "Sec-WebSocket-Version: 13\r\n" +
    `Sec-WebSocket-Key: ${key}\r\n` +
    "Content-Length: 0\r\n" +
    "\r\n"
  );
}

function queryEscape(value: string): string {
  let out = "";
  for (const byte of Buffer.from(value, "utf8")) {
    if (
      (byte >= 0x41 && byte <= 0x5a) ||
      (byte >= 0x61 && byte <= 0x7a) ||
      (byte >= 0x30 && byte <= 0x39) ||
      byte === 0x2d ||
      byte === 0x5f ||
      byte === 0x2e ||
      byte === 0x7e
    ) {
      out += String.fromCharCode(byte);
    } else if (byte === 0x20) {
      out += "+";
    } else {
      out += `%${byte.toString(16).toUpperCase().padStart(2, "0")}`;
    }
  }
  return out;
}

async function writeSink(sink: ByteSink | undefined, data: Uint8Array): Promise<void> {
  if (sink === undefined || data.byteLength === 0) {
    return;
  }
  if (typeof sink === "function") {
    await sink(data);
    return;
  }
  await writeWritableSink(sink, data);
}

function writeWritableSink(sink: NodeJS.WritableStream, data: Uint8Array): Promise<void> {
  return new Promise((resolve, reject) => {
    const onDrain = (): void => {
      cleanup();
      resolve();
    };
    const onError = (error: Error): void => {
      cleanup();
      reject(error);
    };
    const cleanup = (): void => {
      sink.off("drain", onDrain);
      sink.off("error", onError);
    };

    sink.once("error", onError);
    const flushed = sink.write(Buffer.from(data));
    if (flushed) {
      cleanup();
      resolve();
      return;
    }
    sink.once("drain", onDrain);
  });
}

function byteChunk(value: ByteChunk): Uint8Array {
  if (typeof value === "string") {
    return Buffer.from(value);
  }
  return value;
}

function toByteAsyncIterator(source: ByteChunk | Iterable<ByteChunk> | AsyncIterable<ByteChunk>): AsyncIterator<ByteChunk> {
  if (typeof source === "string" || source instanceof Uint8Array) {
    return asyncFromSyncIterator([source].values());
  }
  if (isAsyncIterable(source)) {
    return source[Symbol.asyncIterator]();
  }
  if (isIterable(source)) {
    return asyncFromSyncIterator(source[Symbol.iterator]());
  }
  return asyncFromSyncIterator([source].values());
}

function toWinchAsyncIterator(source: WinchSource): AsyncIterator<WinchSize> {
  if (isAsyncIterable(source)) {
    return source[Symbol.asyncIterator]();
  }
  return asyncFromSyncIterator(source[Symbol.iterator]());
}

function asyncFromSyncIterator<T>(iterator: Iterator<T>): AsyncIterator<T> {
  return {
    next(): Promise<IteratorResult<T>> {
      return Promise.resolve(iterator.next());
    },
    return(value?: T): Promise<IteratorResult<T>> {
      if (typeof iterator.return === "function") {
        return Promise.resolve(iterator.return(value));
      }
      return Promise.resolve({ done: true, value });
    },
  };
}

function isAsyncIterable<T>(value: unknown): value is AsyncIterable<T> {
  return typeof value === "object" && value !== null && Symbol.asyncIterator in value;
}

function isIterable<T>(value: unknown): value is Iterable<T> {
  return (typeof value === "object" || typeof value === "string") && value !== null && Symbol.iterator in Object(value);
}

function isAbortSignalAborted(signal: AbortSignal | undefined): signal is AbortSignal {
  return signal !== undefined && signal.aborted;
}

function nextWithAbort<T>(iterator: AsyncIterator<T>, signal: AbortSignal): Promise<IteratorResult<T>> {
  if (signal.aborted) {
    return Promise.reject(abortError(signal));
  }
  return new Promise((resolve, reject) => {
    const onAbort = (): void => {
      void returnIterator(iterator);
      reject(abortError(signal));
    };
    signal.addEventListener("abort", onAbort, { once: true });
    void iterator.next().then(
      (next) => {
        signal.removeEventListener("abort", onAbort);
        resolve(next);
      },
      (error: unknown) => {
        signal.removeEventListener("abort", onAbort);
        reject(error);
      },
    );
  });
}

async function returnIterator<T>(iterator: AsyncIterator<T>): Promise<void> {
  if (typeof iterator.return === "function") {
    await iterator.return();
  }
}

function abortError(signal: AbortSignal): Error {
  const reason: unknown = signal.reason;
  if (reason instanceof Error) {
    return reason;
  }
  if (typeof reason === "string" && reason !== "") {
    return new Error(reason);
  }
  return new Error("operation aborted");
}

function toError(value: unknown): Error {
  if (value instanceof Error) {
    return value;
  }
  if (typeof value === "string") {
    return new Error(value);
  }
  return new Error("unknown error");
}
