import type { IncomingMessage } from "node:http";

export async function* ndjsonLines(source: AsyncIterable<unknown>): AsyncGenerator<string> {
  let pending = "";
  for await (const chunk of source) {
    pending += chunkToText(chunk);
    for (;;) {
      const newline = pending.indexOf("\n");
      if (newline < 0) {
        break;
      }
      const line = pending.slice(0, newline);
      pending = pending.slice(newline + 1);
      if (line !== "") {
        yield line;
      }
    }
  }
  if (pending !== "") {
    yield pending;
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

function chunkToText(chunk: unknown): string {
  if (typeof chunk === "string") {
    return chunk;
  }
  if (chunk instanceof Uint8Array) {
    return Buffer.from(chunk).toString("utf8");
  }
  throw new Error("ndjson: response chunk is not bytes");
}
