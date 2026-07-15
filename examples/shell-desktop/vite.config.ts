import { fileURLToPath } from "node:url";

import { tanstackStart } from "@tanstack/react-start/plugin/vite";
import viteReact from "@vitejs/plugin-react";
import { nitro } from "nitro/vite";
import { defineConfig } from "vite";

const repoRoot = fileURLToPath(new URL("../..", import.meta.url));
const sdkEntry = fileURLToPath(
  new URL("../../clients/ts/src/index.ts", import.meta.url),
);

export default defineConfig({
  resolve: {
    alias: {
      "@portal/sdk": sdkEntry,
    },
  },
  server: {
    fs: {
      allow: [repoRoot],
    },
  },
  plugins: [nitro(), tanstackStart(), viteReact()],
});
