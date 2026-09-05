import { describe, expect, it } from "vitest";

import { mapLimit } from "./map-limit";

const tick = () => new Promise((r) => setTimeout(r, 0));

describe("mapLimit", () => {
  it("never exceeds the concurrency limit", async () => {
    let inFlight = 0;
    let peak = 0;
    await mapLimit(Array.from({ length: 40 }, (_, i) => i), 5, async (n) => {
      inFlight += 1;
      peak = Math.max(peak, inFlight);
      await tick();
      inFlight -= 1;
      return n;
    });
    expect(peak).toBeLessThanOrEqual(5);
    expect(peak).toBeGreaterThan(1); // and it is genuinely concurrent, not serial
  });

  it("returns results in input order, not completion order", async () => {
    const out = await mapLimit([30, 10, 20, 0], 4, async (ms, i) => {
      await new Promise((r) => setTimeout(r, ms));
      return i;
    });
    expect(out).toEqual([0, 1, 2, 3]);
  });

  it("runs every item — a scan that skips a workspace would under-report", async () => {
    const seen: number[] = [];
    await mapLimit(Array.from({ length: 25 }, (_, i) => i), 4, async (n) => {
      seen.push(n);
      return n;
    });
    expect(seen.sort((a, b) => a - b)).toEqual(Array.from({ length: 25 }, (_, i) => i));
  });

  it("handles an empty list without opening a worker", async () => {
    expect(await mapLimit([], 8, async () => 1)).toEqual([]);
  });
});
