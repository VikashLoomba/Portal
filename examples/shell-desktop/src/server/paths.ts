import { isAbsolute, join, resolve } from "node:path";

const unixSocketPathLimit = 104;

export interface PortalPaths {
  home: string;
  configDir: string;
  apiSock: string;
  controlSock: string;
}

// The desktop tasks run with a scoped --allow-env list, so environment reads are
// enumerated explicitly — Deno.env.toObject() would demand blanket env access.
const portalEnvNames = [
  "HOME",
  "PATH",
  "PORTAL_BIN",
  "PORTAL_CONFIG_DIR",
  "PORTAL_API_SOCK",
  "PORTAL_SOCK",
] as const;

export function readPortalEnv(): Record<string, string> {
  const env: Record<string, string> = {};
  for (const name of portalEnvNames) {
    const value = Deno.env.get(name);
    if (value !== undefined) {
      env[name] = value;
    }
  }
  return env;
}

export function resolvePortalPaths(
  env: Record<string, string> = readPortalEnv(),
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
