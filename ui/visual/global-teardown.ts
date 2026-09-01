// global-teardown.ts — assemble the sweep report from the workers' shards (M151).
//
// Playwright runs workers in separate processes, so no single one of them sees
// the whole run. They each write a shard here; this merges them once, when the
// run is genuinely over, and deletes the shards. The previous read-modify-write
// merge in the spec's afterAll was a lost update across processes — a four-worker
// sweep reported 18 of 24 renders while 24 screenshots sat on disk.

import fs from "node:fs";
import path from "node:path";

interface Row {
  documentScrollsX: boolean;
  offenders: unknown[];
  id: string;
  width: number;
  theme: string;
}

export default function globalTeardown(): void {
  const label = process.env.VISUAL_LABEL ?? process.env.VISUAL_MODE ?? "populated";
  const reportDir = path.resolve("visual/report");
  const shardDir = path.join(reportDir, ".shards");
  if (!fs.existsSync(shardDir)) return;

  const rows: Row[] = [];
  for (const f of fs.readdirSync(shardDir)) {
    if (!f.startsWith(`${label}.`) || !f.endsWith(".json")) continue;
    rows.push(...(JSON.parse(fs.readFileSync(path.join(shardDir, f), "utf8")) as Row[]));
  }
  // Deterministic order so two runs of the same commit produce the same file.
  rows.sort((a, b) => a.id.localeCompare(b.id) || a.width - b.width || a.theme.localeCompare(b.theme));

  const failing = rows.filter((r) => r.documentScrollsX || r.offenders.length > 0);
  fs.mkdirSync(reportDir, { recursive: true });
  fs.writeFileSync(
    path.join(reportDir, `${label}.json`),
    JSON.stringify(
      {
        label,
        mode: process.env.VISUAL_MODE ?? "populated",
        generated: new Date().toISOString(),
        totals: { renders: rows.length, clean: rows.length - failing.length, failing: failing.length },
        rows,
      },
      null,
      2,
    ),
  );
  fs.rmSync(shardDir, { recursive: true, force: true });
}
