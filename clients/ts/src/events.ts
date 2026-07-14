import { request } from "node:http";
import type { ClientRequest, IncomingMessage } from "node:http";

import type { Event } from "./dto.ts";
import { apiErrorFromStatusBody } from "./http.ts";
import type { PortalRequestOptions } from "./http.ts";
import { ndjsonLines, readErrorBody } from "./ndjson.ts";

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
      const body = await readErrorBody(resp, eventsBodyLimit);
      throw apiErrorFromStatusBody(status, resp.statusMessage ?? "", body);
    }

    for await (const line of ndjsonLines(resp)) {
      yield parseEventLine(line);
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
