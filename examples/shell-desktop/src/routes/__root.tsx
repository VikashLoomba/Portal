import {
  createRootRoute,
  HeadContent,
  Outlet,
  Scripts,
  useNavigate,
  useRouterState,
} from "@tanstack/react-router";
import type { Event as PortalEvent, Status } from "@portal/sdk";
import { createContext, useContext, useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";

import type { SidecarPhase, SidecarSnapshot } from "../server/lifecycle.ts";
import appCss from "../styles.css?url";

interface PortalContextValue {
  phase: SidecarPhase;
  error: string;
  socketPath: string;
  status: Status | null;
  events: PortalEvent[];
  eventsError: string;
}

const PortalContext = createContext<PortalContextValue | null>(null);

export const Route = createRootRoute({
  head: () => ({
    meta: [
      { charSet: "utf-8" },
      { name: "viewport", content: "width=device-width, initial-scale=1" },
      { title: "Portal Shell" },
    ],
    links: [{ rel: "stylesheet", href: appCss }],
  }),
  component: RootComponent,
  shellComponent: RootDocument,
});

export function usePortal(): PortalContextValue {
  const value = useContext(PortalContext);
  if (value === null) {
    throw new Error("usePortal must be used inside PortalProvider");
  }
  return value;
}

function RootComponent() {
  return (
    <PortalProvider>
      <PortalGate />
    </PortalProvider>
  );
}

function PortalProvider({ children }: { children: ReactNode }) {
  const [snapshot, setSnapshot] = useState<SidecarSnapshot>({
    phase: "starting",
    error: "",
    socketPath: "",
    status: null,
  });
  const [portalEvents, setPortalEvents] = useState<PortalEvent[]>([]);
  const [eventsError, setEventsError] = useState("");

  useEffect(() => {
    const lifecycleSource = new EventSource("/api/lifecycle");
    lifecycleSource.onmessage = (message) => {
      const next = parseSnapshot(message.data);
      if (next !== null) {
        setSnapshot(next);
      }
    };

    const eventsSource = new EventSource("/api/events");
    eventsSource.onmessage = (message) => {
      const envelope = parseEventEnvelope(message.data);
      if (envelope === null) {
        return;
      }
      if (envelope.kind === "error") {
        setEventsError(envelope.message);
        return;
      }
      setEventsError("");
      setPortalEvents((current) => [...current.slice(-199), envelope.event]);
      if (
        envelope.event.status !== undefined && envelope.event.status !== null
      ) {
        setSnapshot((current) => ({
          ...current,
          status: envelope.event.status ?? current.status,
        }));
      }
    };
    eventsSource.onerror = () => setEventsError("events stream reconnecting");

    return () => {
      lifecycleSource.close();
      eventsSource.close();
    };
  }, []);

  const value = useMemo<PortalContextValue>(() => ({
    phase: snapshot.phase,
    error: snapshot.error,
    socketPath: snapshot.socketPath,
    status: snapshot.status,
    events: portalEvents,
    eventsError,
  }), [snapshot, portalEvents, eventsError]);

  return (
    <PortalContext.Provider value={value}>{children}</PortalContext.Provider>
  );
}

function PortalGate() {
  const portal = usePortal();
  const navigate = useNavigate();
  const pathname = useRouterState({
    select: (state) => state.location.pathname,
  });

  useEffect(() => {
    if (portal.phase !== "ready" || portal.status === null) {
      return;
    }
    const target = portal.status.host === "" ? "/onboarding" : "/shell";
    if (pathname !== target) {
      void navigate({ to: target, replace: true });
    }
  }, [navigate, pathname, portal.phase, portal.status]);

  if (portal.phase === "error") {
    return (
      <LifecyclePanel
        title="Portal could not start"
        message={portal.error}
        failed
      />
    );
  }
  if (portal.phase !== "ready" || portal.status === null) {
    return (
      <LifecyclePanel
        title="Starting Portal Shell"
        message="Waiting for the app-scoped portal sidecar…"
      />
    );
  }
  return <Outlet />;
}

function LifecyclePanel(
  { title, message, failed = false }: {
    title: string;
    message: string;
    failed?: boolean;
  },
) {
  return (
    <main className="lifecycle-screen">
      <section className="lifecycle-card">
        <div className={failed ? "status-dot failed" : "status-dot"} />
        <h1>{title}</h1>
        <p className={failed ? "is-error" : ""}>{message}</p>
        {failed && (
          <p className="remediation">
            Fix the path or permissions shown above, then relaunch. In
            development, run <code>make portal</code> first.
          </p>
        )}
      </section>
    </main>
  );
}

function RootDocument({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <head>
        <HeadContent />
      </head>
      <body>
        {children}
        <Scripts />
      </body>
    </html>
  );
}

function parseSnapshot(text: string): SidecarSnapshot | null {
  try {
    const value: unknown = JSON.parse(text);
    if (
      !isRecord(value) || !isPhase(value.phase) ||
      typeof value.error !== "string" ||
      typeof value.socketPath !== "string" ||
      !(value.status === null || isRecord(value.status))
    ) {
      return null;
    }
    return {
      phase: value.phase,
      error: value.error,
      socketPath: value.socketPath,
      status: value.status as Status | null,
    };
  } catch {
    return null;
  }
}

type EventEnvelope = { kind: "event"; event: PortalEvent } | {
  kind: "error";
  message: string;
};

function parseEventEnvelope(text: string): EventEnvelope | null {
  try {
    const value: unknown = JSON.parse(text);
    if (!isRecord(value)) {
      return null;
    }
    if (value.kind === "error" && typeof value.message === "string") {
      return { kind: "error", message: value.message };
    }
    if (value.kind === "event" && isPortalEvent(value.event)) {
      return { kind: "event", event: value.event };
    }
    return null;
  } catch {
    return null;
  }
}

function isPhase(value: unknown): value is SidecarPhase {
  return value === "starting" || value === "ready" || value === "error";
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function isPortalEvent(value: unknown): value is PortalEvent {
  return isRecord(value) && typeof value.type === "string";
}
