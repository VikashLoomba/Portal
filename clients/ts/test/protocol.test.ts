import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { PassThrough, Writable } from "node:stream";
import { setTimeout as delay } from "node:timers/promises";
import { createServer } from "node:net";
import type { Server, Socket } from "node:net";
import test from "node:test";
import type { TestContext } from "node:test";

import {
  ExecStreamError,
  ExecStreamExit,
  ExecStreamStdin,
  ExecStreamStdout,
  ExecStreamWinch,
  decodeExecFrame,
  encodeExecFrame,
} from "../src/cbor.ts";
import { PTY_UNSUPPORTED_MESSAGE, exec, runExec } from "../src/exec.ts";
import {
  MaxPayload,
  OpBinary,
  OpClose,
  WebSocketFrameReader,
  acceptKey,
  writeClose,
  writeFrame,
} from "../src/wsframe.ts";
import type { Frame } from "../src/wsframe.ts";

const crlfcrlf = Uint8Array.of(13, 10, 13, 10);

test("upgrade handshake completes and request headers match", async (t) => {
  const fake = await FakeExecServer.start(t, async (conn) => {
    const reader = new WebSocketFrameReader(conn);
    const req = await readUpgradeRequest(reader);
    assert.equal(req.method, "POST");
    assert.equal(req.target, "/v1/exec?arg=echo&arg=ok");
    assert.equal(req.version, "HTTP/1.1");
    assert.equal(header(req, "host"), "unix");
    assert.equal(tokenHeaderContains(req, "upgrade", "websocket"), true);
    assert.equal(tokenHeaderContains(req, "connection", "upgrade"), true);
    assert.equal(header(req, "sec-websocket-version"), "13");
    assert.equal(Buffer.from(header(req, "sec-websocket-key") ?? "", "base64").byteLength, 16);
    assert.equal(header(req, "content-length"), "0");

    await write101(conn, header(req, "sec-websocket-key") ?? "");
    const eof = decodeExecFrame((await reader.readFrame(true)).payload);
    assert.equal(eof.stream, ExecStreamStdin);
    assert.equal(eof.data.byteLength, 0);
    await sendExecFrame(conn, { stream: ExecStreamExit, code: 0 });
  });

  const result = await runExec(fake.path, { argv: ["echo", "ok"] });
  assert.equal(result.code, 0);
  await fake.done;
});

test("wrong Sec-WebSocket-Accept makes the client fail", async (t) => {
  const fake = await FakeExecServer.start(t, async (conn) => {
    const reader = new WebSocketFrameReader(conn);
    const req = await readUpgradeRequest(reader);
    await writeRaw(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: wrong\r\n\r\n");
    await waitForNoData(conn, 100);
    assert.notEqual(header(req, "sec-websocket-key"), undefined);
  });

  const err = await captureError(withTimeout(runExec(fake.path, { argv: ["true"] }), 1000));
  assert.notEqual(err, null);
  assert.match(err?.message ?? "", /Sec-WebSocket-Accept mismatch/);
  await fake.done;
});

test("stdin to stdout echo round-trip", async (t) => {
  const fake = await FakeExecServer.start(t, async (conn) => {
    const reader = new WebSocketFrameReader(conn);
    const req = await readUpgradeRequest(reader);
    await write101(conn, header(req, "sec-websocket-key") ?? "");
    const stdin = decodeExecFrame((await reader.readFrame(true)).payload);
    assert.equal(stdin.stream, ExecStreamStdin);
    assert.deepStrictEqual(Buffer.from(stdin.data), Buffer.from("payload"));
    await sendExecFrame(conn, { stream: ExecStreamStdout, data: stdin.data });
    await sendExecFrame(conn, { stream: ExecStreamExit, code: 0 });
  });

  const chunks: Uint8Array[] = [];
  const result = await runExec(fake.path, {
    argv: ["cat"],
    stdin: "payload",
    stdout(chunk) {
      chunks.push(Buffer.from(chunk));
    },
  });
  assert.equal(result.code, 0);
  assert.equal(Buffer.concat(chunks).toString("utf8"), "payload");
  await fake.done;
});

test("non-PTY stdin EOF sends exactly one zero-length stdin frame", async (t) => {
  const fake = await FakeExecServer.start(t, async (conn) => {
    const reader = new WebSocketFrameReader(conn);
    const req = await readUpgradeRequest(reader);
    await write101(conn, header(req, "sec-websocket-key") ?? "");
    const eof = decodeExecFrame((await reader.readFrame(true)).payload);
    assert.equal(eof.stream, ExecStreamStdin);
    assert.equal(eof.data.byteLength, 0);
    await sendExecFrame(conn, { stream: ExecStreamExit, code: 0 });
  });

  const result = await runExec(fake.path, { argv: ["cat"], stdin: [] });
  assert.equal(result.code, 0);
  await fake.done;
});

test("PTY resize sends winch frame", async (t) => {
  const fake = await FakeExecServer.start(t, async (conn) => {
    const reader = new WebSocketFrameReader(conn);
    const req = await readUpgradeRequest(reader);
    assert.equal(req.target, "/v1/exec?arg=sh&pty=1&term=xterm&rows=24&cols=80");
    await write101(conn, header(req, "sec-websocket-key") ?? "", true);
    const winch = decodeExecFrame((await reader.readFrame(true)).payload);
    assert.equal(winch.stream, ExecStreamWinch);
    assert.equal(winch.rows, 40);
    assert.equal(winch.cols, 120);
    assert.equal(winch.data.byteLength, 0);
    assert.equal(winch.code, 0);
    await sendExecFrame(conn, { stream: ExecStreamExit, code: 0 });
  });

  const session = exec(fake.path, { argv: ["sh"], pty: { term: "xterm", rows: 24, cols: 80 } });
  await session.resize(40, 120);
  assert.equal((await session.result).code, 0);
  await fake.done;
});

test("PTY skew hard-fails before sending frames", async (t) => {
  const fake = await FakeExecServer.start(t, async (conn) => {
    const reader = new WebSocketFrameReader(conn);
    const req = await readUpgradeRequest(reader);
    assert.equal(req.target, "/v1/exec?arg=cat&pty=1&term=&rows=24&cols=80");
    await write101(conn, header(req, "sec-websocket-key") ?? "", false);
    await waitForNoData(conn, 100);
  });

  const session = exec(fake.path, {
    argv: ["cat"],
    pty: { rows: 24, cols: 80 },
    stdin: "payload",
  });
  const err = await captureError(withTimeout(session.result, 1000));
  assert.notEqual(err, null);
  assert.match(err?.message ?? "", new RegExp(PTY_UNSUPPORTED_MESSAGE.replace(/[()]/g, "\\$&")));
  await fake.done;
});

test("PTY stdin EOF does not emit zero-length stdin", async (t) => {
  const fake = await FakeExecServer.start(t, async (conn) => {
    const reader = new WebSocketFrameReader(conn);
    const req = await readUpgradeRequest(reader);
    await write101(conn, header(req, "sec-websocket-key") ?? "", true);
    await waitForNoData(conn, 100);
    await sendExecFrame(conn, { stream: ExecStreamExit, code: 0 });
  });

  const result = await runExec(fake.path, {
    argv: ["sh"],
    pty: { rows: 24, cols: 80 },
    stdin: [],
  });
  assert.equal(result.code, 0);
  await fake.done;
});

test("exit code is passed through", async (t) => {
  const fake = await FakeExecServer.start(t, async (conn) => {
    const reader = new WebSocketFrameReader(conn);
    const req = await readUpgradeRequest(reader);
    await write101(conn, header(req, "sec-websocket-key") ?? "");
    await sendExecFrame(conn, { stream: ExecStreamExit, code: 7 });
  });

  assert.equal((await runExec(fake.path, { argv: ["false"] })).code, 7);
  await fake.done;
});

test("error frame rejects with the message", async (t) => {
  const fake = await FakeExecServer.start(t, async (conn) => {
    const reader = new WebSocketFrameReader(conn);
    const req = await readUpgradeRequest(reader);
    await write101(conn, header(req, "sec-websocket-key") ?? "");
    await sendExecFrame(conn, { stream: ExecStreamError, data: Buffer.from("remote failed") });
  });

  const err = await captureError(withTimeout(runExec(fake.path, { argv: ["boom"] }), 1000));
  assert.notEqual(err, null);
  assert.match(err?.message ?? "", /remote failed/);
  await fake.done;
});

test("wsframe parses a frame split across writes", async () => {
  const stream = new PassThrough();
  const frameBytes = await collectWrittenFrame(OpBinary, Buffer.from("split"), false);
  const reader = new WebSocketFrameReader(stream);
  const read = reader.readFrame(false);
  stream.write(frameBytes.subarray(0, 3));
  await delay(1);
  stream.end(frameBytes.subarray(3));
  const frame = await read;
  assert.equal(frame.opcode, OpBinary);
  assert.equal(Buffer.from(frame.payload).toString("utf8"), "split");
});

test("wsframe handles 126 and 127 payload length forms", async () => {
  for (const size of [126, 65536]) {
    const payload = patterned(size);
    const frameBytes = await collectWrittenFrame(OpBinary, payload, false);
    const stream = new PassThrough();
    const reader = new WebSocketFrameReader(stream);
    const read = reader.readFrame(false);
    stream.end(frameBytes);
    const frame = await read;
    assert.equal(frame.opcode, OpBinary);
    assert.deepStrictEqual(Buffer.from(frame.payload), Buffer.from(payload));
  }
});

test("wsframe rejects oversize payloads", async () => {
  const stream = new PassThrough();
  const reader = new WebSocketFrameReader(stream);
  const header = Buffer.alloc(10);
  header[0] = 0x80 | OpBinary;
  header[1] = 127;
  header.writeBigUInt64BE(BigInt(MaxPayload + 1), 2);
  stream.end(header);
  await assert.rejects(reader.readFrame(false), /exceeds limit/);
});

test("wsframe rejects fragmentation", async () => {
  const stream = new PassThrough();
  const reader = new WebSocketFrameReader(stream);
  stream.end(Buffer.from([OpBinary, 0]));
  await assert.rejects(reader.readFrame(false), /fragmented messages/);
});

test("wsframe rejects wrong mask direction", async () => {
  const maskedBytes = await collectWrittenFrame(OpBinary, Buffer.from("masked"), true);
  const masked = new PassThrough();
  const maskedReader = new WebSocketFrameReader(masked);
  masked.end(maskedBytes);
  await assert.rejects(maskedReader.readFrame(false), /server frame is masked/);

  const unmaskedBytes = await collectWrittenFrame(OpBinary, Buffer.from("unmasked"), false);
  const unmasked = new PassThrough();
  const unmaskedReader = new WebSocketFrameReader(unmasked);
  unmasked.end(unmaskedBytes);
  await assert.rejects(unmaskedReader.readFrame(true), /client frame is unmasked/);
});

test("wsframe close helper writes close opcode and payload", async () => {
  const sink = new BufferWritable();
  await writeClose(sink, false, 1000, "done");
  const stream = new PassThrough();
  const reader = new WebSocketFrameReader(stream);
  stream.end(sink.bytes());
  const frame = await reader.readFrame(false);
  assert.equal(frame.opcode, OpClose);
  assert.equal(Buffer.from(frame.payload).readUInt16BE(0), 1000);
  assert.equal(Buffer.from(frame.payload.subarray(2)).toString("utf8"), "done");
});

class FakeExecServer {
  path: string;
  server: Server;
  done: Promise<void>;

  constructor(socketPath: string, server: Server, done: Promise<void>) {
    this.path = socketPath;
    this.server = server;
    this.done = done;
  }

  static async start(t: TestContext, handler: (conn: Socket) => Promise<void>): Promise<FakeExecServer> {
    const dir = mkdtempSync(path.join(tmpdir(), "pt-"));
    const socketPath = path.join(dir, "s.sock");
    let resolveDone: () => void = () => {};
    let rejectDone: (error: Error) => void = () => {};
    const done = new Promise<void>((resolve, reject) => {
      resolveDone = resolve;
      rejectDone = reject;
    });
    const server = createServer((conn) => {
      server.close();
      void (async () => {
        try {
          await handler(conn);
          resolveDone();
        } catch (error) {
          rejectDone(toError(error));
        } finally {
          conn.destroy();
        }
      })();
    });
    server.once("error", rejectDone);
    await new Promise<void>((resolve, reject) => {
      server.once("error", reject);
      server.listen(socketPath, resolve);
    });
    t.after(() => {
      server.close();
      rmSync(dir, { recursive: true, force: true });
    });
    return new FakeExecServer(socketPath, server, done);
  }
}

interface UpgradeRequest {
  method: string;
  target: string;
  version: string;
  headers: Map<string, string[]>;
}

async function readUpgradeRequest(reader: WebSocketFrameReader): Promise<UpgradeRequest> {
  const head = await reader.readUntil(crlfcrlf, 64 * 1024);
  const lines = head.toString("latin1").split("\r\n");
  const requestLine = lines.shift() ?? "";
  const parts = requestLine.split(" ");
  assert.equal(parts.length, 3);
  const headers = new Map<string, string[]>();
  for (const line of lines) {
    const colon = line.indexOf(":");
    assert.notEqual(colon, -1);
    const name = line.slice(0, colon).trim().toLowerCase();
    const value = line.slice(colon + 1).trim();
    const existing = headers.get(name);
    if (existing === undefined) {
      headers.set(name, [value]);
    } else {
      existing.push(value);
    }
  }
  return { method: parts[0], target: parts[1], version: parts[2], headers };
}

function header(req: UpgradeRequest, name: string): string | undefined {
  return req.headers.get(name.toLowerCase())?.[0];
}

function tokenHeaderContains(req: UpgradeRequest, name: string, want: string): boolean {
  const values = req.headers.get(name.toLowerCase()) ?? [];
  for (const value of values) {
    for (const token of value.split(",")) {
      if (token.trim().toLowerCase() === want.toLowerCase()) {
        return true;
      }
    }
  }
  return false;
}

async function write101(conn: Socket, key: string, pty: boolean = false): Promise<void> {
  const ptyHeader = pty ? "X-Portal-Exec-Pty: 1\r\n" : "";
  await writeRaw(
    conn,
    "HTTP/1.1 101 Switching Protocols\r\n" +
      "Upgrade: websocket\r\n" +
      "Connection: Upgrade\r\n" +
      `Sec-WebSocket-Accept: ${acceptKey(key)}\r\n` +
      ptyHeader +
      "\r\n",
  );
}

async function sendExecFrame(conn: Socket, frame: { stream: string; data?: Uint8Array; code?: number; rows?: number; cols?: number }): Promise<void> {
  await writeFrame(conn, OpBinary, encodeExecFrame(frame), false);
}

function writeRaw(conn: Socket, data: string | Uint8Array): Promise<void> {
  return new Promise((resolve, reject) => {
    conn.write(data, (error?: Error | null) => {
      if (error !== undefined && error !== null) {
        reject(error);
      } else {
        resolve();
      }
    });
  });
}

function waitForNoData(conn: Socket, ms: number): Promise<void> {
  return new Promise((resolve, reject) => {
    const cleanup = (): void => {
      clearTimeout(timer);
      conn.off("data", onData);
      conn.off("error", onError);
      conn.off("end", onEnd);
    };
    const onData = (chunk: Buffer): void => {
      cleanup();
      reject(new Error(`unexpected client frame data: ${chunk.toString("hex")}`));
    };
    const onError = (error: Error): void => {
      cleanup();
      reject(error);
    };
    const onEnd = (): void => {
      cleanup();
      resolve();
    };
    const timer = setTimeout(() => {
      cleanup();
      resolve();
    }, ms);
    conn.once("data", onData);
    conn.once("error", onError);
    conn.once("end", onEnd);
  });
}

async function collectWrittenFrame(opcode: Frame["opcode"], payload: Uint8Array, mask: boolean): Promise<Buffer> {
  const sink = new BufferWritable();
  await writeFrame(sink, opcode, payload, mask);
  return sink.bytes();
}

class BufferWritable extends Writable {
  chunks: Buffer[];

  constructor() {
    super();
    this.chunks = [];
  }

  _write(chunk: Buffer | string, encoding: BufferEncoding, callback: (error?: Error | null) => void): void {
    if (typeof chunk === "string") {
      this.chunks.push(Buffer.from(chunk, encoding));
    } else {
      this.chunks.push(Buffer.from(chunk));
    }
    callback();
  }

  bytes(): Buffer {
    return Buffer.concat(this.chunks);
  }
}

function patterned(size: number): Uint8Array {
  const out = new Uint8Array(size);
  for (let i = 0; i < size; i += 1) {
    out[i] = i % 251;
  }
  return out;
}

async function withTimeout<T>(promise: Promise<T>, ms: number): Promise<T> {
  let timeout: NodeJS.Timeout | null = null;
  try {
    return await Promise.race([
      promise,
      new Promise<T>((_, reject) => {
        timeout = setTimeout(() => reject(new Error(`timeout after ${ms}ms`)), ms);
      }),
    ]);
  } finally {
    if (timeout !== null) {
      clearTimeout(timeout);
    }
  }
}

async function captureError(promise: Promise<unknown>): Promise<Error | null> {
  try {
    await promise;
  } catch (error) {
    return toError(error);
  }
  return null;
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
