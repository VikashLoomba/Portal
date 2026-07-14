import handler, { createServerEntry } from "@tanstack/react-start/server-entry";

import "./server/boot.ts";
import { handleExecUpgrade } from "./server/exec-bridge.ts";
import {
  eventsResponse,
  lifecycleResponse,
  setupResponse,
} from "./server/portal-proxy.ts";

export default createServerEntry({
  fetch(request) {
    const pathname = new URL(request.url).pathname;
    if (pathname === "/exec") {
      return handleExecUpgrade(request);
    }
    if (pathname === "/api/lifecycle") {
      return request.method === "GET"
        ? lifecycleResponse(request)
        : methodNotAllowed();
    }
    if (pathname === "/api/events") {
      return request.method === "GET"
        ? eventsResponse(request)
        : methodNotAllowed();
    }
    if (pathname === "/api/setup") {
      return request.method === "POST"
        ? setupResponse(request)
        : methodNotAllowed();
    }
    return handler.fetch(request);
  },
});

function methodNotAllowed(): Response {
  return new Response("method not allowed", { status: 405 });
}
