import { events, setup } from "@portal/sdk";
import type { SetupRequest } from "@portal/sdk";

import { lifecycle, portalPaths } from "./boot.ts";
import type { SidecarSnapshot } from "./lifecycle.ts";
import { isRecord } from "./exec-normalize.ts";

const sseHeaders = {
  "Cache-Control": "no-cache, no-transform",
  "Content-Type": "text/event-stream; charset=utf-8",
  "X-Accel-Buffering": "no",
};
const encoder = new TextEncoder();

export function lifecycleResponse(request: Request): Response {
  let unsubscribe = () => {};
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      const send = (snapshot: SidecarSnapshot): void =>
        enqueue(controller, snapshot);
      unsubscribe = lifecycle.subscribe(send);
      send(lifecycle.snapshot());
      request.signal.addEventListener("abort", () => {
        unsubscribe();
        close(controller);
      }, { once: true });
    },
    cancel() {
      unsubscribe();
    },
  });
  return new Response(stream, { headers: sseHeaders });
}

export function eventsResponse(request: Request): Response {
  const abort = new AbortController();
  request.signal.addEventListener("abort", () => abort.abort(), { once: true });
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      void pumpEvents(controller, abort.signal).finally(() =>
        close(controller)
      );
    },
    cancel() {
      abort.abort();
    },
  });
  return new Response(stream, { headers: sseHeaders });
}

export async function setupResponse(request: Request): Promise<Response> {
  let value: unknown;
  try {
    value = await request.json();
  } catch {
    return Response.json({ error: "invalid JSON body" }, { status: 400 });
  }
  const setupRequest = normalizeSetupRequest(value);
  if (setupRequest === null) {
    return Response.json({ error: "invalid setup request" }, { status: 400 });
  }

  let paths;
  try {
    paths = portalPaths();
  } catch (error) {
    return Response.json({ error: toErrorMessage(error) }, { status: 503 });
  }
  if (lifecycle.snapshot().phase !== "ready") {
    return Response.json({ error: "portal sidecar is not ready" }, {
      status: 503,
    });
  }

  const abort = new AbortController();
  request.signal.addEventListener("abort", () => abort.abort(), { once: true });
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      void (async () => {
        try {
          for await (
            const event of setup(paths.apiSock, setupRequest, {
              signal: abort.signal,
            })
          ) {
            enqueue(controller, event);
          }
        } catch (error) {
          enqueue(controller, {
            step: "done",
            status: "fail",
            error: { code: "bridge", message: toErrorMessage(error) },
          });
        } finally {
          close(controller);
        }
      })();
    },
    cancel() {
      abort.abort();
    },
  });
  return new Response(stream, { headers: sseHeaders });
}

async function pumpEvents(
  controller: ReadableStreamDefaultController<Uint8Array>,
  signal: AbortSignal,
): Promise<void> {
  while (!signal.aborted) {
    await waitUntilReady(signal);
    if (signal.aborted) {
      return;
    }
    try {
      const paths = portalPaths();
      for await (const event of events(paths.apiSock, { signal })) {
        if (event.status !== undefined && event.status !== null) {
          lifecycle.updateStatus(event.status);
        }
        enqueue(controller, { kind: "event", event });
      }
    } catch (error) {
      if (!signal.aborted) {
        enqueue(controller, { kind: "error", message: toErrorMessage(error) });
        await delay(1_500, signal);
      }
    }
  }
}

function waitUntilReady(signal: AbortSignal): Promise<void> {
  if (signal.aborted || lifecycle.snapshot().phase === "ready") {
    return Promise.resolve();
  }
  return new Promise((resolve) => {
    const unsubscribe = lifecycle.subscribe((snapshot) => {
      if (snapshot.phase === "ready") {
        unsubscribe();
        signal.removeEventListener("abort", onAbort);
        resolve();
      }
    });
    const onAbort = (): void => {
      unsubscribe();
      resolve();
    };
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

function normalizeSetupRequest(value: unknown): SetupRequest | null {
  if (!isRecord(value)) {
    return null;
  }
  return {
    host: typeof value.host === "string" ? value.host : "",
    force: value.force === true,
  };
}

function enqueue(
  controller: ReadableStreamDefaultController<Uint8Array>,
  value: unknown,
): void {
  try {
    controller.enqueue(encoder.encode(`data: ${JSON.stringify(value)}\n\n`));
  } catch {
    // The request was cancelled between the producer's abort check and enqueue.
  }
}

function close(controller: ReadableStreamDefaultController<Uint8Array>): void {
  try {
    controller.close();
  } catch {
    // Cancellation may close the stream before the producer unwinds.
  }
}

function delay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve();
      return;
    }
    const timer = setTimeout(resolve, ms);
    signal.addEventListener("abort", () => {
      clearTimeout(timer);
      resolve();
    }, { once: true });
  });
}

function toErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
