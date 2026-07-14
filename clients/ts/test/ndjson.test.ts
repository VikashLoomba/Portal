import assert from "node:assert/strict";
import test from "node:test";

import { ndjsonLines } from "../src/ndjson.ts";

test("ndjsonLines preserves split UTF-8 code points and JSON lines", async () => {
  const encoded = new TextEncoder().encode('{"line":"café"}\n\n{"line":"done"}\n');
  const splitCodePoint = encoded.indexOf(0xc3);
  assert.notEqual(splitCodePoint, -1);

  async function* chunks(): AsyncGenerator<unknown> {
    yield encoded.subarray(0, 5);
    yield encoded.subarray(5, splitCodePoint + 1);
    yield encoded.subarray(splitCodePoint + 1, splitCodePoint + 4);
    yield encoded.subarray(splitCodePoint + 4, encoded.length - 2);
    yield encoded.subarray(encoded.length - 2);
  }

  const lines: string[] = [];
  for await (const line of ndjsonLines(chunks())) {
    lines.push(line);
  }
  assert.deepStrictEqual(lines, ['{"line":"café"}', '{"line":"done"}']);
});

test("ndjsonLines rejects a line larger than 1 MiB", async () => {
  async function* chunks(): AsyncGenerator<unknown> {
    yield new Uint8Array((1 << 20) + 1).fill(0x61);
  }

  await assert.rejects(async () => {
    for await (const _line of ndjsonLines(chunks())) {
      // The oversized line must fail before it can be yielded.
    }
  }, /ndjson: line exceeds 1 MiB limit/);
});
