import { request } from "node:http";
import type { ClientRequest, IncomingMessage } from "node:http";

import type { ErrorDetail, SetupEvent, SetupRequest } from "./dto.ts";
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
  const value: unknown = JSON.parse(line);
  if (!isSetupEvent(value)) {
    throw new Error("setup: invalid event payload");
  }
  return value;
}

function isSetupEvent(value: unknown): value is SetupEvent {
  if (typeof value !== "object" || value === null) {
    return false;
  }
  if (!("step" in value) || typeof value.step !== "string") {
    return false;
  }
  if (!("status" in value) || typeof value.status !== "string") {
    return false;
  }
  if ("line" in value && value.line !== undefined && typeof value.line !== "string") {
    return false;
  }
  return !("error" in value && value.error !== undefined && !isErrorDetail(value.error));
}

function isErrorDetail(value: unknown): value is ErrorDetail {
  return typeof value === "object" && value !== null &&
    "code" in value && typeof value.code === "string" &&
    "message" in value && typeof value.message === "string";
}
