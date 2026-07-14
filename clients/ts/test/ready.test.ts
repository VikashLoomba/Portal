import assert from "node:assert/strict";
import { setTimeout as delay } from "node:timers/promises";
import test from "node:test";

import { waitReady } from "../src/ready.ts";
import { FakeHttpServer } from "./fake-http-server.ts";

test("waitReady retries a failed version probe and resolves", async (t) => {
  let probes = 0;
  const fake = await FakeHttpServer.start(t, (req, resp) => {
    assert.equal(req.method, "GET");
    assert.equal(req.url, "/v1/version");
    probes += 1;
    if (probes === 1) {
      req.socket.destroy();
      return;
    }
    resp.writeHead(200, { "Content-Type": "application/json" });
    resp.end(JSON.stringify({ version: "test", gitSha: "deadbeef", protoVersion: 4 }));
  });

  await waitReady(fake.path, { timeoutMs: 1000, pollIntervalMs: 10 });
  assert.equal(probes, 2);
});

test("waitReady accepts any HTTP response as ready", async (t) => {
  const fake = await FakeHttpServer.start(t, (_req, resp) => {
    resp.writeHead(503, { "Content-Type": "application/json" });
    resp.end(JSON.stringify({ error: { code: "starting", message: "not ready" } }));
  });

  await waitReady(fake.path, { timeoutMs: 1000, pollIntervalMs: 10 });
  await fake.done;
});

test("waitReady times out an accepting server that never responds", async (t) => {
  let requestSeen = false;
  const fake = await FakeHttpServer.start(t, (req) => {
    assert.equal(req.url, "/v1/version");
    requestSeen = true;
  });

  const error = await captureError(withTimeout(waitReady(fake.path, { timeoutMs: 120, pollIntervalMs: 10 }), 1000));
  assert.equal(requestSeen, true);
  assert.equal(error?.message, `waitReady: socket ${fake.path} not ready within 120ms`);
});

test("waitReady preserves the caller abort reason", async (t) => {
  let markSeen: () => void = () => {};
  const seen = new Promise<void>((resolve) => {
    markSeen = resolve;
  });
  const fake = await FakeHttpServer.start(t, () => {
    markSeen();
  });
  const controller = new AbortController();
  const reason = new Error("spawn cancelled");
  const waiting = waitReady(fake.path, { timeoutMs: 1000, signal: controller.signal, pollIntervalMs: 10 });

  await withTimeout(seen, 1000);
  controller.abort(reason);
  const error = await captureError(withTimeout(waiting, 1000));
  assert.strictEqual(error, reason);
});

test("waitReady deadline aborts a retry sleep", async () => {
  const socketPath = `/tmp/portal-ready-missing-${process.pid}-${Date.now()}.sock`;
  const error = await captureError(withTimeout(waitReady(socketPath, { timeoutMs: 50, pollIntervalMs: 1000 }), 500));
  assert.equal(error?.message, `waitReady: socket ${socketPath} not ready within 50ms`);
});

async function captureError(promise: Promise<unknown>): Promise<Error | null> {
  try {
    await promise;
  } catch (error) {
    return toError(error);
  }
  return null;
}

async function withTimeout<T>(promise: Promise<T>, ms: number): Promise<T> {
  const controller = new AbortController();
  try {
    return await Promise.race([
      promise,
      delay(ms, undefined, { signal: controller.signal }).then(() => Promise.reject(new Error(`timeout after ${ms}ms`))),
    ]);
  } finally {
    controller.abort();
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
