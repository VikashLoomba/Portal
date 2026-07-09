import { createHash, randomBytes } from "node:crypto";

export const OpContinuation = 0x0;
export const OpText = 0x1;
export const OpBinary = 0x2;
export const OpClose = 0x8;
export const OpPing = 0x9;
export const OpPong = 0xa;

export type Opcode = 0x0 | 0x1 | 0x2 | 0x8 | 0x9 | 0xa;

export const GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11";
export const MaxPayload = 16 << 20;

const closeCodeLength = 2;
const textEncoder = new TextEncoder();

export interface Frame {
  opcode: Opcode;
  payload: Uint8Array;
}

export function acceptKey(key: string): string {
  return createHash("sha1").update(key + GUID).digest("base64");
}

export class WebSocketFrameReader {
  source: NodeJS.ReadableStream;
  buffer: Buffer;
  ended: boolean;
  error: Error | null;
  waiters: Array<() => void>;

  constructor(source: NodeJS.ReadableStream) {
    this.source = source;
    this.buffer = Buffer.alloc(0);
    this.ended = false;
    this.error = null;
    this.waiters = [];

    source.on("data", (chunk: Buffer | string) => {
      this.push(chunk);
    });
    source.once("end", () => {
      this.ended = true;
      this.wake();
    });
    source.once("close", () => {
      this.ended = true;
      this.wake();
    });
    source.once("error", (err: Error) => {
      this.error = err;
      this.wake();
    });
  }

  async readUntil(delimiter: Uint8Array, maxBytes: number): Promise<Buffer> {
    const needle = Buffer.from(delimiter);
    for (;;) {
      const index = this.buffer.indexOf(needle);
      if (index >= 0) {
        const out = this.buffer.subarray(0, index);
        this.buffer = this.buffer.subarray(index + needle.byteLength);
        return out;
      }
      if (this.buffer.byteLength > maxBytes) {
        throw new Error("websocket: buffered header exceeds limit");
      }
      if (this.error !== null) {
        throw this.error;
      }
      if (this.ended) {
        throw new Error("websocket: unexpected EOF");
      }
      await this.waitForChange();
    }
  }

  async readBytes(length: number): Promise<Buffer> {
    for (;;) {
      if (this.buffer.byteLength >= length) {
        const out = this.buffer.subarray(0, length);
        this.buffer = this.buffer.subarray(length);
        return out;
      }
      if (this.error !== null) {
        throw this.error;
      }
      if (this.ended) {
        throw new Error("websocket: unexpected EOF");
      }
      await this.waitForChange();
    }
  }

  async readToEnd(maxBytes: number): Promise<Buffer> {
    for (;;) {
      if (this.buffer.byteLength > maxBytes) {
        throw new Error("websocket: response body exceeds limit");
      }
      if (this.error !== null) {
        throw this.error;
      }
      if (this.ended) {
        const out = this.buffer;
        this.buffer = Buffer.alloc(0);
        return out;
      }
      await this.waitForChange();
    }
  }

  async readFrame(requireMasked: boolean): Promise<Frame> {
    for (;;) {
      const parsed = parseFrame(this.buffer, requireMasked);
      if (parsed !== null) {
        this.buffer = this.buffer.subarray(parsed.bytesUsed);
        return parsed.frame;
      }
      if (this.error !== null) {
        throw this.error;
      }
      if (this.ended) {
        throw new Error("websocket: unexpected EOF");
      }
      await this.waitForChange();
    }
  }

  push(chunk: Buffer | string): void {
    const next = typeof chunk === "string" ? Buffer.from(chunk) : chunk;
    this.buffer = this.buffer.byteLength === 0 ? Buffer.from(next) : Buffer.concat([this.buffer, next]);
    this.wake();
  }

  waitForChange(): Promise<void> {
    if (this.ended || this.error !== null) {
      return Promise.resolve();
    }
    return new Promise((resolve) => {
      this.waiters.push(resolve);
    });
  }

  wake(): void {
    const waiters = this.waiters;
    this.waiters = [];
    for (const resolve of waiters) {
      resolve();
    }
  }
}

export async function writeFrame(
  w: NodeJS.WritableStream,
  opcode: Opcode,
  payload: Uint8Array = new Uint8Array(0),
  mask: boolean = false,
): Promise<void> {
  const header = buildHeader(opcode, payload.byteLength, mask);
  if (!mask) {
    await writeAll(w, concat([header, payload]));
    return;
  }

  const key = randomBytes(4);
  const masked = new Uint8Array(payload.byteLength);
  for (let i = 0; i < payload.byteLength; i += 1) {
    masked[i] = payload[i] ^ key[i % 4];
  }
  await writeAll(w, concat([header, key, masked]));
}

export async function writeClose(
  w: NodeJS.WritableStream,
  mask: boolean,
  code: number = 1000,
  reason: string = "",
): Promise<void> {
  const reasonBytes = textEncoder.encode(reason);
  const payload = new Uint8Array(closeCodeLength + reasonBytes.byteLength);
  payload[0] = (code >> 8) & 0xff;
  payload[1] = code & 0xff;
  payload.set(reasonBytes, closeCodeLength);
  await writeFrame(w, OpClose, payload, mask);
}

export async function writePing(
  w: NodeJS.WritableStream,
  payload: Uint8Array = new Uint8Array(0),
  mask: boolean = false,
): Promise<void> {
  await writeFrame(w, OpPing, payload, mask);
}

export async function writePong(
  w: NodeJS.WritableStream,
  payload: Uint8Array = new Uint8Array(0),
  mask: boolean = false,
): Promise<void> {
  await writeFrame(w, OpPong, payload, mask);
}

interface ParsedFrame {
  frame: Frame;
  bytesUsed: number;
}

function parseFrame(buffer: Buffer, requireMasked: boolean): ParsedFrame | null {
  if (buffer.byteLength < 2) {
    return null;
  }
  const first = buffer[0];
  const second = buffer[1];
  const fin = (first & 0x80) !== 0;
  if ((first & 0x70) !== 0) {
    throw new Error("websocket: reserved bits set");
  }
  const opcode = first & 0x0f;
  if (!isOpcode(opcode)) {
    throw new Error(`websocket: reserved opcode 0x${opcode.toString(16)}`);
  }
  if (opcode === OpContinuation) {
    throw new Error("websocket: continuation frames are unsupported");
  }
  if (!fin) {
    throw new Error("websocket: fragmented messages are unsupported");
  }

  const masked = (second & 0x80) !== 0;
  if (requireMasked && !masked) {
    throw new Error("websocket: client frame is unmasked");
  }
  if (!requireMasked && masked) {
    throw new Error("websocket: server frame is masked");
  }

  const len7 = second & 0x7f;
  const lengthInfo = parsePayloadLength(buffer, len7);
  if (lengthInfo === null) {
    return null;
  }
  if (lengthInfo.length > MaxPayload) {
    throw new Error(`websocket: payload length ${lengthInfo.length} exceeds limit`);
  }

  const maskLength = masked ? 4 : 0;
  const payloadStart = lengthInfo.headerLength + maskLength;
  const frameLength = payloadStart + lengthInfo.length;
  if (buffer.byteLength < frameLength) {
    return null;
  }

  const payload = Buffer.from(buffer.subarray(payloadStart, frameLength));
  if (masked) {
    const key = buffer.subarray(lengthInfo.headerLength, lengthInfo.headerLength + 4);
    for (let i = 0; i < payload.byteLength; i += 1) {
      payload[i] ^= key[i % 4];
    }
  }
  return { frame: { opcode, payload }, bytesUsed: frameLength };
}

interface PayloadLength {
  length: number;
  headerLength: number;
}

function parsePayloadLength(buffer: Buffer, len7: number): PayloadLength | null {
  if (len7 === 126) {
    if (buffer.byteLength < 4) {
      return null;
    }
    return { length: buffer.readUInt16BE(2), headerLength: 4 };
  }
  if (len7 === 127) {
    if (buffer.byteLength < 10) {
      return null;
    }
    const raw = buffer.readBigUInt64BE(2);
    if (raw > BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new Error("websocket: payload length exceeds JavaScript safe integer range");
    }
    return { length: Number(raw), headerLength: 10 };
  }
  return { length: len7, headerLength: 2 };
}

function isOpcode(value: number): value is Opcode {
  return value === OpContinuation || value === OpText || value === OpBinary || value === OpClose || value === OpPing || value === OpPong;
}

function buildHeader(opcode: Opcode, length: number, mask: boolean): Uint8Array {
  const maskBit = mask ? 0x80 : 0;
  if (length <= 125) {
    return Uint8Array.of(0x80 | opcode, maskBit | length);
  }
  if (length <= 0xffff) {
    return Uint8Array.of(0x80 | opcode, maskBit | 126, (length >> 8) & 0xff, length & 0xff);
  }
  const raw = BigInt(length);
  return Uint8Array.of(
    0x80 | opcode,
    maskBit | 127,
    Number((raw >> 56n) & 0xffn),
    Number((raw >> 48n) & 0xffn),
    Number((raw >> 40n) & 0xffn),
    Number((raw >> 32n) & 0xffn),
    Number((raw >> 24n) & 0xffn),
    Number((raw >> 16n) & 0xffn),
    Number((raw >> 8n) & 0xffn),
    Number(raw & 0xffn),
  );
}

function writeAll(w: NodeJS.WritableStream, data: Uint8Array): Promise<void> {
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
      w.off("drain", onDrain);
      w.off("error", onError);
    };

    w.once("error", onError);
    const flushed = w.write(Buffer.from(data));
    if (flushed) {
      cleanup();
      resolve();
      return;
    }
    w.once("drain", onDrain);
  });
}

function concat(chunks: Uint8Array[]): Uint8Array {
  let total = 0;
  for (const chunk of chunks) {
    total += chunk.byteLength;
  }
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out;
}
