import { isAbsolute, join, resolve } from "node:path";

const unixSocketPathLimit = 104;

export interface PortalPaths {
  home: string;
  configDir: string;
  apiSock: string;
  controlSock: string;
}

export function resolvePortalPaths(
  env: Record<string, string> = Deno.env.toObject(),
): PortalPaths {
  const home = env.HOME;
  if (home === undefined || home === "") {
    throw new Error("HOME is required to derive portal's app-scoped paths");
  }

  const configDir = absolute(
    env.PORTAL_CONFIG_DIR ?? join(home, ".portal-shell"),
  );
  const apiSock = absolute(env.PORTAL_API_SOCK ?? join(configDir, "api.sock"));
  const controlSock = absolute(env.PORTAL_SOCK ?? join(configDir, "cm.sock"));
  assertUnixSocketPath(apiSock, "PORTAL_API_SOCK");
  assertUnixSocketPath(controlSock, "PORTAL_SOCK");

  return { home, configDir, apiSock, controlSock };
}

export function assertUnixSocketPath(path: string, name: string): void {
  const bytes = new TextEncoder().encode(path).byteLength;
  if (bytes >= unixSocketPathLimit) {
    throw new Error(
      `${name} is ${bytes} bytes; macOS Unix socket paths must be under ${unixSocketPathLimit} bytes`,
    );
  }
}

function absolute(path: string): string {
  return isAbsolute(path) ? path : resolve(path);
}
