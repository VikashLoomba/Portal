import assert from "node:assert/strict";
import { Buffer } from "node:buffer";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { ExecStreamWinch, decode, decodeExecFrame, encode, encodeExecFrame } from "../src/cbor.ts";
import type { CborMap, CborValue, ExecFrame } from "../src/cbor.ts";

const vectorDir = fileURLToPath(new URL("../../../docs/vectors/", import.meta.url));
const exactIntMarker = "__PORTAL_JSON_INT__:";

const execVectors = ["exec_stdin", "exec_stdout", "exec_stderr", "exec_exit", "exec_error", "exec_winch", "exec_pty_stdout"];

for (const name of execVectors) {
  test(`exec vector ${name}`, async () => {
    const encoded = await readHexVector(name);
    const decoded = decodeExecFrame(encoded);
    const expected = await readExecSidecar(name);
    assertExecFrame(decoded, expected);

    const redecoded = decodeExecFrame(encodeExecFrame(decoded));
    assertExecFrame(redecoded, expected);
  });
}

test("encodeExecFrame enforces uint16 winch dimensions", () => {
  const cases = [
    { name: "rows over uint16", frame: { stream: ExecStreamWinch, rows: 70000 } },
    { name: "cols over uint16", frame: { stream: ExecStreamWinch, cols: 70000 } },
    { name: "rows negative", frame: { stream: ExecStreamWinch, rows: -1 } },
    { name: "rows fractional", frame: { stream: ExecStreamWinch, rows: 1.5 } },
  ];
  for (const tt of cases) {
    assert.throws(
      () => encodeExecFrame(tt.frame),
      /cbor: exec frame (rs|cs) must be an integer in \[0, 65535\]/,
      tt.name,
    );
  }

  const decoded = decodeExecFrame(encodeExecFrame({ stream: ExecStreamWinch, rows: 65535, cols: 65535 }));
  assert.equal(decoded.rows, 65535);
  assert.equal(decoded.cols, 65535);
});

const protocolMappings: ProtocolMapping[] = [
  {
    name: "protocol_hello",
    cborArm: "hello",
    jsonArm: "Hello",
    fields: [
      ["pv", "ProtoVersion"],
      ["sha", "ClientGitSHA"],
      ["pid", "ClientPID"],
      ["poll_ms", "PollIntervalMs"],
      ["destroy_mc", "WantDestroyMC"],
      ["services", "Services"],
    ],
  },
  {
    name: "protocol_subscribe",
    cborArm: "subscribe",
    jsonArm: "Subscribe",
    fields: [
      ["deny", "Deny"],
      ["allow", "Allow"],
      ["exc_eph", "ExcludeEphemeral"],
      ["rsid", "ResubscribeID"],
    ],
  },
  { name: "protocol_ping", cborArm: "ping", jsonArm: "Ping", fields: [["n", "Nonce"]] },
  { name: "protocol_req_snap", cborArm: "req_snap", jsonArm: "ReqSnap", fields: [] },
  { name: "protocol_shutdown", cborArm: "shutdown", jsonArm: "Shutdown", fields: [["reason", "Reason"]] },
  {
    name: "protocol_hello_ack",
    cborArm: "hello_ack",
    jsonArm: "HelloAck",
    fields: [
      ["pv", "ProtoVersion"],
      ["sha", "AgentGitSHA"],
      ["pid", "AgentPID"],
      ["kern", "Kernel"],
      ["boot", "BootID"],
      ["emin", "EphemMin"],
      ["emax", "EphemMax"],
      ["now", "NowUnixNano"],
      ["services", "Services"],
    ],
  },
  { name: "protocol_subscribe_ack", cborArm: "subscribe_ack", jsonArm: "SubscribeAck", fields: [["rsid", "ResubscribeID"]] },
  {
    name: "protocol_snapshot",
    cborArm: "snapshot",
    jsonArm: "Snapshot",
    fields: [
      ["seq", "Seq"],
      ["ts", "GeneratedAt"],
      ["ports", "Ports", "ports"],
    ],
  },
  {
    name: "protocol_port_added",
    cborArm: "port_added",
    jsonArm: "PortAdded",
    fields: [
      ["seq", "Seq"],
      ["p", "Port", "port"],
      ["ts", "At"],
    ],
  },
  {
    name: "protocol_port_removed",
    cborArm: "port_removed",
    jsonArm: "PortRemoved",
    fields: [
      ["seq", "Seq"],
      ["port", "Port"],
      ["fam", "Family"],
      ["ts", "At"],
      ["src", "Source"],
    ],
  },
  {
    name: "protocol_heartbeat",
    cborArm: "heartbeat",
    jsonArm: "Heartbeat",
    fields: [
      ["seq", "Seq"],
      ["up", "UptimeNano"],
      ["now", "Now"],
      ["n", "Nonce"],
    ],
  },
  {
    name: "protocol_agent_error",
    cborArm: "agent_error",
    jsonArm: "AgentError",
    fields: [
      ["code", "Code"],
      ["msg", "Msg"],
      ["fatal", "Fatal"],
    ],
  },
  { name: "protocol_bye", cborArm: "bye", jsonArm: "Bye", fields: [["reason", "Reason"]] },
  {
    name: "protocol_msg",
    cborArm: "msg",
    jsonArm: "Msg",
    fields: [
      ["svc", "Service"],
      ["k", "Kind"],
      ["seq", "Seq"],
      ["p", "Payload", "payload"],
    ],
  },
  {
    name: "protocol_cred_request_full",
    cborArm: "msg",
    jsonArm: "Msg",
    fields: [
      ["svc", "Service"],
      ["k", "Kind"],
      ["seq", "Seq"],
      ["p", "Payload", "payload"],
    ],
  },
  {
    name: "protocol_cred_request_minimal",
    cborArm: "msg",
    jsonArm: "Msg",
    fields: [
      ["svc", "Service"],
      ["k", "Kind"],
      ["seq", "Seq"],
      ["p", "Payload", "payload"],
    ],
  },
  {
    name: "protocol_cred_response_ok",
    cborArm: "msg",
    jsonArm: "Msg",
    fields: [
      ["svc", "Service"],
      ["k", "Kind"],
      ["seq", "Seq"],
      ["p", "Payload", "payload"],
    ],
  },
  {
    name: "protocol_cred_response_deny",
    cborArm: "msg",
    jsonArm: "Msg",
    fields: [
      ["svc", "Service"],
      ["k", "Kind"],
      ["seq", "Seq"],
      ["p", "Payload", "payload"],
    ],
  },
];

for (const mapping of protocolMappings) {
  test(`protocol vector ${mapping.name}`, async () => {
    const encoded = await readHexVector(mapping.name);
    const payload = stripProtocolFrame(encoded);
    const decoded = decode(payload);
    assert.deepStrictEqual(Buffer.from(encode(decoded)), Buffer.from(payload));
    const actualEnvelope = requireCborMap(decoded, "envelope");
    assert.deepEqual(Object.keys(actualEnvelope), [mapping.cborArm]);

    const actualArm = normalizeActualArm(mapping, actualEnvelope[mapping.cborArm]);
    const sidecar = requireExactObject(parseExactJson(await readJsonText(mapping.name)), "sidecar");
    assertSidecarArms(sidecar, mapping.jsonArm);
    const expectedArm = normalizeExpectedArm(mapping, sidecar[mapping.jsonArm]);
    assert.deepStrictEqual(actualArm, expectedArm);
  });
}

interface ExecSidecar {
  Stream: string;
  Data: Uint8Array;
  Code: number;
  Rows: number;
  Cols: number;
}

type FieldShape = "plain" | "port" | "ports" | "payload";
type FieldMapping = [cbor: string, json: string, shape?: FieldShape];

interface ProtocolMapping {
  name: string;
  cborArm: string;
  jsonArm: string;
  fields: FieldMapping[];
}

type ExactValue = null | boolean | string | bigint | ExactValue[] | ExactObject;

interface ExactObject {
  [key: string]: ExactValue;
}

async function readHexVector(name: string): Promise<Uint8Array> {
  const text = await readFile(new URL(`${name}.hex`, `file://${vectorDir}/`), "utf8");
  return Buffer.from(text.split(/\s+/).join(""), "hex");
}

function readJsonText(name: string): Promise<string> {
  return readFile(new URL(`${name}.json`, `file://${vectorDir}/`), "utf8");
}

async function readExecSidecar(name: string): Promise<ExecSidecar> {
  const parsed: unknown = JSON.parse(await readJsonText(name));
  const object = requireUnknownRecord(parsed, `${name}.json`);
  const stream = object.Stream;
  const data = object.Data;
  const code = object.Code;
  const rows = object.Rows;
  const cols = object.Cols;
  if (typeof stream !== "string") {
    throw new Error(`${name}.json Stream must be a string`);
  }
  if (typeof code !== "number" || typeof rows !== "number" || typeof cols !== "number") {
    throw new Error(`${name}.json numeric fields must be numbers`);
  }
  if (data !== null && typeof data !== "string") {
    throw new Error(`${name}.json Data must be base64 or null`);
  }
  return {
    Stream: stream,
    Data: data === null ? new Uint8Array(0) : Buffer.from(data, "base64"),
    Code: code,
    Rows: rows,
    Cols: cols,
  };
}

function assertExecFrame(actual: ExecFrame, expected: ExecSidecar): void {
  assert.equal(actual.stream, expected.Stream);
  assert.deepStrictEqual(Buffer.from(actual.data), Buffer.from(expected.Data));
  assert.equal(actual.code, expected.Code);
  assert.equal(actual.rows, expected.Rows);
  assert.equal(actual.cols, expected.Cols);
}

function stripProtocolFrame(encoded: Uint8Array): Uint8Array {
  const bytes = Buffer.from(encoded);
  assert.equal(bytes[0], 0x50);
  assert.equal(bytes[1], 0x46);
  const length = bytes.readUInt32BE(2);
  assert.equal(length, bytes.byteLength - 6);
  return bytes.subarray(6);
}

function normalizeActualArm(mapping: ProtocolMapping, arm: CborValue | undefined): ExactObject {
  const source = requireCborMap(arm, mapping.cborArm);
  const out: ExactObject = {};
  for (const field of mapping.fields) {
    const [cborName, jsonName, shape = "plain"] = field;
    if (!(cborName in source)) {
      continue;
    }
    out[jsonName] = normalizeActualValue(source[cborName], shape);
  }
  return out;
}

function normalizeExpectedArm(mapping: ProtocolMapping, value: ExactValue | undefined): ExactObject {
  const source = requireExactObject(value, mapping.jsonArm);
  const out: ExactObject = {};
  for (const field of mapping.fields) {
    const [, jsonName, shape = "plain"] = field;
    if (!(jsonName in source)) {
      continue;
    }
    if (shape === "payload") {
      const rawPayload = source[jsonName];
      if (typeof rawPayload !== "string") {
        throw new Error(`${mapping.name} Payload must be base64`);
      }
      out[jsonName] = normalizeNumbers(decode(Buffer.from(rawPayload, "base64")));
    } else {
      out[jsonName] = source[jsonName];
    }
  }
  return out;
}

function normalizeActualValue(value: CborValue | undefined, shape: FieldShape): ExactValue {
  if (value === undefined) {
    throw new Error("protocol vector field missing");
  }
  if (shape === "port") {
    return normalizePort(requireCborMap(value, "port"));
  }
  if (shape === "ports") {
    if (!Array.isArray(value)) {
      throw new Error("protocol ports field must be an array");
    }
    const ports: ExactValue[] = [];
    for (const item of value) {
      ports.push(normalizePort(requireCborMap(item, "port")));
    }
    return ports;
  }
  return normalizeNumbers(value);
}

function normalizePort(value: CborMap): ExactObject {
  return {
    Port: normalizeNumbers(value.port),
    Family: normalizeNumbers(value.fam),
    Addr: normalizeNumbers(value.addr),
    InodeNS: normalizeNumbers(value.ns),
  };
}

function normalizeNumbers(value: CborValue | undefined): ExactValue {
  if (value === undefined) {
    throw new Error("cannot normalize missing value");
  }
  if (typeof value === "number") {
    assert.equal(Number.isInteger(value), true);
    return BigInt(value);
  }
  if (typeof value === "bigint" || typeof value === "string" || typeof value === "boolean" || value === null) {
    return value;
  }
  if (value instanceof Uint8Array) {
    return Buffer.from(value).toString("base64");
  }
  if (Array.isArray(value)) {
    return value.map((item) => normalizeNumbers(item));
  }
  const out: ExactObject = {};
  for (const [key, nested] of Object.entries(value)) {
    out[key] = normalizeNumbers(nested);
  }
  return out;
}

function parseExactJson(text: string): ExactValue {
  const parsed: unknown = JSON.parse(markIntegerNumbers(text));
  return unmarkIntegers(parsed);
}

function markIntegerNumbers(text: string): string {
  let out = "";
  let i = 0;
  let inString = false;
  let escaping = false;
  while (i < text.length) {
    const ch = text[i];
    if (inString) {
      out += ch;
      if (escaping) {
        escaping = false;
      } else if (ch === "\\") {
        escaping = true;
      } else if (ch === "\"") {
        inString = false;
      }
      i += 1;
      continue;
    }
    if (ch === "\"") {
      inString = true;
      out += ch;
      i += 1;
      continue;
    }
    if (ch === "-" || isDigit(ch)) {
      const start = i;
      i = scanJsonNumber(text, i);
      const token = text.slice(start, i);
      if (!token.includes(".") && !token.includes("e") && !token.includes("E")) {
        out += `"${exactIntMarker}${token}"`;
      } else {
        out += token;
      }
      continue;
    }
    out += ch;
    i += 1;
  }
  return out;
}

function scanJsonNumber(text: string, start: number): number {
  let i = start;
  if (text[i] === "-") {
    i += 1;
  }
  while (i < text.length && isDigit(text[i])) {
    i += 1;
  }
  if (text[i] === ".") {
    i += 1;
    while (i < text.length && isDigit(text[i])) {
      i += 1;
    }
  }
  if (text[i] === "e" || text[i] === "E") {
    i += 1;
    if (text[i] === "+" || text[i] === "-") {
      i += 1;
    }
    while (i < text.length && isDigit(text[i])) {
      i += 1;
    }
  }
  return i;
}

function isDigit(ch: string): boolean {
  return ch >= "0" && ch <= "9";
}

function unmarkIntegers(value: unknown): ExactValue {
  if (typeof value === "string") {
    if (value.startsWith(exactIntMarker)) {
      return BigInt(value.slice(exactIntMarker.length));
    }
    return value;
  }
  if (typeof value === "boolean" || value === null) {
    return value;
  }
  if (Array.isArray(value)) {
    return value.map((item) => unmarkIntegers(item));
  }
  const record = requireUnknownRecord(value, "json object");
  const out: ExactObject = {};
  for (const [key, nested] of Object.entries(record)) {
    out[key] = unmarkIntegers(nested);
  }
  return out;
}

function assertSidecarArms(sidecar: ExactObject, active: string): void {
  const arms = [
    "Hello",
    "Subscribe",
    "Ping",
    "ReqSnap",
    "Shutdown",
    "HelloAck",
    "SubscribeAck",
    "Snapshot",
    "PortAdded",
    "PortRemoved",
    "Heartbeat",
    "AgentError",
    "Bye",
    "Msg",
  ];
  for (const arm of arms) {
    if (arm === active) {
      assert.notEqual(sidecar[arm], null);
    } else {
      assert.equal(sidecar[arm], null);
    }
  }
}

function requireCborMap(value: CborValue | undefined, label: string): CborMap {
  if (!isCborMap(value)) {
    throw new Error(`${label} must be a CBOR map`);
  }
  return value;
}

function requireExactObject(value: ExactValue | undefined, label: string): ExactObject {
  if (!isExactObject(value)) {
    throw new Error(`${label} must be an object`);
  }
  return value;
}

function requireUnknownRecord(value: unknown, label: string): Record<string, unknown> {
  if (!isUnknownRecord(value)) {
    throw new Error(`${label} must be an object`);
  }
  return value;
}

function isCborMap(value: CborValue | undefined): value is CborMap {
  return typeof value === "object" && value !== null && !Array.isArray(value) && !(value instanceof Uint8Array);
}

function isExactObject(value: ExactValue | undefined): value is ExactObject {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function isUnknownRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
