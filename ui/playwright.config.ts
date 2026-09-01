// playwright.config.ts — the visual sweep (M151).
//
// This is NOT a functional e2e suite; the black-box e2e lives in the brain's
// harness (ADR 0004). This exists for one thing the type-checker and the unit
// tests cannot do: render every route at every width in both themes and assert
// that it fits, then leave a screenshot a person can look at.
//
// It runs against `vite preview` over the real production build, not the dev
// server, so what is asserted is what ships.

import { defineConfig, devices } from "@playwright/test";

const PORT = Number(process.env.VISUAL_PORT ?? 4173);

export default defineConfig({
  testDir: "./visual",
  globalSetup: "./visual/global-setup.ts",
  globalTeardown: "./visual/global-teardown.ts",
  testMatch: /.*\.spec\.ts$/,
  outputDir: "./visual/.artifacts",
  fullyParallel: true,
  workers: process.env.CI ? 2 : 4,
  retries: 0,
  reporter: [["list"], ["json", { outputFile: "visual/report/playwright.json" }]],
  timeout: 60_000,
  expect: { timeout: 10_000 },
  use: {
    ...devices["Desktop Chrome"],
    baseURL: `http://127.0.0.1:${PORT}`,
    // Deterministic rendering: fixed DPR, locale and clock, and no animation —
    // a screenshot that differs run to run is a screenshot nobody trusts.
    deviceScaleFactor: 1,
    locale: "en-GB",
    timezoneId: "UTC",
    contextOptions: { reducedMotion: "reduce" },
  },
  webServer: {
    command: `pnpm exec vite preview --host 127.0.0.1 --port ${PORT} --strictPort`,
    port: PORT,
    reuseExistingServer: !process.env.CI,
    timeout: 120_000,
  },
});
