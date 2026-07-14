import { strict as assert } from "node:assert";
import { resolve } from "node:path";

import { selectBinarySource } from "./binary.ts";
import { normalizeExecSize, normalizeExecStart } from "./exec-normalize.ts";
import { resolvePortalPaths } from "./paths.ts";

Deno.test("derives all portal paths inside the app scope", () => {
  const paths = resolvePortalPaths({ HOME: "/Users/test" });
  assert.equal(paths.configDir, "/Users/test/.portal-shell");
  assert.equal(paths.apiSock, "/Users/test/.portal-shell/api.sock");
  assert.equal(paths.controlSock, "/Users/test/.portal-shell/cm.sock");
});

Deno.test("honors explicit app-scoped path overrides", () => {
  const paths = resolvePortalPaths({
    HOME: "/Users/test",
    PORTAL_CONFIG_DIR: "/tmp/ps-config",
    PORTAL_API_SOCK: "/tmp/ps-api.sock",
    PORTAL_SOCK: "/tmp/ps-cm.sock",
  });
  assert.equal(paths.configDir, "/tmp/ps-config");
  assert.equal(paths.apiSock, "/tmp/ps-api.sock");
  assert.equal(paths.controlSock, "/tmp/ps-cm.sock");
});

Deno.test("rejects a Unix socket path at the macOS sun_path limit", () => {
  assert.throws(
    () => resolvePortalPaths({ HOME: `/${"x".repeat(100)}` }),
    /must be under 104 bytes/,
  );
});

Deno.test("resolves PORTAL_BIN before packaged and development binaries", async () => {
  const selected = await selectBinarySource({
    env: { PORTAL_BIN: "/custom/portal" },
    packaged: true,
    moduleDir: "/vfs/.output/server/chunks",
    cwd: "/repo/examples/shell-desktop",
  }, (path) => path === "/custom/portal" || path === "/vfs/portal");
  assert.deepEqual(selected, { kind: "override", path: "/custom/portal" });
});

Deno.test("finds the packaged resource from the framework output", async () => {
  const selected = await selectBinarySource({
    env: {},
    packaged: true,
    moduleDir: "/vfs/.output/server/chunks",
    cwd: "/repo/examples/shell-desktop",
  }, (path) => path === "/vfs/portal");
  assert.deepEqual(selected, { kind: "packaged", path: "/vfs/portal" });
});

Deno.test("uses the repository-root development binary", async () => {
  const cwd = "/repo/examples/shell-desktop";
  const development = resolve(cwd, "../../portal");
  const selected = await selectBinarySource({
    env: {},
    packaged: false,
    moduleDir: "/repo/examples/shell-desktop/src/server",
    cwd,
  }, (path) => path === development);
  assert.deepEqual(selected, { kind: "development", path: "/repo/portal" });
});

Deno.test("normalizes exec start and resize bounds", () => {
  const argv: unknown[] = Array.from(
    { length: 40 },
    (_, index) => index % 2 === 0 ? `arg-${index}` : index,
  );
  const start = normalizeExecStart({ argv, term: "", rows: 2, cols: 900 });
  assert.notEqual(start, null);
  if (start === null) {
    throw new Error("normalizeExecStart returned null");
  }
  assert.equal(start.argv.length, 20);
  assert.equal(start.term, "xterm-256color");
  assert.equal(start.rows, 6);
  assert.equal(start.cols, 500);

  assert.deepEqual(normalizeExecSize({ rows: "bad", cols: 10 }), {
    rows: 24,
    cols: 20,
  });
});
