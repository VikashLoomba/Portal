import type { IncomingMessage } from "node:http";

const ndjsonLineLimit = 1 << 20;

export async function* ndjsonLines(source: AsyncIterable<unknown>): AsyncGenerator<string> {
  let pending = "";
  let pendingBytes = 0;
  const decoder = new TextDecoder();
  const encoder = new TextEncoder();
  for await (const chunk of source) {
    const text = chunkToText(decoder, chunk);
    let start = 0;
    for (;;) {
      const newline = text.indexOf("\n", start);
      if (newline < 0) {
        break;
      }
      const tail = text.slice(start, newline);
      pendingBytes += encoder.encode(tail).byteLength;
      checkLineSize(pendingBytes);
      const line = pending + tail;
      pending = "";
      pendingBytes = 0;
      start = newline + 1;
      if (line !== "") {
        yield line;
      }
    }
    const tail = text.slice(start);
    pendingBytes += encoder.encode(tail).byteLength;
    checkLineSize(pendingBytes);
    pending += tail;
  }
  const tail = decoder.decode();
  pendingBytes += encoder.encode(tail).byteLength;
  checkLineSize(pendingBytes);
  pending += tail;
  if (pending !== "") {
    yield pending;
  }
}

function checkLineSize(size: number): void {
  if (size > ndjsonLineLimit) {
    throw new Error("ndjson: line exceeds 1 MiB limit");
  }
}

export function readErrorBody(resp: IncomingMessage, limit: number): Promise<string> {
  return new Promise((resolve, reject) => {
    let out = "";
    resp.setEncoding("utf8");
    resp.on("data", (chunk: string) => {
      out += chunk;
      if (out.length > limit) {
        reject(new Error("ndjson: error response body exceeds limit"));
        resp.destroy();
      }
    });
    resp.once("end", () => {
      resolve(out);
    });
    resp.once("error", reject);
  });
}

function chunkToText(decoder: TextDecoder, chunk: unknown): string {
  if (typeof chunk === "string") {
    return decoder.decode() + chunk;
  }
  if (chunk instanceof Uint8Array) {
    return decoder.decode(chunk, { stream: true });
  }
  throw new Error("ndjson: response chunk is not bytes");
}
