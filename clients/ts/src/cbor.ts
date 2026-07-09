const textDecoder = new TextDecoder();
const textEncoder = new TextEncoder();
const maxUint16 = 0xffff;

export const ExecStreamStdin = "stdin";
export const ExecStreamStdout = "stdout";
export const ExecStreamStderr = "stderr";
export const ExecStreamExit = "exit";
export const ExecStreamError = "error";
export const ExecStreamWinch = "winch";

export type CborValue =
  | number
  | bigint
  | Uint8Array
  | string
  | boolean
  | null
  | CborValue[]
  | CborMap;

export interface CborMap {
  [key: string]: CborValue;
}

export interface ExecFrame {
  stream: string;
  data: Uint8Array;
  code: number;
  rows: number;
  cols: number;
}

export interface ExecFrameInit {
  stream: string;
  data?: Uint8Array;
  code?: number;
  rows?: number;
  cols?: number;
}

export function decode(input: Uint8Array): CborValue {
  const decoder = new CborDecoder(input);
  const value = decoder.decodeItem();
  if (!decoder.done()) {
    throw new Error("cbor: trailing bytes after top-level item");
  }
  return value;
}

export function decodeExecFrame(input: Uint8Array): ExecFrame {
  const value = decode(input);
  if (!isCborMap(value)) {
    throw new Error("cbor: exec frame must be a map");
  }

  return {
    stream: readOptionalString(value, "s", ""),
    data: readOptionalBytes(value, "d"),
    code: readOptionalSafeInteger(value, "c", 0),
    rows: readOptionalSafeInteger(value, "rs", 0),
    cols: readOptionalSafeInteger(value, "cs", 0),
  };
}

export function encodeExecFrame(frame: ExecFrame | ExecFrameInit): Uint8Array {
  const entries: EncodedMapEntry[] = [["s", encodeText(frame.stream)]];
  if (frame.data !== undefined && frame.data.byteLength > 0) {
    entries.push(["d", encodeBytes(frame.data)]);
  }
  if (frame.code !== undefined && frame.code !== 0) {
    entries.push(["c", encodeInteger(frame.code)]);
  }
  if (frame.rows !== undefined && frame.rows !== 0) {
    entries.push(["rs", encodeUint16Field("rs", frame.rows)]);
  }
  if (frame.cols !== undefined && frame.cols !== 0) {
    entries.push(["cs", encodeUint16Field("cs", frame.cols)]);
  }
  return encodeMap(entries);
}

type EncodedMapEntry = [string, Uint8Array];

class CborDecoder {
  input: Uint8Array;
  offset: number;

  constructor(input: Uint8Array) {
    this.input = input;
    this.offset = 0;
  }

  done(): boolean {
    return this.offset === this.input.byteLength;
  }

  decodeItem(): CborValue {
    const first = this.readByte();
    const major = first >> 5;
    const additional = first & 0x1f;

    switch (major) {
      case 0:
        return integerResult(this.readArgument(additional));
      case 1:
        return negativeIntegerResult(this.readArgument(additional));
      case 2:
        return this.readByteString(additional);
      case 3:
        return this.readTextString(additional);
      case 4:
        return this.readArray(additional);
      case 5:
        return this.readMap(additional);
      case 6:
        throw new Error("cbor: semantic tags are unsupported");
      case 7:
        return this.readSimple(additional);
      default:
        throw new Error("cbor: unsupported major type");
    }
  }

  readByteString(additional: number): Uint8Array {
    const length = this.readLength(additional);
    const start = this.offset;
    const end = start + length;
    if (end > this.input.byteLength) {
      throw new Error("cbor: truncated byte string");
    }
    this.offset = end;
    return this.input.slice(start, end);
  }

  readTextString(additional: number): string {
    return textDecoder.decode(this.readByteString(additional));
  }

  readArray(additional: number): CborValue[] {
    const length = this.readLength(additional);
    const out: CborValue[] = [];
    for (let i = 0; i < length; i += 1) {
      out.push(this.decodeItem());
    }
    return out;
  }

  readMap(additional: number): CborMap {
    const length = this.readLength(additional);
    const out: CborMap = {};
    for (let i = 0; i < length; i += 1) {
      const key = this.decodeItem();
      if (typeof key !== "string") {
        throw new Error("cbor: map keys must be text strings");
      }
      out[key] = this.decodeItem();
    }
    return out;
  }

  readSimple(additional: number): CborValue {
    switch (additional) {
      case 20:
        return false;
      case 21:
        return true;
      case 22:
        return null;
      case 25:
      case 26:
      case 27:
        throw new Error("cbor: floating point values are unsupported");
      case 31:
        throw new Error("cbor: indefinite lengths are unsupported");
      default:
        throw new Error(`cbor: unsupported simple value ${additional}`);
    }
  }

  readArgument(additional: number): bigint {
    switch (additional) {
      case 24:
        return BigInt(this.readByte());
      case 25:
        return BigInt(this.readUint16());
      case 26:
        return BigInt(this.readUint32());
      case 27:
        return this.readUint64();
      case 28:
      case 29:
      case 30:
        throw new Error(`cbor: reserved additional information ${additional}`);
      case 31:
        throw new Error("cbor: indefinite lengths are unsupported");
      default:
        return BigInt(additional);
    }
  }

  readLength(additional: number): number {
    const raw = this.readArgument(additional);
    if (raw > BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new Error("cbor: length exceeds JavaScript safe integer range");
    }
    const length = Number(raw);
    if (length > this.input.byteLength - this.offset) {
      throw new Error("cbor: declared length exceeds remaining input");
    }
    return length;
  }

  readByte(): number {
    if (this.offset >= this.input.byteLength) {
      throw new Error("cbor: unexpected end of input");
    }
    const value = this.input[this.offset];
    this.offset += 1;
    return value;
  }

  readUint16(): number {
    const bytes = this.readExact(2);
    return (bytes[0] << 8) | bytes[1];
  }

  readUint32(): number {
    const bytes = this.readExact(4);
    return bytes[0] * 0x1000000 + ((bytes[1] << 16) | (bytes[2] << 8) | bytes[3]);
  }

  readUint64(): bigint {
    const bytes = this.readExact(8);
    let value = 0n;
    for (const byte of bytes) {
      value = (value << 8n) | BigInt(byte);
    }
    return value;
  }

  readExact(length: number): Uint8Array {
    const start = this.offset;
    const end = start + length;
    if (end > this.input.byteLength) {
      throw new Error("cbor: unexpected end of input");
    }
    this.offset = end;
    return this.input.slice(start, end);
  }
}

function integerResult(value: bigint): number | bigint {
  if (value <= BigInt(Number.MAX_SAFE_INTEGER)) {
    return Number(value);
  }
  return value;
}

function negativeIntegerResult(encoded: bigint): number | bigint {
  const value = -1n - encoded;
  if (value >= BigInt(Number.MIN_SAFE_INTEGER)) {
    return Number(value);
  }
  return value;
}

function isCborMap(value: CborValue): value is CborMap {
  return typeof value === "object" && value !== null && !Array.isArray(value) && !(value instanceof Uint8Array);
}

function readOptionalString(map: CborMap, key: string, fallback: string): string {
  const value = map[key];
  if (value === undefined) {
    return fallback;
  }
  if (typeof value !== "string") {
    throw new Error(`cbor: exec frame ${key} must be a text string`);
  }
  return value;
}

function readOptionalBytes(map: CborMap, key: string): Uint8Array {
  const value = map[key];
  if (value === undefined) {
    return new Uint8Array(0);
  }
  if (!(value instanceof Uint8Array)) {
    throw new Error(`cbor: exec frame ${key} must be a byte string`);
  }
  return value;
}

function readOptionalSafeInteger(map: CborMap, key: string, fallback: number): number {
  const value = map[key];
  if (value === undefined) {
    return fallback;
  }
  if (typeof value === "number" && Number.isInteger(value)) {
    return value;
  }
  throw new Error(`cbor: exec frame ${key} must be a safe integer`);
}

function encodeMap(entries: EncodedMapEntry[]): Uint8Array {
  const chunks: Uint8Array[] = [encodeHead(5, entries.length)];
  for (const [key, value] of entries) {
    chunks.push(encodeText(key), value);
  }
  return concat(chunks);
}

function encodeText(value: string): Uint8Array {
  const bytes = textEncoder.encode(value);
  return concat([encodeHead(3, bytes.byteLength), bytes]);
}

function encodeBytes(value: Uint8Array): Uint8Array {
  return concat([encodeHead(2, value.byteLength), value]);
}

function encodeInteger(value: number): Uint8Array {
  if (!Number.isInteger(value)) {
    throw new Error("cbor: integers must be whole numbers");
  }
  if (value >= 0) {
    return encodeUnsigned(value);
  }
  return encodeHead(1, BigInt(-1 - value));
}

function encodeUnsigned(value: number): Uint8Array {
  if (!Number.isInteger(value) || value < 0) {
    throw new Error("cbor: unsigned integers must be non-negative whole numbers");
  }
  return encodeHead(0, value);
}

function encodeUint16Field(key: string, value: number): Uint8Array {
  if (!Number.isInteger(value) || value < 0 || value > maxUint16) {
    throw new Error(`cbor: exec frame ${key} must be an integer in [0, 65535]`);
  }
  return encodeUnsigned(value);
}

function encodeHead(major: number, value: number | bigint): Uint8Array {
  const raw = typeof value === "bigint" ? value : BigInt(value);
  if (raw < 0n) {
    throw new Error("cbor: negative head argument");
  }
  if (raw <= 23n) {
    return Uint8Array.of((major << 5) | Number(raw));
  }
  if (raw <= 0xffn) {
    return Uint8Array.of((major << 5) | 24, Number(raw));
  }
  if (raw <= 0xffffn) {
    return Uint8Array.of((major << 5) | 25, Number(raw >> 8n), Number(raw & 0xffn));
  }
  if (raw <= 0xffffffffn) {
    return Uint8Array.of(
      (major << 5) | 26,
      Number((raw >> 24n) & 0xffn),
      Number((raw >> 16n) & 0xffn),
      Number((raw >> 8n) & 0xffn),
      Number(raw & 0xffn),
    );
  }
  if (raw <= 0xffffffffffffffffn) {
    return Uint8Array.of(
      (major << 5) | 27,
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
  throw new Error("cbor: integer exceeds uint64 range");
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
