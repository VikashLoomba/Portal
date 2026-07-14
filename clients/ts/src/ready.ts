import { setTimeout as delay } from "node:timers/promises";

import { createClient } from "./http.ts";

export interface WaitReadyOptions {
  timeoutMs: number;
  signal?: AbortSignal;
  pollIntervalMs?: number;
}

export async function waitReady(socketPath: string, options: WaitReadyOptions): Promise<void> {
  const deadline = AbortSignal.timeout(options.timeoutMs);
  const composed = options.signal ? AbortSignal.any([options.signal, deadline]) : deadline;
  const client = createClient(socketPath);
  const timeoutError = (): Error => new Error(`waitReady: socket ${socketPath} not ready within ${options.timeoutMs}ms`);
  const throwIfAborted = (): void => {
    if (options.signal?.aborted) {
      throw options.signal.reason;
    }
    if (composed.aborted) {
      throw timeoutError();
    }
  };

  for (;;) {
    try {
      await client.available({ signal: composed });
      return;
    } catch {
      throwIfAborted();
    }

    try {
      await delay(options.pollIntervalMs ?? 50, undefined, { signal: composed });
    } catch (error) {
      throwIfAborted();
      throw error;
    }
  }
}
