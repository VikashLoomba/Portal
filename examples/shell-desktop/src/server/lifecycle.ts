import type { Status } from "@portal/sdk";

export type SidecarPhase = "starting" | "ready" | "error";

export interface SidecarSnapshot {
  phase: SidecarPhase;
  error: string;
  socketPath: string;
  status: Status | null;
}

type SidecarListener = (snapshot: SidecarSnapshot) => void;

export class LifecycleStore {
  #snapshot: SidecarSnapshot = {
    phase: "starting",
    error: "",
    socketPath: "",
    status: null,
  };
  #listeners = new Set<SidecarListener>();

  snapshot(): SidecarSnapshot {
    return this.#snapshot;
  }

  setSocketPath(socketPath: string): void {
    this.#publish({ ...this.#snapshot, socketPath });
  }

  setStarting(): void {
    this.#publish({
      ...this.#snapshot,
      phase: "starting",
      error: "",
      status: null,
    });
  }

  setReady(status: Status): void {
    this.#publish({ ...this.#snapshot, phase: "ready", error: "", status });
  }

  setError(error: string): void {
    this.#publish({ ...this.#snapshot, phase: "error", error, status: null });
  }

  updateStatus(status: Status): void {
    if (this.#snapshot.phase === "ready") {
      this.#publish({ ...this.#snapshot, status });
    }
  }

  subscribe(listener: SidecarListener): () => void {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }

  #publish(snapshot: SidecarSnapshot): void {
    this.#snapshot = snapshot;
    for (const listener of this.#listeners) {
      listener(snapshot);
    }
  }
}
