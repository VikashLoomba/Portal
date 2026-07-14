import assert from "node:assert/strict";
import test from "node:test";

import type { Event } from "../src/dto.ts";
import { events } from "../src/events.ts";
import { ApiError } from "../src/http.ts";
import { FakeHttpServer } from "./fake-http-server.ts";

test("events reads the complete NDJSON stream", async (t) => {
  const expected: Event[] = [
    { type: "status", status: null },
    { type: "notify", notify: null },
  ];
  const fake = await FakeHttpServer.start(t, (req, resp) => {
    assert.equal(req.method, "GET");
    assert.equal(req.url, "/v1/events");
    resp.writeHead(200, { "Content-Type": "application/x-ndjson" });
    resp.write(`${JSON.stringify(expected[0])}\n\n`);
    resp.end(JSON.stringify(expected[1]));
  });

  const got: Event[] = [];
  for await (const event of events(fake.path)) {
    got.push(event);
  }
  assert.deepStrictEqual(got, expected);
  await fake.done;
});

test("events decodes a non-2xx error envelope", async (t) => {
  const fake = await FakeHttpServer.start(t, (_req, resp) => {
    resp.writeHead(503, { "Content-Type": "application/json" });
    resp.end(JSON.stringify({ error: { code: "not_configured", message: "no active host" } }));
  });

  let failure: Error | null = null;
  try {
    for await (const _event of events(fake.path)) {
      // A non-2xx response cannot yield stream events.
    }
  } catch (error) {
    failure = toError(error);
  }
  assert.ok(failure instanceof ApiError);
  assert.equal(failure.status, 503);
  assert.equal(failure.code, "not_configured");
  assert.equal(failure.detail, "no active host");
  await fake.done;
});

function toError(value: unknown): Error {
  if (value instanceof Error) {
    return value;
  }
  if (typeof value === "string") {
    return new Error(value);
  }
  return new Error("unknown error");
}
