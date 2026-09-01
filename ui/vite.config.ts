import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { fileURLToPath, URL } from "node:url";

// Vite config for the ctxmesh SPA.
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
    // NEVER inline a font as a data: URI.
    //
    // Vite base64-inlines assets under 4KB, and one JetBrains Mono subset fell
    // under it — which the BFF's Content-Security-Policy (`font-src 'self'`,
    // internal/bff/server.go) then REFUSED on every page load. The fixture
    // sweep could not see it: `vite preview` sets no CSP, so the font loaded
    // there and failed only in production. The live walkthrough found it on all
    // 208 visits.
    //
    // Weakening the CSP to allow `data:` would have been the smaller diff and
    // the wrong fix: a strict font-src is worth more than four kilobytes, and
    // an inlined font is uncacheable besides.
    assetsInlineLimit: (filePath: string) => {
      if (/\.(woff2?|ttf|otf|eot)$/i.test(filePath)) return false;
      return undefined;
    },
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
    // Test isolation (m52.G8): the suite was flaky in the FULL run while every failing test passed in
    // isolation — the signature of state surviving a test boundary. Page tests stub `fetch` with
    // vi.stubGlobal and mocks with vi.fn; without these, a stub outlives the test that installed it, so a
    // late resolution from the previous test can render into the next one's freshly-mounted tree (the
    // observed symptom: findByText resolves a node that is no longer in the document). Restoring both
    // after every test makes each test start from the same clean global surface.
    restoreMocks: true,
    unstubGlobals: true,
    unstubEnvs: true,
  },
});
