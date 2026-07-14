import { createClient, waitReady } from "@portal/sdk";

import { resolvePortalBinary } from "./binary.ts";
import { LifecycleStore } from "./lifecycle.ts";
import type { PortalPaths } from "./paths.ts";

const readyTimeoutMs = 15_000;
const stableUptimeMs = 30_000;
const maxRespawnAttempts = 5;

export class SidecarSupervisor {
  #paths: PortalPaths;
  #lifecycle: LifecycleStore;
  #binary = "";
  #child: Deno.ChildProcess | null = null;
  #generationAbort: AbortController | null = null;
  #respawnTimer: ReturnType<typeof setTimeout> | null = null;
  #respawnAttempts = 0;
  #started = false;
  #quitting = false;

  constructor(paths: PortalPaths, lifecycle: LifecycleStore) {
    this.#paths = paths;
    this.#lifecycle = lifecycle;
  }

  async start(): Promise<void> {
    if (this.#started) {
      return;
    }
    this.#started = true;
    try {
      await Deno.mkdir(this.#paths.configDir, { recursive: true, mode: 0o700 });
      await this.#runGeneration();
    } catch (error) {
      if (!this.#quitting) {
        this.#scheduleRespawn(toErrorMessage(error), true);
      }
    }
  }

  async stop(): Promise<void> {
    if (this.#quitting) {
      return;
    }
    this.#quitting = true;
    this.#generationAbort?.abort();
    if (this.#respawnTimer !== null) {
      clearTimeout(this.#respawnTimer);
      this.#respawnTimer = null;
    }
    const child = this.#child;
    this.#child = null;
    if (child !== null) {
      await terminateChild(child);
    }
  }

  async #runGeneration(): Promise<void> {
    if (this.#quitting) {
      return;
    }
    this.#lifecycle.setStarting();
    if (this.#binary === "") {
      try {
        this.#binary = await resolvePortalBinary(this.#paths.configDir);
      } catch (error) {
        this.#scheduleRespawn(toErrorMessage(error), true);
        return;
      }
    }
    const startedAt = Date.now();
    const abort = new AbortController();
    this.#generationAbort = abort;

    let child: Deno.ChildProcess;
    try {
      child = new Deno.Command(this.#binary, {
        args: ["run"],
        env: {
          ...Deno.env.toObject(),
          HOME: this.#paths.home,
          PORTAL_CONFIG_DIR: this.#paths.configDir,
          PORTAL_API_SOCK: this.#paths.apiSock,
          PORTAL_SOCK: this.#paths.controlSock,
        },
        stdin: "null",
        stdout: "null",
        stderr: "null",
      }).spawn();
    } catch (error) {
      throw new Error(`Unable to spawn portal: ${toErrorMessage(error)}`);
    }

    this.#child = child;
    const statusPromise = child.status;
    const readyPromise = waitReady(this.#paths.apiSock, {
      timeoutMs: readyTimeoutMs,
      signal: abort.signal,
    });

    try {
      const outcome: GenerationOutcome = await Promise.race([
        readyPromise.then((): GenerationOutcome => ({ kind: "ready" })),
        statusPromise.then((status): GenerationOutcome => ({
          kind: "departed",
          status,
        })),
      ]);
      if (outcome.kind === "departed") {
        abort.abort();
        await readyPromise.catch(() => {});
        throw new Error(departureMessage(outcome.status));
      }

      const status = await createClient(this.#paths.apiSock).status({
        signal: abort.signal,
      });
      if (this.#quitting || this.#child !== child) {
        return;
      }
      this.#lifecycle.setReady(status);

      const departure = await statusPromise;
      throw new Error(departureMessage(departure));
    } catch (error) {
      if (this.#child === child) {
        this.#child = null;
      }
      abort.abort();
      if (!this.#quitting) {
        await terminateChild(child);
        if (Date.now() - startedAt >= stableUptimeMs) {
          this.#respawnAttempts = 0;
        }
        this.#scheduleRespawn(
          toErrorMessage(error),
          this.#lifecycle.snapshot().phase !== "ready",
        );
      }
    } finally {
      if (this.#generationAbort === abort) {
        this.#generationAbort = null;
      }
    }
  }

  #scheduleRespawn(message: string, preserveError: boolean): void {
    if (this.#quitting) {
      return;
    }
    this.#respawnAttempts += 1;
    if (this.#respawnAttempts >= maxRespawnAttempts) {
      this.#lifecycle.setError(`Portal stopped repeatedly: ${message}`);
      return;
    }

    if (preserveError) {
      this.#lifecycle.setError(message);
    } else {
      this.#lifecycle.setStarting();
    }
    const backoffMs = Math.min(250 * (2 ** (this.#respawnAttempts - 1)), 4_000);
    this.#respawnTimer = setTimeout(() => {
      this.#respawnTimer = null;
      void this.#runGeneration().catch((error: unknown) => {
        if (!this.#quitting) {
          this.#scheduleRespawn(toErrorMessage(error), true);
        }
      });
    }, backoffMs);
  }
}

type GenerationOutcome = { kind: "ready" } | {
  kind: "departed";
  status: Deno.CommandStatus;
};

async function terminateChild(child: Deno.ChildProcess): Promise<void> {
  const status = child.status;
  const exitedAfterTerm = waitForStatus(status, 2_000);
  try {
    child.kill("SIGTERM");
  } catch {
    return;
  }
  if (await exitedAfterTerm) {
    return;
  }

  const exitedAfterKill = waitForStatus(status, 1_000);
  try {
    child.kill("SIGKILL");
  } catch {
    return;
  }
  await exitedAfterKill;
}

function waitForStatus(
  status: Promise<Deno.CommandStatus>,
  timeoutMs: number,
): Promise<boolean> {
  return new Promise((resolve) => {
    const timer = setTimeout(() => resolve(false), timeoutMs);
    status.then(
      () => {
        clearTimeout(timer);
        resolve(true);
      },
      () => {
        clearTimeout(timer);
        resolve(true);
      },
    );
  });
}

function departureMessage(status: Deno.CommandStatus): string {
  return status.signal === null
    ? `portal exited with code ${status.code}`
    : `portal exited from ${status.signal}`;
}

function toErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
