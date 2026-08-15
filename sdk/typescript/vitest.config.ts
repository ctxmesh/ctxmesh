import { defineConfig } from "vitest/config";

// vitest config for the ctxmesh TS SDK. `globals: true` mirrors the ui/ setup so
// tests read describe/it/expect without imports. The node environment matches the
// runtime (this is a Node library that wraps the launcher localhost plane).
export default defineConfig({
  test: {
    globals: true,
    environment: "node",
    include: ["test/**/*.{test,spec}.ts"],
  },
});
