import { request } from "node:http";
import type { ClientRequest, IncomingMessage } from "node:http";

import type { SetupEvent, SetupRequest } from "./dto.ts";
import { apiErrorFromStatusBody } from "./http.ts";
import type { PortalRequestOptions } from "./http.ts";
import { ndjsonLines, readErrorBody } from "./ndjson.ts";

const setupBodyLimit = 1 << 20;

export async function* setup(
  socketPath: string,
  req: SetupRequest,
  options: PortalRequestOptions = {},
): AsyncGenerator<SetupEvent> {
  const body = JSON.stringify(req);
  let clientReq: ClientRequest | null = null;
  let resp: IncomingMessage | null = null;

  try {
    const opened = await openSetup(socketPath, body, options);
    clientReq = opened.req;
    resp = opened.resp;

    const status = resp.statusCode ?? 0;
    if (status !== 200) {
      throw apiErrorFromStatusBody(status, resp.statusMessage ?? "", await readErrorBody(resp, setupBodyLimit));
    }

    for await (const line of ndjsonLines(resp)) {
      yield parseSetupEventLine(line);
    }
  } finally {
    resp?.destroy();
    clientReq?.destroy();
  }
}

interface OpenedSetup {
  req: ClientRequest;
  resp: IncomingMessage;
}

function openSetup(socketPath: string, body: string, options: PortalRequestOptions): Promise<OpenedSetup> {
  return new Promise((resolve, reject) => {
    const clientReq = request(
      {
        socketPath,
        method: "POST",
        path: "/v1/setup",
        headers: {
          "Content-Type": "application/json",
          "Content-Length": String(Buffer.byteLength(body)),
        },
        signal: options.signal,
      },
      (resp) => {
        resolve({ req: clientReq, resp });
      },
    );
    clientReq.once("error", reject);
    clientReq.end(body);
  });
}

function parseSetupEventLine(line: string): SetupEvent {
  // The setup stream is line-delimited local API JSON; dto.ts is the schema boundary.
  return JSON.parse(line) as SetupEvent;
}
