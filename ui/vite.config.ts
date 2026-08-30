import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { fileURLToPath, URL } from "node:url";

// Vite config for the agentry SPA.
//
// - Builds to STATIC ASSETS (dist/) — no Node runtime is deployed; the Go BFF
//   serves dist/ in production (see internal/bff).
// - `@` -> src alias mirrors tsconfig paths.
// - Dev-only proxy: `/api/*` -> the Go BFF on :9090 so `pnpm dev` talks to a
//   locally running BFF exactly as the deployed SPA does (same-origin /api).
//   The proxy is a dev convenience only; it is not part of the build output.
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  build: {
    outDir: "dist",
    // Fail the build on any asset over the warn limit so bundle bloat is caught.
    chunkSizeWarningLimit: 900,
  },
  server: {
    proxy: {
      "/api": {
        target: "http://localhost:9090",
        changeOrigin: true,
      },
    },
  },
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    css: false,
    // Only unit/component tests live here (tier0). The UI e2e (Playwright) is
    // harness-owned (ADR 0004) and never imports UI source.
    include: ["src/**/*.{test,spec}.{ts,tsx}"],
  },
});
