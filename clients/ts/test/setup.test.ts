import assert from "node:assert/strict";
import type { IncomingMessage, ServerResponse } from "node:http";
import test from "node:test";

import type { SetupEvent } from "../src/dto.ts";
import { ApiError, createClient } from "../src/http.ts";
import { setup } from "../src/setup.ts";
import { FakeHttpServer } from "./fake-http-server.ts";

const successfulTail: SetupEvent[] = [
  { step: "configure", status: "running" },
  { step: "configure", status: "ok" },
  { step: "xdg-open", status: "running" },
  { step: "xdg-open", status: "ok" },
  { step: "clip-shims", status: "running" },
  { step: "clip-shims", status: "ok" },
  { step: "agent-symlink", status: "running" },
  { step: "agent-symlink", status: "ok" },
  { step: "activate", status: "running" },
  { step: "activate", status: "ok" },
  { step: "doctor", status: "running" },
  { step: "doctor", status: "ok", report: { host: "devbox", checks: [] } },
  { step: "done", status: "ok" },
];

test("setup posts the DTO and yields the complete success stream", async (t) => {
  const expected: SetupEvent[] = [
    { step: "validate", status: "running", line: "checking ssh" },
    { step: "validate", status: "ok" },
    ...successfulTail,
  ];
  const fake = await FakeHttpServer.start(t, async (req, resp) => {
    assert.equal(req.method, "POST");
    assert.equal(req.url, "/v1/setup");
    assert.equal(req.headers["content-type"], "application/json");
    const body = await readRequestBody(req);
    assert.equal(req.headers["content-length"], String(Buffer.byteLength(body)));
    const parsed: unknown = JSON.parse(body);
    assert.deepStrictEqual(parsed, { host: "user@box", force: false });
    writeSetupEvents(resp, expected, false, true);
  });

  const got = await collectSetup(fake.path, { host: "user@box", force: false });
  assert.deepStrictEqual(got, expected);
  assert.deepStrictEqual(got.at(-2)?.report, { host: "devbox", checks: [] });
  await fake.done;
});

test("setup yields an in-band configure failure without throwing", async (t) => {
  const expected: SetupEvent[] = [
    { step: "validate", status: "running" },
    { step: "validate", status: "ok" },
    { step: "configure", status: "running" },
    { step: "configure", status: "fail", error: { code: "configure_failed", message: "disk full" } },
    { step: "done", status: "fail" },
  ];
  const fake = await FakeHttpServer.start(t, (_req, resp) => writeSetupEvents(resp, expected));

  assert.deepStrictEqual(await collectSetup(fake.path, { host: "box" }), expected);
  await fake.done;
});

test("setup sends force and yields validate warn followed by done ok", async (t) => {
  const expected: SetupEvent[] = [
    { step: "validate", status: "running" },
    { step: "validate", status: "warn", error: { code: "validation_failed", message: "ssh unreachable" } },
    ...successfulTail,
  ];
  const fake = await FakeHttpServer.start(t, async (req, resp) => {
    const parsed: unknown = JSON.parse(await readRequestBody(req));
    assert.deepStrictEqual(parsed, { host: "box", force: true });
    writeSetupEvents(resp, expected);
  });

  assert.deepStrictEqual(await collectSetup(fake.path, { host: "box", force: true }), expected);
  await fake.done;
});

test("setup yields activate failure and done fail without throwing", async (t) => {
  const activeHost = "old-box";
  const expected: SetupEvent[] = [
    { step: "validate", status: "running" },
    { step: "validate", status: "ok" },
    { step: "configure", status: "running" },
    { step: "configure", status: "ok" },
    { step: "xdg-open", status: "running" },
    { step: "xdg-open", status: "ok" },
    { step: "clip-shims", status: "running" },
    { step: "clip-shims", status: "ok" },
    { step: "agent-symlink", status: "running" },
    { step: "agent-symlink", status: "ok" },
    { step: "activate", status: "running" },
    { step: "activate", status: "fail", error: { code: "activate_failed", message: "construct failed" } },
    { step: "done", status: "fail" },
  ];
  const fake = await FakeHttpServer.start(t, (req, resp) => {
    if (req.method === "GET" && req.url === "/v1/status") {
      resp.writeHead(200, { "Content-Type": "application/json" });
      resp.end(JSON.stringify({ host: activeHost }));
      return;
    }
    assert.equal(req.method, "POST");
    assert.equal(req.url, "/v1/setup");
    writeSetupEvents(resp, expected);
  });

  assert.deepStrictEqual(await collectSetup(fake.path, { host: "new-box" }), expected);
  assert.equal((await createClient(fake.path).status()).host, "old-box");
  await fake.done;
});

for (const rejection of [
  { status: 400, code: "invalid_request", message: "host is required" },
  { status: 409, code: "setup_in_progress", message: "setup already running" },
]) {
  test(`setup throws ApiError for pre-stream ${rejection.status}`, async (t) => {
    const fake = await FakeHttpServer.start(t, (_req, resp) => {
      resp.writeHead(rejection.status, { "Content-Type": "application/json" });
      resp.end(JSON.stringify({ error: { code: rejection.code, message: rejection.message } }));
    });

    const error = await captureError(collectSetup(fake.path, { host: "box" }));
    assert.ok(error instanceof ApiError);
    assert.equal(error.status, rejection.status);
    assert.equal(error.code, rejection.code);
    assert.equal(error.detail, rejection.message);
    await fake.done;
  });
}

test("setup abort disconnects an in-flight stream", async (t) => {
  const fake = await FakeHttpServer.start(t, async (req, resp) => {
    await readRequestBody(req);
    resp.writeHead(200, { "Content-Type": "application/x-ndjson" });
    resp.write(`${JSON.stringify({ step: "validate", status: "running" })}\n`);
    await new Promise<void>((resolve) => req.socket.once("close", resolve));
  });
  const controller = new AbortController();
  const iterator = setup(fake.path, { host: "box" }, { signal: controller.signal })[Symbol.asyncIterator]();

  const first = await withTimeout(iterator.next(), 1000);
  assert.deepStrictEqual(first, { done: false, value: { step: "validate", status: "running" } });
  controller.abort();
  await withTimeout(iterator.next().then(() => undefined, () => undefined), 1000);
  await withTimeout(fake.done, 1000);
});

async function collectSetup(socketPath: string, req: { host?: string; force?: boolean }): Promise<SetupEvent[]> {
  const events: SetupEvent[] = [];
  for await (const event of setup(socketPath, req)) {
    events.push(event);
  }
  return events;
}

async function readRequestBody(req: IncomingMessage): Promise<string> {
  req.setEncoding("utf8");
  let body = "";
  for await (const chunk of req) {
    if (typeof chunk !== "string") {
      throw new Error("request body chunk is not text");
    }
    body += chunk;
  }
  return body;
}

function writeSetupEvents(resp: ServerResponse, events: readonly SetupEvent[], finalNewline: boolean = true, blankLines: boolean = false): void {
  resp.writeHead(200, { "Content-Type": "application/x-ndjson" });
  const separator = blankLines ? "\n\n" : "\n";
  const body = events.map((event) => JSON.stringify(event)).join(separator);
  resp.end(finalNewline ? `${body}\n` : body);
}

async function captureError(promise: Promise<unknown>): Promise<Error | null> {
  try {
    await promise;
  } catch (error) {
    return toError(error);
  }
  return null;
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

function toError(value: unknown): Error {
  if (value instanceof Error) {
    return value;
  }
  if (typeof value === "string") {
    return new Error(value);
  }
  return new Error("unknown error");
}
