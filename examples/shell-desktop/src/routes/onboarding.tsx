import { createFileRoute } from "@tanstack/react-router";
import type { SetupEvent } from "@portal/sdk";
import { useState } from "react";

import { readSse } from "../lib/sse.ts";
import { usePortal } from "./__root.tsx";

export const Route = createFileRoute("/onboarding")({
  component: OnboardingRoute,
});

function OnboardingRoute() {
  const portal = usePortal();
  const [host, setHost] = useState("");
  const [force, setForce] = useState(false);
  const [running, setRunning] = useState(false);
  const [steps, setSteps] = useState<SetupEvent[]>([]);
  const [message, setMessage] = useState(
    "Enter the SSH host for your development box.",
  );
  const failed = steps.some((step) => step.status === "fail");

  const startSetup = async (): Promise<void> => {
    if (running) {
      return;
    }
    setRunning(true);
    setSteps([]);
    setMessage("Setup is running. Keep this window open.");
    try {
      const response = await fetch("/api/setup", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ host, force }),
      });
      await readSse(response, (record) => {
        const event = parseSetupEvent(record.data);
        if (event === null) {
          return;
        }
        setSteps((current) => [...current, event]);
        if (event.step === "done") {
          setMessage(
            event.status === "ok"
              ? "Configured. Waiting for the existing events stream to reveal the shell…"
              : event.error?.message ??
                "Setup failed. Correct the problem or retry with force.",
          );
        }
      });
    } catch (error) {
      setMessage(toErrorMessage(error));
    } finally {
      setRunning(false);
    }
  };

  return (
    <main className="lifecycle-screen onboarding-screen">
      <section className="lifecycle-card onboarding-card">
        <p className="eyebrow">First run</p>
        <h1>Connect Portal Shell</h1>
        <p>{message}</p>
        <p className="socket">API socket: {portal.socketPath}</p>

        <form
          className="first-run-form"
          onSubmit={(event) => {
            event.preventDefault();
            void startSetup();
          }}
        >
          <label>
            SSH host
            <input
              type="text"
              value={host}
              onChange={(event) => setHost(event.target.value)}
              placeholder="devbox.example.com"
              disabled={running}
              autoFocus
            />
          </label>
          <label className="force-option">
            <input
              type="checkbox"
              checked={force}
              onChange={(event) => setForce(event.target.checked)}
              disabled={running}
            />
            Continue past validation warnings
          </label>
          <button type="submit" disabled={running || host.trim() === ""}>
            {running ? "Running…" : failed ? "Retry setup" : "Set up portal"}
          </button>
        </form>

        {steps.length > 0 && (
          <ol className="setup-steps" aria-live="polite">
            {steps.map((step, index) => (
              <li
                className={`setup-step status-${step.status}`}
                key={`${step.step}-${index}`}
              >
                <strong>{step.step}</strong>
                <span>{step.line ?? step.error?.message ?? step.status}</span>
              </li>
            ))}
          </ol>
        )}
      </section>
    </main>
  );
}

function parseSetupEvent(text: string): SetupEvent | null {
  try {
    const value: unknown = JSON.parse(text);
    if (
      typeof value !== "object" || value === null || !("step" in value) ||
      typeof value.step !== "string" ||
      !("status" in value) || typeof value.status !== "string"
    ) {
      return null;
    }
    return value as SetupEvent;
  } catch {
    return null;
  }
}

function toErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
