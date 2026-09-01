// global-setup.ts — start every sweep from a clean slate (M151).
//
// The spec's afterAll hook MERGES its rows into the report file, because
// Playwright runs workers in separate processes and each holds only its own
// slice. That merge is correct within a run and wrong across runs: without this
// setup, a second sweep appends to the first and the totals double.

import fs from "node:fs";
import path from "node:path";

export default function globalSetup(): void {
  const label = process.env.VISUAL_LABEL ?? process.env.VISUAL_MODE ?? "populated";
  for (const p of [
    path.resolve("visual/report", `${label}.json`),
    path.resolve("visual/report/.shards"),
    path.resolve("visual/shots", label),
  ]) {
    fs.rmSync(p, { recursive: true, force: true });
  }
}
