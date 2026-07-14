import { mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:http";
import type { IncomingMessage, Server, ServerResponse } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import type { Socket } from "node:net";
import type { TestContext } from "node:test";

export type FakeHttpHandler = (req: IncomingMessage, resp: ServerResponse) => void | Promise<void>;

export class FakeHttpServer {
  path: string;
  server: Server;
  done: Promise<void>;

  private constructor(socketPath: string, server: Server, done: Promise<void>) {
    this.path = socketPath;
    this.server = server;
    this.done = done;
  }

  static async start(t: TestContext, handler: FakeHttpHandler): Promise<FakeHttpServer> {
    const dir = mkdtempSync(path.join(tmpdir(), "pt-http-"));
    const socketPath = path.join(dir, "s.sock");
    const sockets = new Set<Socket>();
    let handlerError: Error | null = null;
    let doneSettled = false;
    let resolveDone: () => void = () => {};
    let rejectDone: (error: Error) => void = () => {};
    const done = new Promise<void>((resolve, reject) => {
      resolveDone = resolve;
      rejectDone = reject;
    });
    void done.catch(() => {});

    const server = createServer((req, resp) => {
      void (async () => {
        try {
          await handler(req, resp);
          if (!doneSettled) {
            doneSettled = true;
            resolveDone();
          }
        } catch (error) {
          const err = toError(error);
          handlerError ??= err;
          if (!doneSettled) {
            doneSettled = true;
            rejectDone(err);
          }
          resp.destroy(err);
        }
      })();
    });
    server.on("connection", (socket) => {
      sockets.add(socket);
      socket.once("close", () => sockets.delete(socket));
    });
    await new Promise<void>((resolve, reject) => {
      server.once("error", reject);
      server.listen(socketPath, resolve);
    });

    t.after(async () => {
      try {
        await closeServer(server, sockets);
        if (handlerError !== null) {
          throw handlerError;
        }
      } finally {
        rmSync(dir, { recursive: true, force: true });
      }
    });
    return new FakeHttpServer(socketPath, server, done);
  }
}

function closeServer(server: Server, sockets: Set<Socket>): Promise<void> {
  return new Promise((resolve, reject) => {
    if (!server.listening) {
      resolve();
      return;
    }
    server.close((error) => {
      if (error === undefined) {
        resolve();
      } else {
        reject(error);
      }
    });
    for (const socket of sockets) {
      socket.destroy();
    }
  });
}

function toError(value: unknown): Error {
  if (value instanceof Error) {
    return value;
  }
  if (typeof value === "string") {
    return new Error(value);
  }
  return new Error("unknown fake HTTP server error");
}
