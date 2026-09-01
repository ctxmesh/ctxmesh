import { describe, expect, it } from "vitest";

import {
  formatCompact,
  formatLatency,
  formatTokens,
  formatUSD,
  latencyStats,
  shortTraceId,
} from "@/lib/format";

describe("formatTokens", () => {
  it("renders 0 (not captured) as a dash, not a misleading '0'", () => {
    expect(formatTokens(0)).toBe("—");
  });
  it("renders a real count with locale grouping", () => {
    expect(formatTokens(1234)).toBe((1234).toLocaleString());
  });
  it("renders a non-finite value as a dash", () => {
    expect(formatTokens(NaN)).toBe("—");
  });
});

describe("formatUSD", () => {
  it("renders zero as $0.00, not a dash", () => {
    expect(formatUSD(0)).toBe("$0.00");
  });
  it("rounds sub-cent to the four-decimal register, and never to a zero-looking figure", () => {
    expect(formatUSD(0.000297)).toBe("$0.0003");
    // The floor matters more than the rounding: `$0.0000` is the one string
    // reserved as indistinguishable from an unknown, so a real cost below it
    // says it is small rather than saying it is nothing.
    expect(formatUSD(0.00002)).toBe("<$0.0001");
    expect(formatUSD(0.00002)).not.toBe("$0.0000");
  });
  it("rounds whole-dollar amounts to cents", () => {
    expect(formatUSD(12.404)).toBe("$12.40");
  });
});

describe("formatCompact", () => {
  it("compacts large token counts", () => {
    expect(formatCompact(426986)).toBe("427K");
  });
});

describe("formatLatency", () => {
  it("shows a non-positive latency as an em dash (never 0ms)", () => {
    expect(formatLatency(0)).toBe("—");
    expect(formatLatency(-5)).toBe("—");
  });
  it("shows sub-second in ms and seconds with a unit", () => {
    expect(formatLatency(450)).toBe("450ms");
    // 1.448s — the case that used to render as "1ms" before the seconds→ms fix.
    expect(formatLatency(1448)).toBe("1.45s");
  });
  it("shows minutes for very long runs", () => {
    expect(formatLatency(65000)).toBe("1m 5s");
  });
});

describe("latencyStats", () => {
  it("ignores unknown (non-positive) latencies and computes avg + p95", () => {
    const s = latencyStats([100, 0, 200, 300, -1]);
    expect(s.count).toBe(3);
    expect(s.avgMs).toBe(200);
    expect(s.p95Ms).toBe(300);
  });
  it("is all-zero for an empty set", () => {
    expect(latencyStats([])).toEqual({ count: 0, avgMs: 0, p95Ms: 0 });
  });
});

describe("shortTraceId", () => {
  it("passes through short ids and truncates long ones", () => {
    expect(shortTraceId("t-abc")).toBe("t-abc");
    expect(shortTraceId("0123456789abcdef")).toBe("0123456789ab…");
  });
});
