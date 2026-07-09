import { request } from "node:http";
import type { IncomingMessage } from "node:http";

import type { ErrorBody, PortStatus, Status, VersionInfo } from "./dto.ts";

export interface PortalRequestOptions {
  signal?: AbortSignal;
}

export class ApiError extends Error {
  status: number;
  code: string;
  detail: string;

  constructor(status: number, code: string, detail: string) {
    const machineCode = code === "" ? "http_error" : code;
    super(`localapi ${status} ${machineCode}: ${detail}`);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.detail = detail;
  }
}

export class PortalClient {
  socketPath: string;

  constructor(socketPath: string) {
    this.socketPath = socketPath;
  }

  version(options: PortalRequestOptions = {}): Promise<VersionInfo> {
    return this.requestJson("GET", "/v1/version", 200, undefined, options);
  }

  status(options: PortalRequestOptions = {}): Promise<Status> {
    return this.requestJson("GET", "/v1/status", 200, undefined, options);
  }

  ports(options: PortalRequestOptions = {}): Promise<PortStatus[]> {
    return this.requestJson("GET", "/v1/ports", 200, undefined, options);
  }

  async allow(port: number, options: PortalRequestOptions = {}): Promise<number[]> {
    const out = await this.requestJson<AllowlistResponse>("PUT", `/v1/allow/${encodePathSegment(String(port))}`, 200, undefined, options);
    return out.allowed ?? [];
  }

  async unallow(port: number, options: PortalRequestOptions = {}): Promise<number[]> {
    const out = await this.requestJson<AllowlistResponse>("DELETE", `/v1/allow/${encodePathSegment(String(port))}`, 200, undefined, options);
    return out.allowed ?? [];
  }

  features(options: PortalRequestOptions = {}): Promise<Record<string, boolean>> {
    return this.requestJson("GET", "/v1/features", 200, undefined, options);
  }

  setFeature(name: string, enabled: boolean, options: PortalRequestOptions = {}): Promise<Record<string, boolean>> {
    return this.requestJson("PUT", `/v1/features/${encodePathSegment(name)}`, 200, JSON.stringify({ enabled }), options);
  }

  async reconcile(options: PortalRequestOptions = {}): Promise<void> {
    await this.requestText("POST", "/v1/reconcile", 202, undefined, options);
  }

  async requestJson<T>(
    method: string,
    path: string,
    expectedStatus: number,
    body: string | undefined,
    options: PortalRequestOptions,
  ): Promise<T> {
    const text = await this.requestText(method, path, expectedStatus, body, options);
    return parseJsonTrusted<T>(text);
  }

  requestText(
    method: string,
    path: string,
    expectedStatus: number,
    body: string | undefined,
    options: PortalRequestOptions,
  ): Promise<string> {
    return new Promise((resolve, reject) => {
      const headers: Record<string, string> = {};
      if (body !== undefined) {
        headers["Content-Type"] = "application/json";
        headers["Content-Length"] = String(Buffer.byteLength(body));
      }

      const req = request(
        {
          socketPath: this.socketPath,
          method,
          path,
          headers,
          signal: options.signal,
        },
        (resp) => {
          void handleTextResponse(resp, expectedStatus).then(resolve, reject);
        },
      );
      req.once("error", reject);
      if (body === undefined) {
        req.end();
      } else {
        req.end(body);
      }
    });
  }
}

export function createClient(socketPath: string): PortalClient {
  return new PortalClient(socketPath);
}

export function apiErrorFromStatusBody(status: number, statusMessage: string, body: string): ApiError {
  const envelope = parseErrorBody(body);
  if (envelope !== null) {
    return new ApiError(status, envelope.error.code, envelope.error.message);
  }
  const detail = body.trim() === "" ? statusMessage || `HTTP ${status}` : body.trim();
  return new ApiError(status, "", detail);
}

interface AllowlistResponse {
  allowed: number[] | null;
}

async function handleTextResponse(resp: IncomingMessage, expectedStatus: number): Promise<string> {
  const body = await readBodyText(resp);
  const status = resp.statusCode ?? 0;
  if (status !== expectedStatus) {
    throw apiErrorFromStatusBody(status, resp.statusMessage ?? "", body);
  }
  return body;
}

function readBodyText(resp: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    let out = "";
    resp.setEncoding("utf8");
    resp.on("data", (chunk: string) => {
      out += chunk;
    });
    resp.once("end", () => {
      resolve(out);
    });
    resp.once("error", reject);
  });
}

function parseErrorBody(text: string): ErrorBody | null {
  if (text.trim() === "") {
    return null;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return null;
  }
  if (isErrorBody(parsed)) {
    return parsed;
  }
  return null;
}

function isErrorBody(value: unknown): value is ErrorBody {
  if (!isRecord(value)) {
    return false;
  }
  const detail = value.error;
  if (!isRecord(detail)) {
    return false;
  }
  return typeof detail.code === "string" && typeof detail.message === "string";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseJsonTrusted<T>(text: string): T {
  // The local API schema is mirrored in dto.ts; this is the single DTO trust boundary.
  return JSON.parse(text) as T;
}

function encodePathSegment(value: string): string {
  return encodeURIComponent(value);
}
