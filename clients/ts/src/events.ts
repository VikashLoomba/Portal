import { request } from "node:http";
import type { ClientRequest, IncomingMessage } from "node:http";

import type { Event } from "./dto.ts";
import { apiErrorFromStatusBody } from "./http.ts";
import type { PortalRequestOptions } from "./http.ts";

const eventsBodyLimit = 1 << 20;

export async function* events(socketPath: string, options: PortalRequestOptions = {}): AsyncGenerator<Event> {
  let req: ClientRequest | null = null;
  let resp: IncomingMessage | null = null;

  try {
    const opened = await openEvents(socketPath, options);
    req = opened.req;
    resp = opened.resp;

    const status = resp.statusCode ?? 0;
    if (status !== 200) {
      const body = await readBodyText(resp, eventsBodyLimit);
      throw apiErrorFromStatusBody(status, resp.statusMessage ?? "", body);
    }

    let pending = "";
    for await (const chunk of resp) {
      pending += chunkToText(chunk);
      for (;;) {
        const newline = pending.indexOf("\n");
        if (newline < 0) {
          break;
        }
        const line = pending.slice(0, newline);
        pending = pending.slice(newline + 1);
        if (line !== "") {
          yield parseEventLine(line);
        }
      }
    }
    if (pending !== "") {
      yield parseEventLine(pending);
    }
  } finally {
    resp?.destroy();
    req?.destroy();
  }
}

interface OpenedEvents {
  req: ClientRequest;
  resp: IncomingMessage;
}

function openEvents(socketPath: string, options: PortalRequestOptions): Promise<OpenedEvents> {
  return new Promise((resolve, reject) => {
    const req = request(
      {
        socketPath,
        method: "GET",
        path: "/v1/events",
        signal: options.signal,
      },
      (resp) => {
        resolve({ req, resp });
      },
    );
    req.once("error", reject);
    req.end();
  });
}

function parseEventLine(line: string): Event {
  // The events stream is line-delimited local API JSON; dto.ts is the schema boundary.
  return JSON.parse(line) as Event;
}

function readBodyText(resp: IncomingMessage, limit: number): Promise<string> {
  return new Promise((resolve, reject) => {
    let out = "";
    resp.setEncoding("utf8");
    resp.on("data", (chunk: string) => {
      out += chunk;
      if (out.length > limit) {
        reject(new Error("events: error response body exceeds limit"));
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
  throw new Error("events: response chunk is not bytes");
}
